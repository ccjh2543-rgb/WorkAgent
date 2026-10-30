package tools

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"simpleAgent/domain/tool"
)

// ToolOutputReadMaxBytes 单次回读的字节上限。
// 留存文件可能很大，回读必须限长，避免一次工具结果再次挤爆上下文。
const ToolOutputReadMaxBytes = 256 * 1024

// ToolOutputReader 读取被折叠工具输出的完整内容。
//
// 目录提供者每次调用时才求值：Agent 的会话 ID 在 Run 期间才确定，
// 构造时固定目录会把所有会话混在一起，造成跨会话数据可见。
type ToolOutputReader struct {
	dir func() string
}

// NewToolOutputReader 创建工具输出回取工具。dir 返回当前会话的留存目录。
func NewToolOutputReader(dir func() string) *ToolOutputReader {
	return &ToolOutputReader{dir: dir}
}

// Name 返回工具名。
func (t *ToolOutputReader) Name() string { return "read_tool_output" }

// Description 返回工具说明，会被拼进系统提示词与原生工具声明。
func (t *ToolOutputReader) Description() string {
	return "读取被折叠的工具输出全文。" +
		"当工具结果里出现「完整输出已留存」路径且你确实需要中间细节时使用；" +
		"只能读取当前会话留存的文件。"
}

// Spec 返回 read_tool_output 的参数规格。
func (t *ToolOutputReader) Spec() tool.ToolSpec {
	return tool.ToolSpec{
		Name: "read_tool_output",
		Params: []tool.ParamSpec{
			{Name: "path", Type: tool.TypeString, Required: true, Description: "折叠结果里给出的留存路径，或当前会话留存目录下的文件名"},
		},
	}
}

// toolOutputArgs read_tool_output 的 JSON 参数。
type toolOutputArgs struct {
	Path string `json:"path"`
}

// Run 读取留存文件。只允许当前会话留存目录内的路径，防目录穿越。
func (t *ToolOutputReader) Run(args string) (string, error) {
	if t.dir == nil {
		return "", errors.New("工具输出留存未初始化")
	}
	root := strings.TrimSpace(t.dir())
	if root == "" {
		return "", errors.New("当前会话没有可回取的工具输出")
	}
	var parsed toolOutputArgs
	if err := decodeToolArgs(args, &parsed); err != nil {
		return "", err
	}
	path := strings.TrimSpace(parsed.Path)
	if path == "" {
		return "", errors.New(`参数 path 不能为空，例如：{"path":"<折叠结果中给出的路径>"}`)
	}

	// 相对路径按留存目录解析；绝对路径也必须落在留存目录内
	target := path
	if !filepath.IsAbs(target) {
		target = filepath.Join(root, target)
	}
	rootAbs, err := filepath.Abs(root)
	if err != nil {
		return "", fmt.Errorf("解析留存目录失败: %w", err)
	}
	// 目录与文件都可能是指向别处的符号链接，先解析再比较
	if real, err := filepath.EvalSymlinks(rootAbs); err == nil {
		rootAbs = real
	}
	targetAbs, err := filepath.Abs(target)
	if err != nil {
		return "", fmt.Errorf("解析路径失败: %w", err)
	}
	realTarget, err := filepath.EvalSymlinks(targetAbs)
	if err != nil {
		return "", fmt.Errorf("读取留存文件失败: %w", err)
	}
	if !withinDir(rootAbs, realTarget) {
		return "", errors.New("只能读取当前会话留存目录内的文件")
	}

	data, err := os.ReadFile(realTarget)
	if err != nil {
		return "", fmt.Errorf("读取留存文件失败: %w", err)
	}
	if len(data) > ToolOutputReadMaxBytes {
		data = data[:ToolOutputReadMaxBytes]
		return string(data) + "\n...[留存文件较长，已截断]", nil
	}
	return string(data), nil
}

// withinDir 判断 target 是否位于 root 之内（含 root 本身）。
func withinDir(root, target string) bool {
	rel, err := filepath.Rel(root, target)
	if err != nil {
		return false
	}
	if rel == "." {
		return true
	}
	return !filepath.IsAbs(rel) && rel != ".." && !strings.HasPrefix(rel, ".."+string(os.PathSeparator))
}
