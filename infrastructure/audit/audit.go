// Package audit 提供追加写入的 JSONL 审计日志。
//
// 审计文件位于工作区 `.simpleagent/audit` 下，而该目录已被路径策略整体拒绝，
// 因此 Agent 无法读取或篡改自己的审计记录。
package audit

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// Event 一条审计记录。Detail 只放相对路径、工具名等非敏感信息，
// 禁止写入 Token、密钥或命令参数原文。
type Event struct {
	Time   time.Time `json:"time"`
	Type   string    `json:"type"`             // auth / approval / skill / file_write
	Action string    `json:"action,omitempty"` // allow / deny / error
	Source string    `json:"source,omitempty"` // 来源 IP、cli、ws
	Detail string    `json:"detail,omitempty"` // 事件细节
}

// Logger 并发安全的审计写入器。
type Logger struct {
	mu   sync.Mutex
	file *os.File
	enc  *json.Encoder
}

// Open 打开（不存在则创建）审计文件，目录权限 0700、文件权限 0600。
func Open(path string) (*Logger, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, fmt.Errorf("创建审计目录失败: %w", err)
	}
	file, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return nil, fmt.Errorf("打开审计文件失败: %w", err)
	}
	return &Logger{file: file, enc: json.NewEncoder(file)}, nil
}

// Log 写入一条审计记录。nil Logger 安全空操作，便于测试与可选装配。
func (l *Logger) Log(ev Event) {
	if l == nil {
		return
	}
	ev.Time = time.Now().UTC()
	l.mu.Lock()
	defer l.mu.Unlock()
	if err := l.enc.Encode(ev); err != nil {
		// 审计失败不能反过来阻塞业务，但必须留下可见告警
		fmt.Fprintf(os.Stderr, "[审计写入失败] %v\n", err)
	}
}

// Close 关闭审计文件。
func (l *Logger) Close() error {
	if l == nil {
		return nil
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.file.Close()
}
