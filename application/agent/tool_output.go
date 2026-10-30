package agent

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"

	domainagent "simpleAgent/domain/agent"
	"simpleAgent/domain/session"
	"simpleAgent/infrastructure/memory"
)

const (
	// toolOutputDirName 工具输出留存目录名，位于工作区数据目录下。
	toolOutputDirName = "tool_outputs"
	// toolOutputDirPerm 留存目录权限。
	toolOutputDirPerm = 0o750
	// toolOutputFilePerm 留存文件权限，仅属主与同组可读。
	toolOutputFilePerm = 0o640
)

// toolOutputSessionKey 把会话 ID 映射为固定长度的目录名。
//
// 会话 ID 由调用方传入，可能包含路径分隔符或超长字符，直接拼路径不安全；
// 取哈希既规避了路径注入，也避免不同会话的输出互相可见。
func toolOutputSessionKey(sessionID string) string {
	sum := sha256.Sum256([]byte(sessionID))
	return hex.EncodeToString(sum[:6])
}

// toolOutputDir 返回当前会话的工具输出留存目录；状态目录未初始化时返回空串。
func (a *Agent) toolOutputDir() string {
	if a.stateDir == "" {
		return ""
	}
	return filepath.Join(a.stateDir, memory.WorkspaceDirName, toolOutputDirName,
		toolOutputSessionKey(a.sessionID))
}

// spillToolOutput 把被折叠的工具输出完整留存到磁盘，返回可回取的绝对路径。
//
// 折叠只影响「发给模型的副本」，但被折叠掉的中间内容常常还需要回看。
// 留存文件名取正文哈希，因此同一内容重复折叠只写一次；写入失败返回空串，
// 由调用方退化为「只折叠、无引用」，不阻断本轮推理。
func (a *Agent) spillToolOutput(content string) string {
	dir := a.toolOutputDir()
	if dir == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(content))
	path := filepath.Join(dir, hex.EncodeToString(sum[:16])+".txt")
	if _, err := os.Stat(path); err == nil {
		return path
	}
	if err := os.MkdirAll(dir, toolOutputDirPerm); err != nil {
		fmt.Printf("[工具输出留存目录创建失败]:%v\n", err)
		return ""
	}
	tmp, err := os.CreateTemp(dir, ".spill-*.tmp")
	if err != nil {
		fmt.Printf("[工具输出留存失败]:%v\n", err)
		return ""
	}
	tmpName := tmp.Name()
	// Rename 成功后临时文件已不存在，此处 Remove 失败可安全忽略，仅用于异常路径清理
	defer func() {
		_ = os.Remove(tmpName)
	}()
	if _, err := tmp.WriteString(content); err != nil {
		_ = tmp.Close()
		fmt.Printf("[工具输出留存失败]:%v\n", err)
		return ""
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		fmt.Printf("[工具输出留存失败]:%v\n", err)
		return ""
	}
	if err := tmp.Close(); err != nil {
		fmt.Printf("[工具输出留存失败]:%v\n", err)
		return ""
	}
	if err := os.Chmod(tmpName, toolOutputFilePerm); err != nil {
		fmt.Printf("[工具输出留存失败]:%v\n", err)
		return ""
	}
	if err := os.Rename(tmpName, path); err != nil {
		fmt.Printf("[工具输出留存失败]:%v\n", err)
		return ""
	}
	return path
}

// foldToolResult 折叠单条工具输出，并为被折叠的内容附加可回取的路径引用。
//
// 引用只在确实发生折叠时追加；未超阈值的输出保持原样，避免给模型
// 制造「还有一份完整版」的错觉。
func (a *Agent) foldToolResult(content string) string {
	folded := session.FoldToolResult(content, a.foldHeadRunes, a.foldTailRunes)
	if folded == content {
		return content
	}
	if path := a.spillToolOutput(content); path != "" {
		folded += fmt.Sprintf("\n[完整输出已留存：%s；需要细节时用 read_tool_output 读取该路径]", path)
	}
	return folded
}

// foldEntries 折叠过长的工具输出（Agent 方法版：折叠同时留存原文）。
func (a *Agent) foldEntries(entries []domainagent.Entry) []domainagent.Entry {
	folded := make([]domainagent.Entry, len(entries))
	copy(folded, entries)
	for i, entry := range folded {
		if !entry.Kind.Foldable() {
			continue
		}
		folded[i].Content = a.foldToolResult(entry.Content)
	}
	return folded
}
