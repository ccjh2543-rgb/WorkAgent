package tools

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestToolOutputReaderReadsWithinRoot 验证绝对路径与相对文件名都能读取。
func TestToolOutputReaderReadsWithinRoot(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "out.txt")
	if err := os.WriteFile(path, []byte("完整工具输出"), 0o640); err != nil {
		t.Fatalf("写入测试文件失败: %v", err)
	}
	tool := NewToolOutputReader(func() string { return root })

	for _, arg := range []string{path, "out.txt"} {
		out, err := tool.Run(`{"path":"` + strings.ReplaceAll(arg, `\`, `\\`) + `"}`)
		if err != nil {
			t.Fatalf("读取 %q 失败: %v", arg, err)
		}
		if out != "完整工具输出" {
			t.Fatalf("读取 %q 内容不符: %q", arg, out)
		}
	}
}

// TestToolOutputReaderRejectsEscape 验证目录穿越与外部绝对路径都会被拒绝。
func TestToolOutputReaderRejectsEscape(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	secret := filepath.Join(outside, "secret.txt")
	if err := os.WriteFile(secret, []byte("secret"), 0o640); err != nil {
		t.Fatalf("写入测试文件失败: %v", err)
	}
	tool := NewToolOutputReader(func() string { return root })

	cases := []string{
		`{"path":"../secret.txt"}`,
		`{"path":"` + strings.ReplaceAll(secret, `\`, `\\`) + `"}`,
		`{"path":""}`,
	}
	for _, arg := range cases {
		if _, err := tool.Run(arg); err == nil {
			t.Fatalf("应拒绝越界路径: %s", arg)
		}
	}
}

// TestToolOutputReaderTruncatesLargeFile 验证超大留存文件按上限截断。
func TestToolOutputReaderTruncatesLargeFile(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "big.txt")
	content := strings.Repeat("x", ToolOutputReadMaxBytes+1024)
	if err := os.WriteFile(path, []byte(content), 0o640); err != nil {
		t.Fatalf("写入测试文件失败: %v", err)
	}
	tool := NewToolOutputReader(func() string { return root })

	out, err := tool.Run(`{"path":"big.txt"}`)
	if err != nil {
		t.Fatalf("读取大文件失败: %v", err)
	}
	if !strings.HasSuffix(out, "已截断]") {
		t.Fatalf("大文件应带截断提示，实际结尾: %q", out[len(out)-20:])
	}
	if strings.Count(out, "x") != ToolOutputReadMaxBytes {
		t.Fatalf("截断长度不符: got %d, want %d", strings.Count(out, "x"), ToolOutputReadMaxBytes)
	}
}
