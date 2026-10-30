package agent

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"simpleAgent/domain/session"
)

// newTestManager 用临时根目录创建会话实例，避免测试之间共享状态。
// 清理顺序很重要：管理器必须先关闭实例，再删除目录，否则 Windows 会因
// SQLite 文件仍被占用而删除失败（t.Cleanup 按注册的逆序执行）。
func newTestManager(t *testing.T) *Manager {
	t.Helper()
	base, err := os.MkdirTemp("", "agent-manager-*")
	if err != nil {
		t.Fatalf("创建临时目录失败: %v", err)
	}
	t.Cleanup(func() {
		_ = os.RemoveAll(base)
	})

	seq := 0
	m := NewManager(func() (*Agent, error) {
		seq++
		return NewAgentWithOptions(zoneStubModel{reply: "ok"}, Options{
			WorkspaceRoot: filepath.Join(base, fmt.Sprintf("w-%d", seq)),
			StateRoot:     filepath.Join(base, fmt.Sprintf("s-%d", seq)),
		}), nil
	})
	t.Cleanup(func() {
		if err := m.Close(); err != nil {
			t.Errorf("关闭管理器失败: %v", err)
		}
	})
	return m
}

// TestManagerIsolatesSessions 验证不同会话拿到不同实例，运行态互不影响。
func TestManagerIsolatesSessions(t *testing.T) {
	m := newTestManager(t)

	a1, release1, err := m.Acquire("sess-a")
	if err != nil {
		t.Fatalf("获取会话 A 失败: %v", err)
	}
	defer release1()
	a2, release2, err := m.Acquire("sess-b")
	if err != nil {
		t.Fatalf("获取会话 B 失败: %v", err)
	}
	defer release2()

	if a1 == a2 {
		t.Fatal("不同会话不应共享同一个 Agent 实例")
	}
	a1.memory.Add(session.KindPrompt, session.RoleUser, "会话 A 的问题")
	if a2.memory.Len() != 0 {
		t.Fatalf("会话 B 的上下文被会话 A 污染: %d 条", a2.memory.Len())
	}
}

// TestManagerSerializesSameSession 验证同一会话的并发获取会串行等待。
func TestManagerSerializesSameSession(t *testing.T) {
	m := newTestManager(t)

	_, release1, err := m.Acquire("sess-same")
	if err != nil {
		t.Fatalf("首次获取失败: %v", err)
	}

	done := make(chan struct{})
	go func() {
		_, release2, err := m.Acquire("sess-same")
		if err == nil {
			release2()
		}
		close(done)
	}()

	select {
	case <-done:
		t.Fatal("同一会话的第二次获取不应立即返回")
	case <-time.After(50 * time.Millisecond):
	}

	release1()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("释放后第二次获取应继续执行")
	}
}

// TestManagerEvictsIdleInstances 验证超出上限时回收最久未用的空闲实例。
func TestManagerEvictsIdleInstances(t *testing.T) {
	m := newTestManager(t)
	m.maxAgents = 1

	_, release1, err := m.Acquire("sess-1")
	if err != nil {
		t.Fatalf("获取 sess-1 失败: %v", err)
	}
	release1()

	_, release2, err := m.Acquire("sess-2")
	if err != nil {
		t.Fatalf("获取 sess-2 失败: %v", err)
	}
	defer release2()

	if got := m.Len(); got != 1 {
		t.Fatalf("实例数应被限制为 1，实际 %d", got)
	}
}

// TestManagerEvictIdle 验证显式清理空闲实例。
func TestManagerEvictIdle(t *testing.T) {
	m := newTestManager(t)

	_, release, err := m.Acquire("sess-idle")
	if err != nil {
		t.Fatalf("获取会话失败: %v", err)
	}
	release()

	if got := m.EvictIdle(); got != 1 {
		t.Fatalf("应回收 1 个空闲实例，实际 %d", got)
	}
	if got := m.Len(); got != 0 {
		t.Fatalf("回收后实例数应为 0，实际 %d", got)
	}
}

// TestManagerFactoryError 验证创建失败时向上返回错误且不缓存半成品。
func TestManagerFactoryError(t *testing.T) {
	wantErr := errors.New("模型未配置")
	m := NewManager(func() (*Agent, error) { return nil, wantErr })
	defer m.Close()

	if _, _, err := m.Acquire("sess-x"); !errors.Is(err, wantErr) {
		t.Fatalf("应返回工厂错误，实际 %v", err)
	}
	if got := m.Len(); got != 0 {
		t.Fatalf("创建失败不应留下实例，实际 %d", got)
	}
}

// TestNewSessionAgentIsolatesRuntime 验证克隆实例的运行态与父实例相互独立。
func TestNewSessionAgentIsolatesRuntime(t *testing.T) {
	base := NewAgentWithOptions(zoneStubModel{reply: "ok"}, Options{
		WorkspaceRoot: t.TempDir(),
		StateRoot:     t.TempDir(),
	})
	defer base.Close()

	clone, err := base.NewSessionAgent()
	if err != nil {
		t.Fatalf("创建会话实例失败: %v", err)
	}
	defer clone.Close()

	if clone == base {
		t.Fatal("会话实例不应等于父实例")
	}
	if clone.contextBudget != base.contextBudget {
		t.Fatalf("会话实例应继承上下文预算: got %d, want %d", clone.contextBudget, base.contextBudget)
	}
	clone.memory.Add(session.KindPrompt, session.RoleUser, "克隆实例的问题")
	if base.memory.Len() != 0 {
		t.Fatalf("父实例上下文被克隆污染: %d 条", base.memory.Len())
	}
	if clone.traceFile != nil {
		t.Fatal("会话实例不应打开进程级 trace 文件")
	}
}
