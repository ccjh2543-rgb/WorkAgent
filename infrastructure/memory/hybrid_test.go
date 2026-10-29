package memory

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// fakeEmbedder 用固定关键词规则生成向量，避免测试依赖真实的 embedding 服务。
// 每条文本映射为「命中关键词标记」组成的定长向量。
type fakeEmbedder struct {
	model    string
	keywords []string
	calls    [][]string // 记录每次请求的文本，用于验证向量缓存是否生效
	err      error
}

func (f *fakeEmbedder) Model() string { return f.model }

func (f *fakeEmbedder) Embed(ctx context.Context, texts []string) ([][]float32, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	f.calls = append(f.calls, append([]string(nil), texts...))
	if f.err != nil {
		return nil, f.err
	}
	vectors := make([][]float32, len(texts))
	for i, text := range texts {
		vec := make([]float32, len(f.keywords))
		for k, keyword := range f.keywords {
			if strings.Contains(text, keyword) {
				vec[k] = 1
			}
		}
		vectors[i] = vec
	}
	return vectors, nil
}

// fakeIndex 内存索引，用于在不依赖 SQLite 的前提下测试混合检索的编排逻辑
type fakeIndex struct {
	fragments  []Fragment
	vectors    map[string][]float32
	keyword    []MemoryRecord
	vector     []MemoryRecord
	syncErr    error
	searchErr  error
	syncCalls  int
	savedCalls int
}

func newFakeIndex() *fakeIndex {
	return &fakeIndex{vectors: make(map[string][]float32)}
}

func (f *fakeIndex) Sync(_ context.Context, _ string, fragments []Fragment) error {
	f.syncCalls++
	if f.syncErr != nil {
		return f.syncErr
	}
	f.fragments = append([]Fragment(nil), fragments...)
	return nil
}

func (f *fakeIndex) PendingTexts(_ context.Context, _ string) ([]PendingText, error) {
	pending := make([]PendingText, 0, len(f.fragments))
	for _, frag := range f.fragments {
		hash := TextHash(frag.Section, frag.Content)
		if _, ok := f.vectors[hash]; ok {
			continue
		}
		pending = append(pending, PendingText{Hash: hash, Text: EmbedText(frag.Section, frag.Content)})
	}
	return pending, nil
}

func (f *fakeIndex) SaveVectors(_ context.Context, _ string, vectors map[string][]float32) error {
	f.savedCalls++
	for hash, vec := range vectors {
		f.vectors[hash] = vec
	}
	return nil
}

func (f *fakeIndex) KeywordSearch(_ context.Context, _ string, _ int) ([]MemoryRecord, error) {
	if f.searchErr != nil {
		return nil, f.searchErr
	}
	return f.keyword, nil
}

func (f *fakeIndex) VectorSearch(_ context.Context, _ string, _ []float32, _ int) ([]MemoryRecord, error) {
	if f.searchErr != nil {
		return nil, f.searchErr
	}
	return f.vector, nil
}

func (f *fakeIndex) Close() error { return nil }

func newTestHybridRetriever(t *testing.T, index Index, embedder Embedder) *HybridRetriever {
	t.Helper()
	retriever, err := NewHybridRetriever(index, embedder, "MEMORY.md")
	if err != nil {
		t.Fatalf("创建混合检索器失败: %v", err)
	}
	return retriever
}

// TestHybridRetrieverRankFusesBothBranches 两路同时命中的记忆必须排在最前
func TestHybridRetrieverRankFusesBothBranches(t *testing.T) {
	ctx := context.Background()
	fragments := []Fragment{
		{Scope: ScopeWorkspace, Section: "架构约定", Content: "严格 DDD 分层"},
		{Scope: ScopeWorkspace, Section: "沟通偏好", Content: "优先中文输出"},
		{Scope: ScopeWorkspace, Section: "交付规范", Content: "提交前执行 go test"},
	}

	index := newFakeIndex()
	index.keyword = []MemoryRecord{
		newIndexRecord("架构约定", "严格 DDD 分层", 0.9),
		newIndexRecord("交付规范", "提交前执行 go test", 0.5),
	}
	index.vector = []MemoryRecord{
		newIndexRecord("架构约定", "严格 DDD 分层", 0.99),
		newIndexRecord("沟通偏好", "优先中文输出", 0.88),
	}
	embedder := &fakeEmbedder{model: "fake-model", keywords: []string{"分层", "中文", "test"}}

	got, err := newTestHybridRetriever(t, index, embedder).Rank(ctx, "架构分层", fragments, 3)
	if err != nil {
		t.Fatalf("混合检索失败: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("期望返回 3 条, got %d", len(got))
	}
	if got[0].Section != "架构约定" {
		t.Fatalf("两路同时命中的记忆应排在首位, got %q", got[0].Section)
	}
	// 仅被单路命中的两条得分相同，此时保持「关键词路优先」的稳定顺序
	if got[1].Section != "交付规范" || got[2].Section != "沟通偏好" {
		t.Fatalf("同分时应保持关键词路优先, got %q, %q", got[1].Section, got[2].Section)
	}
}

// TestHybridRetrieverEmbedsEachMemoryOnce 记忆向量只生成一次，后续检索不再重复调用
func TestHybridRetrieverEmbedsEachMemoryOnce(t *testing.T) {
	ctx := context.Background()
	fragments := []Fragment{
		{Scope: ScopeWorkspace, Section: "架构约定", Content: "严格 DDD 分层"},
	}
	index := newFakeIndex()
	embedder := &fakeEmbedder{model: "fake-model", keywords: []string{"分层"}}
	retriever := newTestHybridRetriever(t, index, embedder)

	for i := 0; i < 3; i++ {
		if _, err := retriever.Rank(ctx, "架构分层", fragments, 3); err != nil {
			t.Fatalf("第 %d 轮混合检索失败: %v", i+1, err)
		}
	}

	if len(index.vectors) != 1 {
		t.Fatalf("记忆向量应只生成一次, got %d 条", len(index.vectors))
	}
	if index.savedCalls != 1 {
		t.Fatalf("向量应只落库一次, got %d 次", index.savedCalls)
	}
	// 首次为记忆批量向量化 + 查询向量化，之后每轮只有查询向量化
	if len(embedder.calls) != 4 {
		t.Fatalf("embedding 调用次数不匹配: got %d, want 4", len(embedder.calls))
	}
}

// TestHybridRetrieverPropagatesErrors 任一依赖失败都必须向上返回错误，不能静默吞掉
func TestHybridRetrieverPropagatesErrors(t *testing.T) {
	ctx := context.Background()
	fragments := []Fragment{{Scope: ScopeWorkspace, Section: "架构约定", Content: "严格 DDD 分层"}}

	t.Run("索引同步失败", func(t *testing.T) {
		index := newFakeIndex()
		index.syncErr = errors.New("同步失败")
		retriever := newTestHybridRetriever(t, index, &fakeEmbedder{model: "m"})

		if _, err := retriever.Rank(ctx, "架构分层", fragments, 3); err == nil {
			t.Fatal("索引同步失败时应返回错误")
		}
	})

	t.Run("embedding 失败", func(t *testing.T) {
		index := newFakeIndex()
		embedder := &fakeEmbedder{model: "m", err: errors.New("embedding 服务不可用")}
		retriever := newTestHybridRetriever(t, index, embedder)

		if _, err := retriever.Rank(ctx, "架构分层", fragments, 3); err == nil {
			t.Fatal("embedding 失败时应返回错误")
		}
	})

	t.Run("检索失败", func(t *testing.T) {
		index := newFakeIndex()
		index.searchErr = errors.New("查询失败")
		retriever := newTestHybridRetriever(t, index, &fakeEmbedder{model: "m"})

		if _, err := retriever.Rank(ctx, "架构分层", fragments, 3); err == nil {
			t.Fatal("检索失败时应返回错误")
		}
	})
}

// TestNewHybridRetrieverValidatesArgs 依赖缺失必须在构造期暴露
func TestNewHybridRetrieverValidatesArgs(t *testing.T) {
	tests := []struct {
		name     string
		index    Index
		embedder Embedder
		source   string
	}{
		{"索引为空", nil, &fakeEmbedder{model: "m"}, "MEMORY.md"},
		{"embedder 为空", newFakeIndex(), nil, "MEMORY.md"},
		{"真源标识为空", newFakeIndex(), &fakeEmbedder{model: "m"}, ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := NewHybridRetriever(tt.index, tt.embedder, tt.source); err == nil {
				t.Fatal("参数不合法时应返回错误")
			}
		})
	}
}

// TestHybridRetrieverFallsBackForTokenlessQuery 切不出词元的查询直接回退：
// 索引仍要同步（保证索引跟随真源），但不该触发任何 embedding 调用。
func TestHybridRetrieverFallsBackForTokenlessQuery(t *testing.T) {
	ctx := context.Background()
	fragments := []Fragment{
		{Scope: ScopeWorkspace, Section: "架构约定", Content: "严格 DDD 分层"},
		{Scope: ScopeWorkspace, Section: "沟通偏好", Content: "优先中文输出"},
	}
	index := newFakeIndex()
	embedder := &fakeEmbedder{model: "m"}
	retriever := newTestHybridRetriever(t, index, embedder)

	got, err := retriever.Rank(ctx, "a b", fragments, 1)
	if err != nil {
		t.Fatalf("混合检索失败: %v", err)
	}
	if len(got) != 1 || got[0] != fragments[0] {
		t.Fatalf("应回退为真源顺序首条, got %+v", got)
	}
	if index.syncCalls != 1 {
		t.Fatalf("回退路径仍应同步索引, got %d 次", index.syncCalls)
	}
	if len(embedder.calls) != 0 {
		t.Fatalf("回退路径不应调用 embedding, got %d 次", len(embedder.calls))
	}
}

// TestHybridRetrieverSyncsWhenSourceEmptied 真源被清空时索引也必须跟着清空，
// 否则用户删掉全部记忆后，索引里会残留已不存在的记忆。
func TestHybridRetrieverSyncsWhenSourceEmptied(t *testing.T) {
	ctx := context.Background()
	index := newFakeIndex()
	embedder := &fakeEmbedder{model: "m"}
	retriever := newTestHybridRetriever(t, index, embedder)

	got, err := retriever.Rank(ctx, "架构约定", nil, 3)
	if err != nil {
		t.Fatalf("混合检索失败: %v", err)
	}
	if got != nil {
		t.Fatalf("真源为空时应返回 nil, got %+v", got)
	}
	if index.syncCalls != 1 {
		t.Fatalf("真源为空时仍应同步索引, got %d 次", index.syncCalls)
	}
	if len(index.fragments) != 0 {
		t.Fatalf("索引应被清空, got %d 条", len(index.fragments))
	}
	if len(embedder.calls) != 0 {
		t.Fatalf("真源为空时不应调用 embedding, got %d 次", len(embedder.calls))
	}
}

// TestFuseRRFEmpty 无召回结果时返回 nil 而不是空切片
func TestFuseRRFEmpty(t *testing.T) {
	if got := fuseRRF(defaultRRFK, 5); got != nil {
		t.Fatalf("无召回结果时应返回 nil, got %+v", got)
	}
	if got := fuseRRF(defaultRRFK, 5, nil, nil); got != nil {
		t.Fatalf("召回结果全为空时应返回 nil, got %+v", got)
	}
}

// newIndexRecord 构造索引层的记忆记录
func newIndexRecord(section, content string, score float64) MemoryRecord {
	return MemoryRecord{
		Fragment: Fragment{Scope: ScopeWorkspace, Section: section, Content: content},
		Source:   "MEMORY.md",
		Score:    score,
	}
}
