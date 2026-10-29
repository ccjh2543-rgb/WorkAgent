package sqlitestore

import (
	"context"
	"database/sql"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"simpleAgent/domain/session"
)

// newTestStore 在临时目录中创建会话库，并在用例结束后关闭
func newTestStore(t *testing.T) *Store {
	t.Helper()
	store, err := New(filepath.Join(t.TempDir(), "sessions.db"))
	if err != nil {
		t.Fatalf("创建会话库失败: %v", err)
	}
	t.Cleanup(func() {
		if err := store.Close(); err != nil {
			t.Errorf("关闭会话库失败: %v", err)
		}
	})
	return store
}

// msg 构造一条待落盘的消息，Kind 与 Role 必须显式给出
func msg(kind session.Kind, role session.Role, content string) session.Message {
	return session.Message{Kind: kind, Role: role, Content: content}
}

// prompt / answer 是测试里最常用的两种消息
func prompt(content string) session.Message {
	return msg(session.KindPrompt, session.RoleUser, content)
}

func answer(content string) session.Message {
	return msg(session.KindAnswer, session.RoleAssistant, content)
}

// mustAppend 追加消息，失败即终止用例
func mustAppend(t *testing.T, store *Store, sessionID string, messages ...session.Message) {
	t.Helper()
	if err := store.Append(context.Background(), sessionID, messages...); err != nil {
		t.Fatalf("追加消息失败: %v", err)
	}
}

// mustLoad 读取分支，失败即终止用例
func mustLoad(t *testing.T, store *Store, sessionID string, limit int) []session.Message {
	t.Helper()
	messages, err := store.LoadBranch(context.Background(), sessionID, limit)
	if err != nil {
		t.Fatalf("读取分支失败: %v", err)
	}
	return messages
}

// TestAppendAndLoadBranch 追加必须串成 parent 链，并按正序读回
func TestAppendAndLoadBranch(t *testing.T) {
	store := newTestStore(t)

	mustAppend(t, store, "main", prompt("第一个问题"), answer("第一个回答"), prompt("第二个问题"))

	got := mustLoad(t, store, "main", 0)
	if len(got) != 3 {
		t.Fatalf("消息条数不匹配: got %d, want 3", len(got))
	}
	for i, message := range got {
		if i == 0 {
			if message.ParentID != 0 {
				t.Fatalf("根消息的父指针应为 0: %+v", message)
			}
			continue
		}
		if message.ParentID != got[i-1].ID {
			t.Fatalf("第 %d 条消息的父指针应指向前一条: got %d, want %d",
				i, message.ParentID, got[i-1].ID)
		}
	}
	if got[0].Content != "第一个问题" || got[2].Content != "第二个问题" {
		t.Fatalf("消息顺序错误: %+v", got)
	}
}

// TestLoadBranchLimitKeepsLatest 超出条数上限时必须保留最近的
func TestLoadBranchLimitKeepsLatest(t *testing.T) {
	store := newTestStore(t)
	mustAppend(t, store, "main",
		prompt("一"), prompt("二"), prompt("三"), prompt("四"), prompt("五"))

	got := mustLoad(t, store, "main", 2)
	if len(got) != 2 {
		t.Fatalf("应只返回 2 条: got %d", len(got))
	}
	if got[0].Content != "四" || got[1].Content != "五" {
		t.Fatalf("应返回最近 2 条且按正序排列: %+v", got)
	}
}

// TestAppendAdvancesHead 每次追加后 HEAD 必须指向最后一条消息
func TestAppendAdvancesHead(t *testing.T) {
	store := newTestStore(t)
	mustAppend(t, store, "main", prompt("一"), answer("二"))

	path := mustLoad(t, store, "main", 0)
	headID, err := store.headID(context.Background(), "main")
	if err != nil {
		t.Fatalf("读取 HEAD 失败: %v", err)
	}
	if headID != path[len(path)-1].ID {
		t.Fatalf("HEAD 未推进到最后一条: got %d, want %d", headID, path[len(path)-1].ID)
	}
}

// TestRollbackThenAppendCreatesBranch 这是树形模型的核心行为：
// 回滚只移动指针，继续对话会自然长出分支，旧路径一条不少。
func TestRollbackThenAppendCreatesBranch(t *testing.T) {
	ctx := context.Background()
	store := newTestStore(t)

	mustAppend(t, store, "main",
		prompt("问题一"), answer("回答一"), prompt("问题二"), answer("回答二"))
	original := mustLoad(t, store, "main", 0)
	target := original[1] // 回滚到「回答一」

	archiveID, err := store.Rollback(ctx, "main", target.ID)
	if err != nil {
		t.Fatalf("回滚失败: %v", err)
	}
	if archiveID == "" {
		t.Fatal("回滚必须返回自动建立的存档分支 ID")
	}

	// 回滚后当前分支只剩到目标为止
	afterRollback := mustLoad(t, store, "main", 0)
	if len(afterRollback) != 2 {
		t.Fatalf("回滚后分支长度不匹配: got %d, want 2", len(afterRollback))
	}
	if afterRollback[1].ID != target.ID {
		t.Fatalf("回滚后 HEAD 应指向目标消息: got %d, want %d", afterRollback[1].ID, target.ID)
	}

	// 继续追加，新消息挂在回滚目标之下
	mustAppend(t, store, "main", prompt("换个问法"))

	branched := mustLoad(t, store, "main", 0)
	if len(branched) != 3 {
		t.Fatalf("回滚后追加应形成 3 条的新分支: got %d", len(branched))
	}
	if branched[2].Content != "换个问法" {
		t.Fatalf("新消息内容不匹配: %+v", branched[2])
	}

	// 关键断言：原路径完整保留，没有被覆盖
	archived := mustLoad(t, store, archiveID, 0)
	if len(archived) != 4 {
		t.Fatalf("存档分支应保留原路径的 4 条消息: got %d", len(archived))
	}
	if archived[2].Content != "问题二" || archived[3].Content != "回答二" {
		t.Fatalf("存档分支内容被污染: %+v", archived)
	}
	if archived[1].ID != target.ID {
		t.Fatalf("两条分支应共享同一个祖先节点: got %d, want %d", archived[1].ID, target.ID)
	}
}

// TestRollbackRejectsNonAncestor 回滚目标必须是当前 HEAD 的祖先，防止跨树乱跳
func TestRollbackRejectsNonAncestor(t *testing.T) {
	ctx := context.Background()
	store := newTestStore(t)

	mustAppend(t, store, "main", prompt("主线一"), answer("主线二"))
	mainPath := mustLoad(t, store, "main", 0)

	otherBranch, err := store.Fork(ctx, "main", 0)
	if err != nil {
		t.Fatalf("分叉失败: %v", err)
	}
	mustAppend(t, store, otherBranch, prompt("旁支提问"), answer("旁支回答"))
	otherPath := mustLoad(t, store, otherBranch, 0)

	// 旁支上的节点不是主线 HEAD 的祖先
	_, err = store.Rollback(ctx, "main", otherPath[3].ID)
	if err == nil {
		t.Fatal("回滚到非祖先节点应报错")
	}
	if !strings.Contains(err.Error(), "不是当前 HEAD") {
		t.Fatalf("错误信息应说明祖先校验失败: %v", err)
	}

	// 主线不应被这次失败的操作改动
	if now := mustLoad(t, store, "main", 0); len(now) != len(mainPath) {
		t.Fatalf("失败的回滚不应改动主线: got %d 条", len(now))
	}
}

// TestRollbackEdgeCases 空分支与回滚到 HEAD 自身都要明确报错
func TestRollbackEdgeCases(t *testing.T) {
	ctx := context.Background()
	store := newTestStore(t)

	if _, err := store.Rollback(ctx, "空会话", 1); err == nil {
		t.Fatal("空分支回滚应报错")
	}
	if _, err := store.Rollback(ctx, "空会话", 0); err == nil {
		t.Fatal("回滚目标为空应报错")
	}

	mustAppend(t, store, "main", prompt("唯一一条"))
	path := mustLoad(t, store, "main", 0)
	if _, err := store.Rollback(ctx, "main", path[0].ID); err == nil {
		t.Fatal("回滚到当前 HEAD 应报错")
	}
}

// TestForkCreatesIndependentBranch 从当前 HEAD 分叉后，两条分支互不影响
func TestForkCreatesIndependentBranch(t *testing.T) {
	ctx := context.Background()
	store := newTestStore(t)

	mustAppend(t, store, "main", prompt("共同的问题"), answer("共同的回答"))
	branchID, err := store.Fork(ctx, "main", 0)
	if err != nil {
		t.Fatalf("分叉失败: %v", err)
	}

	mustAppend(t, store, branchID, prompt("分支专属提问"), answer("分支专属回答"))

	if got := mustLoad(t, store, branchID, 0); len(got) != 4 {
		t.Fatalf("分叉分支应有 4 条消息: got %d", len(got))
	}
	if got := mustLoad(t, store, "main", 0); len(got) != 2 {
		t.Fatalf("原分支不应受分叉影响: got %d", len(got))
	}
}

// TestForkFromAncestor 可以从历史中间节点分叉，而不只是当前 HEAD
func TestForkFromAncestor(t *testing.T) {
	ctx := context.Background()
	store := newTestStore(t)

	mustAppend(t, store, "main",
		prompt("问题一"), answer("回答一"), prompt("问题二"), answer("回答二"))
	path := mustLoad(t, store, "main", 0)

	branchID, err := store.Fork(ctx, "main", path[1].ID)
	if err != nil {
		t.Fatalf("从祖先节点分叉失败: %v", err)
	}
	got := mustLoad(t, store, branchID, 0)
	if len(got) != 2 {
		t.Fatalf("新分支应从分叉点开始: got %d 条", len(got))
	}
	if got[1].ID != path[1].ID {
		t.Fatalf("新分支 HEAD 应指向分叉点: got %d, want %d", got[1].ID, path[1].ID)
	}
}

// TestForkRejectsForeignMessage 不允许跨对话分叉
func TestForkRejectsForeignMessage(t *testing.T) {
	ctx := context.Background()
	store := newTestStore(t)

	mustAppend(t, store, "tree-a", prompt("A 树的消息"))
	mustAppend(t, store, "tree-b", prompt("B 树的消息"))

	aPath := mustLoad(t, store, "tree-a", 0)
	if _, err := store.Fork(ctx, "tree-b", aPath[0].ID); err == nil {
		t.Fatal("跨对话分叉应报错")
	}
}

// TestListBranches 同一棵树上的所有分支都要被列出，并标出消息条数与摘要
func TestListBranches(t *testing.T) {
	ctx := context.Background()
	store := newTestStore(t)

	mustAppend(t, store, "main", prompt("问题一"), answer("回答一"), prompt("问题二"))
	path := mustLoad(t, store, "main", 0)

	archiveID, err := store.Rollback(ctx, "main", path[0].ID)
	if err != nil {
		t.Fatalf("回滚失败: %v", err)
	}
	mustAppend(t, store, "main", prompt("换个问法"))
	forkID, err := store.Fork(ctx, "main", 0)
	if err != nil {
		t.Fatalf("分叉失败: %v", err)
	}

	branches, err := store.ListBranches(ctx, "main")
	if err != nil {
		t.Fatalf("列出分支失败: %v", err)
	}
	if len(branches) != 3 {
		t.Fatalf("应列出 3 条分支: got %d", len(branches))
	}

	byID := make(map[string]session.Branch, len(branches))
	for _, branch := range branches {
		byID[branch.SessionID] = branch
	}
	for _, want := range []string{"main", archiveID, forkID} {
		if _, ok := byID[want]; !ok {
			t.Fatalf("分支 %s 未被列出: %+v", want, branches)
		}
	}
	if got := byID[archiveID].MessageCount; got != 3 {
		t.Fatalf("存档分支应为 3 条消息: got %d", got)
	}
	if got := byID["main"].Preview; got == "" {
		t.Fatal("分支应带有 HEAD 摘要，便于人工辨认")
	}
}

// TestDeleteExpiredRemovesWholeTree 过期清理要删掉整棵树，包括所有分支
func TestDeleteExpiredRemovesWholeTree(t *testing.T) {
	ctx := context.Background()
	store := newTestStore(t)

	mustAppend(t, store, "旧会话", prompt("很久以前"))
	forkID, err := store.Fork(ctx, "旧会话", 0)
	if err != nil {
		t.Fatalf("分叉失败: %v", err)
	}
	mustAppend(t, store, "新会话", prompt("刚刚"))

	old := time.Now().Add(-100 * 24 * time.Hour).Unix()
	if _, err := store.db.Exec(
		`UPDATE conversations SET updated_at = ? WHERE id IN (SELECT conversation_id FROM sessions WHERE id = ?)`,
		old, "旧会话"); err != nil {
		t.Fatalf("改写对话活跃时间失败: %v", err)
	}

	removed, err := store.DeleteExpired(ctx, time.Now().Add(-30*24*time.Hour))
	if err != nil {
		t.Fatalf("清理过期会话失败: %v", err)
	}
	if removed != 1 {
		t.Fatalf("应删除 1 棵对话树: got %d", removed)
	}

	// 旧树的两个分支及其消息都要消失
	for _, sessionID := range []string{"旧会话", forkID} {
		if got := mustLoad(t, store, sessionID, 0); len(got) != 0 {
			t.Fatalf("过期会话 %s 的消息应被清除: %+v", sessionID, got)
		}
	}
	var orphan int
	if err := store.db.QueryRow(
		`SELECT COUNT(*) FROM messages WHERE content = '很久以前'`).Scan(&orphan); err != nil {
		t.Fatalf("统计残留消息失败: %v", err)
	}
	if orphan != 0 {
		t.Fatalf("过期对话的消息未清理干净: %d 条", orphan)
	}

	// 新会话必须保留
	if got := mustLoad(t, store, "新会话", 0); len(got) != 1 {
		t.Fatalf("未过期的会话不应被删除: %+v", got)
	}
}

// TestAppendIsolatesConversations 不同会话的消息互不干扰
func TestAppendIsolatesConversations(t *testing.T) {
	ctx := context.Background()
	store := newTestStore(t)

	mustAppend(t, store, "s1", prompt("会话一"))
	mustAppend(t, store, "s2", prompt("会话二"))

	first := mustLoad(t, store, "s1", 0)
	if len(first) != 1 || first[0].Content != "会话一" {
		t.Fatalf("会话一历史被污染: %+v", first)
	}
	second := mustLoad(t, store, "s2", 0)
	if len(second) != 1 || second[0].Content != "会话二" {
		t.Fatalf("会话二历史被污染: %+v", second)
	}

	// 两个会话各自的对话树相互独立
	conversationOne, err := store.sessionByID(ctx, store.db, "s1")
	if err != nil {
		t.Fatalf("读取会话失败: %v", err)
	}
	conversationTwo, err := store.sessionByID(ctx, store.db, "s2")
	if err != nil {
		t.Fatalf("读取会话失败: %v", err)
	}
	if conversationOne.ConversationID == conversationTwo.ConversationID {
		t.Fatal("不同会话应属于不同的对话树")
	}
}

// TestAppendEdgeCases 参数校验与空消息处理
func TestAppendEdgeCases(t *testing.T) {
	ctx := context.Background()
	store := newTestStore(t)

	if err := store.Append(ctx, "", prompt("x")); err == nil {
		t.Fatal("会话 ID 为空时应返回错误")
	}
	if err := store.Append(ctx, "main"); err != nil {
		t.Fatalf("空消息列表不应报错: %v", err)
	}
	// 空内容没有记录价值，跳过但不报错
	if err := store.Append(ctx, "main", answer("")); err != nil {
		t.Fatalf("空内容不应报错: %v", err)
	}
	if got := mustLoad(t, store, "main", 0); len(got) != 0 {
		t.Fatalf("空内容不应落盘: %+v", got)
	}
	// Kind 与 Role 是必填项，缺失必须立刻失败而不是写入脏数据
	if err := store.Append(ctx, "main", session.Message{Role: session.RoleUser, Content: "缺种类"}); err == nil {
		t.Fatal("消息种类为空时应返回错误")
	}
	if err := store.Append(ctx, "main", session.Message{Kind: session.KindPrompt, Content: "缺角色"}); err == nil {
		t.Fatal("消息角色为空时应返回错误")
	}
}

// TestLoadBranchEdgeCases 非法入参与不存在的会话
func TestLoadBranchEdgeCases(t *testing.T) {
	ctx := context.Background()
	store := newTestStore(t)

	if _, err := store.LoadBranch(ctx, "", 10); err == nil {
		t.Fatal("会话 ID 为空时应返回错误")
	}
	got, err := store.LoadBranch(ctx, "不存在", 10)
	if err != nil {
		t.Fatalf("会话不存在不应报错: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("会话不存在应返回空: %+v", got)
	}
}

// TestNewRejectsEmptyPath 空路径属于非法入参
func TestNewRejectsEmptyPath(t *testing.T) {
	if _, err := New(""); err == nil {
		t.Fatal("路径为空时应返回错误")
	}
}

// TestMigrateRejectsUnknownVersion 未知版本必须报错而不是清库
func TestMigrateRejectsUnknownVersion(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "sessions.db")

	store, err := New(dbPath)
	if err != nil {
		t.Fatalf("创建会话库失败: %v", err)
	}
	mustAppend(t, store, "s1", prompt("要保住的对话"))
	if _, err := store.db.Exec(`UPDATE meta SET value = '999' WHERE key = 'schema_version'`); err != nil {
		t.Fatalf("改写结构版本失败: %v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatalf("关闭会话库失败: %v", err)
	}

	_, err = New(dbPath)
	if err == nil {
		t.Fatal("未知版本应返回错误")
	}
	if !strings.Contains(err.Error(), "结构版本不兼容") {
		t.Fatalf("错误信息应说明版本不兼容: %v", err)
	}

	// 关键断言：失败退出后数据必须原封不动
	raw, err := sql.Open("sqlite", "file:"+dbPath)
	if err != nil {
		t.Fatalf("直接打开会话库失败: %v", err)
	}
	defer func() {
		if closeErr := raw.Close(); closeErr != nil {
			t.Errorf("关闭连接失败: %v", closeErr)
		}
	}()
	var survived int
	if err := raw.QueryRow(
		`SELECT COUNT(*) FROM messages WHERE content = '要保住的对话'`).Scan(&survived); err != nil {
		t.Fatalf("查询会话消息失败: %v", err)
	}
	if survived != 1 {
		t.Fatalf("版本不兼容时绝不能丢数据: 期望 1 条, got %d", survived)
	}
}
