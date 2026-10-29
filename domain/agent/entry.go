package agent

import (
	"sync"

	"simpleAgent/domain/session"
)

// Entry 会话上下文中的一条消息。
//
// 相比裸的模型消息多带了 Kind：
// 上下文压缩需要区分「用户提问」与「工具返回」——
// 前者承载任务目标，后者只是过程性信息，两者的压缩策略完全不同。
type Entry struct {
	Kind    session.Kind
	Role    session.Role
	Content string
	// ToolCalls 仅 assistant 条目携带：模型在这条回复里请求的工具调用。
	// 保留它是为了让后续请求能把「调用」与「结果」配对回放给模型。
	ToolCalls []ToolCall
	// ToolCallID 仅 tool 结果条目携带：本结果对应哪一次工具调用。
	ToolCallID string
}

// Message 转换为发送给模型的请求消息（带上工具调用的配对信息）
func (e Entry) Message() ChatMessage {
	return ChatMessage{
		Role:       e.Role,
		Content:    e.Content,
		ToolCalls:  e.ToolCalls,
		ToolCallID: e.ToolCallID,
	}
}

// Memory 临时会话上下文，承载本轮对话的消息序列。
//
// 刻意只存放对话消息，不存放系统提示词与记忆块：
// 后两者是每次请求时按当前状态重新拼装的，混进来会让「从持久化历史恢复」
// 变得难以区分哪些该恢复、哪些该重建。
//
// 并发安全：所有读写都通过互斥锁串行化；读方法返回副本，
// 避免调用方在锁释放后继续访问共享底层数组。
type Memory struct {
	mu      sync.Mutex
	entries []Entry
}

// NewMemory 创建空的会话上下文
func NewMemory() *Memory {
	return &Memory{entries: make([]Entry, 0)}
}

// Add 按种类与角色追加一条消息
func (m *Memory) Add(kind session.Kind, role session.Role, content string) {
	m.AddEntry(Entry{Kind: kind, Role: role, Content: content})
}

// AddEntry 追加一条完整条目（可携带工具调用与调用 ID 的配对信息）。
func (m *Memory) AddEntry(entry Entry) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.entries = append(m.entries, entry)
}

// Replace 用给定消息整体替换上下文，用于从持久化的会话历史恢复。
// 采用整体替换而非追加，避免重复载入历史时把消息叠加两份。
func (m *Memory) Replace(entries []Entry) {
	m.mu.Lock()
	defer m.mu.Unlock()
	replaced := make([]Entry, len(entries))
	copy(replaced, entries)
	m.entries = replaced
}

// Entries 返回全部消息的副本
func (m *Memory) Entries() []Entry {
	m.mu.Lock()
	defer m.mu.Unlock()
	entries := make([]Entry, len(m.entries))
	copy(entries, m.entries)
	return entries
}

// Len 返回消息条数
func (m *Memory) Len() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.entries)
}

// ToEntries 把持久化的会话消息转换为上下文条目
func ToEntries(messages []session.Message) []Entry {
	entries := make([]Entry, 0, len(messages))
	for _, message := range messages {
		entries = append(entries, Entry{
			Kind:    message.Kind,
			Role:    message.Role,
			Content: message.Content,
		})
	}
	return entries
}
