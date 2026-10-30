// Package session 提供对话历史的持久化模型。
//
// 消息以有向树组织：每条消息只有 parent 指针，没有「下一条」。
// 一根会话（Session）就是树上的一个指针，指向某个节点即 HEAD。
// 由此得到两个基本能力：
//
//   - 回滚：把 HEAD 移到某个祖先节点，历史一条都不删
//   - 分叉：新建一根指针指向任意已有节点，之后两条路径独立生长
//
// 回滚之后继续对话会自然产生新的分支——因为新消息挂在回滚目标之下，
// 原来那条路径仍然完整地留在树上。
//
// 与 memory 包的分工：
//   - memory 保存「被提炼过的长期事实」，是 Markdown 真源加可重建索引
//   - session 保存「对话原文」，是唯一真源，不可从任何地方重建
package session

import (
	"context"
	"time"
)

// Role 消息角色，取值与 OpenAI ChatCompletion 的角色对齐
type Role string

const (
	// RoleSystem 系统提示消息
	RoleSystem Role = "system"
	// RoleUser 用户消息
	RoleUser Role = "user"
	// RoleAssistant 模型消息
	RoleAssistant Role = "assistant"
	// RoleTool 工具返回
	RoleTool Role = "tool"
)

// Kind 消息种类。
//
// 区分种类的核心目的，是让「回滚点」只能落在完整轮次的边界上：
// 一轮 Run 会写入提问、工具调用、工具结果、回答等多条消息，
// 若回滚点落在工具调用与工具结果之间，恢复出的上下文会自相矛盾。
type Kind string

const (
	// KindPrompt 用户真实提问，是唯一合法的回滚点
	KindPrompt Kind = "prompt"
	// KindAnswer 模型的最终回答
	KindAnswer Kind = "answer"
	// KindToolCall 模型发起的工具调用
	KindToolCall Kind = "tool_call"
	// KindToolResult 系统注入的工具执行结果
	KindToolResult Kind = "tool_result"
	// KindGuidance 人工介入给出的补充指令
	KindGuidance Kind = "guidance"
	// KindSubtaskResult Planner-Worker 模式下记录到主会话的子任务结果摘要
	KindSubtaskResult Kind = "subtask_result"
	// KindCompaction 上下文压缩记录：保存摘要与覆盖范围，不参与模型消息序列。
	// 它随会话树持久化，使压缩结果在重启、断点续跑与分支切换后仍然有效。
	KindCompaction Kind = "compaction"
)

// Rollbackable 判断该消息能否作为回滚点
func (k Kind) Rollbackable() bool {
	return k == KindPrompt
}

// Foldable 判断该消息在上下文吃紧时能否被机械折叠。
//
// 只有工具调用与工具返回这类过程性信息可以折叠：
// 用户提问与人工指令承载任务目标，模型的回答承载结论，
// 折叠它们等于篡改任务本身。
func (k Kind) Foldable() bool {
	return k == KindToolCall || k == KindToolResult
}

// Message 树上的一个节点。
// Content 在落盘前已按 Sanitize 做过脱敏与长度截断，
// 因此库里的内容与内存中送给模型的原文可能不同。
type Message struct {
	ID        int64     // 树上的唯一标识，回滚与分叉的目标
	ParentID  int64     // 父消息，0 表示这是树根
	Kind      Kind      // 消息种类
	Role      Role      // 消息角色
	Content   string    // 消息正文
	CreatedAt time.Time // 写入时间
}

// Session 树上的一个指针，也就是一条分支
type Session struct {
	ID             string
	ConversationID string
	Label          string // 人类可读的说明，例如「回滚前自动存档」
	HeadID         int64  // 当前 HEAD，0 表示空分支
	CreatedAt      time.Time
	UpdatedAt      time.Time
}

// Branch 用于向用户展示的一条分支
type Branch struct {
	SessionID    string
	Label        string
	HeadID       int64
	MessageCount int    // 从根到 HEAD 的消息条数
	Preview      string // HEAD 处消息的摘要，便于人工辨认
	UpdatedAt    time.Time
}

// Conversation 一棵对话树及其分支概况，用于前端按会话维度展示。
type Conversation struct {
	ID        string    // 对话树 ID
	UpdatedAt time.Time // 最近活跃时间
	Branches  []Branch  // 该树全部分支
}

// Repository 会话仓储端口，由 infrastructure 层实现
type Repository interface {
	// Append 在会话 HEAD 之后追加消息，并把 HEAD 推进到最后一条。
	// 消息的 ParentID 由实现负责串接，调用方无需填写。
	// 会话不存在时自动创建，同时创建它所属的对话树。
	Append(ctx context.Context, sessionID string, messages ...Message) error

	// LoadBranch 从 HEAD 沿 parent 回溯到树根，返回其中最近 limit 条（时间正序）。
	// limit 非正数表示不限制条数。会话不存在或为空时返回空切片。
	LoadBranch(ctx context.Context, sessionID string, limit int) ([]Message, error)

	// Rollback 把 HEAD 移到指定祖先节点，并返回为原路径自动建立的存档分支 ID。
	// targetID 必须是当前 HEAD 的祖先，否则返回错误——防止跨树乱跳。
	// 任何消息都不会被删除。
	Rollback(ctx context.Context, sessionID string, targetID int64) (archiveID string, err error)

	// Fork 新建一条分支，其 HEAD 指向 fromID；fromID 为 0 表示复制当前 HEAD。
	// 返回新分支的会话 ID。
	Fork(ctx context.Context, sessionID string, fromID int64) (string, error)

	// ListBranches 列出同一条对话树上的所有分支，按最近活跃排序
	ListBranches(ctx context.Context, sessionID string) ([]Branch, error)

	// ListConversations 列出全部对话树及其分支，按最近活跃排序
	ListConversations(ctx context.Context) ([]Conversation, error)

	// DeleteExpired 删除在 before 之前就没有再更新过的对话树及其全部消息与分支，
	// 返回被删除的对话数。
	DeleteExpired(ctx context.Context, before time.Time) (int64, error)

	// Close 释放底层资源
	Close() error
}
