package memory

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestConsolidateDedupesAcrossSections 验证同一条事实写在两个章节时只保留第一条。
func TestConsolidateDedupesAcrossSections(t *testing.T) {
	doc := &Document{Title: "项目记忆", Sections: []Section{
		{Title: "架构约定", Fragments: []Fragment{{Content: "严格 DDD 分层"}}},
		{Title: "补充说明", Fragments: []Fragment{{Content: " 严格   DDD 分层 "}}},
	}}

	report, archived := Consolidate(doc, 1000, DefaultArchiveAge)
	if report.Deduped != 1 {
		t.Fatalf("应去重 1 条，实际 %d", report.Deduped)
	}
	if report.Remaining != 1 || len(archived) != 0 {
		t.Fatalf("应保留 1 条且无归档，实际 remaining=%d archived=%d", report.Remaining, len(archived))
	}
	if len(doc.AllFragments()) != 1 {
		t.Fatalf("整合后应只剩 1 条，实际 %d", len(doc.AllFragments()))
	}
	if len(doc.Sections) != 1 {
		t.Fatalf("空章节应被移除，实际 %d 个章节", len(doc.Sections))
	}
}

// TestConsolidateArchivesStaleProcedure 验证过期的流程类记忆被归档，其它类型不受影响。
func TestConsolidateArchivesStaleProcedure(t *testing.T) {
	now := int64(200 * 24 * 3600)
	doc := &Document{Title: "项目记忆", Sections: []Section{{
		Title: "操作流程",
		Fragments: []Fragment{
			{Content: "过期流程", Kind: KindProcedure, UpdatedAt: 1},
			{Content: "新流程", Kind: KindProcedure, UpdatedAt: now},
			{Content: "历史条目", UpdatedAt: 1},
			{Content: "长期决策", Kind: KindDecision, UpdatedAt: 1},
		},
	}}}

	report, archived := Consolidate(doc, now, DefaultArchiveAge)
	if report.Archived != 1 || len(archived) != 1 || archived[0].Content != "过期流程" {
		t.Fatalf("应归档过期流程，实际 %+v / %+v", report, archived)
	}
	if report.Remaining != 3 {
		t.Fatalf("应保留 3 条，实际 %d", report.Remaining)
	}
}

// TestWorkspaceStoreConsolidate 验证整合作业会写归档文件并原子更新主记忆。
func TestWorkspaceStoreConsolidate(t *testing.T) {
	store := NewWorkspaceStore(t.TempDir())
	ctx := context.Background()
	now := time.Now()

	stale := Fragment{
		Section:   "操作流程",
		Content:   "每次导出前手动清理临时目录",
		Kind:      KindProcedure,
		Source:    SourceAgent,
		UpdatedAt: now.Add(-200 * 24 * time.Hour).Unix(),
	}
	if err := store.AppendRecord(ctx, stale); err != nil {
		t.Fatalf("写入过期流程失败: %v", err)
	}
	if err := store.AppendRecord(ctx, Fragment{Section: "架构约定", Content: "严格 DDD 分层"}); err != nil {
		t.Fatalf("写入记忆失败: %v", err)
	}
	if err := store.AppendRecord(ctx, Fragment{Section: "补充说明", Content: "严格 DDD 分层"}); err != nil {
		t.Fatalf("写入重复记忆失败: %v", err)
	}

	report, err := store.Consolidate(ctx, now, DefaultArchiveAge)
	if err != nil {
		t.Fatalf("整合作业失败: %v", err)
	}
	if report.Archived != 1 || report.Deduped != 1 || report.Remaining != 1 {
		t.Fatalf("整合统计不符: %+v", report)
	}

	doc, err := store.Load(ctx)
	if err != nil {
		t.Fatalf("读取记忆失败: %v", err)
	}
	fragments := doc.AllFragments()
	if len(fragments) != 1 || fragments[0].Content != "严格 DDD 分层" {
		t.Fatalf("主记忆整合结果不符: %+v", fragments)
	}

	archives, err := filepath.Glob(filepath.Join(store.Dir(), archiveDirName, "MEMORY-*.md"))
	if err != nil || len(archives) != 1 {
		t.Fatalf("应生成 1 个归档文件，实际 %d（err=%v）", len(archives), err)
	}
	data, err := os.ReadFile(archives[0])
	if err != nil {
		t.Fatalf("读取归档文件失败: %v", err)
	}
	if !strings.Contains(string(data), "每次导出前手动清理临时目录") {
		t.Fatalf("归档文件缺少被归档条目:\n%s", string(data))
	}
}
