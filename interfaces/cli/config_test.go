package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// configEnvNames LoadConfig 会读取的全部环境变量。
// 每次用例前统一清空，避免开发机上的真实配置影响测试结果。
var configEnvNames = []string{
	"SIMPLEAGENT_ADDR",
	"SIMPLEAGENT_AUTH_TOKEN",
	"SIMPLEAGENT_ALLOWED_ORIGINS",
	"SIMPLEAGENT_ALLOW_NO_AUTH",
	"SIMPLEAGENT_HEADLESS",
	"SIMPLEAGENT_ALLOW_SKILL_ADMIN",
	"SIMPLEAGENT_ALLOW_SKILL_EXEC",
	"SIMPLEAGENT_WORKSPACE_ROOT",
	"SIMPLEAGENT_PATH_ALLOW",
	"SIMPLEAGENT_REQUEST_MAX_BYTES",
	"SIMPLEAGENT_SESSION_TTL",
	"SIMPLEAGENT_APPROVAL_TIMEOUT",
}

// clearConfigEnv 清空配置相关环境变量，保证用例之间互不影响。
func clearConfigEnv(t *testing.T) {
	t.Helper()
	for _, name := range configEnvNames {
		t.Setenv(name, "")
	}
}

// TestLoadConfig 表驱动覆盖监听地址、Token、运行模式、工作区与数值配置的校验组合。
func TestLoadConfig(t *testing.T) {
	validWorkspace := t.TempDir()
	longToken := strings.Repeat("a", 32)
	missingDir := filepath.Join(t.TempDir(), "missing")

	cases := []struct {
		name    string
		env     map[string]string
		args    []string
		wantErr string // 非空表示期望报错，值为期望包含的子串
		check   func(t *testing.T, cfg *Config)
	}{
		{
			name: "默认 loopback 自动生成 Token",
			check: func(t *testing.T, cfg *Config) {
				if cfg.Addr != defaultAddr {
					t.Fatalf("默认地址应为 %s，实际 %s", defaultAddr, cfg.Addr)
				}
				if !cfg.Loopback {
					t.Fatal("默认地址应判定为 loopback")
				}
				if !cfg.GeneratedToken || len(cfg.AuthToken) != 64 {
					t.Fatalf("应生成 64 字符 Token，实际 generated=%v len=%d", cfg.GeneratedToken, len(cfg.AuthToken))
				}
				if cfg.RequestMaxBytes != defaultRequestMaxBytes {
					t.Fatalf("请求体上限默认值错误: %d", cfg.RequestMaxBytes)
				}
				if cfg.SessionTTL != defaultSessionTTL || cfg.ApprovalTimeout != defaultApprovalTimeout {
					t.Fatalf("时长默认值错误: ttl=%v approval=%v", cfg.SessionTTL, cfg.ApprovalTimeout)
				}
			},
		},
		{
			name: "显式 Token 不再生成",
			env:  map[string]string{"SIMPLEAGENT_AUTH_TOKEN": longToken},
			check: func(t *testing.T, cfg *Config) {
				if cfg.GeneratedToken {
					t.Fatal("显式配置 Token 时不应标记为生成")
				}
				if cfg.AuthToken != longToken {
					t.Fatalf("Token 应原样保留，实际 %q", cfg.AuthToken)
				}
			},
		},
		{
			name:    "非 loopback 无 Token 拒绝启动",
			env:     map[string]string{"SIMPLEAGENT_ADDR": "0.0.0.0:8080"},
			wantErr: "SIMPLEAGENT_AUTH_TOKEN",
		},
		{
			name: "非 loopback 带 Token 允许",
			env: map[string]string{
				"SIMPLEAGENT_ADDR":       "0.0.0.0:8080",
				"SIMPLEAGENT_AUTH_TOKEN": longToken,
			},
			check: func(t *testing.T, cfg *Config) {
				if cfg.Loopback {
					t.Fatal("0.0.0.0 不应判定为 loopback")
				}
			},
		},
		{
			name: "localhost 视为 loopback",
			env:  map[string]string{"SIMPLEAGENT_ADDR": "localhost:9090"},
			check: func(t *testing.T, cfg *Config) {
				if !cfg.Loopback {
					t.Fatal("localhost 应判定为 loopback")
				}
			},
		},
		{
			name: "免认证仅 loopback 可用",
			env:  map[string]string{"SIMPLEAGENT_ALLOW_NO_AUTH": "1"},
			check: func(t *testing.T, cfg *Config) {
				if !cfg.AllowNoAuth || cfg.GeneratedToken || cfg.AuthToken != "" {
					t.Fatalf("免认证模式不应生成 Token，实际 %+v", cfg)
				}
			},
		},
		{
			name: "非 loopback 免认证拒绝",
			env: map[string]string{
				"SIMPLEAGENT_ADDR":          "0.0.0.0:8080",
				"SIMPLEAGENT_ALLOW_NO_AUTH": "1",
				"SIMPLEAGENT_AUTH_TOKEN":    longToken,
			},
			wantErr: "SIMPLEAGENT_ALLOW_NO_AUTH",
		},
		{
			name: "非 loopback headless 拒绝",
			env: map[string]string{
				"SIMPLEAGENT_ADDR":       "0.0.0.0:8080",
				"SIMPLEAGENT_AUTH_TOKEN": longToken,
				"SIMPLEAGENT_HEADLESS":   "1",
			},
			wantErr: "SIMPLEAGENT_HEADLESS",
		},
		{
			name: "loopback headless 允许",
			env:  map[string]string{"SIMPLEAGENT_HEADLESS": "1"},
			check: func(t *testing.T, cfg *Config) {
				if !cfg.Headless {
					t.Fatal("headless 开关未生效")
				}
			},
		},
		{
			name:    "过短 Token 拒绝",
			env:     map[string]string{"SIMPLEAGENT_AUTH_TOKEN": "short"},
			wantErr: "至少需要",
		},
		{
			name:    "非法地址拒绝",
			env:     map[string]string{"SIMPLEAGENT_ADDR": "not-an-address"},
			wantErr: "不合法",
		},
		{
			name:    "工作区不存在拒绝",
			env:     map[string]string{"SIMPLEAGENT_WORKSPACE_ROOT": missingDir},
			wantErr: "工作区目录",
		},
		{
			name: "工作区存在允许",
			env:  map[string]string{"SIMPLEAGENT_WORKSPACE_ROOT": validWorkspace},
			check: func(t *testing.T, cfg *Config) {
				want, err := filepath.Abs(validWorkspace)
				if err != nil {
					t.Fatalf("解析期望路径失败: %v", err)
				}
				if cfg.WorkspaceRoot != want {
					t.Fatalf("工作区应为绝对路径 %s，实际 %s", want, cfg.WorkspaceRoot)
				}
			},
		},
		{
			name:    "非法请求体上限拒绝",
			env:     map[string]string{"SIMPLEAGENT_REQUEST_MAX_BYTES": "abc"},
			wantErr: "SIMPLEAGENT_REQUEST_MAX_BYTES",
		},
		{
			name:    "非法会话时长拒绝",
			env:     map[string]string{"SIMPLEAGENT_SESSION_TTL": "one-day"},
			wantErr: "SIMPLEAGENT_SESSION_TTL",
		},
		{
			name: "位置参数覆盖监听地址",
			args: []string{"127.0.0.1:9999"},
			check: func(t *testing.T, cfg *Config) {
				if cfg.Addr != "127.0.0.1:9999" {
					t.Fatalf("位置参数应覆盖地址，实际 %s", cfg.Addr)
				}
			},
		},
		{
			name: "自定义时长与体积",
			env: map[string]string{
				"SIMPLEAGENT_SESSION_TTL":       "2h",
				"SIMPLEAGENT_APPROVAL_TIMEOUT":  "90s",
				"SIMPLEAGENT_REQUEST_MAX_BYTES": "2048",
			},
			check: func(t *testing.T, cfg *Config) {
				if cfg.SessionTTL != 2*time.Hour || cfg.ApprovalTimeout != 90*time.Second || cfg.RequestMaxBytes != 2048 {
					t.Fatalf("自定义配置未生效: %+v", cfg)
				}
			},
		},
		{
			name: "路径例外按逗号拆分",
			env:  map[string]string{"SIMPLEAGENT_PATH_ALLOW": " config/app.db , , docs/notes.md "},
			check: func(t *testing.T, cfg *Config) {
				want := []string{"config/app.db", "docs/notes.md"}
				if len(cfg.PathAllow) != len(want) {
					t.Fatalf("路径例外数量错误: %v", cfg.PathAllow)
				}
				for i := range want {
					if cfg.PathAllow[i] != want[i] {
						t.Fatalf("路径例外应为 %v，实际 %v", want, cfg.PathAllow)
					}
				}
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			clearConfigEnv(t)
			for k, v := range tc.env {
				t.Setenv(k, v)
			}

			cfg, err := LoadConfig(tc.args)
			if tc.wantErr != "" {
				if err == nil {
					t.Fatalf("期望报错（包含 %q），实际成功: %+v", tc.wantErr, cfg)
				}
				if !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("错误信息应包含 %q，实际: %v", tc.wantErr, err)
				}
				return
			}
			if err != nil {
				t.Fatalf("不应报错: %v", err)
			}
			if tc.check != nil {
				tc.check(t, cfg)
			}
		})
	}
}

// TestResolveWorkspaceRootDefaultsToCwd 验证未配置工作区时回退到进程当前目录。
func TestResolveWorkspaceRootDefaultsToCwd(t *testing.T) {
	got, err := resolveWorkspaceRoot("")
	if err != nil {
		t.Fatalf("解析默认工作区失败: %v", err)
	}
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatalf("获取当前目录失败: %v", err)
	}
	if got != cwd {
		t.Fatalf("默认工作区应为当前目录 %s，实际 %s", cwd, got)
	}
}
