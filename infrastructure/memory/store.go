package memory

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

const (
	// WorkspaceDirName 工作区数据目录名。记忆、会话等本机数据都放在该目录下
	WorkspaceDirName = ".simpleagent"
	// memoryFileName 工作区长期记忆文件名
	memoryFileName = "MEMORY.md"
	// memoryFilePerm 记忆文件权限，仅属主与同组可读写
	memoryFilePerm = 0o640
	// memoryDirPerm 记忆目录权限
	memoryDirPerm = 0o750
)

// WorkspaceDir 返回项目的工作区数据目录 <projectDir>/.simpleagent。
// 记忆、会话等本机持久化数据统一放在该目录下，便于用户整体备份或清理。
func WorkspaceDir(projectDir string) string {
	return filepath.Join(projectDir, WorkspaceDirName)
}

// WorkspaceStore 工作区记忆仓储：负责 MEMORY.md 的读取与追加写入。
//
// 并发安全：
//   - 同一个 Store 实例内部用互斥锁串行化读写
//   - 跨进程使用「写临时文件 + Rename 原子替换」，避免写入中断导致记忆文件损坏
type WorkspaceStore struct {
	path string
	mu   sync.Mutex
}

// NewWorkspaceStore 基于项目根目录创建工作区记忆仓储，
// 记忆文件路径为 <projectDir>/.simpleagent/memory/MEMORY.md。
func NewWorkspaceStore(projectDir string) *WorkspaceStore {
	return &WorkspaceStore{
		path: filepath.Join(WorkspaceDir(projectDir), "memory", memoryFileName),
	}
}

// Path 返回记忆文件路径，便于日志输出与提示用户手动编辑。
func (s *WorkspaceStore) Path() string {
	return s.path
}

// Dir 返回工作区记忆目录（与 MEMORY.md 同级），索引文件应放在该目录下。
func (s *WorkspaceStore) Dir() string {
	return filepath.Dir(s.path)
}

// Load 读取并解析记忆文件。
// 文件不存在属于正常情况（首次运行），返回空文档而不是错误。
func (s *WorkspaceStore) Load(ctx context.Context) (*Document, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.loadLocked()
}

// Append 追加一条记忆到指定章节；章节不存在时自动创建，正文完全重复时跳过写入。
// section 为空时归入「未分类」章节，content 为空返回参数错误。
func (s *WorkspaceStore) Append(ctx context.Context, section, content string) error {
	content = strings.TrimSpace(content)
	if content == "" {
		return errors.New("记忆内容不能为空")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	section = strings.TrimSpace(section)

	s.mu.Lock()
	defer s.mu.Unlock()

	doc, err := s.loadLocked()
	if err != nil {
		return err
	}
	if !doc.Add(section, content) {
		// 内容已存在，无需重复写入
		return nil
	}
	return s.writeAtomic(doc.Render())
}

// loadLocked 在已持有锁的前提下读取并解析记忆文件
func (s *WorkspaceStore) loadLocked() (*Document, error) {
	data, err := os.ReadFile(s.path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return &Document{Title: defaultTitle}, nil
		}
		return nil, fmt.Errorf("读取工作区记忆失败: %w", err)
	}
	return ParseDocument(string(data)), nil
}

// writeAtomic 以原子方式整量替换记忆文件内容。
// 先写入同目录下的临时文件并 fsync，再 Rename 覆盖目标文件，
// 保证读者看到的要么是旧内容、要么是完整的新内容。
func (s *WorkspaceStore) writeAtomic(content string) error {
	dir := filepath.Dir(s.path)
	if err := os.MkdirAll(dir, memoryDirPerm); err != nil {
		return fmt.Errorf("创建工作区记忆目录失败: %w", err)
	}
	tmp, err := os.CreateTemp(dir, ".memory-*.tmp")
	if err != nil {
		return fmt.Errorf("创建临时记忆文件失败: %w", err)
	}
	tmpName := tmp.Name()
	// Rename 成功后临时文件已不存在，此处 Remove 失败可安全忽略，仅用于异常路径清理
	defer func() {
		_ = os.Remove(tmpName)
	}()

	if _, err := tmp.WriteString(content); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("写入临时记忆文件失败: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("同步临时记忆文件失败: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("关闭临时记忆文件失败: %w", err)
	}
	if err := os.Chmod(tmpName, memoryFilePerm); err != nil {
		return fmt.Errorf("设置记忆文件权限失败: %w", err)
	}
	if err := os.Rename(tmpName, s.path); err != nil {
		return fmt.Errorf("替换记忆文件失败: %w", err)
	}
	return nil
}
