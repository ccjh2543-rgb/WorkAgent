package tools

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"simpleAgent/domain/tool"
	"simpleAgent/infrastructure/memory"
)

// MemorySearcher 记忆检索端口。
//
// tools 包只依赖这个窄接口，不关心检索是关键词、向量还是混合实现，
// 便于测试替换，也避免工具层直接耦合索引细节。
type MemorySearcher interface {
	Search(ctx context.Context, query string, topK int) ([]memory.Fragment, error)
}

// MemorySearchTool 让 Agent 在运行中按需召回长期记忆。
//
// 与「运行开始时注入一小段记忆」互补：注入保证不遗漏关键约定，
// 工具召回让模型在遇到具体问题时自己决定查什么，避免一次性塞满上下文。
type MemorySearchTool struct {
	searcher MemorySearcher
}

// NewMemorySearchTool 创建记忆检索工具。
func NewMemorySearchTool(searcher MemorySearcher) *MemorySearchTool {
	return &MemorySearchTool{searcher: searcher}
}

// Name 返回工具名。
func (t *MemorySearchTool) Name() string { return "memory_search" }

// Description 返回工具说明，会被拼进系统提示词与原生工具声明。
func (t *MemorySearchTool) Description() string {
	return "按关键词检索工作区长期记忆（项目约定、偏好、决策、教训、流程）。" +
		"当你不确定项目里已有的约定，或需要回忆以前做过什么决定时使用。"
}

// Spec 返回 memory_search 的参数规格。
func (t *MemorySearchTool) Spec() tool.ToolSpec {
	return tool.ToolSpec{
		Name: "memory_search",
		Params: []tool.ParamSpec{
			{Name: "query", Type: tool.TypeString, Required: true, Description: "检索关键词或问题，如「数据库选型」「代码风格」"},
			{Name: "limit", Type: tool.TypeInteger, Description: "最多返回几条，默认 5，上限 20"},
		},
	}
}

// memorySearchArgs memory_search 的 JSON 参数。
type memorySearchArgs struct {
	Query string `json:"query"`
	Limit int    `json:"limit"`
}

// 检索条数的默认值与上限。
const (
	defaultMemorySearchLimit = 5
	maxMemorySearchLimit     = 20
)

// Run 执行记忆检索，返回带章节与类型的条目列表。
func (t *MemorySearchTool) Run(args string) (string, error) {
	if t.searcher == nil {
		return "", errors.New("记忆检索未初始化")
	}
	var parsed memorySearchArgs
	if err := decodeToolArgs(args, &parsed); err != nil {
		return "", err
	}
	query := strings.TrimSpace(parsed.Query)
	if query == "" {
		return "", errors.New(`参数 query 不能为空，例如：{"query":"数据库选型"}`)
	}
	limit := parsed.Limit
	if limit <= 0 {
		limit = defaultMemorySearchLimit
	}
	if limit > maxMemorySearchLimit {
		limit = maxMemorySearchLimit
	}

	fragments, err := t.searcher.Search(context.Background(), query, limit)
	if err != nil {
		return "", err
	}
	if len(fragments) == 0 {
		return "没有找到相关记忆。", nil
	}

	var sb strings.Builder
	fmt.Fprintf(&sb, "找到 %d 条相关记忆：\n", len(fragments))
	for _, f := range fragments {
		sb.WriteString(formatMemoryFragment(f))
	}
	return sb.String(), nil
}

// formatMemoryFragment 单条记忆的展示格式：[章节/类型] 正文。
func formatMemoryFragment(f memory.Fragment) string {
	parts := make([]string, 0, 2)
	if f.Section != "" {
		parts = append(parts, f.Section)
	}
	if f.Kind != "" {
		parts = append(parts, string(f.Kind))
	}
	if len(parts) == 0 {
		return fmt.Sprintf("- %s\n", f.Content)
	}
	return fmt.Sprintf("- [%s] %s\n", strings.Join(parts, "/"), f.Content)
}
