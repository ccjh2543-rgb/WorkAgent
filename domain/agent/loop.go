package agent

import "strings"

// runOutcome step 单步推进后的状态回报。
type RunOutcome int

const (
	// RunContinue 本轮尚未收敛，继续下一轮思考
	RunContinue RunOutcome = iota
	// RunFinal 已得到最终答案，可读 runProgress.finalAnswer
	RunFinal
	// RunPaused 模型请求工具但需外部执行，可读 pausedTool/pausedArg
	RunPaused
	// RunLimit 无 headless 人工兜底时，循环或连续失败已耗尽
	RunLimit
)

// ToolCall 一条待执行的工具调用指令。
//
// 有两个来源，共用同一类型：
//   - 原生 tool calling：由模型适配器直接把结构化调用转成本类型，带 ID；
//   - 文本协议：由 ParseToolCalls 从模型回复里解析 !TOOL: 行，无 ID。
//
// 同一批调用入队后逐个执行，避免「回复含多个 !TOOL: 时只有第一个被执行、
// 其余被静默丢弃」的漏动作缺陷。
type ToolCall struct {
	// ID 模型为本次调用分配的唯一标识，用于把工具结果配对回发起它的调用。
	// 文本协议解析出的调用没有 ID，此时为空串（结果按普通用户消息回注）。
	ID string
	// Name 工具名
	Name string
	// Arg 原始参数串（JSON 或位置参数），逐字传给执行端
	Arg string
}

// RenderToolCalls 把一批工具调用渲染成 !TOOL: 文本形式。
// 用途：原生 tool calling 的调用本身没有正文，落盘会话历史时用本函数
// 生成一段可读记录，保证历史自洽、重启后仍能读懂当时做过什么。
// 它是 ParseToolCalls 的逆运算（往返后工具名与参数保持一致）。
func RenderToolCalls(calls []ToolCall) string {
	var sb strings.Builder
	for i, call := range calls {
		if i > 0 {
			sb.WriteByte('\n')
		}
		sb.WriteString("!TOOL:" + call.Name + "|" + call.Arg)
	}
	return sb.String()
}

// splitToolCallLine 把一行内紧跟 !TOOL: 的工具调用拆成工具名与参数。
// 参数取首个 | 之后直到行尾的内容：遇到换行即截断。这能防止多行 !TOOL: 时
// 后几行被误并入第一行参数（会导致 JSON 非法、参数错位）。
// 参数只做拆分不做加工，以便人工审批看到的内容与实际执行的内容完全一致。
// 返回 hasTool=false 表示该 !TOOL: 格式不完整（缺 |）或工具名为空。
func splitToolCallLine(rest string) (name, args string, hasTool bool) {
	content := rest[len("!TOOL:"):]
	parts := strings.SplitN(content, "|", 2)
	if len(parts) != 2 {
		return "", "", false
	}
	rawArgs := parts[1]
	if i := strings.IndexByte(rawArgs, '\n'); i >= 0 {
		rawArgs = rawArgs[:i]
	}
	return strings.TrimSpace(parts[0]), strings.TrimSpace(rawArgs), true
}

// ParseToolCalls 解析工具调用指令，返回文本中出现的**全部** !TOOL: 调用。
//
// 这是批量漏动作修复的核心：旧实现只认第一个 !TOOL:（parseToolCall），
// 模型在同一响应里输出「!TOOL:name1|...\n!TOOL:name2|...」时，只有第一行被执行，
// 其余被静默丢弃——这正是「要求删 4 封只删 1 封却宣称完成」「多收件人转发只发第一个」的根因。
// 现在逐行扫描，提取所有调用供调用方逐个执行。
//
// 识别方式是从全文中逐个找 !TOOL: 前缀，而非要求它位于整段开头——模型偶尔会在
// 调用前加一句说明（如「我先查一下」）。行内参数到换行为止，避免串行吞参。
// 每行命中一次 !TOOL:（即使跟在说明文字后）都记一次调用；缺 | 或工具名为空的行被跳过。
func ParseToolCalls(output string) (calls []ToolCall) {
	rest := output
	for {
		idx := strings.Index(rest, "!TOOL:")
		if idx < 0 {
			break
		}
		name, args, ok := splitToolCallLine(rest[idx:])
		if ok && name != "" {
			calls = append(calls, ToolCall{Name: name, Arg: args})
		}
		// 越过这一行，继续往后找（行内若在同一行内还有 !TOOL: 也照常处理）
		rest = rest[idx+len("!TOOL:"):]
		if i := strings.IndexByte(rest, '\n'); i >= 0 {
			rest = rest[i+1:]
		} else {
			break
		}
	}
	return calls
}

// ParseToolCall 返回文本中第一个 !TOOL: 调用（为兼容既有单调用语义与测试的封装）。
// 生产与评估循环均已改走 ParseToolCalls 以支持批量；此函数仅用于单次取首调用。
func ParseToolCall(output string) (name, args string, hasTool bool) {
	calls := ParseToolCalls(output)
	if len(calls) == 0 {
		return "", "", false
	}
	return calls[0].Name, calls[0].Arg, true
}

// pseudoXMLTags 识别模型绕过 !TOOL: 协议、改用伪 XML 表达工具调用的标签。
// 这些标签来自模型训练时见过的 tool-calling 格式，在本文本协议环境会被误当答案。
// 只作启发式提示，不追求完备。
var pseudoXMLTags = []string{"<invoke", "<tool_call", "<function_call", "<parameter",
	"</invoke>", "</tool_call>", "</function>", "<search>", "<use_tool"}

// IsPseudoXMLToolCall 判断模型回复是否以伪 XML 形式表达了工具调用，
// 而非本环境要求的 !TOOL: 文本协议。命中多个标签时的置信度由命中数加权。
func IsPseudoXMLToolCall(output string) bool {
	if output == "" {
		return false
	}
	low := strings.ToLower(output)
	hit := 0
	for _, tag := range pseudoXMLTags {
		if strings.Contains(low, tag) {
			hit++
		}
	}
	// 至少命中一个明显的调用容器标签，才认定是伪 XML 调用而非普通正文。
	return hit >= 1 && strings.Contains(low, "invoke") ||
		hit >= 2
}
