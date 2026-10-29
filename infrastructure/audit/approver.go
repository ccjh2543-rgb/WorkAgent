package audit

import (
	"context"
	"fmt"

	"simpleAgent/domain/approval"
)

// Approver 装饰器：把每次人工确认的结果写入审计日志，再原样返回给调用方。
type Approver struct {
	Inner  approval.Approver
	Log    *Logger
	Source string
}

// WrapApprover 包装审批器；log 为 nil 时仍可安全使用。
func WrapApprover(inner approval.Approver, log *Logger, source string) approval.Approver {
	return &Approver{Inner: inner, Log: log, Source: source}
}

// Approve 执行内层审批并记录决策。审计失败不影响审批结果。
func (a *Approver) Approve(ctx context.Context, req approval.ApprovalRequest) (approval.ApprovalResult, error) {
	res, err := a.Inner.Approve(ctx, req)
	action := "error"
	if err == nil {
		switch res.Decision {
		case approval.DecisionApprove:
			action = "allow"
		case approval.DecisionReject:
			action = "deny"
		}
	}
	a.Log.Log(Event{
		Type:   "approval",
		Action: action,
		Source: a.Source,
		Detail: fmt.Sprintf("kind=%s tool=%s risk=%d", req.Kind, req.ToolName, req.Risk),
	})
	return res, err
}
