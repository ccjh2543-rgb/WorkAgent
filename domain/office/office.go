// Package office 定义办公文档的领域模型与命名规则。
//
// 本包只描述“一个表格/文档应该长什么样”，不依赖 zip、XML 或文件系统；
// 具体编解码由 infrastructure/office 实现。
package office

import (
	"errors"
	"fmt"
	"strings"
)

// 表格规模上限。
//
// 上限的作用是保护上下文与内存：工具返回会原样进入对话历史，
// 超大表即使被截断也会挤掉有用信息。
const (
	// MaxSheetRows 单个工作表的最大行数。
	MaxSheetRows = 5000
	// MaxSheetCols 单个工作表的最大列数。
	MaxSheetCols = 100
	// MaxCells 一个工作簿所有工作表的总单元格上限。
	MaxCells = 50000
	// MaxSheetNameRunes 工作表名称的最大字符数，与 Excel 限制一致。
	MaxSheetNameRunes = 31
)

// invalidSheetNameChars 工作表名称禁止出现的字符，与 Excel 规则一致。
const invalidSheetNameChars = "[]:*?/\\"

// Sheet 一个工作表。
//
// Rows 使用字符串表示单元格内容：编解码层负责把数字与文本还原成
// 正确的单元格类型，领域层只关心结构与命名规则。
type Sheet struct {
	Name string
	Rows [][]string
}

// Workbook 一个工作簿。至少包含一个工作表。
type Workbook struct {
	Sheets []Sheet
}

// NormalizeWorkbook 校验并归一化工作簿。
//
// 返回的工作簿与入参共享底层数据，只对名称做 trim 与空行裁剪；
// 校验失败时返回明确的中文错误，避免损坏的工作簿落到磁盘。
func NormalizeWorkbook(wb Workbook) (Workbook, error) {
	if len(wb.Sheets) == 0 {
		return Workbook{}, errors.New("工作簿至少需要一个工作表")
	}
	seen := make(map[string]bool, len(wb.Sheets))
	totalCells := 0
	out := Workbook{Sheets: make([]Sheet, 0, len(wb.Sheets))}

	for i, sheet := range wb.Sheets {
		name := strings.TrimSpace(sheet.Name)
		if name == "" {
			name = fmt.Sprintf("Sheet%d", i+1)
		}
		if err := validateSheetName(name); err != nil {
			return Workbook{}, err
		}
		key := strings.ToLower(name)
		if seen[key] {
			return Workbook{}, fmt.Errorf("工作表名称重复: %s", name)
		}
		seen[key] = true

		rows, err := normalizeRows(name, sheet.Rows)
		if err != nil {
			return Workbook{}, err
		}
		totalCells += countCells(rows)
		if totalCells > MaxCells {
			return Workbook{}, fmt.Errorf("工作簿总单元格数超过上限 %d", MaxCells)
		}
		out.Sheets = append(out.Sheets, Sheet{Name: name, Rows: rows})
	}
	return out, nil
}

// validateSheetName 校验单个工作表名称。
func validateSheetName(name string) error {
	if len([]rune(name)) > MaxSheetNameRunes {
		return fmt.Errorf("工作表名称 %s 超过 %d 个字符", name, MaxSheetNameRunes)
	}
	if strings.ContainsAny(name, invalidSheetNameChars) {
		return fmt.Errorf("工作表名称 %s 含非法字符（%s）", name, invalidSheetNameChars)
	}
	if strings.HasPrefix(name, "'") || strings.HasSuffix(name, "'") {
		return fmt.Errorf("工作表名称 %s 不能以单引号开头或结尾", name)
	}
	return nil
}

// normalizeRows 裁剪行尾空白列与末尾空行，并校验行列上限。
func normalizeRows(sheetName string, rows [][]string) ([][]string, error) {
	if len(rows) > MaxSheetRows {
		return nil, fmt.Errorf("工作表 %s 有 %d 行，超过上限 %d", sheetName, len(rows), MaxSheetRows)
	}
	out := make([][]string, 0, len(rows))
	for _, row := range rows {
		if len(row) > MaxSheetCols {
			return nil, fmt.Errorf("工作表 %s 有 %d 列，超过上限 %d", sheetName, len(row), MaxSheetCols)
		}
		trimmed := trimTrailingEmpty(row)
		out = append(out, trimmed)
	}
	for len(out) > 0 && isEmptyRow(out[len(out)-1]) {
		out = out[:len(out)-1]
	}
	return out, nil
}

// trimTrailingEmpty 去掉行尾的空单元格，减少无意义的稀疏列。
func trimTrailingEmpty(row []string) []string {
	end := len(row)
	for end > 0 && row[end-1] == "" {
		end--
	}
	out := make([]string, end)
	copy(out, row[:end])
	return out
}

// isEmptyRow 判断一行是否全部为空。
func isEmptyRow(row []string) bool {
	for _, cell := range row {
		if cell != "" {
			return false
		}
	}
	return true
}

// countCells 统计非空单元格数量。
func countCells(rows [][]string) int {
	total := 0
	for _, row := range rows {
		total += len(row)
	}
	return total
}

// SheetByName 按名称查找工作表，名称比较不区分大小写。
func (wb Workbook) SheetByName(name string) (Sheet, bool) {
	key := strings.ToLower(strings.TrimSpace(name))
	for _, sheet := range wb.Sheets {
		if strings.ToLower(sheet.Name) == key {
			return sheet, true
		}
	}
	return Sheet{}, false
}
