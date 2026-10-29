package main

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// 安全相关默认值。默认只监听本机回环，避免把未鉴权服务暴露到局域网。
const (
	defaultAddr            = "127.0.0.1:8080"
	defaultRequestMaxBytes = 1 << 20 // 1MB，文件上传接口后续单独放宽
	defaultSessionTTL      = 24 * time.Hour
	defaultApprovalTimeout = 2 * time.Minute
	// minAuthTokenLen 显式配置的 Token 最短长度（32 字节随机数的 hex 长度为 64）
	minAuthTokenLen = 32
)

// Config 汇总 HTTP 服务的运行模式与安全开关。
//
// 所有字段都从环境变量或 --serve 位置参数解析而来，服务层不再自行读取环境变量，
// 便于测试构造不同安全组合，也避免安全默认值散落在多个包。
type Config struct {
	Addr            string        // 监听地址，形如 host:port
	Loopback        bool          // 监听地址是否为本机回环（由 Validate 填充）
	AuthToken       string        // 访问 Token；AllowNoAuth 为 true 时可为空
	GeneratedToken  bool          // Token 是否为启动时生成（仅用于打印一次）
	AllowedOrigins  []string      // Origin 白名单
	AllowNoAuth     bool          // 本地免认证调试开关，仅 loopback 生效
	Headless        bool          // 无人值守模式
	AllowSkillAdmin bool          // 是否允许登录用户管理技能
	AllowSkillExec  bool          // Headless 下是否允许执行命令技能
	WorkspaceRoot   string        // Agent 工作区根目录的绝对路径
	PathAllow       []string      // 敏感路径拒绝清单的显式例外
	RequestMaxBytes int64         // 请求体上限
	SessionTTL      time.Duration // 登录会话有效期
	ApprovalTimeout time.Duration // 人工审批超时，超时按拒绝处理
}

// LoadConfig 读取环境变量与 --serve 的位置参数并完成校验。
//
// args 是 --serve 之后的参数，args[0] 若存在则覆盖 SIMPLEAGENT_ADDR。
// 返回的 Config 中 Token 为空且未开启免认证时，会自动生成一次性随机 Token。
func LoadConfig(args []string) (*Config, error) {
	cfg := &Config{
		Addr:            strings.TrimSpace(os.Getenv("SIMPLEAGENT_ADDR")),
		AuthToken:       strings.TrimSpace(os.Getenv("SIMPLEAGENT_AUTH_TOKEN")),
		AllowedOrigins:  splitList(os.Getenv("SIMPLEAGENT_ALLOWED_ORIGINS")),
		AllowNoAuth:     envBool("SIMPLEAGENT_ALLOW_NO_AUTH"),
		Headless:        envBool("SIMPLEAGENT_HEADLESS"),
		AllowSkillAdmin: envBool("SIMPLEAGENT_ALLOW_SKILL_ADMIN"),
		AllowSkillExec:  envBool("SIMPLEAGENT_ALLOW_SKILL_EXEC"),
		WorkspaceRoot:   strings.TrimSpace(os.Getenv("SIMPLEAGENT_WORKSPACE_ROOT")),
		PathAllow:       splitList(os.Getenv("SIMPLEAGENT_PATH_ALLOW")),
	}
	if cfg.Addr == "" {
		cfg.Addr = defaultAddr
	}
	// 保持 `--serve [addr]` 兼容，显式位置参数优先于环境变量
	if len(args) > 0 && strings.TrimSpace(args[0]) != "" {
		cfg.Addr = strings.TrimSpace(args[0])
	}

	var err error
	if cfg.RequestMaxBytes, err = envInt64("SIMPLEAGENT_REQUEST_MAX_BYTES", defaultRequestMaxBytes); err != nil {
		return nil, err
	}
	if cfg.SessionTTL, err = envDuration("SIMPLEAGENT_SESSION_TTL", defaultSessionTTL); err != nil {
		return nil, err
	}
	if cfg.ApprovalTimeout, err = envDuration("SIMPLEAGENT_APPROVAL_TIMEOUT", defaultApprovalTimeout); err != nil {
		return nil, err
	}
	if cfg.WorkspaceRoot, err = resolveWorkspaceRoot(cfg.WorkspaceRoot); err != nil {
		return nil, err
	}

	if err := cfg.Validate(); err != nil {
		return nil, err
	}

	// 回环地址允许未配置 Token，此时生成一次性凭据；
	// 非回环地址已在 Validate 中拒绝无 Token 启动，不会走到这里。
	if cfg.AuthToken == "" && !cfg.AllowNoAuth {
		token, err := generateAuthToken()
		if err != nil {
			return nil, fmt.Errorf("生成访问 Token 失败: %w", err)
		}
		cfg.AuthToken = token
		cfg.GeneratedToken = true
	}
	return cfg, nil
}

// Validate 校验运行模式组合，阻止危险配置静默启动。
//
// 硬约束：
//   - 非 loopback 地址必须配置 Token；
//   - 免认证与 headless 只能在 loopback 使用；
//   - 显式 Token 不得短于 minAuthTokenLen；
//   - 工作区目录必须存在。
func (c *Config) Validate() error {
	host, _, err := net.SplitHostPort(c.Addr)
	if err != nil {
		return fmt.Errorf("监听地址 %q 不合法，应为 host:port: %w", c.Addr, err)
	}
	c.Loopback = isLoopbackHost(host)

	if c.AuthToken != "" && len(c.AuthToken) < minAuthTokenLen {
		return fmt.Errorf("SIMPLEAGENT_AUTH_TOKEN 至少需要 %d 个字符", minAuthTokenLen)
	}
	if !c.Loopback && c.AuthToken == "" {
		return fmt.Errorf("监听非本机地址 %s 时必须设置 SIMPLEAGENT_AUTH_TOKEN", c.Addr)
	}
	if c.AllowNoAuth && !c.Loopback {
		return errors.New("SIMPLEAGENT_ALLOW_NO_AUTH 只允许在 loopback 地址使用")
	}
	if c.Headless && !c.Loopback {
		return errors.New("SIMPLEAGENT_HEADLESS 只允许在 loopback 地址使用")
	}
	if c.RequestMaxBytes <= 0 {
		return errors.New("SIMPLEAGENT_REQUEST_MAX_BYTES 必须大于 0")
	}
	if c.SessionTTL <= 0 {
		return errors.New("SIMPLEAGENT_SESSION_TTL 必须大于 0")
	}
	if c.ApprovalTimeout <= 0 {
		return errors.New("SIMPLEAGENT_APPROVAL_TIMEOUT 必须大于 0")
	}
	info, err := os.Stat(c.WorkspaceRoot)
	if err != nil {
		return fmt.Errorf("工作区目录 %s 不可用: %w", c.WorkspaceRoot, err)
	}
	if !info.IsDir() {
		return fmt.Errorf("工作区路径 %s 不是目录", c.WorkspaceRoot)
	}
	return nil
}

// isLoopbackHost 判断监听主机是否为本机回环。
//
// 空主机表示绑定所有网卡，必须按非 loopback 处理；
// 无法识别的域名也按非 loopback 处理，宁可要求 Token 也不放宽默认值。
func isLoopbackHost(host string) bool {
	host = strings.TrimSpace(host)
	if host == "" {
		return false
	}
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// resolveWorkspaceRoot 把工作区配置解析为绝对路径；为空时使用进程当前目录。
func resolveWorkspaceRoot(raw string) (string, error) {
	if raw == "" {
		cwd, err := os.Getwd()
		if err != nil {
			return "", fmt.Errorf("获取工作目录失败: %w", err)
		}
		return cwd, nil
	}
	abs, err := filepath.Abs(raw)
	if err != nil {
		return "", fmt.Errorf("解析工作区目录 %s 失败: %w", raw, err)
	}
	return abs, nil
}

// generateAuthToken 生成 32 字节随机 Token（hex 编码后 64 字符）。
func generateAuthToken() (string, error) {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return hex.EncodeToString(buf), nil
}

// splitList 把逗号分隔的配置拆成去空白、去空的字符串切片。
func splitList(raw string) []string {
	parts := strings.Split(raw, ",")
	out := make([]string, 0, len(parts))
	for _, part := range parts {
		part = strings.TrimSpace(part)
		if part != "" {
			out = append(out, part)
		}
	}
	return out
}

// envBool 读取布尔型环境变量，接受 1/true/yes/on（大小写不敏感）。
func envBool(name string) bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv(name))) {
	case "1", "true", "yes", "on":
		return true
	default:
		return false
	}
}

// envInt64 读取整数型环境变量，未设置时返回默认值。
func envInt64(name string, def int64) (int64, error) {
	raw := strings.TrimSpace(os.Getenv(name))
	if raw == "" {
		return def, nil
	}
	v, err := strconv.ParseInt(raw, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("%s 不是合法整数: %w", name, err)
	}
	return v, nil
}

// envDuration 读取时长型环境变量（如 24h、2m），未设置时返回默认值。
func envDuration(name string, def time.Duration) (time.Duration, error) {
	raw := strings.TrimSpace(os.Getenv(name))
	if raw == "" {
		return def, nil
	}
	v, err := time.ParseDuration(raw)
	if err != nil {
		return 0, fmt.Errorf("%s 不是合法时长（示例：24h、2m）: %w", name, err)
	}
	return v, nil
}
