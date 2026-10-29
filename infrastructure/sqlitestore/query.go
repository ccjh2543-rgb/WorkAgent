package sqlitestore

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"time"

	"simpleAgent/domain/session"
)

// ErrSessionNotFound 会话不存在
var ErrSessionNotFound = errors.New("会话不存在")

// querier 抽象 *sql.DB 与 *sql.Tx 的公共查询能力，
// 让同一段读取逻辑既能跑在事务里也能跑在事务外。
type querier interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

// sessionByID 读取会话；不存在时返回包装了 ErrSessionNotFound 的错误
func (s *Store) sessionByID(ctx context.Context, q querier, sessionID string) (*session.Session, error) {
	var (
		id, conversationID, label string
		headID                    sql.NullInt64
		createdAt, updatedAt      int64
	)
	err := q.QueryRowContext(ctx, `
		SELECT id, conversation_id, label, head_id, created_at, updated_at
		FROM sessions WHERE id = ?`, sessionID).
		Scan(&id, &conversationID, &label, &headID, &createdAt, &updatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("%w: %s", ErrSessionNotFound, sessionID)
	}
	if err != nil {
		return nil, fmt.Errorf("读取会话失败: %w", err)
	}
	return &session.Session{
		ID:             id,
		ConversationID: conversationID,
		Label:          label,
		HeadID:         headID.Int64,
		CreatedAt:      time.Unix(createdAt, 0),
		UpdatedAt:      time.Unix(updatedAt, 0),
	}, nil
}

// ensureSession 取出会话，不存在时新建会话及其所属对话树
func (s *Store) ensureSession(ctx context.Context, tx *sql.Tx, sessionID string) (*session.Session, error) {
	current, err := s.sessionByID(ctx, tx, sessionID)
	if err == nil {
		return current, nil
	}
	// 只有「会话不存在」才自动创建，其余错误一律上抛，避免掩盖真实故障
	if !errors.Is(err, ErrSessionNotFound) {
		return nil, err
	}

	conversationID, err := newID("conv")
	if err != nil {
		return nil, err
	}
	now := time.Now().Unix()
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO conversations(id, created_at, updated_at) VALUES(?, ?, ?)`,
		conversationID, now, now); err != nil {
		return nil, fmt.Errorf("创建对话失败: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO sessions(id, conversation_id, label, head_id, created_at, updated_at)
		VALUES(?, ?, '', NULL, ?, ?)`, sessionID, conversationID, now, now); err != nil {
		return nil, fmt.Errorf("创建会话失败: %w", err)
	}
	return &session.Session{
		ID:             sessionID,
		ConversationID: conversationID,
		CreatedAt:      time.Unix(now, 0),
		UpdatedAt:      time.Unix(now, 0),
	}, nil
}

// headID 返回会话当前 HEAD；会话不存在或为空时返回 0
func (s *Store) headID(ctx context.Context, sessionID string) (int64, error) {
	var head sql.NullInt64
	err := s.db.QueryRowContext(ctx, `SELECT head_id FROM sessions WHERE id = ?`, sessionID).Scan(&head)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf("读取会话 HEAD 失败: %w", err)
	}
	return head.Int64, nil
}

// insertMessage 插入一条消息并返回新分配的 ID。
// parentID 为 0 时写入 NULL，表示这是树根。
func (s *Store) insertMessage(
	ctx context.Context, tx *sql.Tx,
	conversationID string, parentID int64, message session.Message, now int64,
) (int64, error) {
	if message.Kind == "" {
		return 0, errors.New("消息种类不能为空")
	}
	if message.Role == "" {
		return 0, errors.New("消息角色不能为空")
	}
	var parent any
	if parentID != 0 {
		parent = parentID
	}
	result, err := tx.ExecContext(ctx, `
		INSERT INTO messages(conversation_id, parent_id, kind, role, content, created_at)
		VALUES(?, ?, ?, ?, ?, ?)`,
		conversationID, parent, string(message.Kind), string(message.Role), message.Content, now)
	if err != nil {
		return 0, fmt.Errorf("写入会话消息失败: %w", err)
	}
	newID, err := result.LastInsertId()
	if err != nil {
		return 0, fmt.Errorf("获取消息 ID 失败: %w", err)
	}
	return newID, nil
}

// loadBranch 从指定节点沿 parent 回溯到树根，返回其中最近 limit 条（时间正序）。
// limit 非正数表示不限制条数。
func (s *Store) loadBranch(ctx context.Context, headID int64, limit int) ([]session.Message, error) {
	if headID == 0 {
		return nil, nil
	}
	// SQLite 把 LIMIT -1 视为不限制
	effective := limit
	if effective <= 0 {
		effective = -1
	}
	// depth 0 是 HEAD，向上递增；取最近 limit 条后再在 Go 侧反转成正序
	rows, err := s.db.QueryContext(ctx, `
		WITH RECURSIVE path(id, parent_id, kind, role, content, created_at, depth) AS (
			SELECT id, parent_id, kind, role, content, created_at, 0
			FROM messages WHERE id = ?
			UNION ALL
			SELECT m.id, m.parent_id, m.kind, m.role, m.content, m.created_at, p.depth + 1
			FROM messages m JOIN path p ON m.id = p.parent_id
		)
		SELECT id, parent_id, kind, role, content, created_at
		FROM path ORDER BY depth ASC LIMIT ?`, headID, effective)
	if err != nil {
		return nil, fmt.Errorf("读取会话分支失败: %w", err)
	}
	defer rows.Close()

	messages := make([]session.Message, 0, 32)
	for rows.Next() {
		message, err := scanMessage(rows)
		if err != nil {
			return nil, err
		}
		messages = append(messages, message)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("遍历会话分支失败: %w", err)
	}

	for i, j := 0, len(messages)-1; i < j; i, j = i+1, j-1 {
		messages[i], messages[j] = messages[j], messages[i]
	}
	return messages, nil
}

// scanMessage 从结果集中解析一条消息
func scanMessage(rows *sql.Rows) (session.Message, error) {
	var (
		id, parentID        sql.NullInt64
		kind, role, content string
		createdAt           int64
	)
	if err := rows.Scan(&id, &parentID, &kind, &role, &content, &createdAt); err != nil {
		return session.Message{}, fmt.Errorf("解析会话消息失败: %w", err)
	}
	return session.Message{
		ID:        id.Int64,
		ParentID:  parentID.Int64,
		Kind:      session.Kind(kind),
		Role:      session.Role(role),
		Content:   content,
		CreatedAt: time.Unix(createdAt, 0),
	}, nil
}

// isAncestor 判断 targetID 是否是 headID 的祖先（含 headID 自身）
func (s *Store) isAncestor(ctx context.Context, q querier, headID, targetID int64) (bool, error) {
	var found int
	err := q.QueryRowContext(ctx, `
		WITH RECURSIVE ancestors(id, parent_id) AS (
			SELECT id, parent_id FROM messages WHERE id = ?
			UNION ALL
			SELECT m.id, m.parent_id FROM messages m JOIN ancestors a ON m.id = a.parent_id
		)
		SELECT 1 FROM ancestors WHERE id = ? LIMIT 1`, headID, targetID).Scan(&found)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("校验回滚目标失败: %w", err)
	}
	return true, nil
}

// inConversation 判断消息是否属于指定对话树
func (s *Store) inConversation(ctx context.Context, q querier, messageID int64, conversationID string) (bool, error) {
	var found int
	err := q.QueryRowContext(ctx,
		`SELECT 1 FROM messages WHERE id = ? AND conversation_id = ?`, messageID, conversationID).Scan(&found)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("校验消息归属失败: %w", err)
	}
	return true, nil
}

// touchConversation 刷新对话的活跃时间，保留期清理依赖它
func (s *Store) touchConversation(ctx context.Context, tx *sql.Tx, conversationID string, now int64) error {
	if _, err := tx.ExecContext(ctx,
		`UPDATE conversations SET updated_at = ? WHERE id = ?`, now, conversationID); err != nil {
		return fmt.Errorf("刷新对话活跃时间失败: %w", err)
	}
	return nil
}

// tableExists 判断表是否存在
func (s *Store) tableExists(name string) (bool, error) {
	var found int
	err := s.db.QueryRow(
		`SELECT 1 FROM sqlite_master WHERE type = 'table' AND name = ?`, name).Scan(&found)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("检查表 %s 是否存在失败: %w", name, err)
	}
	return true, nil
}

// newID 生成带前缀的随机标识
func newID(prefix string) (string, error) {
	buf := make([]byte, 8)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("生成标识失败: %w", err)
	}
	return prefix + "-" + hex.EncodeToString(buf), nil
}
