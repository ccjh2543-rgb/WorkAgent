package office

import (
	"strings"
	"testing"
)

// TestNormalizeWorkbook 覆盖工作簿命名与规模规则。
func TestNormalizeWorkbook(t *testing.T) {
	tests := []struct {
		name    string
		input   Workbook
		wantErr string
		check   func(t *testing.T, wb Workbook)
	}{
		{
			name:    "空工作簿被拒绝",
			input:   Workbook{},
			wantErr: "至少需要一个工作表",
		},
		{
			name:    "名称非法字符被拒绝",
			input:   Workbook{Sheets: []Sheet{{Name: "a/b", Rows: [][]string{{"x"}}}}},
			wantErr: "含非法字符",
		},
		{
			name:    "名称超长被拒绝",
			input:   Workbook{Sheets: []Sheet{{Name: strings.Repeat("a", 32)}}},
			wantErr: "超过 31 个字符",
		},
		{
			name:    "重复名称被拒绝",
			input:   Workbook{Sheets: []Sheet{{Name: "Data"}, {Name: "data"}}},
			wantErr: "工作表名称重复",
		},
		{
			name:  "空名称按序号补齐",
			input: Workbook{Sheets: []Sheet{{Name: "  ", Rows: [][]string{{"a", "b", ""}}}}},
			check: func(t *testing.T, wb Workbook) {
				t.Helper()
				if wb.Sheets[0].Name != "Sheet1" {
					t.Fatalf("工作表名称=%q, want Sheet1", wb.Sheets[0].Name)
				}
				if len(wb.Sheets[0].Rows[0]) != 2 {
					t.Fatalf("行尾空列未裁剪: %#v", wb.Sheets[0].Rows[0])
				}
			},
		},
		{
			name: "末尾空行被裁剪",
			input: Workbook{Sheets: []Sheet{{
				Name: "S",
				Rows: [][]string{{"a"}, {}, {""}},
			}}},
			check: func(t *testing.T, wb Workbook) {
				t.Helper()
				if len(wb.Sheets[0].Rows) != 1 {
					t.Fatalf("行数=%d, want 1", len(wb.Sheets[0].Rows))
				}
			},
		},
		{
			name: "行数超限被拒绝",
			input: Workbook{Sheets: []Sheet{{
				Name: "S",
				Rows: make([][]string, MaxSheetRows+1),
			}}},
			wantErr: "超过上限",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := NormalizeWorkbook(tt.input)
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("err=%v, want contains %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if tt.check != nil {
				tt.check(t, got)
			}
		})
	}
}

// TestSheetByName 验证按名称查找不区分大小写。
func TestSheetByName(t *testing.T) {
	wb := Workbook{Sheets: []Sheet{{Name: "Summary", Rows: [][]string{{"x"}}}}}
	if _, ok := wb.SheetByName("summary"); !ok {
		t.Fatal("应能按小写名称找到工作表")
	}
	if _, ok := wb.SheetByName("missing"); ok {
		t.Fatal("不存在的工作表不应返回 true")
	}
}
