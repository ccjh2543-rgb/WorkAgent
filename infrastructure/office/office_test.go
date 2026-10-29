package office

import (
	"archive/zip"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"simpleAgent/domain/office"
)

// TestXLSXRoundTrip 验证写入后能被自身读取器还原。
func TestXLSXRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "out.xlsx")
	wb := office.Workbook{Sheets: []office.Sheet{
		{
			Name: "汇总",
			Rows: [][]string{
				{"股票代码", "得分", "说明"},
				{"000001", "88.5", "前导零保留"},
				{"600519", "90", "整数"},
			},
		},
		{
			Name: "明细",
			Rows: [][]string{
				{"字段", "值"},
				{"alpha", "1"},
			},
		},
	}}
	if err := WriteXLSX(path, wb); err != nil {
		t.Fatalf("写入失败: %v", err)
	}

	got, err := ReadXLSX(path)
	if err != nil {
		t.Fatalf("读取失败: %v", err)
	}
	if len(got.Sheets) != 2 {
		t.Fatalf("工作表数=%d, want 2", len(got.Sheets))
	}
	if got.Sheets[0].Name != "汇总" || got.Sheets[1].Name != "明细" {
		t.Fatalf("工作表名称=%v", []string{got.Sheets[0].Name, got.Sheets[1].Name})
	}
	wantRows := [][]string{
		{"股票代码", "得分", "说明"},
		{"000001", "88.5", "前导零保留"},
		{"600519", "90", "整数"},
	}
	for i, want := range wantRows {
		gotRow := got.Sheets[0].Rows[i]
		if strings.Join(gotRow, "|") != strings.Join(want, "|") {
			t.Fatalf("第 %d 行=%q, want %q", i+1, gotRow, want)
		}
	}

	if err := NewValidator().ValidateFormat(path, "xlsx"); err != nil {
		t.Fatalf("格式校验失败: %v", err)
	}
}

// TestFormatExcelDate 覆盖日期序列号渲染。
func TestFormatExcelDate(t *testing.T) {
	cases := []struct {
		serial float64
		want   string
	}{
		{1, "1900-01-01"},
		{46023, "2026-01-01"},
		{46023.5, "2026-01-01 12:00:00"},
		{0, "0"},
	}
	for _, c := range cases {
		if got := formatExcelDate(c.serial); got != c.want {
			t.Fatalf("formatExcelDate(%v)=%q, want %q", c.serial, got, c.want)
		}
	}
}

// TestReadDOCX 用最小 docx 包验证段落与表格提取。
func TestReadDOCX(t *testing.T) {
	path := filepath.Join(t.TempDir(), "doc.docx")
	document := `<?xml version="1.0" encoding="UTF-8" standalone="yes"?>
<w:document xmlns:w="http://schemas.openxmlformats.org/wordprocessingml/2006/main"><w:body>
<w:p><w:r><w:t>第一段</w:t></w:r></w:p>
<w:p><w:r><w:t>第二段</w:t></w:r></w:p>
<w:tbl>
<w:tr><w:tc><w:p><w:r><w:t>姓名</w:t></w:r></w:p></w:tc><w:tc><w:p><w:r><w:t>部门</w:t></w:r></w:p></w:tc></w:tr>
<w:tr><w:tc><w:p><w:r><w:t>张三</w:t></w:r></w:p></w:tc><w:tc><w:p><w:r><w:t>研发</w:t></w:r></w:p></w:tc></w:tr>
</w:tbl>
</w:body></w:document>`
	writeTestZip(t, path, map[string]string{
		"[Content_Types].xml": `<?xml version="1.0"?><Types xmlns="http://schemas.openxmlformats.org/package/2006/content-types"/>`,
		"word/document.xml":   document,
	})

	doc, err := ReadDOCX(path)
	if err != nil {
		t.Fatalf("读取失败: %v", err)
	}
	if strings.Join(doc.Paragraphs, "|") != "第一段|第二段" {
		t.Fatalf("段落=%v", doc.Paragraphs)
	}
	if len(doc.Tables) != 1 || len(doc.Tables[0]) != 2 {
		t.Fatalf("表格结构=%v", doc.Tables)
	}
	if doc.Tables[0][1][0] != "张三" || doc.Tables[0][1][1] != "研发" {
		t.Fatalf("表格内容=%v", doc.Tables[0])
	}
}

// TestReadPPTX 用最小 pptx 包验证按页文本提取。
func TestReadPPTX(t *testing.T) {
	path := filepath.Join(t.TempDir(), "deck.pptx")
	presentation := `<?xml version="1.0" encoding="UTF-8" standalone="yes"?>
<p:presentation xmlns:p="http://schemas.openxmlformats.org/presentationml/2006/main" xmlns:r="http://schemas.openxmlformats.org/officeDocument/2006/relationships">
<p:sldIdLst><p:sldId id="256" r:id="rId2"/></p:sldIdLst>
</p:presentation>`
	rels := `<?xml version="1.0" encoding="UTF-8" standalone="yes"?>
<Relationships xmlns="http://schemas.openxmlformats.org/package/2006/relationships">
<Relationship Id="rId2" Type="http://schemas.openxmlformats.org/officeDocument/2006/relationships/slide" Target="slides/slide1.xml"/>
</Relationships>`
	slide := `<?xml version="1.0" encoding="UTF-8" standalone="yes"?>
<p:sld xmlns:p="http://schemas.openxmlformats.org/presentationml/2006/main" xmlns:a="http://schemas.openxmlformats.org/drawingml/2006/main">
<p:cSld><p:spTree><p:sp><p:txBody><a:p><a:r><a:t>季度回顾</a:t></a:r></a:p></p:txBody></p:sp></p:spTree></p:cSld>
</p:sld>`
	writeTestZip(t, path, map[string]string{
		"[Content_Types].xml":             `<?xml version="1.0"?><Types xmlns="http://schemas.openxmlformats.org/package/2006/content-types"/>`,
		"ppt/presentation.xml":            presentation,
		"ppt/_rels/presentation.xml.rels": rels,
		"ppt/slides/slide1.xml":           slide,
	})

	deck, err := ReadPPTX(path)
	if err != nil {
		t.Fatalf("读取失败: %v", err)
	}
	if len(deck.Slides) != 1 || strings.Join(deck.Slides[0].Texts, "|") != "季度回顾" {
		t.Fatalf("幻灯片=%v", deck.Slides)
	}
}

// TestReadPDF 用未压缩内容流验证文本提取。
func TestReadPDF(t *testing.T) {
	path := filepath.Join(t.TempDir(), "tiny.pdf")
	body := "%PDF-1.4\n" +
		"1 0 obj<</Length 44>>stream\n" +
		"BT /F1 12 Tf (Hello PDF) Tj T* (Second line) Tj ET\n" +
		"endstream\nendobj\n%%EOF\n"
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatalf("写入 PDF 失败: %v", err)
	}
	pages, err := ReadPDF(path)
	if err != nil {
		t.Fatalf("读取失败: %v", err)
	}
	joined := strings.Join(pages, "\n")
	if !strings.Contains(joined, "Hello PDF") || !strings.Contains(joined, "Second line") {
		t.Fatalf("提取文本=%q", joined)
	}
}

// TestValidatorRejectsBrokenFiles 验证损坏文件会被格式校验拒绝。
func TestValidatorRejectsBrokenFiles(t *testing.T) {
	dir := t.TempDir()
	broken := filepath.Join(dir, "broken.xlsx")
	if err := os.WriteFile(broken, []byte("not a zip"), 0o644); err != nil {
		t.Fatalf("写入失败: %v", err)
	}
	if err := NewValidator().ValidateFormat(broken, "xlsx"); err == nil {
		t.Fatal("损坏的 xlsx 应校验失败")
	}
	shortPDF := filepath.Join(dir, "short.pdf")
	if err := os.WriteFile(shortPDF, []byte("%PDF-1.4\n"), 0o644); err != nil {
		t.Fatalf("写入失败: %v", err)
	}
	if err := NewValidator().ValidateFormat(shortPDF, "pdf"); err == nil {
		t.Fatal("缺少文件结尾标记的 PDF 应校验失败")
	}
}

// writeTestZip 生成测试用 OOXML 包。
func writeTestZip(t *testing.T, path string, parts map[string]string) {
	t.Helper()
	f, err := os.Create(path)
	if err != nil {
		t.Fatalf("创建测试包失败: %v", err)
	}
	zw := zip.NewWriter(f)
	for name, body := range parts {
		w, err := zw.Create(name)
		if err != nil {
			t.Fatalf("写入部件失败: %v", err)
		}
		if _, err := fmt.Fprint(w, body); err != nil {
			t.Fatalf("写入部件内容失败: %v", err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatalf("关闭测试包失败: %v", err)
	}
	if err := f.Close(); err != nil {
		t.Fatalf("关闭文件失败: %v", err)
	}
}
