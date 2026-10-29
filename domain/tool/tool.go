// Package tool 定义 Agent 工具领域的核心模型。
//
// 本包只承载纯领域规则，不依赖任何外部包（含 eino 与具体 SDK）。
// 工具的执行策略（是否需要人工确认）、风险分级、注册表都在这里定义，
// 这是整条工具链路的领域边界。
package tool

import (
	"errors"
	"fmt"
	"strings"
)

// RiskLevel 操作风险等级，用于向人工展示以及按阈值过滤
type RiskLevel int

const (
	// RiskReadOnly 只读，无副作用：计算器、搜索
	RiskReadOnly RiskLevel = iota
	// RiskWriteLocal 只影响本地项目：写记忆、写本地文件
	RiskWriteLocal
	// RiskWriteExternal 影响外部系统：发消息、调三方 API
	RiskWriteExternal
	// RiskDestructive 不可逆：删文件、执行 shell
	RiskDestructive
)

// String 返回风险等级的中文标签，用于交互提示
func (r RiskLevel) String() string {
	switch r {
	case RiskReadOnly:
		return "只读"
	case RiskWriteLocal:
		return "写本地"
	case RiskWriteExternal:
		return "写外部"
	case RiskDestructive:
		return "不可逆"
	default:
		return "未知"
	}
}

// Tool 工具接口
type Tool interface {
	Name() string
	Description() string
	Run(args string) (string, error)
}

// ActionDescriber 可选接口：工具可以实现它来提供更具体的人工确认说明。
//
// 默认的「即将调用工具 X」无法让人判断该不该放行。
// 写文件这类操作必须让人看到具体路径与影响范围（新建还是覆盖、内容多大），
// 否则人在回路的审批只是一个不得不点的确认框。
type ActionDescriber interface {
	// Describe 返回给人工看的操作说明；返回空串时回退到默认文案
	Describe(args string) string
}

// ToolPolicy 工具的执行策略，决定它是否需要人工确认
type ToolPolicy struct {
	// Risk 操作风险等级，用于向人工展示与按阈值过滤
	Risk RiskLevel
	// AutoApprove 为 true 时跳过审批闸门，直接执行
	AutoApprove bool
}

// ToolRegistry 工具注册表
type ToolRegistry struct {
	tools    map[string]Tool
	policies map[string]ToolPolicy
}

// NewToolRegistry 创建工具注册表
func NewToolRegistry() *ToolRegistry {
	return &ToolRegistry{
		tools:    make(map[string]Tool),
		policies: make(map[string]ToolPolicy),
	}
}

// Register 注册工具但不声明策略。
// 未声明策略的工具一律按「需要人工确认」处理（fail-safe），
// 避免新加的工具因为忘记声明而被静默放行。需要自动放行的请用 RegisterWithPolicy。
func (tr *ToolRegistry) Register(tool Tool) {
	tr.tools[tool.Name()] = tool
}

// RegisterWithPolicy 注册工具并声明其执行策略
func (tr *ToolRegistry) RegisterWithPolicy(tool Tool, policy ToolPolicy) {
	tr.tools[tool.Name()] = tool
	tr.policies[tool.Name()] = policy
}

// Policy 返回工具的执行策略。
// 未显式声明时返回最保守的默认值：写本地 + 需要人工确认。
func (tr *ToolRegistry) Policy(name string) ToolPolicy {
	if policy, ok := tr.policies[name]; ok {
		return policy
	}
	return ToolPolicy{Risk: RiskWriteLocal, AutoApprove: false}
}

// Lookup 按名称取出工具，不存在时返回 nil。
// 用于在审批前判断工具能否提供更具体的操作说明。
func (tr *ToolRegistry) Lookup(name string) Tool {
	return tr.tools[name]
}

// Names 返回当前已注册的全部工具名（顺序不定）。
// 供上层（应用层）展示工具列表使用，外层可用 SortedToolNames 排序。
func (tr *ToolRegistry) Names() []string {
	names := make([]string, 0, len(tr.tools))
	for name := range tr.tools {
		names = append(names, name)
	}
	return names
}

// Unregister 删除指定工具的注册项。
// 仅用于运行时动态移除自定义技能；内置工具不建议走这里。
func (tr *ToolRegistry) Unregister(name string) {
	delete(tr.tools, name)
	delete(tr.policies, name)
}

// Spec 返回工具的结构化参数规格。
// 工具未实现 SpecProvider 时返回 ok=false，调用方应视为「无契约、不校验」。
func (tr *ToolRegistry) Spec(name string) (ToolSpec, bool) {
	t, ok := tr.tools[name]
	if !ok {
		return ToolSpec{}, false
	}
	provider, ok := t.(SpecProvider)
	if !ok {
		return ToolSpec{}, false
	}
	return provider.Spec(), true
}

// ValidateArgs 按工具声明的规格校验参数。
//
// 工具不存在时返回错误；工具未声明规格（未实现 SpecProvider）时直接放行，
// 以保证外部延迟工具与尚未迁移的旧工具行为完全不变。
func (tr *ToolRegistry) ValidateArgs(name, args string) error {
	t, ok := tr.tools[name]
	if !ok {
		return errors.New("工具不存在:" + name)
	}
	provider, ok := t.(SpecProvider)
	if !ok {
		return nil
	}
	return provider.Spec().ValidateArgs(args)
}

// getAllToolsDesc 遍历注册表全部工具，渲染为提示词中的工具列表。
// 传入 allowSet 非空时只渲染白名单内的工具（用于评估模式屏蔽内置工具）。
// 声明了参数规格的工具会额外附带一行 JSON Schema，
// 让模型在调用前就知道合法的参数名与类型，而不是靠试错。
// 注意遍历 map 顺序不定，但工具列表仅作参考信息，顺序不影响语义。
func (tr *ToolRegistry) getAllToolsDesc(allowSet map[string]bool) string {
	var sb strings.Builder
	for _, t := range tr.tools {
		if allowSet != nil && !allowSet[t.Name()] {
			continue
		}
		sb.WriteString(fmt.Sprintf("- %s: %s\n", t.Name(), t.Description()))
		if provider, ok := t.(SpecProvider); ok {
			sb.WriteString(fmt.Sprintf("  参数(JSON Schema): %s\n", provider.Spec().JSONSchema()))
		}
	}
	return sb.String()
}

// GetAllToolsDesc 渲染全部已注册工具（生产模式）
func (tr *ToolRegistry) GetAllToolsDesc() string {
	return tr.getAllToolsDesc(nil)
}

// GetAllToolsDescFiltered 只渲染白名单内的工具（评估模式屏蔽内置工具）
func (tr *ToolRegistry) GetAllToolsDescFiltered(allowSet map[string]bool) string {
	return tr.getAllToolsDesc(allowSet)
}

// ExecuteTool 按名称执行一个工具
func (tr *ToolRegistry) ExecuteTool(toolName, args string) (string, error) {
	t, ok := tr.tools[toolName]
	if !ok {
		return "", errors.New("工具不存在:" + toolName)
	}
	return t.Run(args)
}
