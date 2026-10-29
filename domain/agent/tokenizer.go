package agent

import "context"

// Tokenizer 文本 token 计数端口。
//
// 由基础设施层实现（DeepSeek 官方离线分词器，基于 tokenizer.json 的
// ByteLevel BPE），用于把上下文预算从「字符启发式估算」升级为按真实分词结果
// 计数。端口为 nil 或出错时，调用方应回退到 domain 的 EstimateTokens 兜底估算。
//
// 注意：仅统计单条文本的 token 数，不包含服务端对多消息拼装（encode_messages）
// 的格式开销；两者差异由调用方以安全边距吸收。
//
// 调用示例：
//
//	tk, err := tokenizer.NewDeepSeekTokenizer(path) // 基础设施层实现
//	agent.SetTokenizer(tk)
//	n, err := tk.Count(ctx, "你好，世界")
type Tokenizer interface {
	// Count 返回 text 分词后的 token 数。
	// 出错时返回非 nil error，调用方据此回退估算。
	Count(ctx context.Context, text string) (int, error)
}
