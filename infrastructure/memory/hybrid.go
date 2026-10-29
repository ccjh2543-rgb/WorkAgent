package memory

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
)

// 混合检索默认参数
const (
	// defaultRRFK RRF 平滑常数，值越大越弱化高位排名之间的差距
	defaultRRFK = 60
	// defaultPoolSize 每一路召回的候选数量，融合后再截断到 topK
	defaultPoolSize = 20
)

// HybridRetriever 混合检索器：
// 先把索引同步到真源最新状态并补齐缺失向量，再分别做关键词召回与向量召回，
// 最后用 RRF（Reciprocal Rank Fusion）融合两路结果。
//
// 之所以用 RRF 而不是加权求和：bm25 得分与余弦相似度量纲不可比，
// 手工调权重既不稳定也难以解释；RRF 只用排名，天然规避了这个问题。
type HybridRetriever struct {
	index    Index
	embedder Embedder
	source   string
	rrfK     int
	poolSize int
}

// NewHybridRetriever 创建混合检索器。
// source 为真源标识（通常是 MEMORY.md 的路径），会写入索引用于标记来源。
func NewHybridRetriever(index Index, embedder Embedder, source string) (*HybridRetriever, error) {
	if index == nil {
		return nil, errors.New("记忆索引不能为空")
	}
	if embedder == nil {
		return nil, errors.New("embedding 实现不能为空")
	}
	if source == "" {
		return nil, errors.New("记忆真源标识不能为空")
	}
	return &HybridRetriever{
		index:    index,
		embedder: embedder,
		source:   source,
		rrfK:     defaultRRFK,
		poolSize: defaultPoolSize,
	}, nil
}

// Rank 实现 Retriever 接口。
// candidates 是真源解析出的记忆集合，用于把索引同步到最新状态。
// 任一步失败都会返回错误，由上层决定降级策略，不做静默吞错。
func (h *HybridRetriever) Rank(ctx context.Context, query string, candidates []Fragment, topK int) ([]Fragment, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if topK <= 0 {
		return nil, nil
	}
	// 同步必须放在所有提前返回之前：用户手动增删记忆后，即使本轮查询
	// 走的是回退分支、或真源已被清空，索引也要跟着真源一起更新，
	// 否则索引会残留已删除的记忆。同步本身只做一次哈希集合比对，开销很小。
	if err := h.index.Sync(ctx, h.source, candidates); err != nil {
		return nil, err
	}
	if len(candidates) == 0 {
		return nil, nil
	}
	// 查询串无法切出任何检索词时，退回真源顺序，避免把空串送去向量化
	if len(Tokenize(query)) == 0 {
		return firstN(candidates, topK), nil
	}

	if err := h.ensureVectors(ctx); err != nil {
		return nil, err
	}

	keywordHits, err := h.index.KeywordSearch(ctx, query, h.poolSize)
	if err != nil {
		return nil, err
	}

	queryVector, err := h.embedQuery(ctx, query)
	if err != nil {
		return nil, err
	}
	vectorHits, err := h.index.VectorSearch(ctx, h.embedder.Model(), queryVector, h.poolSize)
	if err != nil {
		return nil, err
	}

	return fuseRRF(h.rrfK, topK, keywordHits, vectorHits), nil
}

// ensureVectors 为尚未向量化的记忆补齐向量。
// 已生成过向量的记忆不会重复请求，因此新增一条记忆只产生一次 embedding 调用。
func (h *HybridRetriever) ensureVectors(ctx context.Context) error {
	pending, err := h.index.PendingTexts(ctx, h.embedder.Model())
	if err != nil {
		return err
	}
	if len(pending) == 0 {
		return nil
	}

	texts := make([]string, 0, len(pending))
	for _, p := range pending {
		texts = append(texts, p.Text)
	}
	vectors, err := h.embedder.Embed(ctx, texts)
	if err != nil {
		return err
	}
	if len(vectors) != len(pending) {
		return fmt.Errorf("向量数量与待处理记忆数量不匹配: got %d, want %d", len(vectors), len(pending))
	}

	vectorMap := make(map[string][]float32, len(pending))
	for i, p := range pending {
		vectorMap[p.Hash] = vectors[i]
	}
	return h.index.SaveVectors(ctx, h.embedder.Model(), vectorMap)
}

// embedQuery 把查询串向量化
func (h *HybridRetriever) embedQuery(ctx context.Context, query string) ([]float32, error) {
	text := strings.TrimSpace(query)
	vectors, err := h.embedder.Embed(ctx, []string{text})
	if err != nil {
		return nil, err
	}
	if len(vectors) != 1 {
		return nil, fmt.Errorf("查询向量数量异常: got %d, want 1", len(vectors))
	}
	return vectors[0], nil
}

// fuseRRF 用 RRF 融合多路召回结果，得分公式为 Σ 1/(k + rank)。
// 多路结果中重复出现的记忆会累加得分，因此被两路同时命中的记忆排名更靠前。
// lists 的顺序即优先级：得分相同时先出现的排前面。
func fuseRRF(k, topK int, lists ...[]MemoryRecord) []Fragment {
	type fused struct {
		record MemoryRecord
		score  float64
		order  int
	}

	byHash := make(map[string]*fused)
	order := 0
	for _, list := range lists {
		for rank, record := range list {
			hash := TextHash(record.Section, record.Content)
			item, ok := byHash[hash]
			if !ok {
				item = &fused{record: record, order: order}
				order++
				byHash[hash] = item
			}
			item.score += 1 / float64(k+rank+1)
		}
	}
	if len(byHash) == 0 {
		return nil
	}

	merged := make([]fused, 0, len(byHash))
	for _, item := range byHash {
		merged = append(merged, *item)
	}
	sort.SliceStable(merged, func(a, b int) bool {
		if merged[a].score != merged[b].score {
			return merged[a].score > merged[b].score
		}
		return merged[a].order < merged[b].order
	})

	fragments := make([]Fragment, 0, min(topK, len(merged)))
	for _, item := range merged {
		if len(fragments) >= topK {
			break
		}
		fragments = append(fragments, item.record.Fragment)
	}
	return fragments
}
