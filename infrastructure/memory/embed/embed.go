// Package embed 提供基于 OpenAI 兼容接口的文本向量化实现。
//
// 默认对接阿里云百炼（DashScope），其 /compatible-mode/v1 与 OpenAI 接口一致，
// 因此直接复用 eino 框架的 openai embedding 组件，不再引入额外的 HTTP 依赖。
// DeepSeek 本身不提供 embedding 模型，向量能力必须来自这类外部服务。
package embed

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/cloudwego/eino-ext/components/embedding/openai"
)

const (
	// defaultBatchSize DashScope 的 embedding 接口对单次文本条数有上限，
	// 默认按 10 条一批提交，避免超限报错
	defaultBatchSize = 10
	// defaultTimeout 单次请求超时，防止网络异常拖死整轮检索
	defaultTimeout = 30 * time.Second
)

// Config 向量化服务的连接配置。
// 敏感字段一律从环境变量读取，禁止硬编码到代码里。
type Config struct {
	// BaseURL OpenAI 兼容接口地址，例如：
	// https://{WorkspaceId}.cn-beijing.maas.aliyuncs.com/compatible-mode/v1
	BaseURL string
	// APIKey 服务商 API Key
	APIKey string
	// Model 模型名，例如 qwen3.7-text-embedding
	Model string
	// BatchSize 单次请求的最大文本条数，非正数时使用默认值
	BatchSize int
	// Timeout 单次请求超时，非正数时使用默认值
	Timeout time.Duration
}

// OpenAICompatibleEmbedder 基于 OpenAI 兼容 /embeddings 接口的向量化实现。
// 内部委托 eino 的 openai embedding 组件，对外保持 float32 的既有契约。
type OpenAICompatibleEmbedder struct {
	embedder  openai.Embedder
	model     string
	batchSize int
}

// NewOpenAICompatibleEmbedder 创建向量化客户端，配置不完整时返回错误。
//
// 调用示例：
//
//	embedder, err := embed.NewOpenAICompatibleEmbedder(embed.Config{
//		BaseURL: os.Getenv("MEMORY_EMBEDDING_BASE_URL"),
//		APIKey:  os.Getenv("MEMORY_EMBEDDING_API_KEY"),
//		Model:   os.Getenv("MEMORY_EMBEDDING_MODEL"),
//	})
func NewOpenAICompatibleEmbedder(cfg Config) (*OpenAICompatibleEmbedder, error) {
	if cfg.BaseURL == "" {
		return nil, errors.New("embedding BaseURL 不能为空")
	}
	if cfg.APIKey == "" {
		return nil, errors.New("embedding APIKey 不能为空")
	}
	if cfg.Model == "" {
		return nil, errors.New("embedding 模型名不能为空")
	}
	if cfg.BatchSize <= 0 {
		cfg.BatchSize = defaultBatchSize
	}
	if cfg.Timeout <= 0 {
		cfg.Timeout = defaultTimeout
	}

	// eino openai embedding 不直接用 Config 里的 HTTPClient 构造超时，
	// 传一个带超时的 client 以保持原有行为。
	embedder, err := openai.NewEmbedder(context.Background(), &openai.EmbeddingConfig{
		APIKey:     cfg.APIKey,
		BaseURL:    cfg.BaseURL,
		Model:      cfg.Model,
		HTTPClient: &http.Client{Timeout: cfg.Timeout},
		APIVersion: "",
	})
	if err != nil {
		return nil, fmt.Errorf("初始化 embedding 客户端失败: %w", err)
	}

	return &OpenAICompatibleEmbedder{
		embedder:  *embedder,
		model:     cfg.Model,
		batchSize: cfg.BatchSize,
	}, nil
}

// Model 返回模型名，用于隔离不同模型产生的向量
func (e *OpenAICompatibleEmbedder) Model() string {
	return e.model
}

// Embed 批量向量化，返回顺序与入参一致；入参为空时返回空结果。
// 文本会按 batchSize 分批提交，任一批次失败即返回错误。
func (e *OpenAICompatibleEmbedder) Embed(ctx context.Context, texts []string) ([][]float32, error) {
	if len(texts) == 0 {
		return nil, nil
	}
	for idx, text := range texts {
		if text == "" {
			return nil, fmt.Errorf("第 %d 条待向量化文本为空", idx)
		}
	}

	vectors := make([][]float32, 0, len(texts))
	for start := 0; start < len(texts); start += e.batchSize {
		end := min(start+e.batchSize, len(texts))
		batch, err := e.embedBatch(ctx, texts[start:end])
		if err != nil {
			return nil, err
		}
		vectors = append(vectors, batch...)
	}
	return vectors, nil
}

// embedBatch 提交单个批次的向量化请求。
// eino 组件返回 float64，这里在边界处转换为内部统一的 float32，
// 保持 memory 层既有数据契约不变。
func (e *OpenAICompatibleEmbedder) embedBatch(ctx context.Context, texts []string) ([][]float32, error) {
	embeddings, err := e.embedder.EmbedStrings(ctx, texts)
	if err != nil {
		return nil, fmt.Errorf("调用 embedding 接口失败: %w", err)
	}
	if len(embeddings) != len(texts) {
		return nil, fmt.Errorf("embedding 返回条数不匹配: got %d, want %d", len(embeddings), len(texts))
	}

	vectors := make([][]float32, len(embeddings))
	for i, vec := range embeddings {
		if len(vec) == 0 {
			return nil, fmt.Errorf("第 %d 条返回的向量为空", i)
		}
		vectors[i] = make([]float32, len(vec))
		for j, v := range vec {
			vectors[i][j] = float32(v)
		}
	}
	return vectors, nil
}
