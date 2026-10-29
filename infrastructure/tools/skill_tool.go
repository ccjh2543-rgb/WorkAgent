package tools

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os/exec"
	"runtime"
	"strings"
	"time"

	"simpleAgent/domain/tool"
	"simpleAgent/infrastructure/audit"
)

// SkillSpec 一个可动态注册的自定义技能的定义。
// 它描述：技能如何被调用、模型如何理解它的用途、以及实际执行的命令模板。
type SkillSpec struct {
	// Name 技能名，必须与 !TOOL:技能名|参数 中的名字一致且不与内置工具冲突
	Name string `json:"name"`
	// Description 给模型看的用途说明，描述其能力与参数约定
	Description string `json:"description"`
	// CommandTemplate 命令模板，可选占位 {{args}} 会被替换为模型传入的参数。
	// 由操作者（可信方）编写；执行前必须经过人工确认。
	CommandTemplate string `json:"command_template"`
}

// CommandSkill 把一段命令封装成可被 Agent 调用的工具/技能。
//
// 安全边界：命令模板由操作者在注册时编写，属信任代码；模型传入的 {{args}}
// 会被原样拼进命令并交由 shell 执行，因此该类技能一律按不可逆级别对待，
// 必须人工确认，绝不自动放行。工作目录固定为工作区根目录，避免越界。
type CommandSkill struct {
	spec      SkillSpec
	workspace *Workspace
	risk      tool.RiskLevel
	audit     *audit.Logger
}

// SetAudit 注入审计日志；nil 表示不记录。
// 只记录技能名，不记录参数，避免命令参数中的敏感信息落盘。
func (s *CommandSkill) SetAudit(log *audit.Logger) { s.audit = log }

// NewCommandSkill 创建一个命令技能。workspace 用于限定命令的工作目录。
// risk 缺省按不可逆处理，任何创建方都应显式选择（这里保守取 RiskDestructive）。
func NewCommandSkill(spec SkillSpec, workspace *Workspace, risk tool.RiskLevel) (*CommandSkill, error) {
	if strings.TrimSpace(spec.Name) == "" {
		return nil, errors.New("技能名不能为空")
	}
	if strings.TrimSpace(spec.Description) == "" {
		return nil, errors.New("技能描述不能为空")
	}
	if strings.TrimSpace(spec.CommandTemplate) == "" {
		return nil, errors.New("技能命令模板不能为空")
	}
	if risk >= tool.RiskDestructive {
		risk = tool.RiskDestructive
	}
	return &CommandSkill{spec: spec, workspace: workspace, risk: risk}, nil
}

// Name 返回技能名，作为 !TOOL 调用的标识
func (s *CommandSkill) Name() string {
	return s.spec.Name
}

// Description 返回给模型看的用途说明
func (s *CommandSkill) Description() string {
	return s.spec.Description
}

// Run 执行命令模板：把 {{args}} 替换为参数后交由系统 shell 执行。
//
// 这里刻意用命令模板而非「可执行文件路径 + 参数数组」，是为了让技能作者能
// 表达完整的管道/脚本逻辑；代价是引入 shell 注入面，这正是它必须人工确认、
// 且命令模板只能由可信操作者编写的原因。
func (s *CommandSkill) Run(args string) (string, error) {
	command := strings.ReplaceAll(s.spec.CommandTemplate, "{{args}}", strings.TrimSpace(args))
	if strings.TrimSpace(command) == "" {
		return "", errors.New("技能命令为空")
	}
	s.audit.Log(audit.Event{Type: "skill_exec", Action: "start", Detail: s.spec.Name})

	ctx, cancel := context.WithTimeout(context.Background(), skillTimeout)
	defer cancel()

	shell, flag := shellCommand()
	cmd := exec.CommandContext(ctx, shell, flag, command)
	if s.workspace != nil {
		// 命令固定在工作区根目录执行，避免影响工作区之外的文件
		cmd.Dir = s.workspace.Root()
	}
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		// 把 stderr 一并返回，模型才能判断哪里出错并自我调整
		detail := strings.TrimSpace(stderr.String())
		if detail == "" {
			detail = err.Error()
		}
		return "", fmt.Errorf("技能执行失败: %s", detail)
	}
	if strings.TrimSpace(stdout.String()) == "" && strings.TrimSpace(stderr.String()) != "" {
		return strings.TrimSpace(stderr.String()), nil
	}
	return strings.TrimSpace(stdout.String()), nil
}

// Describe 给人工审批展示将要实际执行的完整命令（含替换后的参数）
func (s *CommandSkill) Describe(args string) string {
	command := strings.ReplaceAll(s.spec.CommandTemplate, "{{args}}", strings.TrimSpace(args))
	return fmt.Sprintf("将要执行命令（工作目录：工作区根目录）：%s", strings.TrimSpace(command))
}

// Policy 返回本技能的默认执行策略：不可逆 + 必须人工确认
func (s *CommandSkill) Policy() tool.ToolPolicy {
	return tool.ToolPolicy{Risk: s.risk, AutoApprove: false}
}

// skillTimeout 单条技能命令的超时时长，防止模型把它当 RM 跑挂
const skillTimeout = 60 * time.Second

// shellCommand 返回当前平台用于执行命令模板的 shell 与其 -c 标志
func shellCommand() (shell, flag string) {
	if runtime.GOOS == "windows" {
		return "cmd", "/C"
	}
	return "/bin/sh", "-c"
}
