// Package approval 定义 Agent 人工确认领域的核心模型。
//
// 只承载纯领域契约：确认点类型、审批请求/结果、决策枚举与 Approver 端口，
// 不依赖任何外部包。交互实现（终端/WS）由基础设施层完成。
package approval

import (
	"context"

	"simpleAgent/domain/tool"
)

// 确认点类型，不同类型对应不同的交互语义与可选操作
const (
	// KindPlan 计划确认：执行前让人过目任务拆解
	KindPlan = "plan"
	// KindTool 工具审批：副作用操作执行前的确认
	KindTool = "tool"
	// KindStuck 卡住转人工：循环耗尽或连续失败时交给人决定下一步
	KindStuck = "stuck"
)

// ApprovalRequest 一次待人工确认的操作
type ApprovalRequest struct {
	Kind      string         // 确认点类型：plan / tool / stuck
	ToolName  string         // 工具名，非工具类确认点为空
	Arguments string         // 待确认的内容：工具参数、计划文本等
	Summary   string         // 给人看的一句话说明
	Risk      tool.RiskLevel // 风险等级
	Round     int            // 当前是第几轮思考，非循环内确认点为 0
}

// Decision 人工决策
type Decision int

const (
	// DecisionApprove 放行：按原参数执行。
	// 对 KindStuck 而言放行没有意义，会被视为收工，详见 Agent.requestGuidance
	DecisionApprove Decision = iota
	// DecisionReject 拒绝：不执行，Reason 会作为反馈回注到对话
	DecisionReject
	// DecisionModify 修改后执行：用 Arguments 替换原参数
	DecisionModify
)

// ApprovalResult 人工决策结果
type ApprovalResult struct {
	Decision  Decision
	Arguments string // DecisionModify 时生效，用于替换原参数
	Reason    string // 拒绝或修改的理由，会作为反馈注入对话
}

// Approver 人工确认端口。
//
// 实现决定「什么时候真的要问人」：可以全部放行、可以只对高风险操作发问、
// 也可以阻塞读取标准输入。Agent 只依赖这个接口，不关心交互形式。
type Approver interface {
	// Approve 返回人工决策；ctx 被取消时应尽快返回 ctx.Err()
	Approve(ctx context.Context, req ApprovalRequest) (ApprovalResult, error)
}

// AutoApprover 全部放行，等价于关闭人在回路，保持无人工干预的原有行为。
//
// 仅允许在显式声明无人值守的场景使用（如 Office 评测、headless），
// 不能作为 Agent 的隐式默认值。
type AutoApprover struct{}

// Approve 实现 Approver，恒定放行
func (AutoApprover) Approve(context.Context, ApprovalRequest) (ApprovalResult, error) {
	return ApprovalResult{Decision: DecisionApprove}, nil
}

// DenyApprover 默认拒绝，用于「没有可用人工审批通道」的运行模式。
//
// 副作用操作在无人在回路时必须失败关闭（fail-closed），
// 而不是静默执行；拒绝理由会作为反馈回注给模型。
type DenyApprover struct{}

// Approve 实现 Approver，恒定拒绝。
//
// 返回 nil error 是刻意设计：拒绝属于正常业务决策，
// 调用方应读取 Decision 而不是把拒绝当成系统故障。
func (DenyApprover) Approve(context.Context, ApprovalRequest) (ApprovalResult, error) {
	return ApprovalResult{
		Decision: DecisionReject,
		Reason:   "当前运行没有可用的人工审批通道，已按安全默认拒绝",
	}, nil
}
