package bench

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"os"
	"path/filepath"
	"testing"
)

// writeTarGz 构造测试用 tar.gz。
func writeTarGz(t *testing.T, entries []tar.Header, contents map[string]string) string {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for i := range entries {
		hdr := entries[i]
		if hdr.Typeflag == tar.TypeReg {
			hdr.Size = int64(len(contents[hdr.Name]))
		}
		if err := tw.WriteHeader(&hdr); err != nil {
			t.Fatalf("写 header 失败: %v", err)
		}
		if hdr.Typeflag == tar.TypeReg {
			if _, err := tw.Write([]byte(contents[hdr.Name])); err != nil {
				t.Fatalf("写内容失败: %v", err)
			}
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatalf("关闭 tar 失败: %v", err)
	}
	if err := gz.Close(); err != nil {
		t.Fatalf("关闭 gzip 失败: %v", err)
	}
	path := filepath.Join(t.TempDir(), "workspace.tar.gz")
	if err := os.WriteFile(path, buf.Bytes(), 0o644); err != nil {
		t.Fatalf("写入归档失败: %v", err)
	}
	return path
}

// TestExtractTarGz 校验正常解压与目录创建。
func TestExtractTarGz(t *testing.T) {
	archive := writeTarGz(t,
		[]tar.Header{
			{Name: "input/", Typeflag: tar.TypeDir, Mode: 0o755},
			{Name: "input/data.csv", Typeflag: tar.TypeReg, Mode: 0o644},
		},
		map[string]string{"input/data.csv": "a,b\n1,2\n"},
	)
	dest := t.TempDir()
	if err := ExtractTarGz(archive, dest); err != nil {
		t.Fatalf("解压失败: %v", err)
	}
	data, err := os.ReadFile(filepath.Join(dest, "input", "data.csv"))
	if err != nil {
		t.Fatalf("读取解压文件失败: %v", err)
	}
	if string(data) != "a,b\n1,2\n" {
		t.Fatalf("内容不一致: %q", data)
	}
}

// TestExtractTarGzRejectsUnsafe 表驱动覆盖路径穿越、绝对路径与链接文件。
func TestExtractTarGzRejectsUnsafe(t *testing.T) {
	cases := []struct {
		name string
		hdr  tar.Header
	}{
		{"父目录跳转", tar.Header{Name: "../evil.txt", Typeflag: tar.TypeReg, Mode: 0o644}},
		{"绝对路径", tar.Header{Name: "/tmp/evil.txt", Typeflag: tar.TypeReg, Mode: 0o644}},
		{"符号链接", tar.Header{Name: "link", Typeflag: tar.TypeSymlink, Linkname: "/etc/passwd", Mode: 0o777}},
		{"硬链接", tar.Header{Name: "hard", Typeflag: tar.TypeLink, Linkname: "input/a.txt", Mode: 0o644}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			archive := writeTarGz(t, []tar.Header{c.hdr}, map[string]string{c.hdr.Name: "x"})
			if err := ExtractTarGz(archive, t.TempDir()); err == nil {
				t.Fatalf("不安全归档应被拒绝")
			}
		})
	}
}
