package tools

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"simpleAgent/domain/session"
	"simpleAgent/domain/tool"
	"simpleAgent/infrastructure/audit"
)

// 文件工具的各类上限。
// 设上限的目的不是省资源，而是避免一次工具调用就把上下文塞满：
// 工具返回会原样进入对话历史，超长的返回即使被折叠也会挤掉有用信息。
const (
	// readMaxBytes 单次读取的字节上限，超过直接拒绝而不是截断：
	// 基于不完整内容下结论比明确报错更危险
	readMaxBytes = 512 * 1024
	// readMaxLines 未指定行范围时最多返回的行数
	readMaxLines = 2000
	// writeMaxBytes 单次写入的字节上限
	writeMaxBytes = 1024 * 1024
	// listMaxEntries 列目录最多返回的条目数
	listMaxEntries = 200
	// searchMaxHits 搜索最多返回的命中数
	searchMaxHits = 50
	// searchMaxFiles 搜索最多扫描的文件数，避免在大仓库里长时间遍历
	searchMaxFiles = 5000
	// searchMaxFileBytes 搜索时单个文件的字节上限
	searchMaxFileBytes = 1024 * 1024
	// searchSnippetRunes 搜索命中行的摘要长度
	searchSnippetRunes = 200
	// writeFilePerm 新建文件的权限
	writeFilePerm = 0o644
)

// skipDirNames 搜索与遍历时跳过的目录。
// 这些目录要么是版本控制数据，要么是依赖产物，内容对理解项目没有帮助。
var skipDirNames = map[string]struct{}{
	".git":         {},
	".svn":         {},
	".hg":          {},
	".simpleagent": {},
	".idea":        {},
	".vscode":      {},
	"node_modules": {},
	"vendor":       {},
	"dist":         {},
	"build":        {},
	"__pycache__":  {},
}

// shouldSkipDir 判断遍历时是否跳过该目录
func shouldSkipDir(name string) bool {
	if _, ok := skipDirNames[strings.ToLower(name)]; ok {
		return true
	}
	// 其余隐藏目录一律跳过，避免翻进缓存与工具产物
	return strings.HasPrefix(name, ".") && name != "."
}

// ============ 工具参数解析 ============

// decodeToolArgs 把工具的「单行 JSON 对象」参数解码到 dst。
//
// 本地工具的参数统一为 JSON 对象（键为参数名），由 domain/tool 的参数规格校验后
// 才进入这里，因此本函数只做解码与提示。
// 禁止未知字段：参数名写错时立刻报错，而不是被静默忽略后以缺省值继续执行。
// 空参数按 {} 处理，供「全部参数都可选」的工具（如 list_dir）使用。
func decodeToolArgs(args string, dst any) error {
	text := strings.TrimSpace(args)
	if text == "" {
		text = "{}"
	}
	dec := json.NewDecoder(strings.NewReader(text))
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		return fmt.Errorf("参数必须是一个 JSON 对象，且键只能是该工具声明的参数名: %w", err)
	}
	return nil
}

// float64Ptr 返回指向 v 的指针，用于构造参数规格里的数值边界。
func float64Ptr(v float64) *float64 { return &v }

// ============ read_file ============

// ReadFileTool 读取工作区内的文本文件。只读无副作用，注册时自动放行。
type ReadFileTool struct {
	workspace *Workspace
}

// NewReadFileTool 创建读文件工具
func NewReadFileTool(workspace *Workspace) *ReadFileTool {
	return &ReadFileTool{workspace: workspace}
}

// Name 返回工具名
func (t *ReadFileTool) Name() string { return "read_file" }

// Description 返回工具说明
func (t *ReadFileTool) Description() string {
	return "读取工作区内的文本文件。只读，不区分大小写。" +
		"只传 path 读取全文；同时传 start_line 与 end_line 可只读片段（行号从 1 开始，两端包含）。"
}

// Spec 返回 read_file 的参数规格
func (t *ReadFileTool) Spec() tool.ToolSpec {
	return tool.ToolSpec{
		Name: "read_file",
		Params: []tool.ParamSpec{
			{Name: "path", Type: tool.TypeString, Required: true, Description: "工作区内的文件相对路径，如 agent/agent.go"},
			{Name: "start_line", Type: tool.TypeInteger, Min: float64Ptr(1), Description: "起始行（含，从 1 开始）；必须与 end_line 成对出现，省略即读取全文"},
			{Name: "end_line", Type: tool.TypeInteger, Min: float64Ptr(1), Description: "结束行（含）；必须与 start_line 成对出现"},
		},
	}
}

// Run 执行读取，参数为 JSON 对象：{"path":"...","start_line":1,"end_line":200}
func (t *ReadFileTool) Run(args string) (string, error) {
	if t.workspace == nil {
		return "", errors.New("工作区未初始化")
	}
	path, from, to, err := parseReadArgs(args)
	if err != nil {
		return "", err
	}
	target, err := t.workspace.ResolveRead(path)
	if err != nil {
		return "", err
	}

	info, err := os.Stat(target)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return "", fmt.Errorf("文件 %s 不存在", path)
		}
		return "", fmt.Errorf("读取文件 %s 失败: %w", path, err)
	}
	if info.IsDir() {
		return "", fmt.Errorf("%s 是目录，请改用 list_dir", path)
	}
	if info.Size() > readMaxBytes {
		return "", fmt.Errorf("文件 %s 有 %s，超过单次读取上限 %s；请带上 start_line 与 end_line 分段读取",
			path, formatSize(info.Size()), formatSize(readMaxBytes))
	}

	data, err := os.ReadFile(target)
	if err != nil {
		return "", fmt.Errorf("读取文件 %s 失败: %w", path, err)
	}
	if isBinary(data) {
		return "", fmt.Errorf("文件 %s 是二进制内容，无法作为文本读取", path)
	}

	lines := strings.Split(string(data), "\n")
	// 末尾的换行会产生一个空元素，去掉以免行数多算一行
	if len(lines) > 1 && lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	// 起始行整段越界时明确报错，而不是返回一段「已超出范围」的说明文本
	if from > 0 && from > len(lines) {
		return "", fmt.Errorf("起始行 %d 超出范围（%s 共 %d 行）", from, path, len(lines))
	}
	return renderFileContent(path, lines, from, to), nil
}

// readFileArgs read_file 的 JSON 参数
type readFileArgs struct {
	Path      string `json:"path"`
	StartLine int    `json:"start_line"`
	EndLine   int    `json:"end_line"`
}

// parseReadArgs 解析 read_file 的 JSON 参数，返回路径与行范围（未指定行范围时 from/to 为 0）
func parseReadArgs(args string) (path string, from, to int, err error) {
	var parsed readFileArgs
	if err := decodeToolArgs(args, &parsed); err != nil {
		return "", 0, 0, err
	}
	path = strings.TrimSpace(parsed.Path)
	if path == "" {
		return "", 0, 0, errors.New(`参数 path 不能为空，例如：{"path":"agent/agent.go"}`)
	}
	from, to = parsed.StartLine, parsed.EndLine
	if (from == 0) != (to == 0) {
		return "", 0, 0, errors.New("start_line 与 end_line 必须同时提供，或同时省略以读取全文")
	}
	if from > 0 && to < from {
		return "", 0, 0, fmt.Errorf("行范围不合法: %d,%d（start_line 不得大于 end_line）", from, to)
	}
	return path, from, to, nil
}

// renderFileContent 渲染文件内容。
//
// 整篇读取时不加任何装饰，保持与 write_file 的参数格式对称——
// 加了行号的话模型可能把行号一起写回去。
// 只有返回片段时才加说明，让模型知道自己看到的不完整。
func renderFileContent(path string, lines []string, from, to int) string {
	total := len(lines)
	if total == 0 {
		return fmt.Sprintf("[%s 是空文件]", path)
	}

	start, end := 1, total
	if from > 0 {
		start = from
		end = min(to, total)
	} else if total > readMaxLines {
		end = readMaxLines
	}

	content := strings.Join(lines[start-1:end], "\n")
	if start == 1 && end == total {
		return content
	}
	return fmt.Sprintf("[%s 共 %d 行，以下为第 %d-%d 行]\n%s", path, total, start, end, content)
}

// ============ write_file ============

// WriteFileTool 把内容写入工作区内的文件。属于写本地操作，注册时需人工确认。
type WriteFileTool struct {
	workspace *Workspace
	audit     *audit.Logger
}

// NewWriteFileTool 创建写文件工具
func NewWriteFileTool(workspace *Workspace) *WriteFileTool {
	return &WriteFileTool{workspace: workspace}
}

// SetAudit 注入审计日志；nil 表示不记录。
func (t *WriteFileTool) SetAudit(log *audit.Logger) { t.audit = log }

// Name 返回工具名
func (t *WriteFileTool) Name() string { return "write_file" }

// Description 返回工具说明
func (t *WriteFileTool) Description() string {
	return "把内容写入工作区内的文件，父目录不存在时自动创建；已存在的文件会被整体覆盖。" +
		`content 中的换行用 JSON 的 \n 表示（整个参数必须写在一行内）。`
}

// Spec 返回 write_file 的参数规格
func (t *WriteFileTool) Spec() tool.ToolSpec {
	return tool.ToolSpec{
		Name: "write_file",
		Params: []tool.ParamSpec{
			{Name: "path", Type: tool.TypeString, Required: true, Description: "工作区内要写入的文件相对路径，如 notes/plan.md"},
			{Name: "content", Type: tool.TypeString, Required: true, Description: `要写入的完整文件内容；换行写成 \n，整个参数保持单行`},
		},
	}
}

// Run 执行写入，参数为 JSON 对象：{"path":"notes/plan.md","content":"第一步\n第二步"}
func (t *WriteFileTool) Run(args string) (string, error) {
	if t.workspace == nil {
		return "", errors.New("工作区未初始化")
	}
	path, content, err := splitPathContent(args)
	if err != nil {
		return "", err
	}
	target, err := t.workspace.ResolveWrite(path)
	if err != nil {
		t.audit.Log(audit.Event{Type: "file_write", Action: "deny", Detail: path})
		return "", err
	}
	if len(content) > writeMaxBytes {
		t.audit.Log(audit.Event{Type: "file_write", Action: "deny", Detail: path + "（超过大小上限）"})
		return "", fmt.Errorf("待写入内容有 %s，超过单次写入上限 %s",
			formatSize(int64(len(content))), formatSize(writeMaxBytes))
	}

	previousSize, previousLines, overwritten, err := statExisting(path, target)
	if err != nil {
		return "", err
	}
	if err := writeFileAtomic(target, content, writeFilePerm); err != nil {
		t.audit.Log(audit.Event{Type: "file_write", Action: "error", Detail: path})
		return "", fmt.Errorf("写入文件 %s 失败: %w", path, err)
	}
	t.audit.Log(audit.Event{Type: "file_write", Action: "allow", Detail: path})

	newLines := countLines(content)
	if overwritten {
		return fmt.Sprintf("已覆盖 %s：%d 行/%s → %d 行/%s",
			path, previousLines, formatSize(previousSize), newLines, formatSize(int64(len(content)))), nil
	}
	return fmt.Sprintf("已新建 %s：%d 行/%s", path, newLines, formatSize(int64(len(content)))), nil
}

// Describe 让人工确认时看到具体路径与影响范围。
// 默认的「即将调用工具 write_file」无法让人判断该不该放行。
func (t *WriteFileTool) Describe(args string) string {
	path, content, err := splitPathContent(args)
	if err != nil {
		return ""
	}
	target, resolveErr := t.workspace.ResolveWrite(path)
	if resolveErr != nil {
		if errors.Is(resolveErr, ErrPathDenied) {
			return fmt.Sprintf("试图写入受保护的敏感路径 %s，已被拒绝", path)
		}
		return fmt.Sprintf("试图写入工作区之外的路径 %s，已被拒绝", path)
	}

	previousSize, previousLines, overwritten, err := statExisting(path, target)
	if err != nil {
		return ""
	}
	if overwritten {
		return fmt.Sprintf("覆盖已有文件 %s（当前 %d 行/%s，将替换为 %d 行/%s）：\n%s",
			path, previousLines, formatSize(previousSize), countLines(content),
			formatSize(int64(len(content))), session.Preview(content, searchSnippetRunes))
	}
	return fmt.Sprintf("新建文件 %s（%d 行/%s）：\n%s",
		path, countLines(content), formatSize(int64(len(content))), session.Preview(content, searchSnippetRunes))
}

// writeFileArgs write_file 的 JSON 参数
type writeFileArgs struct {
	Path    string `json:"path"`
	Content string `json:"content"`
}

// splitPathContent 解析 write_file 的 JSON 参数。
// content 原样返回（含换行），不做 TrimSpace，避免悄悄改动要写入的内容。
func splitPathContent(args string) (path, content string, err error) {
	var parsed writeFileArgs
	if err := decodeToolArgs(args, &parsed); err != nil {
		return "", "", err
	}
	path = strings.TrimSpace(parsed.Path)
	if path == "" {
		return "", "", errors.New(`参数 path 不能为空，例如：{"path":"notes/plan.md","content":"第一步"}`)
	}
	if strings.TrimSpace(parsed.Content) == "" {
		return "", "", errors.New("参数 content 不能为空")
	}
	return path, parsed.Content, nil
}

// statExisting 返回目标文件的现状；不存在时 overwritten 为 false
func statExisting(path, target string) (size int64, lines int, overwritten bool, err error) {
	info, statErr := os.Stat(target)
	if statErr != nil {
		if errors.Is(statErr, fs.ErrNotExist) {
			return 0, 0, false, nil
		}
		return 0, 0, false, fmt.Errorf("检查目标文件 %s 失败: %w", path, statErr)
	}
	if info.IsDir() {
		return 0, 0, false, fmt.Errorf("%s 是目录，不能作为写入目标", path)
	}
	existing, readErr := os.ReadFile(target)
	if readErr != nil {
		return 0, 0, false, fmt.Errorf("读取目标文件 %s 失败: %w", path, readErr)
	}
	return info.Size(), countLines(string(existing)), true, nil
}

// countLines 统计文本行数。
// 末尾换行不额外算作一行：「a\nb\n」是 2 行而不是 3 行。
func countLines(text string) int {
	if text == "" {
		return 0
	}
	return strings.Count(strings.TrimSuffix(text, "\n"), "\n") + 1
}

// ============ list_dir ============

// ListDirTool 列出工作区内某个目录的条目。只读无副作用，注册时自动放行。
type ListDirTool struct {
	workspace *Workspace
}

// NewListDirTool 创建列目录工具
func NewListDirTool(workspace *Workspace) *ListDirTool {
	return &ListDirTool{workspace: workspace}
}

// Name 返回工具名
func (t *ListDirTool) Name() string { return "list_dir" }

// Description 返回工具说明
func (t *ListDirTool) Description() string {
	return "列出工作区内某个目录的文件与子目录。path 可省略，省略时列出工作区根目录。"
}

// Spec 返回 list_dir 的参数规格
func (t *ListDirTool) Spec() tool.ToolSpec {
	return tool.ToolSpec{
		Name: "list_dir",
		Params: []tool.ParamSpec{
			{Name: "path", Type: tool.TypeString, Description: "工作区内的目录相对路径，如 agent；省略或留空表示工作区根目录"},
		},
	}
}

// listDirArgs list_dir 的 JSON 参数
type listDirArgs struct {
	Path string `json:"path"`
}

// Run 执行列目录，参数为 JSON 对象：{"path":"agent"}；path 省略或为空表示工作区根目录
func (t *ListDirTool) Run(args string) (string, error) {
	if t.workspace == nil {
		return "", errors.New("工作区未初始化")
	}
	var parsed listDirArgs
	if err := decodeToolArgs(args, &parsed); err != nil {
		return "", err
	}
	path := strings.TrimSpace(parsed.Path)
	if path == "" {
		path = "."
	}
	target, err := t.workspace.ResolveRead(path)
	if err != nil {
		return "", err
	}

	entries, err := os.ReadDir(target)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return "", fmt.Errorf("目录 %s 不存在", path)
		}
		return "", fmt.Errorf("读取目录 %s 失败: %w", path, err)
	}

	type item struct {
		name  string
		isDir bool
		size  string
	}
	items := make([]item, 0, len(entries))
	for _, entry := range entries {
		it := item{name: entry.Name(), isDir: entry.IsDir()}
		if entry.IsDir() {
			it.size = "-"
		} else if info, err := entry.Info(); err == nil {
			it.size = formatSize(info.Size())
		} else {
			it.size = "?"
		}
		items = append(items, it)
	}
	// 目录排在前面，同类按名称排序，便于人快速定位
	sort.SliceStable(items, func(i, j int) bool {
		if items[i].isDir != items[j].isDir {
			return items[i].isDir
		}
		return items[i].name < items[j].name
	})

	display := t.workspace.Rel(target)
	if display == "." {
		display = "工作区根目录"
	}
	if len(items) == 0 {
		return fmt.Sprintf("%s 是空目录", display), nil
	}

	truncated := len(items) > listMaxEntries
	if truncated {
		items = items[:listMaxEntries]
	}

	var sb strings.Builder
	fmt.Fprintf(&sb, "%s（共 %d 项）：\n", display, len(entries))
	for _, it := range items {
		name := it.name
		if it.isDir {
			name += "/"
		}
		fmt.Fprintf(&sb, "  %-40s %s\n", name, it.size)
	}
	if truncated {
		fmt.Fprintf(&sb, "… 仅显示前 %d 项\n", listMaxEntries)
	}
	return strings.TrimRight(sb.String(), "\n"), nil
}

// ============ search_files ============

// SearchFilesTool 在工作区内按关键词搜索文件内容。只读无副作用，注册时自动放行。
type SearchFilesTool struct {
	workspace *Workspace
}

// NewSearchFilesTool 创建搜索工具
func NewSearchFilesTool(workspace *Workspace) *SearchFilesTool {
	return &SearchFilesTool{workspace: workspace}
}

// Name 返回工具名
func (t *SearchFilesTool) Name() string { return "search_files" }

// Description 返回工具说明
func (t *SearchFilesTool) Description() string {
	return "在工作区内按关键词搜索文件内容，不区分大小写，返回命中的文件路径与行号。"
}

// Spec 返回 search_files 的参数规格
func (t *SearchFilesTool) Spec() tool.ToolSpec {
	return tool.ToolSpec{
		Name: "search_files",
		Params: []tool.ParamSpec{
			{Name: "keyword", Type: tool.TypeString, Required: true, Description: "要搜索的关键词，如 prepareContext"},
		},
	}
}

// searchFilesArgs search_files 的 JSON 参数
type searchFilesArgs struct {
	Keyword string `json:"keyword"`
}

// Run 执行搜索，参数为 JSON 对象：{"keyword":"prepareContext"}
func (t *SearchFilesTool) Run(args string) (string, error) {
	if t.workspace == nil {
		return "", errors.New("工作区未初始化")
	}
	var parsed searchFilesArgs
	if err := decodeToolArgs(args, &parsed); err != nil {
		return "", err
	}
	keyword := strings.TrimSpace(parsed.Keyword)
	if keyword == "" {
		return "", errors.New(`参数 keyword 不能为空，例如：{"keyword":"prepareContext"}`)
	}
	needle := strings.ToLower(keyword)

	hits := make([]string, 0, searchMaxHits)
	scanned, skippedBinary := 0, 0
	walkErr := filepath.WalkDir(t.workspace.Root(), func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			// 单个条目读不了不应中断整次搜索，跳过继续
			return nil
		}
		if entry.IsDir() {
			if path != t.workspace.Root() && (shouldSkipDir(entry.Name()) || t.workspace.isDenied(entry.Name(), AccessRead)) {
				return fs.SkipDir
			}
			return nil
		}
		if len(hits) >= searchMaxHits || scanned >= searchMaxFiles {
			return fs.SkipAll
		}
		display := t.workspace.Rel(path)
		// 敏感文件不参与搜索，避免命中结果间接泄露密钥或数据库片段
		if t.workspace.isDenied(display, AccessRead) {
			return nil
		}
		info, infoErr := entry.Info()
		if infoErr != nil || info.Size() > searchMaxFileBytes {
			return nil
		}
		scanned++

		data, readErr := os.ReadFile(path)
		if readErr != nil || isBinary(data) {
			if readErr == nil {
				skippedBinary++
			}
			return nil
		}
		for i, line := range strings.Split(string(data), "\n") {
			if !strings.Contains(strings.ToLower(line), needle) {
				continue
			}
			hits = append(hits, fmt.Sprintf("%s:%d: %s", display, i+1, session.Preview(line, searchSnippetRunes)))
			if len(hits) >= searchMaxHits {
				break
			}
		}
		return nil
	})
	if walkErr != nil {
		return "", fmt.Errorf("遍历工作区失败: %w", walkErr)
	}

	if len(hits) == 0 {
		return fmt.Sprintf("没有找到包含 %q 的内容（已扫描 %d 个文件）", keyword, scanned), nil
	}
	var sb strings.Builder
	fmt.Fprintf(&sb, "关键词 %q 命中 %d 处（已扫描 %d 个文件）", keyword, len(hits), scanned)
	if skippedBinary > 0 {
		fmt.Fprintf(&sb, "，跳过 %d 个二进制文件", skippedBinary)
	}
	sb.WriteString("：\n")
	for _, hit := range hits {
		sb.WriteString("  " + hit + "\n")
	}
	return strings.TrimRight(sb.String(), "\n"), nil
}
