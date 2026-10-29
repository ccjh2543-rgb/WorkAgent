package approver

import (
	"context"
	"io"
	"testing"
	"time"

	"simpleAgent/domain/approval"
)

// TestInteractiveApproverTimeoutRejects 验证无人应答时按拒绝返回，而不是永久挂起或报错。
func TestInteractiveApproverTimeoutRejects(t *testing.T) {
	pr, pw := io.Pipe()
	defer func() { _ = pw.Close() }()

	a := NewInteractiveApprover(pr, io.Discard)
	a.timeout = 30 * time.Millisecond

	res, err := a.Approve(context.Background(), approval.ApprovalRequest{
		Kind:     approval.KindTool,
		ToolName: "write_file",
	})
	if err != nil {
		t.Fatalf("超时应按拒绝返回而不是系统错误: %v", err)
	}
	if res.Decision != approval.DecisionReject {
		t.Fatalf("超时应拒绝，实际 decision=%v", res.Decision)
	}
	if res.Reason == "" {
		t.Fatal("超时拒绝应给出理由")
	}
}
