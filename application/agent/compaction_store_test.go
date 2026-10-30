package agent

import (
	"context"
	"testing"

	"simpleAgent/domain/session"
)

// TestCompactionPersistsAndRestores 验证摘要随会话落盘，且恢复后只保留记录之后的消息。
func TestCompactionPersistsAndRestores(t *testing.T) {
	root := t.TempDir()
	state := t.TempDir()
	ctx := context.Background()

	a := NewAgentWithOptions(zoneStubModel{reply: "ok"}, Options{WorkspaceRoot: root, StateRoot: state})
	a.sessionID = "sess-compact"
	a.appendMessage(ctx, session.KindPrompt, session.RoleUser, "整理报价单")
	a.appendMessage(ctx, session.KindToolResult, session.RoleUser, "工具输出")
	a.compaction.summary = "目标：整理报价单；已完成：读取输入"
	a.compaction.compactedCount = a.memory.Len()
	a.persistedCompaction = 0
	a.persistCompaction(ctx)
	if a.persistedCompaction == 0 {
		t.Fatal("压缩摘要未标记为已持久化")
	}
	// 压缩记录之后继续产生的新消息，恢复时应保留
	a.appendMessage(ctx, session.KindAnswer, session.RoleAssistant, "已完成")
	if err := a.Close(); err != nil {
		t.Fatalf("关闭首个 Agent 失败: %v", err)
	}

	b := NewAgentWithOptions(zoneStubModel{reply: "ok"}, Options{WorkspaceRoot: root, StateRoot: state})
	defer b.Close()
	b.sessionID = "sess-compact"
	if err := b.restoreHistory(ctx); err != nil {
		t.Fatalf("恢复历史失败: %v", err)
	}
	if b.compaction.summary != "目标：整理报价单；已完成：读取输入" {
		t.Fatalf("摘要未恢复: %q", b.compaction.summary)
	}
	if got := b.memory.Len(); got != 1 {
		t.Fatalf("恢复后应只保留压缩记录之后的消息，实际 %d 条", got)
	}
	if got := b.memory.Entries()[0].Content; got != "已完成" {
		t.Fatalf("保留的消息不符: %q", got)
	}

	// 压缩记录属于内部元数据，不应出现在给用户看的历史里
	history, err := b.LoadHistory(ctx, "sess-compact")
	if err != nil {
		t.Fatalf("读取历史失败: %v", err)
	}
	if len(history) != 3 {
		t.Fatalf("对用户可见的历史应为 3 条，实际 %d 条", len(history))
	}
	for _, message := range history {
		if message.Kind == session.KindCompaction {
			t.Fatal("压缩记录不应出现在用户历史中")
		}
	}
}

// TestRestoreCompactionBoundaries 验证多条压缩记录以后一条为边界，非法载荷被忽略。
func TestRestoreCompactionBoundaries(t *testing.T) {
	messages := []session.Message{
		{Kind: session.KindPrompt, Role: session.RoleUser, Content: "第一问"},
		{Kind: session.KindCompaction, Role: session.RoleSystem, Content: `{"summary":"摘要一"}`},
		{Kind: session.KindPrompt, Role: session.RoleUser, Content: "第二问"},
		{Kind: session.KindCompaction, Role: session.RoleSystem, Content: `{"summary":"摘要二"}`},
		{Kind: session.KindPrompt, Role: session.RoleUser, Content: "第三问"},
	}
	entries, summary := restoreCompaction(messages)
	if summary != "摘要二" {
		t.Fatalf("应以最后一条有效摘要为准，实际 %q", summary)
	}
	if len(entries) != 1 || entries[0].Content != "第三问" {
		t.Fatalf("应只保留最后一条记录之后的消息，实际 %+v", entries)
	}

	// 非法载荷不能让整次恢复失败，也不能清空已有消息
	broken := []session.Message{
		{Kind: session.KindPrompt, Role: session.RoleUser, Content: "问题"},
		{Kind: session.KindCompaction, Role: session.RoleSystem, Content: "不是 JSON"},
	}
	entries, summary = restoreCompaction(broken)
	if summary != "" {
		t.Fatalf("非法载荷不应产生摘要，实际 %q", summary)
	}
	if len(entries) != 1 || entries[0].Content != "问题" {
		t.Fatalf("非法载荷不应影响消息恢复，实际 %+v", entries)
	}
}
