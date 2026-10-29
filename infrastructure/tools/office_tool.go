package tools

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"simpleAgent/domain/office"
	"simpleAgent/domain/session"
	"simpleAgent/domain/tool"
	"simpleAgent/infrastructure/audit"
	officecodec "simpleAgent/infrastructure/office"
)

// Office 读取工具的行/列展示上限。
//
// 返回内容会原样进入对话历史，超过上限时明确标注截断范围，
// 避免一次工具调用就把上下文塞满。
const (
	// officeReadMaxRows 单次读取展示的最大行数。
	officeReadMaxRows = 200
	// officeReadMaxCols 单次读取展示的最大列数。
	officeReadMaxCols = 40
	// officeReadMaxRunes 单次读取返回文本的最大字符数。
	officeReadMaxRunes = 60000
)

// ============ read_xlsx ============

// ReadXLSXTool 读取工作区内的 xlsx 工作簿。只读无副作用，注册时自动放行。
type ReadXLSXTool struct {
	workspace *Workspace
}

// NewReadXLSXTool 创建读取 xlsx 工具
func NewReadXLSXTool(workspace *Workspace) *ReadXLSXTool {
	return &ReadXLSXTool{workspace: workspace}
}

// Name 返回工具名
func (t *ReadXLSXTool) Name() string { return "read_xlsx" }

// Description 返回工具说明
func (t *ReadXLSXTool) Description() string {
	return "读取工作区内的 Excel 工作簿（.xlsx/.xlsm），返回各工作表的文本表格。" +
		"只传 path 读取全部工作表；可用 sheet 指定单个工作表，用 start_row/end_row 指定行区间。"
}

// Spec 返回 read_xlsx 的参数规格
func (t *ReadXLSXTool) Spec() tool.ToolSpec {
	return tool.ToolSpec{
		Name: "read_xlsx",
		Params: []tool.ParamSpec{
			{Name: "path", Type: tool.TypeString, Required: true, Description: "工作区内的表格相对路径，如 input/台账.xlsx"},
			{Name: "sheet", Type: tool.TypeString, Description: "工作表名；省略则读取全部工作表"},
			{Name: "start_row", Type: tool.TypeInteger, Min: float64Ptr(1), Description: "起始行（含，从 1 开始）；必须与 end_row 成对出现"},
			{Name: "end_row", Type: tool.TypeInteger, Min: float64Ptr(1), Description: "结束行（含）；必须与 start_row 成对出现"},
		},
	}
}

// Run 执行读取，参数为 JSON 对象：{"path":"input/台账.xlsx","sheet":"明细","start_row":1,"end_row":50}
func (t *ReadXLSXTool) Run(args string) (string, error) {
	if t.workspace == nil {
		return "", errors.New("工作区未初始化")
	}
	path, sheetName, from, to, err := parseXLSXArgs(args)
	if err != nil {
		return "", err
	}
	target, err := t.workspace.ResolveRead(path)
	if err != nil {
		return "", err
	}
	if info, err := os.Stat(target); err != nil {
		return "", fmt.Errorf("文件 %s 不存在", path)
	} else if info.IsDir() {
		return "", fmt.Errorf("%s 是目录，请改用 list_dir", path)
	}

	wb, err := officecodec.ReadXLSX(target)
	if err != nil {
		return "", fmt.Errorf("读取工作簿 %s 失败: %w", path, err)
	}

	var sb strings.Builder
	fmt.Fprintf(&sb, "%s：共 %d 个工作表\n", path, len(wb.Sheets))
	matched := false
	for _, sheet := range wb.Sheets {
		if sheetName != "" && !strings.EqualFold(sheetName, sheet.Name) {
			continue
		}
		matched = true
		renderSheet(&sb, sheet, from, to)
	}
	if !matched {
		names := make([]string, 0, len(wb.Sheets))
		for _, sheet := range wb.Sheets {
			names = append(names, sheet.Name)
		}
		return "", fmt.Errorf("工作簿 %s 中没有工作表 %q，可用工作表: %s", path, sheetName, strings.Join(names, "、"))
	}
	return truncateRunes(sb.String(), officeReadMaxRunes), nil
}

// renderSheet 渲染单个工作表为制表符分隔的文本。
func renderSheet(sb *strings.Builder, sheet office.Sheet, from, to int) {
	fmt.Fprintf(sb, "\n【工作表：%s，%d 行】\n", sheet.Name, len(sheet.Rows))
	rows := sheet.Rows
	start, end := 1, len(rows)
	if from > 0 {
		start = from
		if start > len(rows) {
			fmt.Fprintf(sb, "（起始行 %d 超出范围）\n", from)
			return
		}
		end = min(to, len(rows))
	} else if end > officeReadMaxRows {
		end = officeReadMaxRows
	}
	for i := start - 1; i < end; i++ {
		row := rows[i]
		if len(row) > officeReadMaxCols {
			row = row[:officeReadMaxCols]
		}
		fmt.Fprintf(sb, "%d\t%s\n", i+1, strings.Join(row, "\t"))
	}
	if end < len(rows) {
		fmt.Fprintf(sb, "… 已截断，仅显示第 %d-%d 行（共 %d 行）\n", start, end, len(rows))
	}
}

// readXLSXArgs read_xlsx 的 JSON 参数
type readXLSXArgs struct {
	Path     string `json:"path"`
	Sheet    string `json:"sheet"`
	StartRow int    `json:"start_row"`
	EndRow   int    `json:"end_row"`
}

// parseXLSXArgs 解析 read_xlsx 的 JSON 参数，未指定行区间时 from/to 为 0
func parseXLSXArgs(args string) (path, sheet string, from, to int, err error) {
	var parsed readXLSXArgs
	if err := decodeToolArgs(args, &parsed); err != nil {
		return "", "", 0, 0, err
	}
	path = strings.TrimSpace(parsed.Path)
	if path == "" {
		return "", "", 0, 0, errors.New(`参数 path 不能为空，例如：{"path":"input/台账.xlsx"}`)
	}
	sheet = strings.TrimSpace(parsed.Sheet)
	from, to = parsed.StartRow, parsed.EndRow
	if (from == 0) != (to == 0) {
		return "", "", 0, 0, errors.New("start_row 与 end_row 必须同时提供，或同时省略以读取全部行")
	}
	if from < 0 || to < 0 {
		return "", "", 0, 0, fmt.Errorf("行号不能为负数: %d,%d", from, to)
	}
	if from > 0 && to < from {
		return "", "", 0, 0, fmt.Errorf("行范围不合法: %d,%d（start_row 不得大于 end_row）", from, to)
	}
	return path, sheet, from, to, nil
}

// parseOfficeReadArgs 解析 read_docx / read_pptx / read_pdf 共用的 JSON 参数。
func parseOfficeReadArgs(args string) (string, error) {
	var parsed officeReadArgs
	if err := decodeToolArgs(args, &parsed); err != nil {
		return "", err
	}
	path := strings.TrimSpace(parsed.Path)
	if path == "" {
		return "", errors.New(`参数 path 不能为空，例如：{"path":"input/合同.pdf"}`)
	}
	return path, nil
}

// officeReadArgs 只接受单个 path 参数的 Office 读取工具共用参数
type officeReadArgs struct {
	Path string `json:"path"`
}

// ============ read_docx ============

// ReadDOCXTool 读取工作区内的 docx 文档。只读无副作用，注册时自动放行。
type ReadDOCXTool struct {
	workspace *Workspace
}

// NewReadDOCXTool 创建读取 docx 工具
func NewReadDOCXTool(workspace *Workspace) *ReadDOCXTool {
	return &ReadDOCXTool{workspace: workspace}
}

// Name 返回工具名
func (t *ReadDOCXTool) Name() string { return "read_docx" }

// Description 返回工具说明
func (t *ReadDOCXTool) Description() string {
	return "读取工作区内的 Word 文档（.docx）文本，含段落与表格。"
}

// Spec 返回 read_docx 的参数规格
func (t *ReadDOCXTool) Spec() tool.ToolSpec {
	return tool.ToolSpec{
		Name: "read_docx",
		Params: []tool.ParamSpec{
			{Name: "path", Type: tool.TypeString, Required: true, Description: "工作区内的文档相对路径，如 input/手册.docx"},
		},
	}
}

// Run 执行读取，参数为 JSON 对象：{"path":"input/手册.docx"}
func (t *ReadDOCXTool) Run(args string) (string, error) {
	if t.workspace == nil {
		return "", errors.New("工作区未初始化")
	}
	path, err := parseOfficeReadArgs(args)
	if err != nil {
		return "", err
	}
	target, err := t.workspace.ResolveRead(path)
	if err != nil {
		return "", err
	}
	if info, err := os.Stat(target); err != nil {
		return "", fmt.Errorf("文件 %s 不存在", path)
	} else if info.IsDir() {
		return "", fmt.Errorf("%s 是目录，请改用 list_dir", path)
	}
	doc, err := officecodec.ReadDOCX(target)
	if err != nil {
		return "", fmt.Errorf("读取文档 %s 失败: %w", path, err)
	}
	text := doc.Text()
	if strings.TrimSpace(text) == "" {
		return fmt.Sprintf("%s 未提取到文本内容（可能是纯图片文档）", path), nil
	}
	return truncateRunes(text, officeReadMaxRunes), nil
}

// ============ read_pptx ============

// ReadPPTXTool 读取工作区内的 pptx 演示文稿。只读无副作用，注册时自动放行。
type ReadPPTXTool struct {
	workspace *Workspace
}

// NewReadPPTXTool 创建读取 pptx 工具
func NewReadPPTXTool(workspace *Workspace) *ReadPPTXTool {
	return &ReadPPTXTool{workspace: workspace}
}

// Name 返回工具名
func (t *ReadPPTXTool) Name() string { return "read_pptx" }

// Description 返回工具说明
func (t *ReadPPTXTool) Description() string {
	return "按页读取工作区内的 PowerPoint 演示文稿（.pptx）文本，含备注页。"
}

// Spec 返回 read_pptx 的参数规格
func (t *ReadPPTXTool) Spec() tool.ToolSpec {
	return tool.ToolSpec{
		Name: "read_pptx",
		Params: []tool.ParamSpec{
			{Name: "path", Type: tool.TypeString, Required: true, Description: "工作区内的演示文稿相对路径，如 input/模板.pptx"},
		},
	}
}

// Run 执行读取，参数为 JSON 对象：{"path":"input/模板.pptx"}
func (t *ReadPPTXTool) Run(args string) (string, error) {
	if t.workspace == nil {
		return "", errors.New("工作区未初始化")
	}
	path, err := parseOfficeReadArgs(args)
	if err != nil {
		return "", err
	}
	target, err := t.workspace.ResolveRead(path)
	if err != nil {
		return "", err
	}
	if info, err := os.Stat(target); err != nil {
		return "", fmt.Errorf("文件 %s 不存在", path)
	} else if info.IsDir() {
		return "", fmt.Errorf("%s 是目录，请改用 list_dir", path)
	}
	deck, err := officecodec.ReadPPTX(target)
	if err != nil {
		return "", fmt.Errorf("读取演示文稿 %s 失败: %w", path, err)
	}
	return truncateRunes(deck.Text(), officeReadMaxRunes), nil
}

// ============ read_pdf ============

// ReadPDFTool 读取工作区内的 PDF 文本层。只读无副作用，注册时自动放行。
type ReadPDFTool struct {
	workspace *Workspace
}

// NewReadPDFTool 创建读取 PDF 工具
func NewReadPDFTool(workspace *Workspace) *ReadPDFTool {
	return &ReadPDFTool{workspace: workspace}
}

// Name 返回工具名
func (t *ReadPDFTool) Name() string { return "read_pdf" }

// Description 返回工具说明
func (t *ReadPDFTool) Description() string {
	return "读取工作区内 PDF 的文本层内容，按页输出。仅支持含文本层的 PDF（扫描件无文本）。"
}

// Spec 返回 read_pdf 的参数规格
func (t *ReadPDFTool) Spec() tool.ToolSpec {
	return tool.ToolSpec{
		Name: "read_pdf",
		Params: []tool.ParamSpec{
			{Name: "path", Type: tool.TypeString, Required: true, Description: "工作区内的 PDF 相对路径，如 input/合同.pdf"},
		},
	}
}

// Run 执行读取，参数为 JSON 对象：{"path":"input/合同.pdf"}
func (t *ReadPDFTool) Run(args string) (string, error) {
	if t.workspace == nil {
		return "", errors.New("工作区未初始化")
	}
	path, err := parseOfficeReadArgs(args)
	if err != nil {
		return "", err
	}
	target, err := t.workspace.ResolveRead(path)
	if err != nil {
		return "", err
	}
	if info, err := os.Stat(target); err != nil {
		return "", fmt.Errorf("文件 %s 不存在", path)
	} else if info.IsDir() {
		return "", fmt.Errorf("%s 是目录，请改用 list_dir", path)
	}
	pages, err := officecodec.ReadPDF(target)
	if err != nil {
		return "", fmt.Errorf("读取 PDF %s 失败: %w", path, err)
	}
	var sb strings.Builder
	for i, page := range pages {
		fmt.Fprintf(&sb, "【第 %d 页】\n%s\n", i+1, page)
	}
	return truncateRunes(sb.String(), officeReadMaxRunes), nil
}

// ============ write_xlsx ============

// writeXLSXArgs write_xlsx 的 JSON 参数。
type writeXLSXArgs struct {
	Path   string           `json:"path"`
	Sheets []writeXLSXSheet `json:"sheets"`
}

// writeXLSXSheet 单个工作表参数。
//
// Rows 的单元格允许字符串、数字、布尔与 null：Agent 很容易把分数写成
// 数字而不是字符串，这里统一转换为文本后再交给编解码层判断类型。
type writeXLSXSheet struct {
	Name string  `json:"name"`
	Rows [][]any `json:"rows"`
}

// WriteXLSXTool 把工作簿写入工作区内的 xlsx 文件。属于写本地操作，注册时需人工确认。
type WriteXLSXTool struct {
	workspace *Workspace
	audit     *audit.Logger
}

// NewWriteXLSXTool 创建写 xlsx 工具
func NewWriteXLSXTool(workspace *Workspace) *WriteXLSXTool {
	return &WriteXLSXTool{workspace: workspace}
}

// SetAudit 注入审计日志；nil 表示不记录。
func (t *WriteXLSXTool) SetAudit(log *audit.Logger) { t.audit = log }

// Name 返回工具名
func (t *WriteXLSXTool) Name() string { return "write_xlsx" }

// Description 返回工具说明
func (t *WriteXLSXTool) Description() string {
	return "把表格数据写入工作区内的 Excel 文件（.xlsx），父目录不存在时自动创建，同名文件会被覆盖。" +
		`rows 是二维数组，每个元素是一行，行内元素是单元格值（字符串/数字/布尔）。` +
		"数字单元格请直接写数字文本（不要加千分位或百分号）。"
}

// Spec 返回 write_xlsx 的参数规格
func (t *WriteXLSXTool) Spec() tool.ToolSpec {
	return tool.ToolSpec{
		Name: "write_xlsx",
		Params: []tool.ParamSpec{
			{Name: "path", Type: tool.TypeString, Required: true, Description: "要写入的表格相对路径，如 output/result.xlsx"},
			{
				Name: "sheets", Type: tool.TypeArray, Required: true,
				Description: "工作表数组，至少一个",
				Items: &tool.ParamSpec{
					Type: tool.TypeObject,
					Properties: []tool.ParamSpec{
						{Name: "name", Type: tool.TypeString, Required: true, Description: "工作表名，如 Sheet1"},
						{Name: "rows", Type: tool.TypeArray, Required: true, Description: "二维数组：每个元素是一行，行内元素是单元格值"},
					},
				},
			},
		},
	}
}

// Run 执行写入，参数为 JSON 对象：
// {"path":"output/result.xlsx","sheets":[{"name":"Sheet1","rows":[["股票代码","得分"],["600000","88.5"]]}]}
func (t *WriteXLSXTool) Run(args string) (string, error) {
	if t.workspace == nil {
		return "", errors.New("工作区未初始化")
	}
	parsed, wb, err := parseWriteXLSXArgs(args)
	if err != nil {
		return "", err
	}
	target, err := t.workspace.ResolveWrite(parsed.Path)
	if err != nil {
		t.audit.Log(audit.Event{Type: "file_write", Action: "deny", Detail: parsed.Path})
		return "", err
	}
	if err := os.MkdirAll(filepath.Dir(target), 0o750); err != nil {
		return "", fmt.Errorf("创建输出目录失败: %w", err)
	}
	if err := officecodec.WriteXLSX(target, wb); err != nil {
		t.audit.Log(audit.Event{Type: "file_write", Action: "error", Detail: parsed.Path})
		return "", fmt.Errorf("写入工作簿 %s 失败: %w", parsed.Path, err)
	}
	t.audit.Log(audit.Event{Type: "file_write", Action: "allow", Detail: parsed.Path})

	info, statErr := os.Stat(target)
	size := int64(0)
	if statErr == nil {
		size = info.Size()
	}
	var names []string
	rows := 0
	for _, sheet := range wb.Sheets {
		names = append(names, sheet.Name)
		rows += len(sheet.Rows)
	}
	return fmt.Sprintf("已写入 %s：%d 个工作表（%s），共 %d 行，%s",
		parsed.Path, len(wb.Sheets), strings.Join(names, "、"), rows, formatSize(size)), nil
}

// Describe 让人工确认时看到目标路径与数据规模。
func (t *WriteXLSXTool) Describe(args string) string {
	parsed, wb, err := parseWriteXLSXArgs(args)
	if err != nil {
		return ""
	}
	target, resolveErr := t.workspace.ResolveWrite(parsed.Path)
	if resolveErr != nil {
		if errors.Is(resolveErr, ErrPathDenied) {
			return fmt.Sprintf("试图写入受保护的敏感路径 %s，已被拒绝", parsed.Path)
		}
		return fmt.Sprintf("试图写入工作区之外的路径 %s，已被拒绝", parsed.Path)
	}
	var names []string
	rows := 0
	preview := ""
	for _, sheet := range wb.Sheets {
		names = append(names, fmt.Sprintf("%s(%d 行)", sheet.Name, len(sheet.Rows)))
		rows += len(sheet.Rows)
		if preview == "" && len(sheet.Rows) > 0 {
			preview = session.Preview(strings.Join(sheet.Rows[0], "\t"), searchSnippetRunes)
		}
	}
	action := "新建"
	if _, err := os.Stat(target); err == nil {
		action = "覆盖"
	}
	return fmt.Sprintf("%s Excel 文件 %s：%d 个工作表 %s，共 %d 行；首行预览：%s",
		action, parsed.Path, len(wb.Sheets), strings.Join(names, "、"), rows, preview)
}

// parseWriteXLSXArgs 解析并校验 write_xlsx 的 JSON 参数。
func parseWriteXLSXArgs(args string) (writeXLSXArgs, office.Workbook, error) {
	dec := json.NewDecoder(strings.NewReader(strings.TrimSpace(args)))
	dec.DisallowUnknownFields()
	dec.UseNumber()
	var parsed writeXLSXArgs
	if err := dec.Decode(&parsed); err != nil {
		return parsed, office.Workbook{}, fmt.Errorf("参数必须是 JSON 对象，且字段仅限 path、sheets: %w", err)
	}
	if strings.TrimSpace(parsed.Path) == "" {
		return parsed, office.Workbook{}, errors.New("JSON 参数缺少 path")
	}
	if len(parsed.Sheets) == 0 {
		return parsed, office.Workbook{}, errors.New("JSON 参数缺少 sheets，至少需要一个工作表")
	}
	wb := office.Workbook{Sheets: make([]office.Sheet, 0, len(parsed.Sheets))}
	for _, sheet := range parsed.Sheets {
		rows := make([][]string, 0, len(sheet.Rows))
		for rowIdx, rawRow := range sheet.Rows {
			row := make([]string, 0, len(rawRow))
			for colIdx, cell := range rawRow {
				text, err := cellText(cell)
				if err != nil {
					return parsed, office.Workbook{}, fmt.Errorf("工作表 %s 第 %d 行第 %d 列: %w", sheet.Name, rowIdx+1, colIdx+1, err)
				}
				row = append(row, text)
			}
			rows = append(rows, row)
		}
		wb.Sheets = append(wb.Sheets, office.Sheet{Name: sheet.Name, Rows: rows})
	}
	normalized, err := office.NormalizeWorkbook(wb)
	if err != nil {
		return parsed, office.Workbook{}, err
	}
	return parsed, normalized, nil
}

// cellText 把 JSON 单元格值转为文本。
//
// json.Number 保留原始字面量（例如 600519 不会变成 600519.0），
// boolean 转成 Excel 习惯的 TRUE/FALSE，null 视为空单元格。
func cellText(value any) (string, error) {
	switch v := value.(type) {
	case nil:
		return "", nil
	case string:
		return v, nil
	case json.Number:
		return v.String(), nil
	case bool:
		if v {
			return "TRUE", nil
		}
		return "FALSE", nil
	default:
		return "", fmt.Errorf("不支持的单元格类型 %T，只支持字符串、数字、布尔与 null", value)
	}
}

// truncateRunes 在超出上限时截断并给出说明。
func truncateRunes(text string, limit int) string {
	runes := []rune(text)
	if len(runes) <= limit {
		return text
	}
	return string(runes[:limit]) + fmt.Sprintf("\n… 内容过长，已截断到 %d 个字符（完整内容共 %d 个字符）", limit, len(runes))
}
