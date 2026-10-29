// Package agent 定义 Agent 领域层的核心值对象与端口。
//
// 本包只承载纯领域契约，不依赖任何外部包（含 eino 与具体 SDK）：
//   - ChatModel 端口：供 application 层编排，由 infrastructure 层实现；
//   - ChatMessage / ChatRequest / ChatResponse：模型往返的领域值对象；
//   - 会话上下文的值对象（Entry、Memory）；
//   - 工具调用解析等纯函数；
//   - 系统提示词语料。
package agent

import (
	"context"

	"simpleAgent/domain/session"
)

// ChatMessage 一条发送给模型的消息（领域值对象）。
// Role 与 session.Role 对齐（user/assistant/system/tool）。
type ChatMessage struct {
	Role    session.Role
	Content string
	// ToolCalls 仅 assistant 消息携带：模型在这条消息里请求的工具调用。
	// 启用原生 tool calling 时必须原样回放，否则后面的 tool 结果消息
	// 找不到可配对的调用，服务端会直接拒绝整个请求。
	ToolCalls []ToolCall
	// ToolCallID 仅 tool 角色消息携带：本结果对应哪一次工具调用（ToolCall.ID）。
	ToolCallID string
}

// ToolDecl 声明给模型的可调用工具（领域值对象）。
//
// 它只描述「模型能调用什么」，不承载执行逻辑：执行仍由 domain/tool 的
// ToolRegistry 负责。两者刻意分开，避免模型契约与执行注册表互相绑定。
type ToolDecl struct {
	// Name 工具名，须与 ToolRegistry 中注册的名字一致
	Name string
	// Description 工具用途说明，越具体模型越容易在正确时机调用
	Description string
	// Parameters 参数的 JSON Schema 文本；空串表示该工具不接受任何参数
	Parameters string
}

// ChatRequest 一次模型请求的领域值对象。
type ChatRequest struct {
	// Model 模型名
	Model string
	// Messages 上下文消息，按顺序排列
	Messages []ChatMessage
	// Temperature 采样温度，0 表示确定性输出
	Temperature float32
	// ReasoningEffort 思考强度，可选（摘要等轻任务用）
	ReasoningEffort string
	// Tools 本轮允许模型调用的工具声明。
	// 为空表示不启用原生 tool calling，模型只能按文本协议（!TOOL:）表达工具调用。
	Tools []ToolDecl
}

// ChatResponse 一次模型回答的领域值对象。
type ChatResponse struct {
	// Content 文本回答；模型改用原生工具调用时可能为空串
	Content string
	// ToolCalls 模型以原生 tool calling 请求的工具调用，按模型给出的顺序排列。
	// 模型改走文本协议（!TOOL:）时该切片为空，此时由 ParseToolCalls 兜底解析。
	ToolCalls []ToolCall
}

// ChatModel 模型端口：由 infrastructure 层实现，application 编排只依赖它。
//
// Generate 返回模型的完整回答（文本 + 可选的原生工具调用），
// 而不是裸字符串：工具调用是模型输出的一等公民，把它降级成正文文本
// 再回解析，会丢掉调用 ID，导致「工具结果配不上调用」而无法多轮续做。
//
// 调用示例（应用层组装请求并消费原生工具调用）：
//
//	resp, err := model.Generate(ctx, agent.ChatRequest{
//		Model:    "deepseek-chat",
//		Messages: []agent.ChatMessage{{Role: session.RoleUser, Content: "读一下 a.go"}},
//		Tools: []agent.ToolDecl{{
//			Name:        "read_file",
//			Description: "读取文件",
//			Parameters:  `{"type":"object","properties":{"path":{"type":"string"}},"required":["path"]}`,
//		}},
//	})
//	if err != nil {
//		return err
//	}
//	for _, call := range resp.ToolCalls { // 原生调用优先；为空时再 ParseToolCalls(resp.Content)
//		result, err := registry.ExecuteTool(call.Name, call.Arg)
//		// 回注结果时带上 call.ID，模型才能把结果与调用对应起来
//		_ = result
//		_ = err
//	}
type ChatModel interface {
	// Generate 请求模型生成回答
	Generate(ctx context.Context, req ChatRequest) (ChatResponse, error)
}

// NativeToolModel 可选端口：模型适配器用它声明自己支持原生 tool calling。
//
// 能力由适配器声明、而非调用方配置：支持的适配器（DeepSeek / OpenAI 兼容服务）
// 实现它并返回 true，application 层据此在请求里声明 Tools；
// 未实现的适配器保持零改动，继续走已验证的 !TOOL: 文本协议。
type NativeToolModel interface {
	ChatModel
	// SupportsNativeTools 是否支持原生 tool calling
	SupportsNativeTools() bool
}

// NormalizeToolPairing 修正消息序列里「assistant 工具调用 ↔ tool 结果」的配对关系，
// 返回修正后的新切片（不修改入参）。
//
// 为什么需要：上下文压缩会把最旧的消息并入摘要、裁剪窗口会丢弃中间的条目，
// 配对的两端可能一端进了摘要、另一端还留在窗口里。这种残缺序列发出去，
// OpenAI 兼容服务端会以「tool 消息找不到对应的 tool_calls」直接拒绝整个请求。
//
// 修正规则（只降级、不新增）：
//  1. assistant 消息里没有对应结果消息的调用一律摘掉——不能向模型声明
//     「我要调用但永远不给结果」；
//  2. tool 结果消息若找不到对应的调用声明（回链已断，例如进程重启后从
//     持久化历史恢复），降级为普通 user 消息，内容一字不改；
//  3. 修正后既无正文又无调用的消息整体丢弃，避免发出空消息。
func NormalizeToolPairing(messages []ChatMessage) []ChatMessage {
	declared := make(map[string]bool)
	answered := make(map[string]bool)
	for _, msg := range messages {
		if msg.Role == session.RoleAssistant {
			for _, call := range msg.ToolCalls {
				if call.ID != "" {
					declared[call.ID] = true
				}
			}
		}
		if msg.Role == session.RoleTool && msg.ToolCallID != "" {
			answered[msg.ToolCallID] = true
		}
	}
	paired := make(map[string]bool, len(declared))
	for id := range declared {
		if answered[id] {
			paired[id] = true
		}
	}

	out := make([]ChatMessage, 0, len(messages))
	for _, msg := range messages {
		switch msg.Role {
		case session.RoleAssistant:
			if len(msg.ToolCalls) > 0 {
				kept := make([]ToolCall, 0, len(msg.ToolCalls))
				for _, call := range msg.ToolCalls {
					// 空 ID 的调用无法与结果配对，同样按「无结果」处理
					if call.ID != "" && paired[call.ID] {
						kept = append(kept, call)
					}
				}
				msg.ToolCalls = kept
			}
		case session.RoleTool:
			if msg.ToolCallID == "" || !paired[msg.ToolCallID] {
				msg.Role = session.RoleUser
				msg.ToolCallID = ""
			}
		}
		if msg.Role == session.RoleAssistant && msg.Content == "" && len(msg.ToolCalls) == 0 {
			continue
		}
		out = append(out, msg)
	}
	return out
}
