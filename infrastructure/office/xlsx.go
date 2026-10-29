package office

import (
	"archive/zip"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"simpleAgent/domain/office"
)

// ReadXLSX 读取 xlsx 工作簿的全部工作表。
//
// 内容按字符串返回：数字保留原始精度并去掉多余的尾零，
// 日期样式按 ISO 日期或日期时间输出，公式取最近一次计算的缓存值。
func ReadXLSX(file string) (office.Workbook, error) {
	reader, err := openZip(file)
	if err != nil {
		return office.Workbook{}, err
	}
	defer func() { _ = reader.Close() }()

	shared, err := readSharedStrings(reader)
	if err != nil {
		return office.Workbook{}, err
	}
	dateStyles, err := readDateStyles(reader)
	if err != nil {
		return office.Workbook{}, err
	}
	sheets, err := readSheetRefs(reader)
	if err != nil {
		return office.Workbook{}, err
	}

	wb := office.Workbook{Sheets: make([]office.Sheet, 0, len(sheets))}
	for _, ref := range sheets {
		data, err := readPart(reader, ref.Path)
		if err != nil {
			return office.Workbook{}, err
		}
		rows, err := parseSheetRows(data, shared, dateStyles)
		if err != nil {
			return office.Workbook{}, fmt.Errorf("解析工作表 %s 失败: %w", ref.Name, err)
		}
		wb.Sheets = append(wb.Sheets, office.Sheet{Name: ref.Name, Rows: rows})
	}
	if len(wb.Sheets) == 0 {
		return office.Workbook{}, fmt.Errorf("%w: 没有工作表", errNoPart)
	}
	return wb, nil
}

// sheetRef 工作表名称与包内路径的对应关系。
type sheetRef struct {
	Name string
	Path string
}

// readSheetRefs 解析 workbook.xml 与关系文件，得到工作表顺序。
func readSheetRefs(reader *zip.ReadCloser) ([]sheetRef, error) {
	data, err := readPart(reader, "xl/workbook.xml")
	if err != nil {
		return nil, err
	}
	rels, err := readRelationships(reader, "xl/_rels/workbook.xml.rels")
	if err != nil {
		return nil, err
	}

	var wb struct {
		Sheets []struct {
			Name string `xml:"name,attr"`
			RID  string `xml:"http://schemas.openxmlformats.org/officeDocument/2006/relationships id,attr"`
		} `xml:"sheets>sheet"`
	}
	if err := xml.Unmarshal(data, &wb); err != nil {
		return nil, fmt.Errorf("解析 xl/workbook.xml 失败: %w", err)
	}
	refs := make([]sheetRef, 0, len(wb.Sheets))
	for i, s := range wb.Sheets {
		name := strings.TrimSpace(s.Name)
		if name == "" {
			name = fmt.Sprintf("Sheet%d", i+1)
		}
		target := rels[s.RID]
		if target == "" {
			target = fmt.Sprintf("worksheets/sheet%d.xml", i+1)
		}
		refs = append(refs, sheetRef{Name: name, Path: resolveTarget("xl", target)})
	}
	return refs, nil
}

// readRelationships 读取关系文件，返回 ID 到 Target 的映射。
func readRelationships(reader *zip.ReadCloser, name string) (map[string]string, error) {
	data, err := readPart(reader, name)
	if err != nil {
		return nil, err
	}
	var rels struct {
		Items []struct {
			ID     string `xml:"Id,attr"`
			Target string `xml:"Target,attr"`
		} `xml:"Relationship"`
	}
	if err := xml.Unmarshal(data, &rels); err != nil {
		return nil, fmt.Errorf("解析 %s 失败: %w", name, err)
	}
	out := make(map[string]string, len(rels.Items))
	for _, item := range rels.Items {
		out[item.ID] = item.Target
	}
	return out, nil
}

// readSharedStrings 读取共享字符串表；部件缺失时返回空表。
func readSharedStrings(reader *zip.ReadCloser) ([]string, error) {
	if !hasPart(reader, "xl/sharedStrings.xml") {
		return nil, nil
	}
	data, err := readPart(reader, "xl/sharedStrings.xml")
	if err != nil {
		return nil, err
	}
	dec := xml.NewDecoder(strings.NewReader(string(data)))
	out := make([]string, 0, 256)
	for {
		tok, err := dec.Token()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("解析 xl/sharedStrings.xml 失败: %w", err)
		}
		start, ok := tok.(xml.StartElement)
		if !ok || start.Name.Local != "si" {
			continue
		}
		text, err := readRichText(dec, start)
		if err != nil {
			return nil, err
		}
		out = append(out, text)
	}
	return out, nil
}

// readRichText 读取一个 si / is 元素内的全部文本（含富文本分片）。
func readRichText(dec *xml.Decoder, start xml.StartElement) (string, error) {
	var sb strings.Builder
	depth := 1
	for depth > 0 {
		tok, err := dec.Token()
		if errors.Is(err, io.EOF) {
			return sb.String(), nil
		}
		if err != nil {
			return "", fmt.Errorf("读取富文本失败: %w", err)
		}
		switch t := tok.(type) {
		case xml.StartElement:
			depth++
			if t.Name.Local == "t" {
				var text string
				if err := dec.DecodeElement(&text, &t); err != nil {
					return "", fmt.Errorf("读取文本节点失败: %w", err)
				}
				depth--
				sb.WriteString(text)
			}
		case xml.EndElement:
			depth--
		}
	}
	return sb.String(), nil
}

// readDateStyles 返回需要按日期渲染的样式索引集合。
func readDateStyles(reader *zip.ReadCloser) (map[int]bool, error) {
	if !hasPart(reader, "xl/styles.xml") {
		return nil, nil
	}
	data, err := readPart(reader, "xl/styles.xml")
	if err != nil {
		return nil, err
	}
	var styles struct {
		NumFmts []struct {
			ID   int    `xml:"numFmtId,attr"`
			Code string `xml:"formatCode,attr"`
		} `xml:"numFmts>numFmt"`
		CellXfs []struct {
			NumFmtID int `xml:"numFmtId,attr"`
		} `xml:"cellXfs>xf"`
	}
	if err := xml.Unmarshal(data, &styles); err != nil {
		return nil, fmt.Errorf("解析 xl/styles.xml 失败: %w", err)
	}
	custom := make(map[int]bool, len(styles.NumFmts))
	for _, f := range styles.NumFmts {
		custom[f.ID] = looksLikeDateFormat(f.Code)
	}
	out := make(map[int]bool)
	for idx, xf := range styles.CellXfs {
		if custom[xf.NumFmtID] || builtinDateFormat(xf.NumFmtID) {
			out[idx] = true
		}
	}
	return out, nil
}

// builtinDateFormat 判断内置数字格式是否为日期时间。
func builtinDateFormat(id int) bool {
	switch {
	case id >= 14 && id <= 22, id >= 45 && id <= 47:
		return true
	}
	return false
}

// looksLikeDateFormat 粗略判断自定义格式串是否表示日期时间。
func looksLikeDateFormat(code string) bool {
	cleaned := strings.ToLower(code)
	for _, token := range []string{"[h]", "[m]", "[s]"} {
		cleaned = strings.ReplaceAll(cleaned, token, "")
	}
	hasDate := false
	for _, ch := range cleaned {
		if ch == 'y' || ch == 'd' || ch == 'h' || ch == 's' {
			hasDate = true
			break
		}
	}
	if !hasDate {
		hasDate = strings.Contains(cleaned, "m")
	}
	if !hasDate {
		return false
	}
	// 纯数字格式（0.00、#,##0）不含字母，不会走到这里
	return true
}

// parseSheetRows 解析工作表 XML，返回按行列展开的字符串矩阵。
func parseSheetRows(data []byte, shared []string, dateStyles map[int]bool) ([][]string, error) {
	dec := xml.NewDecoder(strings.NewReader(string(data)))
	dec.CharsetReader = func(_ string, input io.Reader) (io.Reader, error) { return input, nil }

	rows := make([][]string, 0, 64)
	var current []string
	rowIndex := 0
	for {
		tok, err := dec.Token()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, err
		}
		switch t := tok.(type) {
		case xml.StartElement:
			switch t.Name.Local {
			case "row":
				rowIndex = 0
				current = []string{}
			case "c":
				value, err := parseCell(dec, t, shared, dateStyles)
				if err != nil {
					return nil, err
				}
				col := columnIndex(attr(t, "r"))
				if col < 0 {
					col = rowIndex
				}
				for len(current) <= col {
					current = append(current, "")
				}
				current[col] = value
				rowIndex = col + 1
			}
		case xml.EndElement:
			if t.Name.Local == "row" {
				rows = append(rows, current)
				current = nil
			}
		}
	}
	if len(current) > 0 {
		rows = append(rows, current)
	}
	return rows, nil
}

// parseCell 解析单个单元格，返回其字符串表示。
func parseCell(dec *xml.Decoder, start xml.StartElement, shared []string, dateStyles map[int]bool) (string, error) {
	cellType := attr(start, "t")
	styleIdx, _ := strconv.Atoi(attr(start, "s"))
	var raw string
	inlineText := ""

	depth := 1
	for depth > 0 {
		tok, err := dec.Token()
		if errors.Is(err, io.EOF) {
			return "", nil
		}
		if err != nil {
			return "", err
		}
		switch t := tok.(type) {
		case xml.StartElement:
			depth++
			switch t.Name.Local {
			case "v":
				var text string
				if err := dec.DecodeElement(&text, &t); err != nil {
					return "", err
				}
				depth--
				raw = text
			case "t":
				var text string
				if err := dec.DecodeElement(&text, &t); err != nil {
					return "", err
				}
				depth--
				inlineText += text
			}
		case xml.EndElement:
			depth--
		}
	}

	switch cellType {
	case "s":
		idx, err := strconv.Atoi(strings.TrimSpace(raw))
		if err != nil || idx < 0 || idx >= len(shared) {
			return raw, nil
		}
		return shared[idx], nil
	case "inlineStr", "str":
		if inlineText != "" {
			return inlineText, nil
		}
		return raw, nil
	case "b":
		if strings.TrimSpace(raw) == "1" {
			return "TRUE", nil
		}
		return "FALSE", nil
	case "e":
		return raw, nil
	}

	if strings.TrimSpace(raw) == "" {
		return "", nil
	}
	value, err := strconv.ParseFloat(strings.TrimSpace(raw), 64)
	if err != nil {
		return raw, nil
	}
	if dateStyles[styleIdx] {
		return formatExcelDate(value), nil
	}
	return formatNumber(value), nil
}

// formatNumber 输出数字文本：整数不带小数点，小数去掉多余的尾零。
func formatNumber(v float64) string {
	if v == math.Trunc(v) && math.Abs(v) < 1e15 {
		return strconv.FormatInt(int64(v), 10)
	}
	return strconv.FormatFloat(v, 'f', -1, 64)
}

// excelEpoch Excel 序列号起点。
//
// 序列号 1 对应 1900-01-01；Excel 为兼容 Lotus 保留了并不存在的
// 1900-02-29（序列号 60），因此 60 及之后需要回退一天。
var excelEpoch = time.Date(1899, 12, 30, 0, 0, 0, 0, time.UTC)

// formatExcelDate 把 Excel 序列号渲染为 ISO 日期或日期时间。
func formatExcelDate(serial float64) string {
	if serial <= 0 {
		return formatNumber(serial)
	}
	days := math.Floor(serial)
	frac := serial - days
	base := excelEpoch
	if days < 60 {
		// 1900 年 2 月 29 日之前没有闰年补偿
		base = excelEpoch.AddDate(0, 0, 1)
	}
	if frac <= 1e-9 {
		return base.AddDate(0, 0, int(days)).Format("2006-01-02")
	}
	seconds := int(math.Round(frac * 86400))
	t := base.AddDate(0, 0, int(days)).Add(time.Duration(seconds) * time.Second)
	if t.Hour() == 0 && t.Minute() == 0 && t.Second() == 0 {
		return t.Format("2006-01-02")
	}
	return t.Format("2006-01-02 15:04:05")
}

// attr 读取 XML 属性值。
func attr(start xml.StartElement, name string) string {
	for _, a := range start.Attr {
		if a.Name.Local == name {
			return a.Value
		}
	}
	return ""
}

// columnIndex 把单元格引用（如 C7）转为 0 基列号；无法解析时返回 -1。
func columnIndex(ref string) int {
	ref = strings.TrimSpace(ref)
	col := 0
	found := false
	for _, ch := range ref {
		upper := ch
		if ch >= 'a' && ch <= 'z' {
			upper = ch - 32
		}
		if upper < 'A' || upper > 'Z' {
			break
		}
		col = col*26 + int(upper-'A'+1)
		found = true
	}
	if !found {
		return -1
	}
	return col - 1
}

// WriteXLSX 把工作簿写为 xlsx 文件。
//
// 写入前先做领域校验；输出使用内联字符串与最小样式表，
// 保证 openpyxl 等标准读取器可以直接打开。
func WriteXLSX(file string, wb office.Workbook) error {
	normalized, err := office.NormalizeWorkbook(wb)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(file), 0o750); err != nil {
		return fmt.Errorf("创建输出目录失败: %w", err)
	}
	tmp, err := os.CreateTemp(filepath.Dir(file), ".xlsx-*.tmp")
	if err != nil {
		return fmt.Errorf("创建临时文件失败: %w", err)
	}
	tmpName := tmp.Name()
	defer func() { _ = os.Remove(tmpName) }()

	zw := newZipWriter(tmp)
	if err := writePackage(zw, normalized); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := zw.Close(); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("写入 xlsx 失败: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("同步文件失败: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("关闭文件失败: %w", err)
	}
	if err := os.Rename(tmpName, file); err != nil {
		return fmt.Errorf("替换目标文件失败: %w", err)
	}
	return nil
}

// zipEntry 待写入的 zip 部件。
type zipEntry struct {
	Name string
	Body string
}

// writePackage 组装 xlsx 的全部部件。
func writePackage(zw *zipWriter, wb office.Workbook) error {
	entries := make([]zipEntry, 0, len(wb.Sheets)+5)
	entries = append(entries, zipEntry{Name: "_rels/.rels", Body: rootRelsXML})
	entries = append(entries, zipEntry{Name: "[Content_Types].xml", Body: contentTypesXML(len(wb.Sheets))})
	entries = append(entries, zipEntry{Name: "xl/workbook.xml", Body: workbookXML(wb)})
	entries = append(entries, zipEntry{Name: "xl/_rels/workbook.xml.rels", Body: workbookRelsXML(len(wb.Sheets))})
	entries = append(entries, zipEntry{Name: "xl/styles.xml", Body: stylesXML})
	entries = append(entries, zipEntry{Name: "docProps/app.xml", Body: appPropsXML})
	entries = append(entries, zipEntry{Name: "docProps/core.xml", Body: corePropsXML()})
	for i, sheet := range wb.Sheets {
		entries = append(entries, zipEntry{
			Name: fmt.Sprintf("xl/worksheets/sheet%d.xml", i+1),
			Body: sheetXML(sheet),
		})
	}
	for _, e := range entries {
		if err := zw.WriteString(e.Name, e.Body); err != nil {
			return fmt.Errorf("写入部件 %s 失败: %w", e.Name, err)
		}
	}
	return nil
}

// contentTypesXML 生成 [Content_Types].xml。
func contentTypesXML(sheetCount int) string {
	var sb strings.Builder
	sb.WriteString(`<?xml version="1.0" encoding="UTF-8" standalone="yes"?>`)
	sb.WriteString(`<Types xmlns="http://schemas.openxmlformats.org/package/2006/content-types">`)
	sb.WriteString(`<Default Extension="rels" ContentType="application/vnd.openxmlformats-package.relationships+xml"/>`)
	sb.WriteString(`<Default Extension="xml" ContentType="application/xml"/>`)
	sb.WriteString(`<Override PartName="/xl/workbook.xml" ContentType="application/vnd.openxmlformats-officedocument.spreadsheetml.sheet.main+xml"/>`)
	sb.WriteString(`<Override PartName="/xl/styles.xml" ContentType="application/vnd.openxmlformats-officedocument.spreadsheetml.styles+xml"/>`)
	sb.WriteString(`<Override PartName="/docProps/app.xml" ContentType="application/vnd.openxmlformats-officedocument.extended-properties+xml"/>`)
	sb.WriteString(`<Override PartName="/docProps/core.xml" ContentType="application/vnd.openxmlformats-package.core-properties+xml"/>`)
	for i := 0; i < sheetCount; i++ {
		fmt.Fprintf(&sb, `<Override PartName="/xl/worksheets/sheet%d.xml" ContentType="application/vnd.openxmlformats-officedocument.spreadsheetml.worksheet+xml"/>`, i+1)
	}
	sb.WriteString(`</Types>`)
	return sb.String()
}

// workbookXML 生成 xl/workbook.xml。
func workbookXML(wb office.Workbook) string {
	var sb strings.Builder
	sb.WriteString(`<?xml version="1.0" encoding="UTF-8" standalone="yes"?>`)
	sb.WriteString(`<workbook xmlns="http://schemas.openxmlformats.org/spreadsheetml/2006/main" xmlns:r="http://schemas.openxmlformats.org/officeDocument/2006/relationships"><sheets>`)
	for i, sheet := range wb.Sheets {
		fmt.Fprintf(&sb, `<sheet name="%s" sheetId="%d" r:id="rId%d"/>`, escapeXML(sheet.Name), i+1, i+1)
	}
	sb.WriteString(`</sheets></workbook>`)
	return sb.String()
}

// workbookRelsXML 生成 xl/_rels/workbook.xml.rels。
func workbookRelsXML(sheetCount int) string {
	var sb strings.Builder
	sb.WriteString(`<?xml version="1.0" encoding="UTF-8" standalone="yes"?>`)
	sb.WriteString(`<Relationships xmlns="http://schemas.openxmlformats.org/package/2006/relationships">`)
	for i := 0; i < sheetCount; i++ {
		fmt.Fprintf(&sb, `<Relationship Id="rId%d" Type="http://schemas.openxmlformats.org/officeDocument/2006/relationships/worksheet" Target="worksheets/sheet%d.xml"/>`, i+1, i+1)
	}
	next := sheetCount + 1
	fmt.Fprintf(&sb, `<Relationship Id="rId%d" Type="http://schemas.openxmlformats.org/officeDocument/2006/relationships/styles" Target="styles.xml"/>`, next)
	sb.WriteString(`</Relationships>`)
	return sb.String()
}

// sheetXML 生成单个工作表 XML。数字单元格不写 t 属性，文本用内联字符串。
func sheetXML(sheet office.Sheet) string {
	var sb strings.Builder
	sb.WriteString(`<?xml version="1.0" encoding="UTF-8" standalone="yes"?>`)
	sb.WriteString(`<worksheet xmlns="http://schemas.openxmlformats.org/spreadsheetml/2006/main"><sheetData>`)
	for r, row := range sheet.Rows {
		fmt.Fprintf(&sb, `<row r="%d">`, r+1)
		for c, cell := range row {
			if cell == "" {
				continue
			}
			ref := columnName(c) + strconv.Itoa(r+1)
			if isNumericCell(cell) {
				fmt.Fprintf(&sb, `<c r="%s"><v>%s</v></c>`, ref, cell)
				continue
			}
			fmt.Fprintf(&sb, `<c r="%s" t="inlineStr"><is><t xml:space="preserve">%s</t></is></c>`, ref, escapeXML(cell))
		}
		sb.WriteString(`</row>`)
	}
	sb.WriteString(`</sheetData></worksheet>`)
	return sb.String()
}

// isNumericCell 判断单元格是否应写为数字。
//
// 需要保留前导零、带分隔符或百分号的文本不会被当作数字，
// 否则会被 Excel 解析成语义不同的数值。
func isNumericCell(cell string) bool {
	trimmed := strings.TrimSpace(cell)
	if trimmed != cell || trimmed == "" {
		return false
	}
	if strings.HasPrefix(trimmed, "0") && len(trimmed) > 1 && trimmed[1] != '.' {
		return false
	}
	if strings.ContainsAny(trimmed, ",%$ ") || strings.HasPrefix(trimmed, "+") {
		return false
	}
	if _, err := strconv.ParseFloat(trimmed, 64); err != nil {
		return false
	}
	return true
}

// columnName 把 0 基列号转为 Excel 列名。
func columnName(index int) string {
	name := ""
	for index >= 0 {
		name = string(rune('A'+index%26)) + name
		index = index/26 - 1
	}
	return name
}

// escapeXML 转义 XML 文本内容。
func escapeXML(s string) string {
	var sb strings.Builder
	if err := xml.EscapeText(&sb, []byte(s)); err != nil {
		return s
	}
	return sb.String()
}

// rootRelsXML 固定的包级关系文件。
const rootRelsXML = `<?xml version="1.0" encoding="UTF-8" standalone="yes"?>` +
	`<Relationships xmlns="http://schemas.openxmlformats.org/package/2006/relationships">` +
	`<Relationship Id="rId1" Type="http://schemas.openxmlformats.org/officeDocument/2006/relationships/officeDocument" Target="xl/workbook.xml"/>` +
	`<Relationship Id="rId2" Type="http://schemas.openxmlformats.org/package/2006/relationships/metadata/core-properties" Target="docProps/core.xml"/>` +
	`<Relationship Id="rId3" Type="http://schemas.openxmlformats.org/officeDocument/2006/relationships/extended-properties" Target="docProps/app.xml"/>` +
	`</Relationships>`

// stylesXML 最小样式表：只有一个常规格式，满足读取器对 cellXfs 的要求。
const stylesXML = `<?xml version="1.0" encoding="UTF-8" standalone="yes"?>` +
	`<styleSheet xmlns="http://schemas.openxmlformats.org/spreadsheetml/2006/main">` +
	`<fonts count="1"><font><sz val="11"/><name val="Calibri"/></font></fonts>` +
	`<fills count="1"><fill><patternFill patternType="none"/></fill></fills>` +
	`<borders count="1"><border/></borders>` +
	`<cellStyleXfs count="1"><xf numFmtId="0" fontId="0" fillId="0" borderId="0"/></cellStyleXfs>` +
	`<cellXfs count="1"><xf numFmtId="0" fontId="0" fillId="0" borderId="0" xfId="0"/></cellXfs>` +
	`<cellStyles count="1"><cellStyle name="Normal" xfId="0" builtinId="0"/></cellStyles>` +
	`</styleSheet>`

// appPropsXML 固定的应用属性，部分读取器会检查该部件。
const appPropsXML = `<?xml version="1.0" encoding="UTF-8" standalone="yes"?>` +
	`<Properties xmlns="http://schemas.openxmlformats.org/officeDocument/2006/extended-properties" xmlns:vt="http://schemas.openxmlformats.org/officeDocument/2006/docPropsVTypes">` +
	`<Application>SimpleAgent</Application></Properties>`

// corePropsXML 生成核心属性，带生成时间便于追溯。
func corePropsXML() string {
	return `<?xml version="1.0" encoding="UTF-8" standalone="yes"?>` +
		`<cp:coreProperties xmlns:cp="http://schemas.openxmlformats.org/package/2006/metadata/core-properties" xmlns:dc="http://purl.org/dc/elements/1.1/" xmlns:dcterms="http://purl.org/dc/terms/" xmlns:xsi="http://www.w3.org/2001/XMLSchema-instance">` +
		`<dc:creator>SimpleAgent</dc:creator>` +
		`<dcterms:created xsi:type="dcterms:W3CDTF">` + time.Now().UTC().Format(time.RFC3339) + `</dcterms:created>` +
		`</cp:coreProperties>`
}

// zipWriter 极简 zip 写入器，只支持字符串内容与 store/deflate 压缩。
type zipWriter struct {
	w *zip.Writer
}

// newZipWriter 创建 zip 写入器。
func newZipWriter(w io.Writer) *zipWriter {
	return &zipWriter{w: zip.NewWriter(w)}
}

// WriteString 写入一个文本部件。
func (z *zipWriter) WriteString(name, body string) error {
	header := &zip.FileHeader{Name: name, Method: zip.Deflate}
	part, err := z.w.CreateHeader(header)
	if err != nil {
		return err
	}
	_, err = part.Write([]byte(body))
	return err
}

// Close 收尾 zip 中央目录。
func (z *zipWriter) Close() error {
	return z.w.Close()
}
