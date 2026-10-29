package approval

import (
	"context"
	"testing"

	"simpleAgent/domain/tool"
)

// TestDenyApproverAlwaysRejects 验证默认审批器对任意请求都返回拒绝，且不是系统错误。
// 表驱动覆盖计划、工具、卡住三类确认点，确保 fail-closed 语义与确认点类型无关。
func TestDenyApproverAlwaysRejects(t *testing.T) {
	cases := []struct {
		name string
		req  ApprovalRequest
	}{
		{name: "计划确认", req: ApprovalRequest{Kind: KindPlan, Summary: "执行计划"}},
		{name: "工具审批", req: ApprovalRequest{Kind: KindTool, ToolName: "write_file", Risk: tool.RiskDestructive}},
		{name: "卡住转人工", req: ApprovalRequest{Kind: KindStuck, Round: 3}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			res, err := DenyApprover{}.Approve(context.Background(), tc.req)
			if err != nil {
				t.Fatalf("拒绝不应返回系统错误: %v", err)
			}
			if res.Decision != DecisionReject {
				t.Fatalf("期望 DecisionReject，实际 %v", res.Decision)
			}
			if res.Reason == "" {
				t.Fatal("拒绝必须给出理由，便于回注给模型")
			}
		})
	}
}
