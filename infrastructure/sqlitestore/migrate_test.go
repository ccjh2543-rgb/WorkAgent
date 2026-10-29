package sqlitestore

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
	"time"

	"simpleAgent/domain/session"
)

// legacySchemaV1 早期线性结构的建表语句。
// 迁移测试需要它来构造一个真实的 v1 库，因此在这里完整保留一份。
var legacySchemaV1 = []string{
	`CREATE TABLE IF NOT EXISTS sessions (
		id         TEXT PRIMARY KEY,
		created_at INTEGER NOT NULL,
		updated_at INTEGER NOT NULL
	)`,
	`CREATE TABLE IF NOT EXISTS messages (
		id         INTEGER PRIMARY KEY,
		session_id TEXT    NOT NULL,
		seq        INTEGER NOT NULL,
		role       TEXT    NOT NULL,
		content    TEXT    NOT NULL,
		created_at INTEGER NOT NULL,
		UNIQUE (session_id, seq)
	)`,
	`CREATE INDEX IF NOT EXISTS idx_messages_session ON messages(session_id, seq)`,
	`CREATE INDEX IF NOT EXISTS idx_sessions_updated ON sessions(updated_at)`,
	`CREATE TABLE IF NOT EXISTS meta (key TEXT PRIMARY KEY, value TEXT NOT NULL)`,
}

// legacyRow 一条 v1 消息
type legacyRow struct {
	sessionID string
	seq       int
	role      string
	content   string
}

// writeLegacyDB 构造一个 v1 结构的会话库
func writeLegacyDB(t *testing.T, dbPath string, sessions []string, rows []legacyRow) {
	t.Helper()

	db, err := sql.Open("sqlite", "file:"+dbPath)
	if err != nil {
		t.Fatalf("打开 v1 会话库失败: %v", err)
	}
	defer func() {
		if closeErr := db.Close(); closeErr != nil {
			t.Errorf("关闭 v1 会话库失败: %v", closeErr)
		}
	}()

	for _, stmt := range legacySchemaV1 {
		if _, err := db.Exec(stmt); err != nil {
			t.Fatalf("建立 v1 结构失败: %v", err)
		}
	}
	now := time.Now().Unix()
	for _, sessionID := range sessions {
		if _, err := db.Exec(
			`INSERT INTO sessions(id, created_at, updated_at) VALUES(?, ?, ?)`, sessionID, now, now); err != nil {
			t.Fatalf("写入 v1 会话失败: %v", err)
		}
	}
	for _, row := range rows {
		if _, err := db.Exec(
			`INSERT INTO messages(session_id, seq, role, content, created_at) VALUES(?, ?, ?, ?, ?)`,
			row.sessionID, row.seq, row.role, row.content, now); err != nil {
			t.Fatalf("写入 v1 消息失败: %v", err)
		}
	}
	if _, err := db.Exec(
		`INSERT INTO meta(key, value) VALUES('schema_version', '1')`); err != nil {
		t.Fatalf("写入 v1 版本标记失败: %v", err)
	}
}

// TestMigrateV1ToV2 线性结构必须被完整迁移为树形，一条消息都不能丢
func TestMigrateV1ToV2(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "sessions.db")

	rows := []legacyRow{
		{"s1", 1, "user", "请计算 123 加 456"},
		{"s1", 2, "assistant", "!TOOL:calculator|123,456"},
		{"s1", 3, "user", "工具执行结果：123 + 456 = 579"},
		{"s1", 4, "assistant", "结果是 579"},
		{"s2", 1, "user", "另一个会话的问题"},
	}
	writeLegacyDB(t, dbPath, []string{"s1", "s2"}, rows)

	store, err := New(dbPath)
	if err != nil {
		t.Fatalf("迁移失败: %v", err)
	}
	defer func() {
		if closeErr := store.Close(); closeErr != nil {
			t.Errorf("关闭会话库失败: %v", closeErr)
		}
	}()

	// 版本标记应被推进
	version, err := store.schemaVersion()
	if err != nil {
		t.Fatalf("读取结构版本失败: %v", err)
	}
	if version != schemaVersionV2 {
		t.Fatalf("迁移后版本号不匹配: got %q, want %q", version, schemaVersionV2)
	}

	// 消息条数与顺序都要保住
	first := mustLoad(t, store, "s1", 0)
	if len(first) != 4 {
		t.Fatalf("迁移后消息条数不匹配: got %d, want 4", len(first))
	}
	for i, want := range []string{
		"请计算 123 加 456",
		"!TOOL:calculator|123,456",
		"工具执行结果：123 + 456 = 579",
		"结果是 579",
	} {
		if first[i].Content != want {
			t.Fatalf("第 %d 条消息内容不匹配: got %q, want %q", i, first[i].Content, want)
		}
	}

	// 必须串成 parent 链
	if first[0].ParentID != 0 {
		t.Fatalf("根消息的父指针应为 0: %+v", first[0])
	}
	for i := 1; i < len(first); i++ {
		if first[i].ParentID != first[i-1].ID {
			t.Fatalf("第 %d 条消息的父指针应指向前一条: got %d, want %d",
				i, first[i].ParentID, first[i-1].ID)
		}
	}

	// 会话之间不能串味
	second := mustLoad(t, store, "s2", 0)
	if len(second) != 1 || second[0].Content != "另一个会话的问题" {
		t.Fatalf("s2 迁移结果不匹配: %+v", second)
	}

	// v1 没有 kind，需要按内容推断，否则回滚点语义会丢
	wantKinds := []session.Kind{
		session.KindPrompt,
		session.KindToolCall,
		session.KindToolResult,
		session.KindAnswer,
	}
	for i, want := range wantKinds {
		if first[i].Kind != want {
			t.Fatalf("第 %d 条消息种类推断错误: got %q, want %q", i, first[i].Kind, want)
		}
	}
}

// TestMigrateV1ToV2SupportsRollback 迁移出来的历史必须能正常回滚与分叉
func TestMigrateV1ToV2SupportsRollback(t *testing.T) {
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "sessions.db")

	writeLegacyDB(t, dbPath, []string{"s1"}, []legacyRow{
		{"s1", 1, "user", "问题一"},
		{"s1", 2, "assistant", "回答一"},
		{"s1", 3, "user", "问题二"},
		{"s1", 4, "assistant", "回答二"},
	})

	store, err := New(dbPath)
	if err != nil {
		t.Fatalf("迁移失败: %v", err)
	}
	defer func() {
		if closeErr := store.Close(); closeErr != nil {
			t.Errorf("关闭会话库失败: %v", closeErr)
		}
	}()

	path := mustLoad(t, store, "s1", 0)
	if len(path) != 4 {
		t.Fatalf("迁移后应有 4 条消息: got %d", len(path))
	}

	// 回滚到「回答一」，再追加消息应该长出分支
	archiveID, err := store.Rollback(ctx, "s1", path[1].ID)
	if err != nil {
		t.Fatalf("迁移后的历史应支持回滚: %v", err)
	}
	mustAppend(t, store, "s1", prompt("换个问法"))

	if got := mustLoad(t, store, "s1", 0); len(got) != 3 {
		t.Fatalf("回滚后追加应形成 3 条的新分支: got %d", len(got))
	}
	if got := mustLoad(t, store, archiveID, 0); len(got) != 4 {
		t.Fatalf("存档分支应保留迁移前的 4 条历史: got %d", len(got))
	}
}

// TestMigrateV1WithoutVersionMarker 有旧表但没有版本标记时也要按 v1 迁移
func TestMigrateV1WithoutVersionMarker(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "sessions.db")
	writeLegacyDB(t, dbPath, []string{"s1"}, []legacyRow{{"s1", 1, "user", "老数据"}})

	// 抹掉版本标记，模拟更早期写出的库
	db, err := sql.Open("sqlite", "file:"+dbPath)
	if err != nil {
		t.Fatalf("打开会话库失败: %v", err)
	}
	if _, err := db.Exec(`DELETE FROM meta WHERE key = 'schema_version'`); err != nil {
		t.Fatalf("删除版本标记失败: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("关闭会话库失败: %v", err)
	}

	store, err := New(dbPath)
	if err != nil {
		t.Fatalf("无版本标记的旧库应能迁移: %v", err)
	}
	defer func() {
		if closeErr := store.Close(); closeErr != nil {
			t.Errorf("关闭会话库失败: %v", closeErr)
		}
	}()

	got := mustLoad(t, store, "s1", 0)
	if len(got) != 1 || got[0].Content != "老数据" {
		t.Fatalf("旧数据未被正确迁移: %+v", got)
	}
	if got[0].Kind != session.KindPrompt {
		t.Fatalf("用户消息应被推断为 prompt: got %q", got[0].Kind)
	}
}

// TestMigrateIsIdempotent 迁移过一次的库再次打开不应重复迁移或丢数据
func TestMigrateIsIdempotent(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "sessions.db")
	writeLegacyDB(t, dbPath, []string{"s1"}, []legacyRow{
		{"s1", 1, "user", "第一条"},
		{"s1", 2, "assistant", "第二条"},
	})

	first, err := New(dbPath)
	if err != nil {
		t.Fatalf("首次迁移失败: %v", err)
	}
	if err := first.Close(); err != nil {
		t.Fatalf("关闭会话库失败: %v", err)
	}

	second, err := New(dbPath)
	if err != nil {
		t.Fatalf("重复打开失败: %v", err)
	}
	defer func() {
		if closeErr := second.Close(); closeErr != nil {
			t.Errorf("关闭会话库失败: %v", closeErr)
		}
	}()

	got := mustLoad(t, second, "s1", 0)
	if len(got) != 2 {
		t.Fatalf("重复打开后消息条数变化: got %d, want 2", len(got))
	}
}
