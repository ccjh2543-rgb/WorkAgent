package audit

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"simpleAgent/domain/approval"
)

// TestLoggerWritesJSONL 验证审计事件按行追加写入，且包含类型与动作。
func TestLoggerWritesJSONL(t *testing.T) {
	path := filepath.Join(t.TempDir(), "audit", "audit.jsonl")
	logger, err := Open(path)
	if err != nil {
		t.Fatalf("打开审计日志失败: %v", err)
	}
	logger.Log(Event{Type: "auth", Action: "deny", Source: "127.0.0.1", Detail: "login"})
	logger.Log(Event{Type: "file_write", Action: "allow", Detail: "notes.md"})
	if err := logger.Close(); err != nil {
		t.Fatalf("关闭审计日志失败: %v", err)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("读取审计文件失败: %v", err)
	}
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	if len(lines) != 2 {
		t.Fatalf("应有 2 条审计记录，实际 %d: %s", len(lines), data)
	}
	for i, line := range lines {
		var ev Event
		if err := json.Unmarshal([]byte(line), &ev); err != nil {
			t.Fatalf("第 %d 行不是合法 JSON: %v", i+1, err)
		}
		if ev.Time.IsZero() || ev.Type == "" {
			t.Fatalf("第 %d 条记录缺少时间或类型: %+v", i+1, ev)
		}
	}
}

// TestNilLoggerSafe 验证未装配审计时不会 panic。
func TestNilLoggerSafe(t *testing.T) {
	var logger *Logger
	logger.Log(Event{Type: "noop"})
	if err := logger.Close(); err != nil {
		t.Fatalf("nil 审计关闭应返回 nil: %v", err)
	}
}

// TestWrapApproverRecordsDecision 验证审批装饰器记录放行决策。
func TestWrapApproverRecordsDecision(t *testing.T) {
	path := filepath.Join(t.TempDir(), "audit", "audit.jsonl")
	logger, err := Open(path)
	if err != nil {
		t.Fatalf("打开审计日志失败: %v", err)
	}
	wrapped := WrapApprover(approval.AutoApprover{}, logger, "test")
	res, err := wrapped.Approve(context.Background(), approval.ApprovalRequest{
		Kind:     approval.KindTool,
		ToolName: "write_file",
	})
	if err != nil {
		t.Fatalf("审批不应报错: %v", err)
	}
	if res.Decision != approval.DecisionApprove {
		t.Fatalf("内层审批结果应原样返回，实际 %v", res.Decision)
	}
	if err := logger.Close(); err != nil {
		t.Fatalf("关闭审计日志失败: %v", err)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("读取审计文件失败: %v", err)
	}
	text := string(data)
	if !strings.Contains(text, `"type":"approval"`) || !strings.Contains(text, `"action":"allow"`) {
		t.Fatalf("审计内容缺少审批放行记录: %s", text)
	}
}
