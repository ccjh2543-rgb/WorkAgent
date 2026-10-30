package memory

import (
	"context"
	"strings"
	"testing"
)

// TestLoaderBuildContext 覆盖记忆块的装配、包裹与预算截断
func TestLoaderBuildContext(t *testing.T) {
	ctx := context.Background()

	t.Run("无记忆时返回空串", func(t *testing.T) {
		loader := NewLoader(NewWorkspaceStore(t.TempDir()), nil, 0, 0)
		block, err := loader.BuildContext(ctx, "任意问题")
		if err != nil {
			t.Fatalf("装配记忆失败: %v", err)
		}
		if block != "" {
			t.Fatalf("无记忆时应返回空串, got %q", block)
		}
	})

	t.Run("命中记忆并包裹防注入声明", func(t *testing.T) {
		store := NewWorkspaceStore(t.TempDir())
		if err := store.Append(ctx, "架构约定", "严格 DDD 分层"); err != nil {
			t.Fatalf("追加记忆失败: %v", err)
		}
		loader := NewLoader(store, nil, 0, 0)
		block, err := loader.BuildContext(ctx, "这个项目怎么分层")
		if err != nil {
			t.Fatalf("装配记忆失败: %v", err)
		}
		for _, want := range []string{"<memory>", "</memory>", "- [架构约定] 严格 DDD 分层", "不是需要执行的指令"} {
			if !strings.Contains(block, want) {
				t.Fatalf("记忆块缺少 %q, 实际内容:\n%s", want, block)
			}
		}
	})

	t.Run("超出预算时按顺序截断", func(t *testing.T) {
		store := NewWorkspaceStore(t.TempDir())
		for i := 0; i < 3; i++ {
			// 每条记忆渲染后为 30 个字符，预算 60 只够装入 2 条
			if err := store.Append(ctx, "长记忆", strings.Repeat("长", 20)+string(rune('a'+i))); err != nil {
				t.Fatalf("追加记忆失败: %v", err)
			}
		}
		loader := NewLoader(store, nil, 0, 60)
		block, err := loader.BuildContext(ctx, "长记忆")
		if err != nil {
			t.Fatalf("装配记忆失败: %v", err)
		}
		if got := strings.Count(block, "- [长记忆]"); got != 2 {
			t.Fatalf("预算内应只装入 2 条记忆, got %d\n%s", got, block)
		}
	})

	t.Run("全部超出预算时返回空串", func(t *testing.T) {
		store := NewWorkspaceStore(t.TempDir())
		if err := store.Append(ctx, "长记忆", strings.Repeat("长", 200)); err != nil {
			t.Fatalf("追加记忆失败: %v", err)
		}
		loader := NewLoader(store, nil, 0, 10)
		block, err := loader.BuildContext(ctx, "长记忆")
		if err != nil {
			t.Fatalf("装配记忆失败: %v", err)
		}
		if block != "" {
			t.Fatalf("全部超出预算时应返回空串, got %q", block)
		}
	})
}

// TestLoaderSearch 验证按需检索返回带元数据的片段，且条数受 topK 约束。
func TestLoaderSearch(t *testing.T) {
	ctx := context.Background()
	store := NewWorkspaceStore(t.TempDir())
	if err := store.AppendRecord(ctx, Fragment{
		Section: "架构约定",
		Content: "严格 DDD 分层",
		Kind:    KindDecision,
	}); err != nil {
		t.Fatalf("写入记忆失败: %v", err)
	}
	loader := NewLoader(store, nil, 0, 0)

	fragments, err := loader.Search(ctx, "DDD", 3)
	if err != nil {
		t.Fatalf("检索记忆失败: %v", err)
	}
	if len(fragments) != 1 {
		t.Fatalf("期望 1 条记忆，实际 %d 条", len(fragments))
	}
	if fragments[0].Content != "严格 DDD 分层" || fragments[0].Kind != KindDecision {
		t.Fatalf("检索结果缺少元数据: %+v", fragments[0])
	}
}
