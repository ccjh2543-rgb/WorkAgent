// Package sqlitestore 基于 SQLite 实现 session.Repository。
//
// 消息以有向树存储：messages 表只有 parent_id 指针，没有线性序号。
// 会话（sessions）是树上的指针，指向某个节点即 HEAD。
// 一次对话（conversations）是「一棵树」，分叉出的多条分支共享同一棵树，
// 因此可以整体查看、导出与清理。
//
// 会话库刻意与记忆索引分开存文件：
// 记忆索引是可随时从 Markdown 真源重建的派生数据，而会话是唯一真源，
// 放在同一个库里会让人误以为可以随手删除。
package sqlitestore

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	// 纯 Go 的 SQLite 驱动，无需 CGO
	_ "modernc.org/sqlite"

	"simpleAgent/domain/session"
)

const (
	// schemaVersionV2 树形结构的版本号
	schemaVersionV2 = "2"
	// schemaVersionV1 早期线性结构的版本号，需要迁移
	schemaVersionV1 = "1"
	// busyTimeoutMS 数据库繁忙时的等待时间
	busyTimeoutMS = 5000
	// dbDirPerm 会话库目录权限
	dbDirPerm = 0o750
	// archiveLabelPrefix 回滚时自动存档分支的说明前缀
	archiveLabelPrefix = "回滚前自动存档"
	// branchPreviewRunes 分支列表里 HEAD 摘要的长度
	branchPreviewRunes = 40
)

// 建表语句。全部使用 IF NOT EXISTS，便于迁移后重复执行。
var schemaV2 = []string{
	`CREATE TABLE IF NOT EXISTS conversations (
		id         TEXT PRIMARY KEY,
		created_at INTEGER NOT NULL,
		updated_at INTEGER NOT NULL
	)`,
	`CREATE TABLE IF NOT EXISTS messages (
		id              INTEGER PRIMARY KEY,
		conversation_id TEXT    NOT NULL,
		parent_id       INTEGER,
		kind            TEXT    NOT NULL,
		role            TEXT    NOT NULL,
		content         TEXT    NOT NULL,
		created_at      INTEGER NOT NULL
	)`,
	`CREATE TABLE IF NOT EXISTS sessions (
		id              TEXT PRIMARY KEY,
		conversation_id TEXT    NOT NULL,
		label           TEXT    NOT NULL DEFAULT '',
		head_id         INTEGER,
		created_at      INTEGER NOT NULL,
		updated_at      INTEGER NOT NULL
	)`,
	`CREATE INDEX IF NOT EXISTS idx_messages_conversation ON messages(conversation_id, id)`,
	`CREATE INDEX IF NOT EXISTS idx_messages_parent ON messages(parent_id)`,
	`CREATE INDEX IF NOT EXISTS idx_sessions_conversation ON sessions(conversation_id)`,
	`CREATE INDEX IF NOT EXISTS idx_conversations_updated ON conversations(updated_at)`,
}

// Store 基于 SQLite 的会话仓储。
//
// 并发安全：连接数限制为 1，写入包在事务里。
// 与记忆索引一样，只支持单进程写入。
type Store struct {
	db *sql.DB
}

// New 打开或创建会话库。dbPath 为完整文件路径，父目录不存在时自动创建。
// 库中若存在 v1 线性结构，会自动迁移为 v2 树形结构。
func New(dbPath string) (*Store, error) {
	if dbPath == "" {
		return nil, errors.New("会话库路径不能为空")
	}
	if err := os.MkdirAll(filepath.Dir(dbPath), dbDirPerm); err != nil {
		return nil, fmt.Errorf("创建会话库目录失败: %w", err)
	}
	dsn := fmt.Sprintf("file:%s?_pragma=busy_timeout(%d)&_pragma=journal_mode(WAL)", dbPath, busyTimeoutMS)
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("打开会话库失败: %w", err)
	}
	db.SetMaxOpenConns(1)

	store := &Store{db: db}
	if err := store.migrate(); err != nil {
		// 迁移失败时必须关闭已打开的连接，否则文件句柄泄漏
		_ = db.Close()
		return nil, err
	}
	return store, nil
}

// Append 在会话 HEAD 之后追加消息，并把 HEAD 推进到最后一条。
// 会话不存在时自动创建，同时创建它所属的对话树。
func (s *Store) Append(ctx context.Context, sessionID string, messages ...session.Message) error {
	if sessionID == "" {
		return errors.New("会话 ID 不能为空")
	}
	if len(messages) == 0 {
		return nil
	}
	if err := ctx.Err(); err != nil {
		return err
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("开启会话写入事务失败: %w", err)
	}
	// 提交成功后 Rollback 返回 ErrTxDone，可安全忽略
	defer func() {
		_ = tx.Rollback()
	}()

	current, err := s.ensureSession(ctx, tx, sessionID)
	if err != nil {
		return err
	}

	now := time.Now().Unix()
	parentID := current.HeadID
	headID := current.HeadID
	for _, message := range messages {
		// 空消息没有记录价值（模型偶尔会返回空内容），跳过但不报错
		if message.Content == "" {
			continue
		}
		newID, err := s.insertMessage(ctx, tx, current.ConversationID, parentID, message, now)
		if err != nil {
			return err
		}
		parentID = newID
		headID = newID
	}

	if _, err := tx.ExecContext(ctx,
		`UPDATE sessions SET head_id = ?, updated_at = ? WHERE id = ?`, headID, now, sessionID); err != nil {
		return fmt.Errorf("推进会话 HEAD 失败: %w", err)
	}
	if err := s.touchConversation(ctx, tx, current.ConversationID, now); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("提交会话写入事务失败: %w", err)
	}
	return nil
}

// LoadBranch 从 HEAD 沿 parent 回溯到树根，返回其中最近 limit 条（时间正序）。
// limit 非正数表示不限制条数。
func (s *Store) LoadBranch(ctx context.Context, sessionID string, limit int) ([]session.Message, error) {
	if sessionID == "" {
		return nil, errors.New("会话 ID 不能为空")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	headID, err := s.headID(ctx, sessionID)
	if err != nil {
		return nil, err
	}
	if headID == 0 {
		return nil, nil
	}
	return s.loadBranch(ctx, headID, limit)
}

// Rollback 把 HEAD 移到指定祖先节点，并返回为原路径自动建立的存档分支 ID。
//
// 只移动指针，任何消息都不会被删除：
// 原路径会被存成一个带说明的分支，随时可以切回去；
// 回滚之后继续追加消息，新消息挂在目标节点之下，自然形成一条新分支。
func (s *Store) Rollback(ctx context.Context, sessionID string, targetID int64) (string, error) {
	if sessionID == "" {
		return "", errors.New("会话 ID 不能为空")
	}
	if targetID <= 0 {
		return "", errors.New("回滚目标不能为空")
	}
	if err := ctx.Err(); err != nil {
		return "", err
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return "", fmt.Errorf("开启回滚事务失败: %w", err)
	}
	// 提交成功后 Rollback 返回 TxDone，可安全忽略
	defer func() {
		_ = tx.Rollback()
	}()

	current, err := s.sessionByID(ctx, tx, sessionID)
	if err != nil {
		return "", err
	}
	if current.HeadID == 0 {
		return "", fmt.Errorf("会话 %s 还没有任何消息，无法回滚", sessionID)
	}
	if current.HeadID == targetID {
		return "", fmt.Errorf("回滚目标就是当前 HEAD，无需回滚")
	}

	// 校验目标是当前 HEAD 的祖先，避免跨树乱跳
	isAncestor, err := s.isAncestor(ctx, tx, current.HeadID, targetID)
	if err != nil {
		return "", err
	}
	if !isAncestor {
		return "", fmt.Errorf("消息 %d 不是当前 HEAD(消息 %d) 的祖先，无法回滚", targetID, current.HeadID)
	}

	// 为原路径自动建存档分支，保证回滚不会让人找不回原来那条路
	archiveID, err := newID("branch")
	if err != nil {
		return "", err
	}
	now := time.Now().Unix()
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO sessions(id, conversation_id, label, head_id, created_at, updated_at)
		VALUES(?, ?, ?, ?, ?, ?)`,
		archiveID, current.ConversationID,
		fmt.Sprintf("%s（HEAD #%d）", archiveLabelPrefix, current.HeadID),
		current.HeadID, now, now); err != nil {
		return "", fmt.Errorf("创建存档分支失败: %w", err)
	}

	if _, err := tx.ExecContext(ctx,
		`UPDATE sessions SET head_id = ?, updated_at = ? WHERE id = ?`, targetID, now, sessionID); err != nil {
		return "", fmt.Errorf("移动会话 HEAD 失败: %w", err)
	}
	if err := s.touchConversation(ctx, tx, current.ConversationID, now); err != nil {
		return "", err
	}
	if err := tx.Commit(); err != nil {
		return "", fmt.Errorf("提交回滚事务失败: %w", err)
	}
	return archiveID, nil
}

// Fork 新建一条分支，其 HEAD 指向 fromID；fromID 为 0 表示复制当前 HEAD。
// 返回新分支的会话 ID。
func (s *Store) Fork(ctx context.Context, sessionID string, fromID int64) (string, error) {
	if sessionID == "" {
		return "", errors.New("会话 ID 不能为空")
	}
	if err := ctx.Err(); err != nil {
		return "", err
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return "", fmt.Errorf("开启分叉事务失败: %w", err)
	}
	// 提交成功后 Rollback 返回 TxDone，可安全忽略
	defer func() {
		_ = tx.Rollback()
	}()

	current, err := s.sessionByID(ctx, tx, sessionID)
	if err != nil {
		return "", err
	}

	target := fromID
	label := "从当前 HEAD 分叉"
	if target == 0 {
		target = current.HeadID
	} else {
		// 允许从同一棵树的任意节点分叉，而不只是当前分支上的节点
		inTree, err := s.inConversation(ctx, tx, target, current.ConversationID)
		if err != nil {
			return "", err
		}
		if !inTree {
			return "", fmt.Errorf("消息 %d 不属于当前对话，无法分叉", target)
		}
		label = fmt.Sprintf("从消息 #%d 分叉", target)
	}

	newSessionID, err := newID("branch")
	if err != nil {
		return "", err
	}
	now := time.Now().Unix()
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO sessions(id, conversation_id, label, head_id, created_at, updated_at)
		VALUES(?, ?, ?, ?, ?, ?)`,
		newSessionID, current.ConversationID, label, target, now, now); err != nil {
		return "", fmt.Errorf("创建分叉失败: %w", err)
	}
	if err := s.touchConversation(ctx, tx, current.ConversationID, now); err != nil {
		return "", err
	}
	if err := tx.Commit(); err != nil {
		return "", fmt.Errorf("提交分叉事务失败: %w", err)
	}
	return newSessionID, nil
}

// ListBranches 列出同一条对话树上的所有分支，按最近活跃排序。
func (s *Store) ListBranches(ctx context.Context, sessionID string) ([]session.Branch, error) {
	if sessionID == "" {
		return nil, errors.New("会话 ID 不能为空")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	current, err := s.sessionByID(ctx, s.db, sessionID)
	if err != nil {
		return nil, err
	}
	return s.branchesOfConversation(ctx, current.ConversationID)
}

// branchesOfConversation 列出同一棵会话树的全部分支，按最近活跃排序。
// 抽出供 ListBranches 与 ListConversations 复用。
func (s *Store) branchesOfConversation(ctx context.Context, conversationID string) ([]session.Branch, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, label, head_id, updated_at
		FROM sessions
		WHERE conversation_id = ?
		ORDER BY updated_at DESC, id`, conversationID)
	if err != nil {
		return nil, fmt.Errorf("查询分支列表失败: %w", err)
	}

	type rawBranch struct {
		id        string
		label     string
		headID    int64
		updatedAt int64
	}
	raw := make([]rawBranch, 0, 8)
	for rows.Next() {
		var item rawBranch
		var headID sql.NullInt64
		if err := rows.Scan(&item.id, &item.label, &headID, &item.updatedAt); err != nil {
			_ = rows.Close()
			return nil, fmt.Errorf("解析分支列表失败: %w", err)
		}
		item.headID = headID.Int64
		raw = append(raw, item)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return nil, fmt.Errorf("遍历分支列表失败: %w", err)
	}
	// 必须显式关闭游标：连接数上限为 1，游标不关就无法执行后续查询
	if err := rows.Close(); err != nil {
		return nil, fmt.Errorf("关闭分支列表游标失败: %w", err)
	}

	// 分支数量很少（通常个位数），逐条取路径长度与摘要换取更清晰的实现
	branches := make([]session.Branch, 0, len(raw))
	for _, item := range raw {
		branch := session.Branch{
			SessionID: item.id,
			Label:     item.label,
			HeadID:    item.headID,
			UpdatedAt: time.Unix(item.updatedAt, 0),
		}
		if item.headID != 0 {
			path, err := s.loadBranch(ctx, item.headID, 0)
			if err != nil {
				return nil, err
			}
			branch.MessageCount = len(path)
			if len(path) > 0 {
				branch.Preview = session.Preview(path[len(path)-1].Content, branchPreviewRunes)
			}
		}
		branches = append(branches, branch)
	}
	return branches, nil
}

// ListConversations 列出全部会话树（对话树）及其分支概况，按最近活跃排序。
func (s *Store) ListConversations(ctx context.Context) ([]session.Conversation, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	rows, err := s.db.QueryContext(ctx, `
		SELECT id FROM conversations
		ORDER BY updated_at DESC, id`)
	if err != nil {
		return nil, fmt.Errorf("查询会话列表失败: %w", err)
	}

	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			_ = rows.Close()
			return nil, fmt.Errorf("解析会话列表失败: %w", err)
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return nil, fmt.Errorf("遍历会话列表失败: %w", err)
	}
	if err := rows.Close(); err != nil {
		return nil, fmt.Errorf("关闭会话列表游标失败: %w", err)
	}

	// 对话树数量通常不多，逐棵取分支换取实现清晰
	conversations := make([]session.Conversation, 0, len(ids))
	for _, id := range ids {
		branches, err := s.branchesOfConversation(ctx, id)
		if err != nil {
			return nil, err
		}
		conv := session.Conversation{ID: id, Branches: branches}
		if len(branches) > 0 {
			conv.UpdatedAt = branches[0].UpdatedAt
		}
		conversations = append(conversations, conv)
	}
	return conversations, nil
}

// DeleteExpired 删除在 before 之前就没有再更新过的对话树，
// 连同它的全部消息与分支一并清理。返回被删除的对话数。
func (s *Store) DeleteExpired(ctx context.Context, before time.Time) (int64, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	cutoff := before.Unix()

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("开启会话清理事务失败: %w", err)
	}
	// 提交成功后 Rollback 返回 ErrTxDone，可安全忽略
	defer func() {
		_ = tx.Rollback()
	}()

	// 顺序不能颠倒：先删从属数据，最后删对话本身，否则子查询会先变空
	if _, err := tx.ExecContext(ctx, `
		DELETE FROM messages
		WHERE conversation_id IN (SELECT id FROM conversations WHERE updated_at < ?)`, cutoff); err != nil {
		return 0, fmt.Errorf("清理过期对话的消息失败: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `
		DELETE FROM sessions
		WHERE conversation_id IN (SELECT id FROM conversations WHERE updated_at < ?)`, cutoff); err != nil {
		return 0, fmt.Errorf("清理过期对话的分支失败: %w", err)
	}
	result, err := tx.ExecContext(ctx, `DELETE FROM conversations WHERE updated_at < ?`, cutoff)
	if err != nil {
		return 0, fmt.Errorf("清理过期对话失败: %w", err)
	}
	removed, err := result.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("统计对话清理结果失败: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("提交会话清理事务失败: %w", err)
	}
	return removed, nil
}

// Close 关闭底层数据库连接
func (s *Store) Close() error {
	if s == nil || s.db == nil {
		return nil
	}
	return s.db.Close()
}
