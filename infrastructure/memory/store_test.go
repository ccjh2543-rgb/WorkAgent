package memory

import (
	"context"
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"
)

// TestWorkspaceStoreLoadMissingFile 记忆文件不存在属于首次运行的正常情况
func TestWorkspaceStoreLoadMissingFile(t *testing.T) {
	store := NewWorkspaceStore(t.TempDir())
	doc, err := store.Load(context.Background())
	if err != nil {
		t.Fatalf("文件不存在时不应返回错误: %v", err)
	}
	if got := len(doc.AllFragments()); got != 0 {
		t.Fatalf("期望空文档, got %d 条记忆", got)
	}
}

// TestWorkspaceStoreAppendAndLoad 验证追加写入能正确落盘并读回
func TestWorkspaceStoreAppendAndLoad(t *testing.T) {
	store := NewWorkspaceStore(t.TempDir())
	ctx := context.Background()

	if err := store.Append(ctx, "架构约定", "严格 DDD 分层"); err != nil {
		t.Fatalf("追加记忆失败: %v", err)
	}
	if err := store.Append(ctx, "架构约定", "domain 层不依赖外部包"); err != nil {
		t.Fatalf("追加记忆失败: %v", err)
	}
	if err := store.Append(ctx, "沟通偏好", "优先中文输出"); err != nil {
		t.Fatalf("追加记忆失败: %v", err)
	}

	doc, err := store.Load(ctx)
	if err != nil {
		t.Fatalf("读取记忆失败: %v", err)
	}
	if got := len(doc.Sections); got != 2 {
		t.Fatalf("章节数不匹配: got %d, want 2", got)
	}
	if got := len(doc.AllFragments()); got != 3 {
		t.Fatalf("记忆条数不匹配: got %d, want 3", got)
	}

	data, err := os.ReadFile(store.Path())
	if err != nil {
		t.Fatalf("记忆文件未落盘: %v", err)
	}
	for _, want := range []string{"# 项目记忆", "## 架构约定", "- 严格 DDD 分层", "## 沟通偏好"} {
		if !strings.Contains(string(data), want) {
			t.Fatalf("记忆文件缺少内容 %q, 实际内容:\n%s", want, string(data))
		}
	}
}

// TestWorkspaceStoreAppendSkipsDuplicate 重复内容不应反复累积
func TestWorkspaceStoreAppendSkipsDuplicate(t *testing.T) {
	store := NewWorkspaceStore(t.TempDir())
	ctx := context.Background()

	for i := 0; i < 3; i++ {
		if err := store.Append(ctx, "架构约定", "严格 DDD 分层"); err != nil {
			t.Fatalf("第 %d 次追加记忆失败: %v", i+1, err)
		}
	}

	doc, err := store.Load(ctx)
	if err != nil {
		t.Fatalf("读取记忆失败: %v", err)
	}
	if got := len(doc.AllFragments()); got != 1 {
		t.Fatalf("重复内容应只保留一条: got %d", got)
	}
}

// TestWorkspaceStoreAppendRejectsEmptyContent 空正文属于非法入参
func TestWorkspaceStoreAppendRejectsEmptyContent(t *testing.T) {
	store := NewWorkspaceStore(t.TempDir())
	if err := store.Append(context.Background(), "架构约定", "   "); err == nil {
		t.Fatal("空记忆内容应返回错误")
	}
}

// TestWorkspaceStoreAppendRecord 验证带元数据的写入会补齐默认值并落盘。
func TestWorkspaceStoreAppendRecord(t *testing.T) {
	store := NewWorkspaceStore(t.TempDir())
	ctx := context.Background()

	if err := store.AppendRecord(ctx, Fragment{
		Section:   "架构约定",
		Content:   "严格 DDD 分层",
		Kind:      KindDecision,
		Source:    SourceAgent,
		UpdatedAt: 1700000000,
	}); err != nil {
		t.Fatalf("写入带元数据的记忆失败: %v", err)
	}

	doc, err := store.Load(ctx)
	if err != nil {
		t.Fatalf("读取记忆失败: %v", err)
	}
	fragments := doc.AllFragments()
	if len(fragments) != 1 {
		t.Fatalf("期望 1 条记忆，实际 %d 条", len(fragments))
	}
	f := fragments[0]
	if f.Kind != KindDecision || f.Source != SourceAgent || f.UpdatedAt != 1700000000 {
		t.Fatalf("元数据未正确落盘: %+v", f)
	}
	if f.ID == "" {
		t.Fatal("缺少正文哈希 ID")
	}

	data, err := os.ReadFile(store.Path())
	if err != nil {
		t.Fatalf("读取记忆文件失败: %v", err)
	}
	if !strings.Contains(string(data), "kind:decision") || !strings.Contains(string(data), "src:agent") {
		t.Fatalf("记忆文件缺少元数据注释:\n%s", string(data))
	}
}

// TestWorkspaceStoreConcurrentAppend 并发写入不应丢数据或损坏文件
func TestWorkspaceStoreConcurrentAppend(t *testing.T) {
	const writers = 20
	store := NewWorkspaceStore(t.TempDir())
	ctx := context.Background()

	var wg sync.WaitGroup
	for i := 0; i < writers; i++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			if err := store.Append(ctx, "并发", fmt.Sprintf("第 %d 条", n)); err != nil {
				t.Errorf("并发追加记忆失败: %v", err)
			}
		}(i)
	}
	wg.Wait()

	doc, err := store.Load(ctx)
	if err != nil {
		t.Fatalf("读取记忆失败: %v", err)
	}
	if got := len(doc.AllFragments()); got != writers {
		t.Fatalf("并发写入出现丢失: got %d, want %d", got, writers)
	}
}
