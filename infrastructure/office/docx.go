package office

import (
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"strings"
)

// Document 从 Office 文档中提取出的纯文本内容。
type Document struct {
	// Paragraphs 按出现顺序排列的段落文本。
	Paragraphs []string
	// Tables 每个表格按行列展开的文本。
	Tables [][][]string
}

// Text 把文档内容渲染为便于阅读的文本。
func (d Document) Text() string {
	var sb strings.Builder
	for _, p := range d.Paragraphs {
		sb.WriteString(p)
		sb.WriteString("\n")
	}
	for i, table := range d.Tables {
		fmt.Fprintf(&sb, "\n[表格 %d]\n", i+1)
		for _, row := range table {
			sb.WriteString(strings.Join(row, "\t"))
			sb.WriteString("\n")
		}
	}
	return strings.TrimRight(sb.String(), "\n")
}

// ReadDOCX 提取 docx 的段落与表格文本。
//
// 只读取 word/document.xml 的文本节点，不处理批注、脚注与页眉页脚。
func ReadDOCX(file string) (Document, error) {
	reader, err := openZip(file)
	if err != nil {
		return Document{}, err
	}
	defer func() { _ = reader.Close() }()

	data, err := readPart(reader, "word/document.xml")
	if err != nil {
		return Document{}, fmt.Errorf("%w: %v", errNoPart, err)
	}
	return parseWordDocument(data)
}

// parseWordDocument 解析 WordprocessingML 的正文。
func parseWordDocument(data []byte) (Document, error) {
	dec := xml.NewDecoder(strings.NewReader(string(data)))
	dec.CharsetReader = func(_ string, input io.Reader) (io.Reader, error) { return input, nil }

	doc := Document{Paragraphs: make([]string, 0, 64), Tables: make([][][]string, 0, 4)}
	// 用栈跟踪当前所在的结构：body > tbl > tr > tc > p
	inTable, inRow, inCell := false, false, false
	var table [][]string
	var row []string
	var cell strings.Builder
	var para strings.Builder

	flushPara := func() {
		text := strings.TrimSpace(para.String())
		para.Reset()
		if inCell {
			if text != "" {
				if cell.Len() > 0 {
					cell.WriteString(" ")
				}
				cell.WriteString(text)
			}
			return
		}
		if text != "" {
			doc.Paragraphs = append(doc.Paragraphs, text)
		}
	}

	for {
		tok, err := dec.Token()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return Document{}, fmt.Errorf("解析 word/document.xml 失败: %w", err)
		}
		switch t := tok.(type) {
		case xml.StartElement:
			switch t.Name.Local {
			case "tbl":
				inTable = true
				table = make([][]string, 0, 8)
			case "tr":
				if inTable {
					inRow = true
					row = make([]string, 0, 8)
				}
			case "tc":
				if inRow {
					inCell = true
					cell.Reset()
				}
			case "t":
				var text string
				if err := dec.DecodeElement(&text, &t); err != nil {
					return Document{}, fmt.Errorf("读取文本节点失败: %w", err)
				}
				para.WriteString(text)
			case "tab":
				para.WriteString("\t")
			case "br", "cr":
				para.WriteString("\n")
			}
		case xml.EndElement:
			switch t.Name.Local {
			case "p":
				flushPara()
			case "tc":
				if inCell {
					row = append(row, strings.TrimSpace(cell.String()))
					cell.Reset()
					inCell = false
				}
			case "tr":
				if inRow {
					table = append(table, row)
					row = nil
					inRow = false
				}
			case "tbl":
				if inTable {
					doc.Tables = append(doc.Tables, table)
					table = nil
					inTable = false
				}
			}
		}
	}
	return doc, nil
}
