package tools

import (
	"strings"
	"testing"
)

// TestWriteAndReadXLSXTool 验证 write_xlsx 与 read_xlsx 的闭环。
func TestWriteAndReadXLSXTool(t *testing.T) {
	ws := newTestWorkspace(t)
	writer := NewWriteXLSXTool(ws)
	args := `{"path":"output/结果.xlsx","sheets":[{"name":"汇总","rows":[["代码","得分"],["000001","88.5"],["600519","90"]]}]}`
	msg, err := writer.Run(args)
	if err != nil {
		t.Fatalf("写入失败: %v", err)
	}
	if !strings.Contains(msg, "1 个工作表") {
		t.Fatalf("写入结果=%q", msg)
	}

	reader := NewReadXLSXTool(ws)
	out, err := reader.Run(`{"path":"output/结果.xlsx"}`)
	if err != nil {
		t.Fatalf("读取失败: %v", err)
	}
	wantParts := []string{"【工作表：汇总，3 行】", "000001", "88.5", "600519", "90"}
	for _, part := range wantParts {
		if !strings.Contains(out, part) {
			t.Fatalf("读取结果缺少 %q: %s", part, out)
		}
	}

	// 指定工作表与行范围
	limited, err := reader.Run(`{"path":"output/结果.xlsx","sheet":"汇总","start_row":2,"end_row":2}`)
	if err != nil {
		t.Fatalf("按范围读取失败: %v", err)
	}
	if !strings.Contains(limited, "2\t000001\t88.5") || strings.Contains(limited, "600519") {
		t.Fatalf("行范围读取异常: %s", limited)
	}

	if _, err := reader.Run(`{"path":"output/结果.xlsx","sheet":"不存在"}`); err == nil {
		t.Fatal("不存在的工作表应返回错误")
	}
}

// TestWriteXLSXToolRejectsBadArgs 覆盖参数校验与路径越界。
func TestWriteXLSXToolRejectsBadArgs(t *testing.T) {
	ws := newTestWorkspace(t)
	writer := NewWriteXLSXTool(ws)

	cases := []struct {
		name string
		args string
	}{
		{"非 JSON", "output/a.xlsx"},
		{"缺少 path", `{"sheets":[{"name":"S","rows":[["a"]]}]}`},
		{"缺少 sheets", `{"path":"output/a.xlsx","sheets":[]}`},
		{"非法工作表名", `{"path":"output/a.xlsx","sheets":[{"name":"a/b","rows":[["x"]]}]}`},
		{"越界路径", `{"path":"../escape.xlsx","sheets":[{"name":"S","rows":[["x"]]}]}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := writer.Run(tc.args); err == nil {
				t.Fatalf("参数 %s 应被拒绝", tc.args)
			}
		})
	}
}

// TestWriteXLSXToolAcceptsJSONNumbers 验证数字/布尔/null 单元格。
func TestWriteXLSXToolAcceptsJSONNumbers(t *testing.T) {
	ws := newTestWorkspace(t)
	writer := NewWriteXLSXTool(ws)
	args := `{"path":"output/n.xlsx","sheets":[{"name":"S","rows":[["名称","数量","启用"],["甲",12.5,true],["乙",null,false]]}]}`
	if _, err := writer.Run(args); err != nil {
		t.Fatalf("写入失败: %v", err)
	}
	out, err := NewReadXLSXTool(ws).Run(`{"path":"output/n.xlsx"}`)
	if err != nil {
		t.Fatalf("读取失败: %v", err)
	}
	for _, want := range []string{"12.5", "TRUE", "FALSE"} {
		if !strings.Contains(out, want) {
			t.Fatalf("读取结果缺少 %q: %s", want, out)
		}
	}
}

// TestWriteXLSXToolDescribe 验证审批描述包含目标与规模。
func TestWriteXLSXToolDescribe(t *testing.T) {
	ws := newTestWorkspace(t)
	writer := NewWriteXLSXTool(ws)
	desc := writer.Describe(`{"path":"output/a.xlsx","sheets":[{"name":"汇总","rows":[["a","b"],["1","2"]]}]}`)
	if !strings.Contains(desc, "新建 Excel 文件 output/a.xlsx") || !strings.Contains(desc, "2 行") {
		t.Fatalf("审批描述=%q", desc)
	}
	denied := writer.Describe(`{"path":"../x.xlsx","sheets":[{"name":"S","rows":[["x"]]}]}`)
	if !strings.Contains(denied, "拒绝") {
		t.Fatalf("越界描述=%q", denied)
	}
}

// newTestWorkspace 创建测试工作区。
func newTestWorkspace(t *testing.T) *Workspace {
	t.Helper()
	ws, err := NewWorkspace(t.TempDir())
	if err != nil {
		t.Fatalf("创建测试工作区失败: %v", err)
	}
	return ws
}
