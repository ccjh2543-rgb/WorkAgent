// Package llm 提供基于 eino 框架的大语言模型适配器。
//
// 它把 eino 的 openai ChatModel 组件封装到领域端口 domain/agent.ChatModel，
// 隔离领域层对具体 SDK 的依赖。DeepSeek 使用 OpenAI 兼容协议，
// 因此直接复用 eino 的 openai 适配器。
package llm

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/cloudwego/eino-ext/components/model/openai"
	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/schema"
	"github.com/eino-contrib/jsonschema"

	domainagent "simpleAgent/domain/agent"
	"simpleAgent/domain/session"
)

// defaultBaseURL 未显式指定服务地址时使用的默认端点。
// 本应用默认模型为 DeepSeek，且旧实现即内置该地址；此处集中兜底，
// 避免 OPENAI_BASE_URL 未配置时落到 OpenAI 默认端点导致调用失败。
const defaultBaseURL = "https://api.deepseek.com"

// Config eino 适配器的连接配置。
// 敏感字段一律从环境变量读取，禁止硬编码。
type Config struct {
	// BaseURL OpenAI 兼容接口地址，例如 https://api.deepseek.com
	BaseURL string
	// APIKey 服务商 API Key
	APIKey string
	// Model 模型名
	Model string
}

// EinoChatModel 把 eino openai ChatModel 适配到领域端口的实现。
type EinoChatModel struct {
	chatModel *openai.ChatModel
}

// NewEinoChatModel 构建基于 eino 的 ChatModel 适配器。
//
// 调用示例：
//
//	m, err := llm.NewEinoChatModel(llm.Config{
//		BaseURL: os.Getenv("OPENAI_BASE_URL"),
//		APIKey:  os.Getenv("OPENAI_API_KEY"),
//		Model:   os.Getenv("OPENAI_MODEL"),
//	})
func NewEinoChatModel(cfg Config) (*EinoChatModel, error) {
	if cfg.APIKey == "" {
		return nil, errors.New("模型 APIKey 不能为空")
	}
	if cfg.Model == "" {
		return nil, errors.New("模型名不能为空")
	}
	baseURL := cfg.BaseURL
	if baseURL == "" {
		baseURL = defaultBaseURL
	}
	cmCfg := &openai.ChatModelConfig{
		APIKey:  cfg.APIKey,
		BaseURL: baseURL,
		Model:   cfg.Model,
	}
	cm, err := openai.NewChatModel(context.Background(), cmCfg)
	if err != nil {
		return nil, fmt.Errorf("初始化模型失败: %w", err)
	}
	return &EinoChatModel{chatModel: cm}, nil
}

// SupportsNativeTools 声明本适配器支持原生 tool calling。
// DeepSeek 走 OpenAI 兼容协议，tools/tool_calls 字段原生可用，
// application 层据此在请求里带 Tools 声明。
func (e *EinoChatModel) SupportsNativeTools() bool { return true }

// Generate 实现 domain/agent.ChatModel：把领域请求转成 eino 消息并调用，
// 再把模型回答（正文 + 原生工具调用）转回领域值对象。
//
// 关键点：模型的 outMsg.ToolCalls 必须原样保留。旧实现只取 Content，
// 把已经解析好的结构化调用丢弃，模型只能改用自然语言描述要调用什么工具，
// 导致「有工具却没动作」——这正是本轮端口扩宽要修的根因。
func (e *EinoChatModel) Generate(ctx context.Context, req domainagent.ChatRequest) (domainagent.ChatResponse, error) {
	if e == nil || e.chatModel == nil {
		return domainagent.ChatResponse{}, errors.New("模型适配器未初始化")
	}

	messages, err := toEinoMessages(domainagent.NormalizeToolPairing(req.Messages))
	if err != nil {
		return domainagent.ChatResponse{}, err
	}

	opts := make([]model.Option, 0, 4)
	if m := req.Model; m != "" {
		opts = append(opts, model.WithModel(m))
	}
	if t := req.Temperature; t != 0 {
		opts = append(opts, model.WithTemperature(t))
	}
	if re := req.ReasoningEffort; re != "" {
		opts = append(opts, openai.WithReasoningEffort(openai.ReasoningEffortLevel(re)))
	}
	if len(req.Tools) > 0 {
		toolInfos, err := toEinoTools(req.Tools)
		if err != nil {
			return domainagent.ChatResponse{}, err
		}
		opts = append(opts, model.WithTools(toolInfos))
	}

	outMsg, err := e.chatModel.Generate(ctx, messages, opts...)
	if err != nil {
		return domainagent.ChatResponse{}, fmt.Errorf("调用模型失败: %w", err)
	}
	if outMsg == nil {
		return domainagent.ChatResponse{}, errors.New("模型返回了空消息")
	}
	return domainagent.ChatResponse{
		Content:   outMsg.Content,
		ToolCalls: fromEinoToolCalls(outMsg.ToolCalls),
	}, nil
}

// toEinoMessages 批量转换领域消息为 eino 消息。
func toEinoMessages(msgs []domainagent.ChatMessage) ([]*schema.Message, error) {
	messages := make([]*schema.Message, 0, len(msgs))
	for _, msg := range msgs {
		converted, err := toEinoMessage(msg)
		if err != nil {
			return nil, err
		}
		messages = append(messages, converted)
	}
	return messages, nil
}

// toEinoMessage 把领域消息转换为 eino 的 schema.Message。
// 支持 system/user/assistant/tool 四种角色，并保留工具调用的双向关联：
// assistant 的 ToolCalls 与 tool 消息的 ToolCallID。
func toEinoMessage(msg domainagent.ChatMessage) (*schema.Message, error) {
	switch msg.Role {
	case session.RoleUser:
		return schema.UserMessage(msg.Content), nil
	case session.RoleAssistant:
		return schema.AssistantMessage(msg.Content, toEinoToolCalls(msg.ToolCalls)), nil
	case session.RoleTool:
		return schema.ToolMessage(msg.Content, msg.ToolCallID), nil
	case session.RoleSystem:
		return schema.SystemMessage(msg.Content), nil
	default:
		return nil, fmt.Errorf("不支持的消息角色: %s", msg.Role)
	}
}

// toEinoToolCalls 把领域工具调用转换为 eino 的 schema.ToolCall。
// 文本协议解析出的调用没有 ID，此处补一个稳定的占位 ID：
// 服务端要求 assistant.tool_calls[].id 非空，且必须与 tool 消息的
// tool_call_id 一致；用「工具名+序号」即可保证同批内唯一且可复现。
func toEinoToolCalls(calls []domainagent.ToolCall) []schema.ToolCall {
	if len(calls) == 0 {
		return nil
	}
	out := make([]schema.ToolCall, 0, len(calls))
	for i, call := range calls {
		id := call.ID
		if id == "" {
			id = fmt.Sprintf("call_%s_%d", call.Name, i)
		}
		out = append(out, schema.ToolCall{
			ID:   id,
			Type: "function",
			Function: schema.FunctionCall{
				Name:      call.Name,
				Arguments: call.Arg,
			},
		})
	}
	return out
}

// fromEinoToolCalls 把 eino 解析好的工具调用转成领域值对象，保留调用 ID。
func fromEinoToolCalls(calls []schema.ToolCall) []domainagent.ToolCall {
	if len(calls) == 0 {
		return nil
	}
	out := make([]domainagent.ToolCall, 0, len(calls))
	for _, call := range calls {
		if call.Function.Name == "" {
			continue
		}
		out = append(out, domainagent.ToolCall{
			ID:   call.ID,
			Name: call.Function.Name,
			Arg:  call.Function.Arguments,
		})
	}
	return out
}

// toEinoTools 把领域工具声明转换为 eino 的 schema.ToolInfo。
// Parameters 是工具契约里渲染好的 JSON Schema 文本，此处解析回结构化形式：
// 传文本而非结构体，是为了让 domain/agent 不必依赖具体 schema 库。
func toEinoTools(decls []domainagent.ToolDecl) ([]*schema.ToolInfo, error) {
	out := make([]*schema.ToolInfo, 0, len(decls))
	for _, decl := range decls {
		info := &schema.ToolInfo{
			Name: decl.Name,
			Desc: decl.Description,
		}
		if decl.Parameters != "" {
			params := &jsonschema.Schema{}
			if err := json.Unmarshal([]byte(decl.Parameters), params); err != nil {
				return nil, fmt.Errorf("工具 %s 的参数 schema 非法: %w", decl.Name, err)
			}
			info.ParamsOneOf = schema.NewParamsOneOfByJSONSchema(params)
		}
		out = append(out, info)
	}
	return out, nil
}
