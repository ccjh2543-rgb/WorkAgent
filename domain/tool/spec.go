package tool

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"
)

// ParamType 工具参数的数据类型。
// 只覆盖本工程工具实际用到的 JSON 类型子集，够用即止，不追求完整实现 JSON Schema 规范。
type ParamType string

const (
	// TypeString 字符串
	TypeString ParamType = "string"
	// TypeInteger 整数
	TypeInteger ParamType = "integer"
	// TypeNumber 浮点数
	TypeNumber ParamType = "number"
	// TypeBoolean 布尔值
	TypeBoolean ParamType = "boolean"
	// TypeArray 数组，元素规格见 ParamSpec.Items
	TypeArray ParamType = "array"
	// TypeObject 嵌套对象，属性规格见 ParamSpec.Properties
	TypeObject ParamType = "object"
)

// ParamSpec 描述单个工具参数的约束，同时承担两个职责：
//  1. 渲染成 JSON Schema 注入系统提示词，让模型知道有哪些参数名、什么类型；
//  2. 作为执行前的校验依据，把「臆造参数名 / 类型不符」挡在工具真正执行之前。
type ParamSpec struct {
	// Name 参数名（即 JSON 对象的键）
	Name string
	// Type 期望类型
	Type ParamType
	// Description 给模型看的中文说明，会出现在提示词的 JSON Schema 中
	Description string
	// Required 是否必填
	Required bool
	// Enum 允许的取值集合，仅对 TypeString 生效；为空表示不限
	Enum []string
	// Min 数值下限（含），仅对 TypeInteger/TypeNumber 生效；nil 表示不限
	Min *float64
	// Max 数值上限（含），仅对 TypeInteger/TypeNumber 生效；nil 表示不限
	Max *float64
	// Items 数组元素规格，仅对 TypeArray 生效
	Items *ParamSpec
	// Properties 嵌套对象属性规格，仅对 TypeObject 生效
	Properties []ParamSpec
}

// ToolSpec 工具的结构化契约：工具名 + 参数规格。
// 工具的自然语言说明仍由 Tool.Description() 提供，此处刻意不重复，
// 避免同一件事存在两处描述而互相不一致。
//
// 调用示例：
//
//	spec := ToolSpec{Name: "read_file", Params: []ParamSpec{
//		{Name: "path", Type: TypeString, Required: true},
//	}}
//	spec.JSONSchema()
//	// {"type":"object","properties":{"path":{"type":"string"}},"required":["path"]}
//	spec.ValidateArgs(`{"path":"a.go"}`) // nil
//	spec.ValidateArgs(`{}`)              // 缺少必填参数 "path"
type ToolSpec struct {
	// Name 工具名，须与 Tool.Name() 保持一致
	Name string
	// Params 参数规格，按声明顺序渲染；为空表示该工具不接受任何参数
	Params []ParamSpec
}

// SpecProvider 可选接口：工具实现它即可对外暴露结构化参数规格。
//
// 这是「工具侧改造」的第一步：不改动 LLM 端口、不改 !TOOL: 文本协议，
// 只把参数从「裸字符串、无任何约束」升级为「有名字、有类型、可校验」的结构化契约。
// 未实现该接口的工具（含外部延迟工具）完全保持旧行为，不做任何校验。
type SpecProvider interface {
	// Spec 返回该工具的参数规格
	Spec() ToolSpec
}

// ValidateArgs 校验模型给出的原始参数串是否符合本规格。
//
// 约定：
//   - Params 为空视为「无参数工具」，空串与 "{}" 均合法，其余一律拒绝；
//   - Params 非空时参数必须是 JSON 对象（空串按 "{}" 处理），
//     缺失必填参数、类型不符、取值越界、出现未声明的参数名，都会返回错误。
//
// 返回的 error 文案会原样回注给模型，因此逐条指明「哪个参数、错在哪、可用参数有哪些」。
func (s ToolSpec) ValidateArgs(raw string) error {
	trimmed := strings.TrimSpace(raw)
	if len(s.Params) == 0 {
		if trimmed == "" || trimmed == "{}" {
			return nil
		}
		return fmt.Errorf("工具 %s 不接受参数，但收到：%s", s.Name, trimmed)
	}
	if trimmed == "" {
		trimmed = "{}"
	}
	var obj map[string]json.RawMessage
	if err := json.Unmarshal([]byte(trimmed), &obj); err != nil {
		return fmt.Errorf("工具 %s 的参数必须是 JSON 对象（%v），实际收到：%s", s.Name, err, trimmed)
	}
	return validateObject(s.Name, s.Params, obj)
}

// JSONSchema 把本规格渲染成一行 JSON Schema 文本，用于注入系统提示词。
//
// 采用手写渲染而非 json.Marshal：一是保证参数顺序与声明顺序一致（便于模型逐个对齐），
// 二是这条路径不存在序列化失败的可能，调用方无需处理 error。
func (s ToolSpec) JSONSchema() string {
	var sb strings.Builder
	sb.WriteString(`{"type":"object"`)
	renderObjectBody(&sb, s.Params)
	sb.WriteString("}")
	return sb.String()
}

// renderObjectBody 渲染对象的 properties 与 required 两段内容（不含外层花括号）。
func renderObjectBody(sb *strings.Builder, params []ParamSpec) {
	sb.WriteString(`,"properties":{`)
	for i, p := range params {
		if i > 0 {
			sb.WriteString(",")
		}
		sb.WriteString(strconv.Quote(p.Name))
		sb.WriteString(":")
		renderParam(sb, p)
	}
	sb.WriteString("}")

	required := requiredNames(params)
	if len(required) > 0 {
		sb.WriteString(`,"required":[`)
		for i, name := range required {
			if i > 0 {
				sb.WriteString(",")
			}
			sb.WriteString(strconv.Quote(name))
		}
		sb.WriteString("]")
	}
}

// renderParam 渲染单个参数的 schema 片段。
func renderParam(sb *strings.Builder, p ParamSpec) {
	sb.WriteString(`{"type":`)
	sb.WriteString(strconv.Quote(string(p.Type)))
	if p.Description != "" {
		sb.WriteString(`,"description":`)
		sb.WriteString(strconv.Quote(p.Description))
	}
	if len(p.Enum) > 0 {
		sb.WriteString(`,"enum":[`)
		for i, v := range p.Enum {
			if i > 0 {
				sb.WriteString(",")
			}
			sb.WriteString(strconv.Quote(v))
		}
		sb.WriteString("]")
	}
	if p.Min != nil {
		sb.WriteString(`,"minimum":` + strconv.FormatFloat(*p.Min, 'f', -1, 64))
	}
	if p.Max != nil {
		sb.WriteString(`,"maximum":` + strconv.FormatFloat(*p.Max, 'f', -1, 64))
	}
	if p.Items != nil {
		sb.WriteString(`,"items":`)
		renderParam(sb, *p.Items)
	}
	if len(p.Properties) > 0 {
		renderObjectBody(sb, p.Properties)
	}
	sb.WriteString("}")
}

// validateObject 校验一个 JSON 对象是否满足参数规格集合。
// scope 用于拼接错误信息，指明出错的工具或嵌套字段。
func validateObject(scope string, params []ParamSpec, obj map[string]json.RawMessage) error {
	known := make(map[string]ParamSpec, len(params))
	for _, p := range params {
		known[p.Name] = p
	}
	// 未声明的参数名直接报错：模型臆造参数名若被静默忽略，
	// 工具会以缺省值继续执行并产出看似成功的结果，比直接失败更难排查。
	for name := range obj {
		if _, ok := known[name]; !ok {
			return fmt.Errorf("%s 不认识参数 %q，可用参数：%s", scope, name, strings.Join(paramNames(params), "、"))
		}
	}
	for _, p := range params {
		rawVal, present := obj[p.Name]
		if !present || isJSONNull(rawVal) {
			if p.Required {
				return fmt.Errorf("%s 缺少必填参数 %q（%s）", scope, p.Name, p.Description)
			}
			continue
		}
		if err := validateValue(scope, p, rawVal); err != nil {
			return fmt.Errorf("%s 的参数 %q 不合法：%v", scope, p.Name, err)
		}
	}
	return nil
}

// validateValue 校验单个参数的取值。
func validateValue(scope string, p ParamSpec, raw json.RawMessage) error {
	switch p.Type {
	case TypeString:
		var s string
		if err := json.Unmarshal(raw, &s); err != nil {
			return errors.New("应为字符串（值需带双引号）")
		}
		if len(p.Enum) > 0 && !containsString(p.Enum, s) {
			return fmt.Errorf("取值必须是 %s 之一，实际为 %q", strings.Join(p.Enum, "、"), s)
		}
	case TypeInteger:
		n, err := asNumber(raw)
		if err != nil {
			return errors.New("应为整数")
		}
		if n != math.Trunc(n) {
			return fmt.Errorf("应为整数，实际为 %s", strconv.FormatFloat(n, 'f', -1, 64))
		}
		return checkRange(p, n)
	case TypeNumber:
		n, err := asNumber(raw)
		if err != nil {
			return errors.New("应为数字")
		}
		return checkRange(p, n)
	case TypeBoolean:
		text := strings.Trim(strings.TrimSpace(string(raw)), `"`)
		if text != "true" && text != "false" {
			return errors.New("应为布尔值 true 或 false")
		}
	case TypeArray:
		var items []json.RawMessage
		if err := json.Unmarshal(raw, &items); err != nil {
			return errors.New("应为数组")
		}
		if p.Items == nil {
			return nil
		}
		for i, item := range items {
			if isJSONNull(item) {
				continue
			}
			if err := validateValue(scope, *p.Items, item); err != nil {
				return fmt.Errorf("第 %d 个元素不合法：%v", i+1, err)
			}
		}
	case TypeObject:
		var obj map[string]json.RawMessage
		if err := json.Unmarshal(raw, &obj); err != nil {
			return errors.New("应为对象")
		}
		return validateObject(scope+"."+p.Name, p.Properties, obj)
	default:
		return fmt.Errorf("未知的参数类型 %q", p.Type)
	}
	return nil
}

// checkRange 校验数值是否落在 [Min, Max] 区间内。
func checkRange(p ParamSpec, n float64) error {
	if p.Min != nil && n < *p.Min {
		return fmt.Errorf("不得小于 %s，实际为 %s", strconv.FormatFloat(*p.Min, 'f', -1, 64), strconv.FormatFloat(n, 'f', -1, 64))
	}
	if p.Max != nil && n > *p.Max {
		return fmt.Errorf("不得大于 %s，实际为 %s", strconv.FormatFloat(*p.Max, 'f', -1, 64), strconv.FormatFloat(n, 'f', -1, 64))
	}
	return nil
}

// asNumber 把 JSON 数值、或被双引号包裹的数值字符串统一解析成 float64。
// 兼容模型的常见写法：本工程的提示词历史上要求「值一律带双引号」，
// 因此 1 与 "1" 都应视为合法，避免因引号问题产生无意义的校验失败。
func asNumber(raw json.RawMessage) (float64, error) {
	text := strings.TrimSpace(string(raw))
	if len(text) >= 2 && text[0] == '"' {
		var s string
		if err := json.Unmarshal(raw, &s); err != nil {
			return 0, err
		}
		text = strings.TrimSpace(s)
	}
	return strconv.ParseFloat(text, 64)
}

// isJSONNull 判断原始 JSON 片段是否为 null。
func isJSONNull(raw json.RawMessage) bool {
	return strings.TrimSpace(string(raw)) == "null"
}

// requiredNames 提取必填参数名，保持声明顺序。
func requiredNames(params []ParamSpec) []string {
	names := make([]string, 0, len(params))
	for _, p := range params {
		if p.Required {
			names = append(names, p.Name)
		}
	}
	return names
}

// paramNames 提取全部参数名，保持声明顺序，用于错误提示。
func paramNames(params []ParamSpec) []string {
	names := make([]string, 0, len(params))
	for _, p := range params {
		names = append(names, p.Name)
	}
	return names
}

// containsString 判断切片是否包含目标字符串
func containsString(list []string, target string) bool {
	for _, v := range list {
		if v == target {
			return true
		}
	}
	return false
}
