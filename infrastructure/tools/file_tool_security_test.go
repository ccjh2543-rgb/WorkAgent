package tools

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestReadFileToolDeniesSensitive 验证读文件工具在真实调用链上拒绝敏感文件。
func TestReadFileToolDeniesSensitive(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, ".env"), []byte("OPENAI_API_KEY=sk-secret"), 0o600); err != nil {
		t.Fatalf("创建 .env 失败: %v", err)
	}
	ws, err := NewWorkspace(root)
	if err != nil {
		t.Fatalf("创建工作区失败: %v", err)
	}

	tool := NewReadFileTool(ws)
	if _, err := tool.Run(`{"path":".env"}`); err == nil {
		t.Fatal("read_file .env 必须被拒绝")
	}
}

// TestWriteFileToolDeniesSensitive 验证写文件工具拒绝敏感路径，且不会留下文件。
func TestWriteFileToolDeniesSensitive(t *testing.T) {
	root := t.TempDir()
	ws, err := NewWorkspace(root)
	if err != nil {
		t.Fatalf("创建工作区失败: %v", err)
	}

	tool := NewWriteFileTool(ws)
	if _, err := tool.Run(`{"path":".env","content":"OPENAI_API_KEY=sk-secret"}`); err == nil {
		t.Fatal("write_file .env 必须被拒绝")
	}
	if _, statErr := os.Stat(filepath.Join(root, ".env")); !os.IsNotExist(statErr) {
		t.Fatalf(".env 不应被创建，stat 结果: %v", statErr)
	}

	if _, err := tool.Run(`{"path":"output/report.xlsx","content":"ok"}`); err != nil {
		t.Fatalf("普通产物应允许写入: %v", err)
	}
	if _, statErr := os.Stat(filepath.Join(root, "output", "report.xlsx")); statErr != nil {
		t.Fatalf("普通产物应落盘: %v", statErr)
	}
}

// TestSearchFilesToolSkipsSensitive 验证内容搜索不会命中敏感文件。
func TestSearchFilesToolSkipsSensitive(t *testing.T) {
	root := t.TempDir()
	files := map[string]string{
		".env":        "API_KEY_MARKER=secret",
		"secrets.txt": "API_KEY_MARKER=secret",
		"notes.md":    "这里提到 API_KEY_MARKER 但属于正常文档",
	}
	for name, content := range files {
		if err := os.WriteFile(filepath.Join(root, name), []byte(content), 0o600); err != nil {
			t.Fatalf("创建 %s 失败: %v", name, err)
		}
	}
	ws, err := NewWorkspace(root)
	if err != nil {
		t.Fatalf("创建工作区失败: %v", err)
	}

	out, err := NewSearchFilesTool(ws).Run(`{"keyword":"API_KEY_MARKER"}`)
	if err != nil {
		t.Fatalf("搜索失败: %v", err)
	}
	if strings.Contains(out, ".env") {
		t.Fatalf("搜索结果不应包含 .env: %s", out)
	}
	if strings.Contains(out, "secrets.txt") {
		t.Fatalf("搜索结果不应包含 secrets.txt: %s", out)
	}
	if !strings.Contains(out, "notes.md") {
		t.Fatalf("搜索结果应包含正常文档: %s", out)
	}
}
