package memory

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
)

// PendingText 一条尚未生成向量的记忆文本
type PendingText struct {
	Hash string // 记忆正文哈希，作为向量表主键
	Text string // 需要向量化的文本
}

// MemoryRecord 索引返回的一条记忆及其检索得分
type MemoryRecord struct {
	Fragment
	Source string  // 来源标识，当前为 Markdown 真源文件路径
	Score  float64 // 检索得分，仅用于排序与调试输出
}

// Index 记忆索引接口。
//
// 索引是 Markdown 真源的派生数据，可以随时删除并从真源重建，
// 因此实现必须保证 Sync 之后「索引中的记忆集合 == 真源中的记忆集合」。
type Index interface {
	// Sync 用真源解析出的记忆列表刷新索引。
	// 内容集合未发生变化时应当直接返回，不做任何写操作。
	Sync(ctx context.Context, source string, fragments []Fragment) error

	// PendingTexts 返回当前模型下尚未生成向量的记忆。
	// 已生成过向量的记忆不会重复返回，避免每轮检索都调用 embedding 接口。
	PendingTexts(ctx context.Context, model string) ([]PendingText, error)

	// SaveVectors 批量保存向量，map 的 key 为 PendingText.Hash
	SaveVectors(ctx context.Context, model string, vectors map[string][]float32) error

	// KeywordSearch 关键词检索，按相关度降序返回
	KeywordSearch(ctx context.Context, query string, topK int) ([]MemoryRecord, error)

	// VectorSearch 向量检索，按余弦相似度降序返回。
	// 向量维度与模型不符的记忆会被跳过而不是报错。
	VectorSearch(ctx context.Context, model string, queryVector []float32, topK int) ([]MemoryRecord, error)

	// Close 释放底层资源
	Close() error
}

// Embedder 文本向量化接口，由具体服务商实现
type Embedder interface {
	// Embed 批量向量化，返回顺序与入参一致
	Embed(ctx context.Context, texts []string) ([][]float32, error)

	// Model 返回模型名，用于隔离不同模型产生的向量
	Model() string
}

// TextHash 计算一条记忆的稳定哈希，用于索引去重与向量复用。
// 内容未变时哈希不变，因此可以避免重复调用 embedding 接口。
func TextHash(section, content string) string {
	sum := sha256.Sum256([]byte(section + "\x00" + content))
	return hex.EncodeToString(sum[:])
}

// EmbedText 返回用于向量化的文本：章节参与向量化，以提升主题层面的相关性
func EmbedText(section, content string) string {
	if section == "" {
		return content
	}
	return section + " " + content
}
