package artifact

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestDetectKind 覆盖常见扩展名与未知扩展名的类型推断。
func TestDetectKind(t *testing.T) {
	cases := []struct {
		path string
		want Kind
	}{
		{"output/a.xlsx", KindXLSX},
		{"output/a.XLSX", KindXLSX},
		{"output/a.docx", KindDOCX},
		{"output/a.pptx", KindPPTX},
		{"output/a.pdf", KindPDF},
		{"output/a.csv", KindCSV},
		{"output/a.json", KindJSON},
		{"output/a.md", KindMarkdown},
		{"output/a.txt", KindTXT},
		{"output/a.png", KindImage},
		{"output/a.bin", KindOther},
		{"noext", KindOther},
	}
	for _, c := range cases {
		if got := DetectKind(c.path); got != c.want {
			t.Fatalf("DetectKind(%q)=%s, want %s", c.path, got, c.want)
		}
	}
}

// TestSafeRelPath 校验交付物路径不能越出根目录。
func TestSafeRelPath(t *testing.T) {
	cases := []struct {
		in      string
		want    string
		wantErr bool
	}{
		{"output/a.json", "output/a.json", false},
		{"./output/a.json", "output/a.json", false},
		{"output\\a.json", "output/a.json", false},
		{"../evil.json", "", true},
		{"/abs/a.json", "", true},
		{"C:\\abs\\a.json", "", true},
		{"", "", true},
	}
	for _, c := range cases {
		got, err := SafeRelPath(c.in)
		if (err != nil) != c.wantErr {
			t.Fatalf("SafeRelPath(%q) err=%v, wantErr=%v", c.in, err, c.wantErr)
		}
		if !c.wantErr && got != c.want {
			t.Fatalf("SafeRelPath(%q)=%q, want %q", c.in, got, c.want)
		}
	}
}

// TestValidateBasic 表驱动覆盖正常、缺失、空文件、损坏 JSON、目录与类型不一致。
func TestValidateBasic(t *testing.T) {
	root := t.TempDir()
	mustWrite := func(rel, content string) {
		full := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatalf("创建目录失败: %v", err)
		}
		if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
			t.Fatalf("写入文件失败: %v", err)
		}
	}
	mustWrite("output/good.json", `{"a":1}`)
	mustWrite("output/empty.txt", "")
	mustWrite("output/broken.json", `{bad`)
	mustWrite("output/report.md", "# ok")
	if err := os.MkdirAll(filepath.Join(root, "output", "dir.json"), 0o755); err != nil {
		t.Fatalf("创建目录失败: %v", err)
	}

	cases := []struct {
		name        string
		art         Artifact
		wantValid   bool
		wantCheck   string
		wantCheckOK bool
	}{
		{
			name:        "正常 JSON",
			art:         Artifact{Path: "output/good.json", DeclaredType: "json", Source: SourceDeclared, Required: true},
			wantValid:   true,
			wantCheck:   "json_parse",
			wantCheckOK: true,
		},
		{
			name:        "缺失文件",
			art:         Artifact{Path: "output/missing.json", DeclaredType: "json", Source: SourceDeclared, Required: true},
			wantValid:   false,
			wantCheck:   "exists",
			wantCheckOK: false,
		},
		{
			name:        "空文件",
			art:         Artifact{Path: "output/empty.txt", DeclaredType: "txt", Source: SourceDeclared},
			wantValid:   false,
			wantCheck:   "non_empty",
			wantCheckOK: false,
		},
		{
			name:        "损坏 JSON",
			art:         Artifact{Path: "output/broken.json", DeclaredType: "json", Source: SourceDeclared},
			wantValid:   false,
			wantCheck:   "json_parse",
			wantCheckOK: false,
		},
		{
			name:        "目录不是文件",
			art:         Artifact{Path: "output/dir.json", Source: SourceDeclared},
			wantValid:   false,
			wantCheck:   "exists",
			wantCheckOK: false,
		},
		{
			name:        "声明类型不一致",
			art:         Artifact{Path: "output/report.md", DeclaredType: "xlsx", Source: SourceDeclared},
			wantValid:   false,
			wantCheck:   "mime_match",
			wantCheckOK: false,
		},
		{
			name:      "路径越界",
			art:       Artifact{Path: "../evil.json", Source: SourceDeclared},
			wantValid: false,
			wantCheck: "inside_workspace",
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			art := c.art
			art.ValidateBasic(root, nil)
			if art.Valid != c.wantValid {
				t.Fatalf("Valid=%v, want %v, checks=%+v", art.Valid, c.wantValid, art.Checks)
			}
			var found *Check
			for i := range art.Checks {
				if art.Checks[i].Name == c.wantCheck {
					found = &art.Checks[i]
					break
				}
			}
			if found == nil {
				t.Fatalf("未找到检查项 %s: %+v", c.wantCheck, art.Checks)
			}
			if c.wantCheckOK && !found.Passed {
				t.Fatalf("检查项 %s 期望通过: %+v", c.wantCheck, found)
			}
			if !c.wantCheckOK && found.Passed {
				t.Fatalf("检查项 %s 期望失败: %+v", c.wantCheck, found)
			}
		})
	}
}

// TestValidateWithFormatValidator 验证二进制格式校验结果的接入。
func TestValidateWithFormatValidator(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "output"), 0o755); err != nil {
		t.Fatalf("创建目录失败: %v", err)
	}
	if err := os.WriteFile(filepath.Join(root, "output", "book.xlsx"), []byte("PK\x03\x04placeholder"), 0o644); err != nil {
		t.Fatalf("写入失败: %v", err)
	}

	art := Artifact{Path: "output/book.xlsx", DeclaredType: "xlsx", Source: SourceDeclared}
	art.ValidateBasic(root, stubValidator{err: errors.New("zip 损坏")})
	if art.Valid {
		t.Fatalf("格式校验失败时应标记为无效: %+v", art.Checks)
	}
	var formatCheck *Check
	for i := range art.Checks {
		if art.Checks[i].Name == "format_check" {
			formatCheck = &art.Checks[i]
		}
	}
	if formatCheck == nil || formatCheck.Passed || !strings.Contains(formatCheck.Detail, "zip 损坏") {
		t.Fatalf("format_check=%+v", formatCheck)
	}

	passed := Artifact{Path: "output/book.xlsx", DeclaredType: "xlsx", Source: SourceDeclared}
	passed.ValidateBasic(root, stubValidator{})
	if !passed.Valid {
		t.Fatalf("格式校验通过时产物应有效: %+v", passed.Checks)
	}

	skipped := Artifact{Path: "output/book.xlsx", DeclaredType: "xlsx", Source: SourceDeclared}
	skipped.ValidateBasic(root, nil)
	for _, check := range skipped.Checks {
		if check.Name == "format_check" && !check.Skipped {
			t.Fatalf("未注入校验器时应标记 skipped: %+v", check)
		}
	}
}

// stubValidator 测试用格式校验器。
type stubValidator struct {
	err error
}

// ValidateFormat 返回预设错误。
func (s stubValidator) ValidateFormat(_ string, _ Kind) error { return s.err }

// TestHashFileStable 校验同一文件多次计算哈希一致且非空。
func TestHashFileStable(t *testing.T) {
	path := filepath.Join(t.TempDir(), "a.txt")
	if err := os.WriteFile(path, []byte("hello"), 0o644); err != nil {
		t.Fatalf("写入失败: %v", err)
	}
	h1, err := HashFile(path)
	if err != nil {
		t.Fatalf("计算哈希失败: %v", err)
	}
	h2, err := HashFile(path)
	if err != nil {
		t.Fatalf("计算哈希失败: %v", err)
	}
	if h1 == "" || h1 != h2 {
		t.Fatalf("哈希不稳定: %q vs %q", h1, h2)
	}
}
