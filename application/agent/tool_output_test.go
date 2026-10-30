package agent

import (
	"context"
	"os"
	"strings"
	"testing"

	"simpleAgent/domain/session"
)

// TestFoldToolResultSpillsFullOutput 验证折叠后的工具输出会完整留存，且模型拿到可回取路径。
func TestFoldToolResultSpillsFullOutput(t *testing.T) {
	a := newZoneTestAgent(t)
	a.sessionID = "sess-spill"
	a.SetContextBudget(200000)

	long := strings.Repeat("明细行-2026-10-30\n", 200)
	a.memory.Add(session.KindToolResult, session.RoleUser, long)

	msgs, err := a.prepareContext(context.Background())
	if err != nil {
		t.Fatalf("装配上下文失败: %v", err)
	}
	var folded string
	for _, m := range msgs {
		// 系统提示词里也会提到「完整输出已留存」，用带冒号的引用特征区分
		if strings.Contains(m.Content, "完整输出已留存：") {
			folded = m.Content
			break
		}
	}
	if folded == "" {
		t.Fatal("折叠后的消息未给出留存引用")
	}
	start := strings.Index(folded, "留存：")
	end := strings.Index(folded, "；需要细节")
	if start < 0 || end <= start {
		t.Fatalf("留存引用格式不符: %q", folded)
	}
	path := folded[start+len("留存：") : end]

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("读取留存文件失败: %v", err)
	}
	if string(data) != long {
		t.Fatalf("留存内容与原始输出不一致: got %d bytes, want %d bytes", len(data), len(long))
	}

	// 同一内容重复装配应复用同一份留存文件
	second, err := a.prepareContext(context.Background())
	if err != nil {
		t.Fatalf("二次装配失败: %v", err)
	}
	var foldedAgain string
	for _, m := range second {
		if strings.Contains(m.Content, "完整输出已留存：") {
			foldedAgain = m.Content
			break
		}
	}
	if !strings.Contains(foldedAgain, path) {
		t.Fatalf("重复装配应复用同一留存路径，首次 %q，二次 %q", path, foldedAgain)
	}
}

// TestFoldToolResultKeepsShortOutput 验证未超阈值的输出不会被改写，也不会留存文件。
func TestFoldToolResultKeepsShortOutput(t *testing.T) {
	a := newZoneTestAgent(t)
	a.sessionID = "sess-short"
	short := "读取成功：3 行"

	if got := a.foldToolResult(short); got != short {
		t.Fatalf("短输出不应被折叠，实际 %q", got)
	}
	if entries, err := os.ReadDir(a.toolOutputDir()); err == nil && len(entries) > 0 {
		t.Fatalf("短输出不应产生留存文件，实际 %d 个", len(entries))
	}
}
