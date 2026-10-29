package tools

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"simpleAgent/infrastructure/audit"
)

// newPolicyWorkspace 创建带默认拒绝策略的临时工作区。
func newPolicyWorkspace(t *testing.T, allow []string) *Workspace {
	t.Helper()
	ws, err := NewWorkspaceWithPolicy(t.TempDir(), DefaultPathPolicy().WithAllow(allow))
	if err != nil {
		t.Fatalf("创建工作区失败: %v", err)
	}
	return ws
}

// TestWorkspacePolicyModes 表驱动覆盖读写模式下的拒绝清单与显式例外。
func TestWorkspacePolicyModes(t *testing.T) {
	cases := []struct {
		name       string
		mode       AccessMode
		path       string
		allow      []string
		wantErr    bool
		wantDenied bool
	}{
		{name: "读取 .env 拒绝", mode: AccessRead, path: ".env", wantErr: true, wantDenied: true},
		{name: "读取 .ENV 大小写变体拒绝", mode: AccessRead, path: ".ENV", wantErr: true, wantDenied: true},
		{name: "读取 .git 配置拒绝", mode: AccessRead, path: ".git/config", wantErr: true, wantDenied: true},
		{name: "读取审计目录拒绝", mode: AccessRead, path: ".simpleagent/skills.json", wantErr: true, wantDenied: true},
		{name: "读取数据库拒绝", mode: AccessRead, path: "app.db", wantErr: true, wantDenied: true},
		{name: "读取 sqlite 拒绝", mode: AccessRead, path: "data/cache.sqlite", wantErr: true, wantDenied: true},
		{name: "读取 wal 文件拒绝", mode: AccessRead, path: "sessions.db-wal", wantErr: true, wantDenied: true},
		{name: "读取私钥拒绝", mode: AccessRead, path: "id_rsa", wantErr: true, wantDenied: true},
		{name: "读取 pem 证书拒绝", mode: AccessRead, path: "certs/server.pem", wantErr: true, wantDenied: true},
		{name: "读取凭据通配拒绝", mode: AccessRead, path: "credentials-prod.json", wantErr: true, wantDenied: true},
		{name: "读取日志拒绝", mode: AccessRead, path: "logs/app.log", wantErr: true, wantDenied: true},
		{name: "路径穿越拒绝", mode: AccessRead, path: "a/../../../.env", wantErr: true},
		{name: "普通文件允许", mode: AccessRead, path: "notes.md"},
		{name: "普通子目录文件允许", mode: AccessRead, path: "docs/plan.md"},
		{name: "读取可执行文件允许", mode: AccessRead, path: "tool.exe"},
		{name: "写入 .env 拒绝", mode: AccessWrite, path: ".env", wantErr: true, wantDenied: true},
		{name: "写入可执行文件拒绝", mode: AccessWrite, path: "app.exe", wantErr: true, wantDenied: true},
		{name: "写入动态库拒绝", mode: AccessWrite, path: "bin/lib.dll", wantErr: true, wantDenied: true},
		{name: "写入普通产物允许", mode: AccessWrite, path: "output/report.xlsx"},
		{name: "显式例外允许", mode: AccessRead, path: "config/app.db", allow: []string{"config/app.db"}},
		{name: "显式目录例外允许其下文件", mode: AccessRead, path: "config/app.db", allow: []string{"config"}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ws := newPolicyWorkspace(t, tc.allow)
			var (
				got string
				err error
			)
			if tc.mode == AccessWrite {
				got, err = ws.ResolveWrite(tc.path)
			} else {
				got, err = ws.ResolveRead(tc.path)
			}

			if tc.wantErr {
				if err == nil {
					t.Fatalf("期望拒绝，实际通过: %s", got)
				}
				if tc.wantDenied && !errors.Is(err, ErrPathDenied) {
					t.Fatalf("期望 ErrPathDenied，实际: %v", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("期望允许，实际拒绝: %v", err)
			}
		})
	}
}

// TestPathDeniedErrorHidesAbsolutePath 验证拒绝信息不泄露工作区绝对路径。
func TestPathDeniedErrorHidesAbsolutePath(t *testing.T) {
	ws := newPolicyWorkspace(t, nil)
	_, err := ws.ResolveRead(".env")
	if !errors.Is(err, ErrPathDenied) {
		t.Fatalf("应返回 ErrPathDenied，实际: %v", err)
	}
	if strings.Contains(err.Error(), ws.Root()) {
		t.Fatalf("错误信息不应包含绝对路径 %s，实际: %v", ws.Root(), err)
	}
	if !strings.Contains(err.Error(), ".env") {
		t.Fatalf("错误信息应包含相对路径便于定位，实际: %v", err)
	}
}

// TestAllowOverrideAudited 验证命中显式例外时写入审计记录。
func TestAllowOverrideAudited(t *testing.T) {
	root := t.TempDir()
	logPath := filepath.Join(t.TempDir(), "audit", "audit.jsonl")
	logger, err := audit.Open(logPath)
	if err != nil {
		t.Fatalf("打开审计日志失败: %v", err)
	}
	t.Cleanup(func() { _ = logger.Close() })

	ws, err := NewWorkspaceWithPolicy(root, DefaultPathPolicy().WithAllow([]string{"config/app.db"}))
	if err != nil {
		t.Fatalf("创建工作区失败: %v", err)
	}
	ws.SetAudit(logger)
	if _, err := ws.ResolveRead("config/app.db"); err != nil {
		t.Fatalf("显式例外应放行: %v", err)
	}
	if err := logger.Close(); err != nil {
		t.Fatalf("关闭审计日志失败: %v", err)
	}

	data, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("读取审计文件失败: %v", err)
	}
	if !strings.Contains(string(data), "allow_override") {
		t.Fatalf("审计应记录例外放行: %s", data)
	}
}

// TestWorkspaceSymlinkBoundary 验证符号链接仍受工作区边界约束。
func TestWorkspaceSymlinkBoundary(t *testing.T) {
	ws := newPolicyWorkspace(t, nil)

	outsideDir := t.TempDir()
	outside := filepath.Join(outsideDir, "secret.txt")
	if err := os.WriteFile(outside, []byte("secret"), 0o600); err != nil {
		t.Fatalf("创建外部文件失败: %v", err)
	}
	if err := os.Symlink(outside, filepath.Join(ws.Root(), "link-out.txt")); err != nil {
		t.Skipf("当前环境不支持创建符号链接: %v", err)
	}
	if _, err := ws.ResolveRead("link-out.txt"); err == nil {
		t.Fatal("指向工作区外的符号链接必须拒绝")
	}

	inside := filepath.Join(ws.Root(), "real.md")
	if err := os.WriteFile(inside, []byte("inside"), 0o600); err != nil {
		t.Fatalf("创建工作区内文件失败: %v", err)
	}
	if err := os.Symlink(inside, filepath.Join(ws.Root(), "link-in.md")); err != nil {
		t.Skipf("当前环境不支持创建符号链接: %v", err)
	}
	if _, err := ws.ResolveRead("link-in.md"); err != nil {
		t.Fatalf("指向工作区内的符号链接应允许: %v", err)
	}
}
