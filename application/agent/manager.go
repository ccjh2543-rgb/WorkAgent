package agent

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"
)

// 会话实例管理的默认参数。
const (
	// defaultMaxSessionAgents 同时保留的空闲实例上限，超出后回收最久未用的。
	defaultMaxSessionAgents = 8
	// defaultSessionAgentTTL 空闲实例的保留时长。
	defaultSessionAgentTTL = 30 * time.Minute
)

// Manager 按会话管理独立的 Agent 运行实例。
//
// 目标：不同会话之间不共享任何可变运行态（消息序列、计划、压缩缓存、
// 指标与工具注册表），同一会话的多次运行自动串行。实例按需创建、
// 空闲回收，避免长期占用数据库连接与内存索引。
//
// 并发约定：
//   - Acquire 返回的 release 必须调用，且只能调用一次；
//   - 回收只发生在 refs == 0 的实例上，因此不会关闭正在使用或排队等待的实例。
type Manager struct {
	mu        sync.Mutex
	factory   func() (*Agent, error)
	agents    map[string]*managedAgent
	maxAgents int
	ttl       time.Duration
	now       func() time.Time
}

// managedAgent 一个会话实例及其占用计数。
type managedAgent struct {
	agent    *Agent
	mu       sync.Mutex // 同一会话串行执行
	refs     int        // 已获取但尚未释放的运行额度（含排队等待者）
	lastUsed time.Time
}

// NewManager 创建会话实例管理器。factory 负责创建一个全新的会话实例。
func NewManager(factory func() (*Agent, error)) *Manager {
	return &Manager{
		factory:   factory,
		agents:    make(map[string]*managedAgent),
		maxAgents: defaultMaxSessionAgents,
		ttl:       defaultSessionAgentTTL,
		now:       time.Now,
	}
}

// Acquire 获取指定会话的运行实例，并占用一次运行额度。
//
// 返回的 release 负责释放额度；调用方必须保证只调用一次，通常用 defer。
// 同一会话的第二次 Acquire 会阻塞到前一次 release，从而实现会话内串行。
func (m *Manager) Acquire(sessionID string) (*Agent, func(), error) {
	sessionID = strings.TrimSpace(sessionID)
	if sessionID == "" {
		return nil, nil, errors.New("会话 ID 不能为空")
	}

	m.mu.Lock()
	entry, ok := m.agents[sessionID]
	if !ok {
		agent, err := m.factory()
		if err != nil {
			m.mu.Unlock()
			return nil, nil, err
		}
		entry = &managedAgent{agent: agent}
		m.agents[sessionID] = entry
	}
	entry.refs++
	entry.lastUsed = m.now()
	m.evictLocked(sessionID)
	m.mu.Unlock()

	entry.mu.Lock()
	var once sync.Once
	release := func() {
		once.Do(func() {
			entry.mu.Unlock()
			m.mu.Lock()
			entry.refs--
			entry.lastUsed = m.now()
			m.mu.Unlock()
		})
	}
	return entry.agent, release, nil
}

// Run 在指定会话上执行一次任务，自动完成实例获取与释放。
func (m *Manager) Run(ctx context.Context, sessionID, query string) (string, error) {
	sessionAgent, release, err := m.Acquire(sessionID)
	if err != nil {
		return "", err
	}
	defer release()
	return sessionAgent.Run(ctx, sessionID, query)
}

// evictLocked 回收空闲且超量或过期的实例。调用方必须持有 m.mu。
func (m *Manager) evictLocked(current string) {
	now := m.now()
	type candidate struct {
		id    string
		entry *managedAgent
	}
	candidates := make([]candidate, 0, len(m.agents))
	for id, entry := range m.agents {
		if id == current || entry.refs > 0 {
			continue
		}
		expired := m.ttl > 0 && now.Sub(entry.lastUsed) > m.ttl
		if expired || len(m.agents) > m.maxAgents {
			candidates = append(candidates, candidate{id: id, entry: entry})
		}
	}
	// 先回收最久未使用的
	sort.Slice(candidates, func(i, j int) bool {
		return candidates[i].entry.lastUsed.Before(candidates[j].entry.lastUsed)
	})
	for _, item := range candidates {
		if len(m.agents) <= m.maxAgents && now.Sub(item.entry.lastUsed) <= m.ttl {
			break
		}
		delete(m.agents, item.id)
		if err := item.entry.agent.Close(); err != nil {
			fmt.Printf("[会话实例回收失败]:%v\n", err)
		}
	}
}

// EvictIdle 关闭当前所有空闲实例，返回关闭数量。
// 用于技能集变化后强制重建实例，让新会话拿到最新的工具集。
func (m *Manager) EvictIdle() int {
	m.mu.Lock()
	victims := make([]*managedAgent, 0, len(m.agents))
	for id, entry := range m.agents {
		if entry.refs > 0 {
			continue
		}
		delete(m.agents, id)
		victims = append(victims, entry)
	}
	m.mu.Unlock()

	for _, entry := range victims {
		if err := entry.agent.Close(); err != nil {
			fmt.Printf("[会话实例回收失败]:%v\n", err)
		}
	}
	return len(victims)
}

// Len 返回当前保留的实例数量（含正在运行的）。
func (m *Manager) Len() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.agents)
}

// Close 关闭全部实例，等待正在运行的任务结束后返回。
func (m *Manager) Close() error {
	m.mu.Lock()
	entries := make([]*managedAgent, 0, len(m.agents))
	for id, entry := range m.agents {
		delete(m.agents, id)
		entries = append(entries, entry)
	}
	m.mu.Unlock()

	var errs []error
	for _, entry := range entries {
		entry.mu.Lock()
		errs = append(errs, entry.agent.Close())
		entry.mu.Unlock()
	}
	return errors.Join(errs...)
}
