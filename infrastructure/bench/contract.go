// Package bench 是评测 harness 的基础设施实现。
//
// 它负责任务包读取、tar 安全解压、工作区快照、JUnit 解析以及
// 本地/容器执行器；应用层只通过 application/eval 定义的端口与之交互。
package bench

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/pelletier/go-toml/v2"
	"gopkg.in/yaml.v3"

	appeval "simpleAgent/application/eval"
	"simpleAgent/domain/artifact"
)

// 默认任务超时，与官方案例的 1800 秒保持一致。
const defaultTaskTimeout = 30 * time.Minute

// verifierFile tests/verifier.toml 的解析结构。
type verifierFile struct {
	Run struct {
		Cwd     string `toml:"cwd"`
		Command string `toml:"command"`
	} `toml:"run"`
	Env map[string]string `toml:"env"`
}

// judgeFile tests/judge.yaml 的解析结构（只读取产物契约）。
type judgeFile struct {
	Artifacts []struct {
		ID       string `yaml:"id"`
		Path     string `yaml:"path"`
		Required *bool  `yaml:"required"`
		Type     string `yaml:"type"`
	} `yaml:"artifacts"`
}

// taskFile task.toml 的解析结构（只取超时与环境限制）。
type taskFile struct {
	Verifier struct {
		TimeoutSec float64 `toml:"timeout_sec"`
	} `toml:"verifier"`
	Agent struct {
		TimeoutSec float64 `toml:"timeout_sec"`
	} `toml:"agent"`
	Environment struct {
		BuildTimeoutSec float64 `toml:"build_timeout_sec"`
		CPUs            int     `toml:"cpus"`
		MemoryMB        int     `toml:"memory_mb"`
		NetworkMode     string  `toml:"network_mode"`
	} `toml:"environment"`
}

// ParseVerifierTOML 读取官方评分入口，缺少 command 时返回错误。
func ParseVerifierTOML(path string) (appeval.VerifierSpec, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return appeval.VerifierSpec{}, fmt.Errorf("读取 verifier.toml 失败: %w", err)
	}
	var f verifierFile
	if err := toml.Unmarshal(data, &f); err != nil {
		return appeval.VerifierSpec{}, fmt.Errorf("解析 verifier.toml 失败: %w", err)
	}
	cmd := strings.TrimSpace(f.Run.Command)
	if cmd == "" {
		return appeval.VerifierSpec{}, fmt.Errorf("verifier.toml 缺少 run.command")
	}
	cwd := strings.TrimSpace(f.Run.Cwd)
	if cwd == "" {
		cwd = "/workspace"
	}
	if !strings.HasPrefix(cwd, "/") {
		return appeval.VerifierSpec{}, fmt.Errorf("verifier.toml 的 run.cwd 必须是容器内绝对路径: %s", cwd)
	}
	env := f.Env
	if env == nil {
		env = map[string]string{}
	}
	return appeval.VerifierSpec{Cwd: cwd, Command: cmd, Env: env}, nil
}

// ParseJudgeYAML 读取 tests/judge.yaml 的产物契约。
//
// 实测 50/50 个任务都声明了 artifacts；缺失时按 dataset_invalid 处理。
func ParseJudgeYAML(path string) ([]appeval.DeclaredArtifact, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("读取 judge.yaml 失败: %w", err)
	}
	var f judgeFile
	if err := yaml.Unmarshal(data, &f); err != nil {
		return nil, fmt.Errorf("解析 judge.yaml 失败: %w", err)
	}
	if len(f.Artifacts) == 0 {
		return nil, fmt.Errorf("judge.yaml 未声明任何 artifacts")
	}
	out := make([]appeval.DeclaredArtifact, 0, len(f.Artifacts))
	for i, a := range f.Artifacts {
		if strings.TrimSpace(a.Path) == "" {
			return nil, fmt.Errorf("judge.yaml 第 %d 个产物缺少 path", i+1)
		}
		if _, err := artifact.SafeRelPath(a.Path); err != nil {
			return nil, fmt.Errorf("judge.yaml 产物路径非法: %w", err)
		}
		required := true
		if a.Required != nil {
			required = *a.Required
		}
		out = append(out, appeval.DeclaredArtifact{
			ID:       strings.TrimSpace(a.ID),
			Path:     strings.TrimSpace(a.Path),
			Type:     strings.TrimSpace(a.Type),
			Required: required,
		})
	}
	return out, nil
}

// ParseTaskTOML 读取任务超时与环境限制。
//
// 文件缺失或字段缺失时回退默认值并返回 warning，而不是让整个任务包失效。
func ParseTaskTOML(path string) (agentTimeout, verifierTimeout time.Duration, warnings []string, err error) {
	agentTimeout = defaultTaskTimeout
	verifierTimeout = defaultTaskTimeout
	data, readErr := os.ReadFile(path)
	if readErr != nil {
		return agentTimeout, verifierTimeout, []string{"缺少 task.toml，使用默认超时 30m"}, nil
	}
	var f taskFile
	if err := toml.Unmarshal(data, &f); err != nil {
		return agentTimeout, verifierTimeout, nil, fmt.Errorf("解析 task.toml 失败: %w", err)
	}
	if f.Agent.TimeoutSec > 0 {
		agentTimeout = time.Duration(f.Agent.TimeoutSec * float64(time.Second))
	} else {
		warnings = append(warnings, "task.toml 缺少 agent.timeout_sec，使用默认 30m")
	}
	if f.Verifier.TimeoutSec > 0 {
		verifierTimeout = time.Duration(f.Verifier.TimeoutSec * float64(time.Second))
	} else {
		warnings = append(warnings, "task.toml 缺少 verifier.timeout_sec，使用默认 30m")
	}
	if f.Environment.MemoryMB <= 0 || f.Environment.CPUs <= 0 {
		warnings = append(warnings, "task.toml 缺少 environment 资源限制")
	}
	return agentTimeout, verifierTimeout, warnings, nil
}

// requireFile 检查任务包中的必填文件。
func requireFile(path, label string) error {
	info, err := os.Stat(path)
	if err != nil {
		return fmt.Errorf("缺少%s: %s", label, path)
	}
	if info.IsDir() {
		return fmt.Errorf("%s不是文件: %s", label, path)
	}
	return nil
}

// testsDir 返回任务包的 tests 目录。
func testsDir(taskDir string) string {
	return filepath.Join(taskDir, "tests")
}
