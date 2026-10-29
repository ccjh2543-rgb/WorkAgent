package tool

import (
	"strings"
	"testing"
)

// floatPtr 构造 ParamSpec 数值边界的便捷函数（测试用）
func floatPtr(v float64) *float64 { return &v }

// sampleSpec 覆盖各类参数形态的测试规格：
// 必填字符串、可选字符串（带枚举）、必填整数（带区间）、布尔、数组（元素为对象）。
func sampleSpec() ToolSpec {
	return ToolSpec{
		Name: "demo",
		Params: []ParamSpec{
			{Name: "path", Type: TypeString, Required: true, Description: "相对路径"},
			{Name: "mode", Type: TypeString, Enum: []string{"read", "write"}, Description: "模式"},
			{Name: "count", Type: TypeInteger, Required: true, Min: floatPtr(1), Max: floatPtr(10)},
			{Name: "force", Type: TypeBoolean},
			{
				Name: "rows", Type: TypeArray,
				Items: &ParamSpec{
					Type: TypeObject,
					Properties: []ParamSpec{
						{Name: "name", Type: TypeString, Required: true},
						{Name: "size", Type: TypeNumber},
					},
				},
			},
		},
	}
}

// TestToolSpecValidateArgs 表驱动校验 ValidateArgs 的各类通过与拒绝分支。
func TestToolSpecValidateArgs(t *testing.T) {
	cases := []struct {
		name    string
		spec    ToolSpec
		args    string
		wantErr bool
		errHint string // 期望错误信息包含的片段，用于确认报错定位准确
	}{
		{
			name: "合法完整参数",
			spec: sampleSpec(),
			args: `{"path":"input/a.xlsx","mode":"read","count":3,"force":true,` +
				`"rows":[{"name":"s1","size":1.5}]}`,
			wantErr: false,
		},
		{
			name:    "仅必填参数",
			spec:    sampleSpec(),
			args:    `{"path":"a","count":1}`,
			wantErr: false,
		},
		{
			name:    "缺失必填参数",
			spec:    sampleSpec(),
			args:    `{"count":1}`,
			wantErr: true,
			errHint: `缺少必填参数 "path"`,
		},
		{
			name:    "非 JSON 对象",
			spec:    sampleSpec(),
			args:    `not-json`,
			wantErr: true,
			errHint: "必须是 JSON 对象",
		},
		{
			name:    "顶层是数组而非对象",
			spec:    sampleSpec(),
			args:    `[1,2]`,
			wantErr: true,
			errHint: "必须是 JSON 对象",
		},
		{
			name:    "臆造参数名",
			spec:    sampleSpec(),
			args:    `{"path":"a","count":1,"oops":1}`,
			wantErr: true,
			errHint: `不认识参数 "oops"`,
		},
		{
			name:    "字符串类型不符",
			spec:    sampleSpec(),
			args:    `{"path":123,"count":1}`,
			wantErr: true,
			errHint: `参数 "path" 不合法`,
		},
		{
			name:    "枚举越界",
			spec:    sampleSpec(),
			args:    `{"path":"a","count":1,"mode":"delete"}`,
			wantErr: true,
			errHint: "取值必须是 read、write 之一",
		},
		{
			name:    "整数下界越界",
			spec:    sampleSpec(),
			args:    `{"path":"a","count":0}`,
			wantErr: true,
			errHint: "不得小于 1",
		},
		{
			name:    "整数上界越界",
			spec:    sampleSpec(),
			args:    `{"path":"a","count":11}`,
			wantErr: true,
			errHint: "不得大于 10",
		},
		{
			name:    "整数位带小数",
			spec:    sampleSpec(),
			args:    `{"path":"a","count":1.5}`,
			wantErr: true,
			errHint: "应为整数",
		},
		{
			name:    "引号包裹的数字视为合法",
			spec:    sampleSpec(),
			args:    `{"path":"a","count":"3"}`,
			wantErr: false,
		},
		{
			name:    "布尔写成字符串",
			spec:    sampleSpec(),
			args:    `{"path":"a","count":1,"force":"false"}`,
			wantErr: false,
		},
		{
			name:    "布尔取值非法",
			spec:    sampleSpec(),
			args:    `{"path":"a","count":1,"force":"yes"}`,
			wantErr: true,
			errHint: "应为布尔值",
		},
		{
			name:    "数组元素缺必填字段",
			spec:    sampleSpec(),
			args:    `{"path":"a","count":1,"rows":[{"size":1}]}`,
			wantErr: true,
			errHint: `缺少必填参数 "name"`,
		},
		{
			name:    "可选参数为 null 视为未提供",
			spec:    sampleSpec(),
			args:    `{"path":"a","count":1,"mode":null,"rows":null}`,
			wantErr: false,
		},
		{
			name:    "必填参数为 null 视为缺失",
			spec:    sampleSpec(),
			args:    `{"path":null,"count":1}`,
			wantErr: true,
			errHint: `缺少必填参数 "path"`,
		},
		{
			name:    "无参数工具收到空串",
			spec:    ToolSpec{Name: "noop"},
			args:    "",
			wantErr: false,
		},
		{
			name:    "无参数工具收到空对象",
			spec:    ToolSpec{Name: "noop"},
			args:    "{}",
			wantErr: false,
		},
		{
			name:    "无参数工具收到多余参数",
			spec:    ToolSpec{Name: "noop"},
			args:    `{"a":1}`,
			wantErr: true,
			errHint: "不接受参数",
		},
		{
			name:    "仅有可选参数时允许空串",
			spec:    ToolSpec{Name: "opt", Params: []ParamSpec{{Name: "a", Type: TypeString}}},
			args:    "",
			wantErr: false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.spec.ValidateArgs(tc.args)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("期望校验失败，实际通过：args=%s", tc.args)
				}
				if !strings.Contains(err.Error(), tc.errHint) {
					t.Fatalf("错误信息未包含 %q，实际为：%v", tc.errHint, err)
				}
				return
			}
			if err != nil {
				t.Fatalf("期望校验通过，实际失败：%v", err)
			}
		})
	}
}

// TestToolSpecJSONSchema 验证 schema 渲染顺序、必填列表与嵌套结构。
func TestToolSpecJSONSchema(t *testing.T) {
	want := `{"type":"object","properties":{` +
		`"path":{"type":"string","description":"相对路径"},` +
		`"mode":{"type":"string","description":"模式","enum":["read","write"]},` +
		`"count":{"type":"integer","minimum":1,"maximum":10},` +
		`"force":{"type":"boolean"},` +
		`"rows":{"type":"array","items":{"type":"object","properties":{` +
		`"name":{"type":"string"},"size":{"type":"number"}},"required":["name"]}}` +
		`},"required":["path","count"]}`
	got := sampleSpec().JSONSchema()
	if got != want {
		t.Fatalf("JSONSchema 渲染不符\n期望：%s\n实际：%s", want, got)
	}
}

// TestRegistryValidateArgs 验证注册表对「有规格」与「无规格」工具的分流行为。
func TestRegistryValidateArgs(t *testing.T) {
	reg := NewToolRegistry()
	reg.Register(specTool{spec: sampleSpec()})
	reg.Register(legacyTool{})

	if err := reg.ValidateArgs("demo", `{"path":"a","count":1}`); err != nil {
		t.Fatalf("有规格工具合法参数应通过，实际：%v", err)
	}
	if err := reg.ValidateArgs("demo", `{"count":1}`); err == nil {
		t.Fatal("有规格工具缺必填参数应报错，实际通过")
	}
	// 未实现 SpecProvider 的旧工具不做任何校验
	if err := reg.ValidateArgs("legacy", "任意裸字符串||都可以"); err != nil {
		t.Fatalf("无规格工具应放行，实际：%v", err)
	}
	if err := reg.ValidateArgs("missing", "x"); err == nil {
		t.Fatal("不存在的工具应报错，实际通过")
	}
	if _, ok := reg.Spec("legacy"); ok {
		t.Fatal("旧工具不应返回规格")
	}
	if _, ok := reg.Spec("demo"); !ok {
		t.Fatal("有规格工具应返回规格")
	}
}

// TestGetAllToolsDescWithSpec 验证提示词渲染会附带 JSON Schema。
func TestGetAllToolsDescWithSpec(t *testing.T) {
	reg := NewToolRegistry()
	reg.Register(specTool{spec: ToolSpec{
		Name:   "demo",
		Params: []ParamSpec{{Name: "path", Type: TypeString, Required: true}},
	}})
	reg.Register(legacyTool{})

	desc := reg.GetAllToolsDesc()
	if !strings.Contains(desc, "参数(JSON Schema): ") {
		t.Fatalf("应渲染参数规格行，实际：%s", desc)
	}
	if !strings.Contains(desc, `"path"`) {
		t.Fatalf("应渲染参数名，实际：%s", desc)
	}
	// 白名单过滤：只放行 legacy 时不应出现 demo 的 schema
	filtered := reg.GetAllToolsDescFiltered(map[string]bool{"legacy": true})
	if strings.Contains(filtered, "参数(JSON Schema)") {
		t.Fatalf("白名单外工具不应渲染，实际：%s", filtered)
	}
}

// specTool 实现 Tool 与 SpecProvider 的测试替身
type specTool struct {
	spec ToolSpec
}

func (s specTool) Name() string { return s.spec.Name }

func (s specTool) Description() string { return "带规格的测试工具" }

func (s specTool) Run(args string) (string, error) { return args, nil }

func (s specTool) Spec() ToolSpec { return s.spec }

// legacyTool 只实现 Tool，用于验证「无规格即放行」的兼容行为
type legacyTool struct{}

func (legacyTool) Name() string { return "legacy" }

func (legacyTool) Description() string { return "旧工具" }

func (legacyTool) Run(args string) (string, error) { return args, nil }
