// Package tools 提供 Agent 可用的具体工具（文件类、记忆、命令技能等）的基础设施实现。
//
// 工具领域的接口与策略（Tool / ToolPolicy / Risk）在 domain/tool 定义，
// 本包只实现具体的执行逻辑，并通过基础设施层的能力（memory、filesystem）落地。
package tools

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"simpleAgent/infrastructure/audit"
)

// AccessMode 路径访问模式。写模式比读模式多一类拒绝目标（可执行文件）。
type AccessMode int

const (
	// AccessRead 读取、列目录、搜索等内容访问
	AccessRead AccessMode = iota
	// AccessWrite 新建、覆盖、修改等写访问
	AccessWrite
)

// ErrPathDenied 表示目标路径命中敏感路径拒绝清单。
//
// 错误信息只包含相对路径，不泄露真实绝对路径；调用方可用 errors.Is 判断。
var ErrPathDenied = errors.New("该路径属于受保护的敏感位置，已拒绝访问")

// PathPolicy 工作区路径拒绝策略。
//
// 它不替代越界校验，而是叠加在越界校验之后的防御纵深：
// 即使路径位于工作区内，敏感文件与目录仍然禁止 Agent 读写。
type PathPolicy struct {
	// DenyNames 精确拒绝的文件名或目录名（大小写不敏感）
	DenyNames map[string]struct{}
	// DenyPatterns 通配符拒绝规则，按文件名（不含目录）匹配（大小写不敏感）
	DenyPatterns []string
	// AllowPaths 显式例外，使用相对工作区的路径；命中即放行，默认空
	AllowPaths []string
}

// DefaultPathPolicy 返回默认拒绝清单。
//
// 清单覆盖密钥、凭据、数据库、审计/技能数据、日志与可执行文件；
// 写模式额外拒绝可执行文件，防止 Agent 把工作区变成恶意程序投放点。
func DefaultPathPolicy() PathPolicy {
	denyNames := map[string]struct{}{
		".env":            {},
		".git":            {},
		".simpleagent":    {},
		".ssh":            {},
		".aws":            {},
		"id_rsa":          {},
		"id_ed25519":      {},
		"llm_trace.jsonl": {},
	}
	return PathPolicy{
		DenyNames: denyNames,
		DenyPatterns: []string{
			".env.*",
			"credentials*",
			"secrets*",
			"*.pem",
			"*.key",
			"*.p12",
			"*.pfx",
			"*.db",
			"*.sqlite",
			"*.sqlite3",
			"*.db-wal",
			"*.db-shm",
			"*.log",
		},
	}
}

// writeDenyPatterns 只在写模式下拒绝的可执行文件，
// 防止 Agent 把工作区变成恶意程序投放点；读取可执行文件本身不构成写入风险。
var writeDenyPatterns = []string{"*.exe", "*.dll", "*.so", "*.dylib"}

// WithAllow 返回带显式例外的新策略，不修改原策略。
// paths 使用相对工作区的路径；目录例外会覆盖其下所有文件。
func (p PathPolicy) WithAllow(paths []string) PathPolicy {
	if len(paths) == 0 {
		return p
	}
	out := p
	out.AllowPaths = append([]string(nil), paths...)
	return out
}

// Workspace 限定文件类工具可访问的目录范围。
//
// 所有涉及路径的工具都必须经它解析：模型完全可以输出 ../.. 或绝对路径，
// 不做校验的话文件工具会变成一个任意文件读写通道。
type Workspace struct {
	root   string
	policy PathPolicy
	audit  *audit.Logger
}

// SetAudit 注入审计日志；nil 表示不记录。
func (w *Workspace) SetAudit(log *audit.Logger) { w.audit = log }

// NewWorkspace 使用默认拒绝策略创建工作区，root 会被解析为绝对路径与真实路径
func NewWorkspace(root string) (*Workspace, error) {
	return NewWorkspaceWithPolicy(root, DefaultPathPolicy())
}

// NewWorkspaceWithPolicy 使用指定路径策略创建工作区。
func NewWorkspaceWithPolicy(root string, policy PathPolicy) (*Workspace, error) {
	if strings.TrimSpace(root) == "" {
		return nil, errors.New("工作区根目录不能为空")
	}
	abs, err := filepath.Abs(root)
	if err != nil {
		return nil, fmt.Errorf("解析工作区根目录失败: %w", err)
	}
	// 根目录本身可能就是符号链接，先解析掉，后续比较才有意义
	if resolved, err := filepath.EvalSymlinks(abs); err == nil {
		abs = resolved
	}
	return &Workspace{root: abs, policy: policy}, nil
}

// Root 返回工作区根目录的绝对路径
func (w *Workspace) Root() string {
	return w.root
}

// Rel 返回路径相对工作区根目录的表示，用于输出给人看的短路径
func (w *Workspace) Rel(target string) string {
	rel, err := filepath.Rel(w.root, target)
	if err != nil {
		return target
	}
	return filepath.ToSlash(rel)
}

// Resolve 把模型给出的路径解析为工作区内的绝对路径（读模式）。
//
// 保留该名称作为 ResolveRead 的兼容别名，已有调用点可继续使用；
// 新增的写操作必须显式使用 ResolveWrite，以获得写模式下的额外限制。
func (w *Workspace) Resolve(path string) (string, error) {
	return w.ResolveRead(path)
}

// ResolveRead 以读模式解析路径：越界校验 + 敏感路径拒绝。
func (w *Workspace) ResolveRead(path string) (string, error) {
	return w.resolve(path, AccessRead)
}

// ResolveWrite 以写模式解析路径：越界校验 + 敏感路径拒绝（含可执行文件）。
func (w *Workspace) ResolveWrite(path string) (string, error) {
	return w.resolve(path, AccessWrite)
}

// resolve 先做工作区边界与符号链接校验，再做敏感路径拒绝判断。
//
// 顺序很重要：必须先得到真实路径并计算相对路径，拒绝清单才能识别
// a/../.env、符号链接指向工作区外等绕过形式。
func (w *Workspace) resolve(path string, mode AccessMode) (string, error) {
	trimmed := strings.TrimSpace(path)
	if trimmed == "" {
		return "", errors.New("路径不能为空")
	}

	var target string
	if filepath.IsAbs(trimmed) {
		target = filepath.Clean(trimmed)
	} else {
		target = filepath.Join(w.root, trimmed)
	}

	real, err := resolveExistingAncestor(target)
	if err != nil {
		return "", err
	}
	rel, err := filepath.Rel(w.root, real)
	if err != nil {
		return "", fmt.Errorf("无法判断路径 %s 是否在工作区内: %w", path, err)
	}
	if rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("路径 %s 超出工作区范围，已拒绝", path)
	}
	if err := w.checkDenied(rel, mode); err != nil {
		return "", err
	}
	// 显式例外命中时记录审计，便于事后追溯谁放开了敏感路径
	clean := cleanRelPath(rel)
	if w.policyAllows(clean) && w.matchesDeny(clean, mode) {
		w.audit.Log(audit.Event{Type: "path", Action: "allow_override", Detail: clean})
	}
	return real, nil
}

// checkDenied 判断相对路径是否命中拒绝清单。
//
// 错误信息只带相对路径，避免把工作区真实位置泄露给模型。
func (w *Workspace) checkDenied(rel string, mode AccessMode) error {
	if w.isDenied(rel, mode) {
		return fmt.Errorf("%w：%s", ErrPathDenied, filepath.ToSlash(rel))
	}
	return nil
}

// isDenied 返回相对路径是否被策略拒绝（供搜索工具跳过敏感文件时复用）。
func (w *Workspace) isDenied(rel string, mode AccessMode) bool {
	clean := cleanRelPath(rel)
	if clean == "" || clean == "." {
		return false
	}
	if w.policyAllows(clean) {
		return false
	}
	return w.matchesDeny(clean, mode)
}

// policyAllows 判断路径是否命中显式例外（按完整路径或目录前缀匹配）。
func (w *Workspace) policyAllows(clean string) bool {
	if clean == "" {
		return false
	}
	for _, allow := range w.policy.AllowPaths {
		allowed := cleanRelPath(allow)
		if allowed == "" {
			continue
		}
		if clean == allowed || strings.HasPrefix(clean, allowed+"/") {
			return true
		}
	}
	return false
}

// matchesDeny 判断路径是否命中拒绝清单（不考虑显式例外）。
func (w *Workspace) matchesDeny(clean string, mode AccessMode) bool {
	if clean == "" {
		return false
	}
	segments := strings.Split(clean, "/")

	// 目录段命中即拒绝：既覆盖目录本身，也覆盖目录下所有文件
	for _, seg := range segments {
		if _, ok := w.policy.DenyNames[strings.ToLower(seg)]; ok {
			return true
		}
	}

	base := strings.ToLower(segments[len(segments)-1])
	for _, pattern := range w.policy.DenyPatterns {
		if matched, err := filepath.Match(strings.ToLower(pattern), base); err == nil && matched {
			return true
		}
	}
	if mode == AccessWrite {
		for _, pattern := range writeDenyPatterns {
			if matched, err := filepath.Match(pattern, base); err == nil && matched {
				return true
			}
		}
	}
	return false
}

// cleanRelPath 把路径统一成正斜杠、去掉前导 ./ 的干净相对路径。
func cleanRelPath(rel string) string {
	clean := filepath.ToSlash(filepath.Clean(strings.TrimSpace(rel)))
	clean = strings.TrimPrefix(clean, "./")
	if clean == "." {
		return ""
	}
	return clean
}

// resolveExistingAncestor 解析路径中「已存在部分」的符号链接，再把剩余段拼回去。
//
// 目标文件可能还不存在（例如写新文件），这种情况解析到最深的已存在祖先即可。
func resolveExistingAncestor(target string) (string, error) {
	remaining := make([]string, 0, 4)
	current := target
	for {
		resolved, err := filepath.EvalSymlinks(current)
		if err == nil {
			for i := len(remaining) - 1; i >= 0; i-- {
				resolved = filepath.Join(resolved, remaining[i])
			}
			return resolved, nil
		}
		if !errors.Is(err, fs.ErrNotExist) {
			return "", fmt.Errorf("解析路径 %s 失败: %w", target, err)
		}
		parent := filepath.Dir(current)
		if parent == current {
			// 已经到根仍不存在，无法继续解析，按原路径处理
			return target, nil
		}
		remaining = append(remaining, filepath.Base(current))
		current = parent
	}
}

// writeFileAtomic 以原子方式写入文件：先写同目录临时文件，再 Rename 覆盖。
//
// 直接写目标文件的话，进程中断会留下一个被截断的半成品；
// Rename 保证读者看到的要么是旧内容、要么是完整的新内容。
func writeFileAtomic(target string, content string, perm fs.FileMode) error {
	dir := filepath.Dir(target)
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return fmt.Errorf("创建目录 %s 失败: %w", dir, err)
	}
	tmp, err := os.CreateTemp(dir, ".write-*.tmp")
	if err != nil {
		return fmt.Errorf("创建临时文件失败: %w", err)
	}
	tmpName := tmp.Name()
	// Rename 成功后临时文件已不存在，此处的 Remove 失败可安全忽略
	defer func() {
		_ = os.Remove(tmpName)
	}()

	if _, err := tmp.WriteString(content); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("写入临时文件失败: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("同步临时文件失败: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("关闭临时文件失败: %w", err)
	}
	if err := os.Chmod(tmpName, perm); err != nil {
		return fmt.Errorf("设置文件权限失败: %w", err)
	}
	if err := os.Rename(tmpName, target); err != nil {
		return fmt.Errorf("替换目标文件失败: %w", err)
	}
	return nil
}

// isBinary 粗略判断内容是否为二进制：开头出现 NUL 字节即视为二进制。
//
// 只用于给出「无法作为文本读取」的提示，不追求精确判断。
func isBinary(data []byte) bool {
	limit := min(len(data), 8192)
	for i := 0; i < limit; i++ {
		if data[i] == 0 {
			return true
		}
	}
	return false
}

// formatSize 把字节数格式化为便于阅读的形式
func formatSize(bytes int64) string {
	switch {
	case bytes >= 1024*1024:
		return fmt.Sprintf("%.1f MB", float64(bytes)/(1024*1024))
	case bytes >= 1024:
		return fmt.Sprintf("%.1f KB", float64(bytes)/1024)
	default:
		return fmt.Sprintf("%d B", bytes)
	}
}
