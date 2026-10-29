package agent

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	dapproval "simpleAgent/domain/approval"
)

// newTestAgent 装配一个不依赖真实模型的 Agent，工作区隔离在临时目录。
// NewAgentWithOptions 只包装 chatModel，不会在装配阶段调用它，因此 nil 是安全的。
func newTestAgent(t *testing.T) *Agent {
	t.Helper()
	dir := t.TempDir()
	a := NewAgentWithOptions(nil, Options{WorkspaceRoot: dir})
	t.Cleanup(func() {
		if err := a.Close(); err != nil {
			t.Errorf("关闭 Agent 失败: %v", err)
		}
	})
	return a
}

// TestDefaultApproverDenies 验证未设置审批器时默认拒绝，而不是全放行。
func TestDefaultApproverDenies(t *testing.T) {
	a := newTestAgent(t)
	res, err := a.approver.Approve(context.Background(), dapproval.ApprovalRequest{Kind: dapproval.KindTool, ToolName: "write_file"})
	if err != nil {
		t.Fatalf("默认审批器不应返回错误: %v", err)
	}
	if res.Decision != dapproval.DecisionReject {
		t.Fatalf("默认审批器必须拒绝，实际 decision=%v", res.Decision)
	}
}

// TestSetApproverNilFallsBackToDeny 验证显式清除审批器时回退到安全默认值。
func TestSetApproverNilFallsBackToDeny(t *testing.T) {
	a := newTestAgent(t)
	a.SetApprover(dapproval.AutoApprover{})
	a.SetApprover(nil)

	res, err := a.approver.Approve(context.Background(), dapproval.ApprovalRequest{Kind: dapproval.KindTool})
	if err != nil {
		t.Fatalf("回退审批器不应返回错误: %v", err)
	}
	if res.Decision != dapproval.DecisionReject {
		t.Fatalf("SetApprover(nil) 必须回退到拒绝，实际 decision=%v", res.Decision)
	}
}

// TestSetApproverExplicitWins 验证显式设置的审批器生效（Office 等无人值守场景依赖此行为）。
func TestSetApproverExplicitWins(t *testing.T) {
	a := newTestAgent(t)
	a.SetApprover(dapproval.AutoApprover{})

	res, err := a.approver.Approve(context.Background(), dapproval.ApprovalRequest{Kind: dapproval.KindTool})
	if err != nil {
		t.Fatalf("显式审批器不应返回错误: %v", err)
	}
	if res.Decision != dapproval.DecisionApprove {
		t.Fatalf("显式 AutoApprover 应放行，实际 decision=%v", res.Decision)
	}
}

// TestNewAgentWithOptionsUsesWorkspaceRoot 验证工作区根目录按配置注入，而非固定取进程 cwd。
func TestNewAgentWithOptionsUsesWorkspaceRoot(t *testing.T) {
	dir := t.TempDir()
	a := NewAgentWithOptions(nil, Options{WorkspaceRoot: dir})
	t.Cleanup(func() {
		if err := a.Close(); err != nil {
			t.Errorf("关闭 Agent 失败: %v", err)
		}
	})
	if a.workspace == nil {
		t.Fatal("指定工作区后文件工具应可用")
	}

	want, err := filepath.Abs(dir)
	if err != nil {
		t.Fatalf("解析期望工作区失败: %v", err)
	}
	if resolved, err := filepath.EvalSymlinks(want); err == nil {
		want = resolved
	}
	if a.workspace.Root() != want {
		t.Fatalf("工作区根目录未生效：期望 %s，实际 %s", want, a.workspace.Root())
	}
	// 装配阶段只应创建数据目录，不应把文件写到进程 cwd
	if _, err := os.Stat(filepath.Join(want, ".simpleagent")); err != nil {
		t.Fatalf("工作区内应创建数据目录: %v", err)
	}
}
