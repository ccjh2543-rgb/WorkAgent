package sqlitestore

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"

	"simpleAgent/domain/session"
)

const (
	// legacySessionLabel 由 v1 线性结构迁移过来的分支说明
	legacySessionLabel = "由 v1 线性结构迁移"
	// toolResultPrefix 系统注入的工具结果前缀，迁移时用于识别消息种类
	toolResultPrefix = "工具执行结果："
	// toolCallPrefix 工具调用指令前缀，迁移时用于识别消息种类
	toolCallPrefix = "!TOOL:"
)

// migrate 建表与结构迁移。
//
// 会话是不可重建的真源，因此对未知版本一律报错而不是清库；
// 只有明确可识别的 v1 线性结构才会被自动迁移。
func (s *Store) migrate() error {
	if _, err := s.db.Exec(`CREATE TABLE IF NOT EXISTS meta (
		key   TEXT PRIMARY KEY,
		value TEXT NOT NULL)`); err != nil {
		return fmt.Errorf("创建会话库元数据表失败: %w", err)
	}

	version, err := s.schemaVersion()
	if err != nil {
		return err
	}

	switch version {
	case "":
		// 没有版本标记。若已存在旧表，说明是更早期写的库，按 v1 处理；
		// 否则视为全新库。
		legacy, err := s.tableExists("messages")
		if err != nil {
			return err
		}
		if legacy {
			if err := s.migrateV1ToV2(); err != nil {
				return err
			}
			return s.setSchemaVersion(schemaVersionV2)
		}
		if err := s.createSchema(); err != nil {
			return err
		}
		return s.setSchemaVersion(schemaVersionV2)

	case schemaVersionV2:
		return s.createSchema()

	case schemaVersionV1:
		if err := s.migrateV1ToV2(); err != nil {
			return err
		}
		return s.setSchemaVersion(schemaVersionV2)

	default:
		return fmt.Errorf(
			"会话库结构版本不兼容（库中为 %s，当前程序需要 %s）。"+
				"会话记录是唯一真源，不会被自动重建，请先自行备份并迁移后重试",
			version, schemaVersionV2)
	}
}

// migrateV1ToV2 把 v1 的线性结构迁移为 v2 的树形结构。
//
// 映射关系：
//   - v1 里每个 session 是一条互不相关的直线，迁移后各自成为一棵独立对话树
//   - 对话与分支都沿用原 session ID，便于追溯
//   - 消息按 seq 升序串成一条 parent 链，分支的 HEAD 指向链尾
//
// v1 没有 kind 字段，只能按内容尽力推断，目的是保住「回滚点」的语义。
func (s *Store) migrateV1ToV2() error {
	ctx := context.Background()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("开启结构迁移事务失败: %w", err)
	}
	// 提交成功后 Rollback 返回 ErrTxDone，可安全忽略
	defer func() {
		_ = tx.Rollback()
	}()

	// 旧索引会与新结构重名，先清掉；ALTER RENAME 会带走剩余索引
	for _, stmt := range []string{
		`DROP INDEX IF EXISTS idx_messages_session`,
		`DROP INDEX IF EXISTS idx_sessions_updated`,
		`ALTER TABLE messages RENAME TO messages_v1`,
		`ALTER TABLE sessions RENAME TO sessions_v1`,
	} {
		if _, err := tx.ExecContext(ctx, stmt); err != nil {
			return fmt.Errorf("准备结构迁移失败: %w", err)
		}
	}
	for _, stmt := range schemaV2 {
		if _, err := tx.ExecContext(ctx, stmt); err != nil {
			return fmt.Errorf("建立新结构失败: %w", err)
		}
	}

	// 必须先把旧数据全部读出来再写，否则同一个连接上会出现读写互等
	legacySessions, err := readLegacySessions(ctx, tx)
	if err != nil {
		return err
	}
	legacyMessages, err := readLegacyMessages(ctx, tx)
	if err != nil {
		return err
	}

	known := make(map[string]struct{}, len(legacySessions))
	for _, legacy := range legacySessions {
		known[legacy.id] = struct{}{}
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO conversations(id, created_at, updated_at) VALUES(?, ?, ?)`,
			legacy.id, legacy.createdAt, legacy.updatedAt); err != nil {
			return fmt.Errorf("迁移对话失败: %w", err)
		}
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO sessions(id, conversation_id, label, head_id, created_at, updated_at)
			VALUES(?, ?, ?, NULL, ?, ?)`,
			legacy.id, legacy.id, legacySessionLabel, legacy.createdAt, legacy.updatedAt); err != nil {
			return fmt.Errorf("迁移会话失败: %w", err)
		}
	}

	parentOf := make(map[string]int64, len(legacySessions))
	headOf := make(map[string]int64, len(legacySessions))
	for _, message := range legacyMessages {
		if _, ok := known[message.sessionID]; !ok {
			return fmt.Errorf("迁移失败：消息 %d 引用了不存在的会话 %s", message.id, message.sessionID)
		}
		newMessageID, err := s.insertMessage(ctx, tx, message.sessionID, parentOf[message.sessionID],
			session.Message{
				Kind:    classifyLegacyMessage(message.role, message.content),
				Role:    message.role,
				Content: message.content,
			}, message.createdAt)
		if err != nil {
			return fmt.Errorf("迁移消息失败: %w", err)
		}
		parentOf[message.sessionID] = newMessageID
		headOf[message.sessionID] = newMessageID
	}
	for sessionID, head := range headOf {
		if _, err := tx.ExecContext(ctx,
			`UPDATE sessions SET head_id = ? WHERE id = ?`, head, sessionID); err != nil {
			return fmt.Errorf("回填会话 HEAD 失败: %w", err)
		}
	}

	for _, stmt := range []string{`DROP TABLE messages_v1`, `DROP TABLE sessions_v1`} {
		if _, err := tx.ExecContext(ctx, stmt); err != nil {
			return fmt.Errorf("清理旧结构失败: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("提交结构迁移事务失败: %w", err)
	}
	return nil
}

// legacySession v1 的会话记录
type legacySession struct {
	id                   string
	createdAt, updatedAt int64
}

// legacyMessage v1 的消息记录
type legacyMessage struct {
	id        int64
	sessionID string
	role      session.Role
	content   string
	createdAt int64
}

// readLegacySessions 读出全部旧会话；返回前会关闭游标，避免占用唯一连接
func readLegacySessions(ctx context.Context, tx *sql.Tx) ([]legacySession, error) {
	rows, err := tx.QueryContext(ctx, `SELECT id, created_at, updated_at FROM sessions_v1`)
	if err != nil {
		return nil, fmt.Errorf("读取旧会话失败: %w", err)
	}
	defer rows.Close()

	sessions := make([]legacySession, 0, 8)
	for rows.Next() {
		var item legacySession
		if err := rows.Scan(&item.id, &item.createdAt, &item.updatedAt); err != nil {
			return nil, fmt.Errorf("解析旧会话失败: %w", err)
		}
		sessions = append(sessions, item)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("遍历旧会话失败: %w", err)
	}
	return sessions, nil
}

// readLegacyMessages 按会话与序号读出全部旧消息；返回前会关闭游标
func readLegacyMessages(ctx context.Context, tx *sql.Tx) ([]legacyMessage, error) {
	rows, err := tx.QueryContext(ctx,
		`SELECT id, session_id, role, content, created_at FROM messages_v1 ORDER BY session_id, seq`)
	if err != nil {
		return nil, fmt.Errorf("读取旧消息失败: %w", err)
	}
	defer rows.Close()

	messages := make([]legacyMessage, 0, 64)
	for rows.Next() {
		var (
			item legacyMessage
			role string
		)
		if err := rows.Scan(&item.id, &item.sessionID, &role, &item.content, &item.createdAt); err != nil {
			return nil, fmt.Errorf("解析旧消息失败: %w", err)
		}
		item.role = session.Role(role)
		messages = append(messages, item)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("遍历旧消息失败: %w", err)
	}
	return messages, nil
}

// classifyLegacyMessage 为没有 kind 字段的历史消息推断种类。
//
// 这是尽力而为的分类：目的是让迁移后回滚点的语义尽量正确。
// 分不清的一律归为非 prompt，宁可少一个回滚点，也不要出现
// 「回滚到工具中间态」这种自相矛盾的上下文。
func classifyLegacyMessage(role session.Role, content string) session.Kind {
	switch {
	case role == session.RoleAssistant && strings.HasPrefix(content, toolCallPrefix):
		return session.KindToolCall
	case role == session.RoleUser && strings.HasPrefix(content, toolResultPrefix):
		return session.KindToolResult
	case role == session.RoleAssistant:
		return session.KindAnswer
	case role == session.RoleUser:
		return session.KindPrompt
	default:
		return session.KindAnswer
	}
}

// schemaVersion 读取库中的结构版本；没有记录时返回空串
func (s *Store) schemaVersion() (string, error) {
	var version string
	err := s.db.QueryRow(`SELECT value FROM meta WHERE key = 'schema_version'`).Scan(&version)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("读取会话库结构版本失败: %w", err)
	}
	return version, nil
}

// setSchemaVersion 写入结构版本
func (s *Store) setSchemaVersion(version string) error {
	if _, err := s.db.Exec(`
		INSERT INTO meta(key, value) VALUES('schema_version', ?)
		ON CONFLICT(key) DO UPDATE SET value = excluded.value`, version); err != nil {
		return fmt.Errorf("写入会话库结构版本失败: %w", err)
	}
	return nil
}

// createSchema 建立 v2 结构，全部使用 IF NOT EXISTS，可重复执行
func (s *Store) createSchema() error {
	for _, stmt := range schemaV2 {
		if _, err := s.db.Exec(stmt); err != nil {
			return fmt.Errorf("初始化会话库结构失败: %w", err)
		}
	}
	return nil
}
