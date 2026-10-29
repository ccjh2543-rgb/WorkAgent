package agent

import "sort"

// AgentEvent 运行过程中的一处可上报的进展。
//
// 它把 Run 内部的关键节点（计划就绪、每轮思考、工具调用与结果、卡住转人工、
// 最终答案）抽成结构化事件，便于 HTTP/WS 服务实时展示；普通 CLI 调用方忽略即可。
type AgentEvent struct {
	// Type 事件类型，见下方 Event* 常量
	Type string
	// SessionID 当前会话
	SessionID string
	// ToolName 工具名，非工具相关事件为空
	ToolName string
	// Args 工具调用参数，非工具相关事件为空
	Args string
	// Result 工具执行结果或最终答案，按事件类型复用
	Result string
	// Message 给人看的一句话说明
	Message string
	// TaskID 子任务 ID，仅 Planner-Worker 的子任务事件使用，其余为 0
	TaskID int
}

// 事件类型常量
const (
	// EventRound 每轮思考开始
	EventRound = "round"
	// EventPlanReady 计划已就绪，等待人工确认
	EventPlanReady = "plan_ready"
	// EventToolCall 模型发起一次工具调用（含 ReadOnly 自动放行的工具）
	EventToolCall = "tool_call"
	// EventToolResult 工具执行完成或失败
	EventToolResult = "tool_result"
	// EventToolRejected 工具被人工拒绝
	EventToolRejected = "tool_rejected"
	// EventStuck 任务卡住，转人工介入
	EventStuck = "stuck"
	// EventFinal 得到最终答案
	EventFinal = "final"
	// EventPlanStart Planner-Worker 模式开始规划
	EventPlanStart = "plan_start"
	// EventSubtaskStart Planner-Worker 模式某个子任务开始执行
	EventSubtaskStart = "subtask_start"
	// EventSubtaskDone Planner-Worker 模式某个子任务执行结束
	EventSubtaskDone = "subtask_done"
	// EventReplan Planner-Worker 模式因失败触发的重规划
	EventReplan = "replan"
)

// ToolInfo 框架对外展示的一个工具条目
type ToolInfo struct {
	Name        string // 工具/技能名
	Description string // 用途说明
	Risk        string // 风险等级
	AutoApprove bool   // 是否自动放行
}

// DefaultToolInfs 从工具注册表的名字-描述-策略快照生成要展示的 ToolInfo 列表。
// tools 由 application 层按需传入（此处不依赖具体注册表类型，保持领域纯净）。
type ToolInfosFunc func() []ToolInfo

// SortedToolNames 对工具名排序，保证展示顺序稳定
func SortedToolNames(names []string) []string {
	out := make([]string, len(names))
	copy(out, names)
	sort.Strings(out)
	return out
}
