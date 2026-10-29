// Package main 是 DDD 架构下的 CLI 装配入口（interfaces/application 装配层）。
//
// 职责：把基础设施层选型（eino 模型适配器、交互式审批器）注入到
// application/agent 的编排器中，再决定以命令行对话还是 HTTP 服务方式提供能力。
// 领域层与应用层不关心具体选了哪个 LLM 实现，仅依赖 domain/agent.ChatModel 端口。
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/joho/godotenv"

	appagent "simpleAgent/application/agent"
	"simpleAgent/domain/agent"
	dapproval "simpleAgent/domain/approval"
	"simpleAgent/domain/session"
	"simpleAgent/infrastructure/approver"
	"simpleAgent/infrastructure/audit"
	"simpleAgent/infrastructure/llm"
	"simpleAgent/infrastructure/memory"
	"simpleAgent/infrastructure/tokenizer"
	server "simpleAgent/interfaces/http"
)

// defaultSessionID 命令行默认使用的分支名。
// 固定同一个值，多次运行即可在同一条分支上延续对话。
const defaultSessionID = "cli-local"

// historyPreviewRunes 历史列表里每条消息的摘要长度
const historyPreviewRunes = 60

// defaultModel 未显式指定 OPENAI_MODEL 时使用的模型名。
const defaultModel = "deepseek-v4-flash"

// buildChatModel 从环境变量装配 eino 模型适配器（领域 ChatModel 端口的实现）。
// 敏感字段仅从环境变量读取，禁止硬编码密钥。
func buildChatModel() (*llm.EinoChatModel, error) {
	model := os.Getenv("OPENAI_MODEL")
	if model == "" {
		model = defaultModel
	}
	cm, err := llm.NewEinoChatModel(llm.Config{
		BaseURL: os.Getenv("OPENAI_BASE_URL"),
		APIKey:  os.Getenv("OPENAI_API_KEY"),
		Model:   model,
	})
	if err != nil {
		return nil, err
	}
	return cm, nil
}

// tokenizerAssetPath 官方 tokenizer.json 的入库路径（相对仓库根目录）。
const tokenizerAssetPath = "infrastructure/tokenizer/assets/tokenizer.json"

// findRepoRoot 从当前目录逐级向上查找 go.mod，返回仓库根目录；找不到返回空串。
// 用于在不依赖进程工作目录的前提下定位仓库内资源（.env、tokenizer 资产）。
func findRepoRoot() string {
	cwd, err := os.Getwd()
	if err != nil {
		return ""
	}
	for dir := cwd; ; {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return ""
		}
		dir = parent
	}
}

// loadDotEnv 加载 .env 配置。
//
// 先找进程当前目录，再回退到仓库根目录：从 bin/ 或任意子目录启动时仍能读到根目录的
// .env，不必强制 cd 到仓库根。两者都不存在时不报错——完全用系统环境变量配置
// （CI 与容器部署的常规做法）是合法启动方式。
func loadDotEnv() error {
	if err := godotenv.Load(); err == nil {
		return nil
	}
	root := findRepoRoot()
	if root == "" {
		return nil
	}
	if err := godotenv.Load(filepath.Join(root, ".env")); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("加载 %s 失败: %w", filepath.Join(root, ".env"), err)
	}
	return nil
}

// resolveTokenizerPath 解析 tokenizer.json 的路径。
//
// 优先级：显式配置 > 仓库根目录下的资产 > 相对当前目录。
// 不直接用相对当前目录，是因为进程一旦 chdir 到任务工作区就再也找不到资产，
// 分词器会静默降级成启发式估算，让 token 预算失真。
func resolveTokenizerPath() string {
	if path := strings.TrimSpace(os.Getenv("SIMPLEAGENT_TOKENIZER_PATH")); path != "" {
		return path
	}
	if root := findRepoRoot(); root != "" {
		return filepath.Join(root, tokenizerAssetPath)
	}
	return tokenizerAssetPath
}

// buildTokenizer 装配 DeepSeek 离线分词器。
// 文件缺失、不可读或解析失败时打印告警并返回 nil，由 Agent 回退到启发式估算，
// 保证启动不被网络或资源问题阻塞。可用 SIMPLEAGENT_TOKENIZER_PATH 覆盖路径。
func buildTokenizer() agent.Tokenizer {
	path := resolveTokenizerPath()
	data, err := os.ReadFile(path)
	if err != nil {
		fmt.Printf("[tokenizer 未装载，回退启发式估算]:%v\n", err)
		return nil
	}
	tk, err := tokenizer.NewDeepSeekTokenizer(data)
	if err != nil {
		fmt.Printf("[tokenizer 解析失败，回退启发式估算]:%v\n", err)
		return nil
	}
	return tk
}

func main() {
	if err := run(); err != nil {
		fmt.Println("执行失败:", err)
	}
}

func run() error {
	if err := loadDotEnv(); err != nil {
		return err
	}
	if os.Getenv("OPENAI_API_KEY") == "" {
		return errors.New("请设置环境变量 OPENAI_API_KEY")
	}
	sessionID := os.Getenv("SESSION_ID")
	if sessionID == "" {
		sessionID = defaultSessionID
	}

	cm, err := buildChatModel()
	if err != nil {
		return err
	}

	ctx := context.Background()
	args := os.Args[1:]

	// Office 办公评测模式：须在 NewAgent 之前 chdir 到任务工作区，
	// 使文件工具绑定正确的 Workspace.root（否则会写到主工程目录）。
	if len(args) > 0 && args[0] == "--office" {
		return runOffice(ctx, args[1:])
	}

	// 评测 harness：--bench run 负责批量执行任务，--bench report 展示历史报告。
	if len(args) > 0 && args[0] == "--bench" {
		return runBench(ctx, args[1:])
	}

	// HTTP 服务模式：安全配置与工作区都由集中配置决定，
	// 避免服务端和 CLI 各自维护一套默认值。
	if len(args) > 0 && args[0] == "--serve" {
		cfg, err := LoadConfig(args[1:])
		if err != nil {
			return err
		}
		auditLog := openAudit(cfg.WorkspaceRoot)
		defer closeAudit(auditLog)
		myAgent := appagent.NewAgentWithOptions(cm, appagent.Options{
			WorkspaceRoot: cfg.WorkspaceRoot,
			PathAllow:     cfg.PathAllow,
			Audit:         auditLog,
		})
		myAgent.SetTokenizer(buildTokenizer())
		defer func() {
			if err := myAgent.Close(); err != nil {
				fmt.Println("关闭 Agent 失败:", err)
			}
		}()
		return runServer(ctx, myAgent, cfg, auditLog)
	}

	// 交互式 CLI 同样记录审计：默认工作区为进程当前目录
	var auditLog *audit.Logger
	agentOpts := appagent.Options{}
	if cwd, cwdErr := os.Getwd(); cwdErr == nil {
		agentOpts.WorkspaceRoot = cwd
		auditLog = openAudit(cwd)
		agentOpts.Audit = auditLog
	}
	defer closeAudit(auditLog)
	myAgent := appagent.NewAgentWithOptions(cm, agentOpts)
	// 注入 DeepSeek 离线分词器；nil 时 Agent 内部回退启发式估算。
	myAgent.SetTokenizer(buildTokenizer())
	// 记忆索引与会话库都持有文件句柄，退出前必须关闭
	defer func() {
		if err := myAgent.Close(); err != nil {
			fmt.Println("关闭 Agent 失败:", err)
		}
	}()

	// 会话操作类子命令，不进入对话循环
	switch {
	case len(args) > 0 && args[0] == "--history":
		return printHistory(ctx, myAgent, sessionID)
	case len(args) > 0 && args[0] == "--branches":
		return printBranches(ctx, myAgent, sessionID)
	case len(args) > 0 && args[0] == "--rollback":
		return doRollback(ctx, myAgent, sessionID, args[1:])
	case len(args) > 0 && args[0] == "--fork":
		return doFork(ctx, myAgent, sessionID, args[1:])
	case len(args) > 0 && args[0] == "--planner-worker":
		question := strings.TrimSpace(strings.Join(args[1:], " "))
		if question == "" {
			return fmt.Errorf("--planner-worker 需要一个目标描述作为参数")
		}
		// Planner-Worker 模式：子任务串行、上下文隔离，审批走交互式
		myAgent.SetApprover(audit.WrapApprover(
			approver.NewInteractiveApprover(os.Stdin, os.Stdout), auditLog, "cli"))
		answer, err := myAgent.RunPlannerWorker(ctx, sessionID, question)
		if err != nil {
			return err
		}
		fmt.Println("\n==== Planner-Worker 最终答案 ====")
		fmt.Println(answer)
		return nil
	case len(args) > 0 && strings.HasPrefix(args[0], "--"):
		return fmt.Errorf("未知的参数 %s，可用：--office、--bench、--serve、--history、--branches、--rollback <序号>、--fork [序号]、--planner-worker <目标>", args[0])
	}

	question := "请先计算 123 + 456，然后给出结果对应的建议"
	if len(args) > 0 {
		question = strings.Join(args, " ")
	}

	myAgent.SetApprover(audit.WrapApprover(
		approver.NewInteractiveApprover(os.Stdin, os.Stdout), auditLog, "cli"))
	answer, err := myAgent.Run(ctx, sessionID, question)
	if err != nil {
		return err
	}
	fmt.Println("\n==== Agent最终答案 ====")
	fmt.Println(answer)
	return nil
}

// runOffice 以 Office 办公评测模式驱动一个任务的单次运行。
// 用法：--office <plan|planner_worker> <workspaceDir> <instructionFile> [maxInner]
//
// 流程：chdir 到任务工作区 → 读取 instruction.md 作为任务描述 → 用选定模式
// 全自动驱动 agent 在本地读写真实文件 → 把最终答案落盘为 <workspaceDir>/final_answer.md。
// workspaceDir 里各任务自带的 eval_core.py 会读取 final_answer.md 并确定性打分。
//
// 办公评测属于显式无人值守场景：这里显式设置 AutoApprover（全放行），
// 与普通交互模式的默认 DenyApprover 区分开，避免安全默认值被隐式绕过。
func runOffice(ctx context.Context, args []string) error {
	if len(args) < 3 {
		return errors.New("--office 用法：--office <plan|planner_worker> <workspaceDir> <instructionFile> [maxInner]")
	}
	mode, dir, instFile := args[0], args[1], args[2]
	maxInner := 20
	if len(args) > 3 {
		if n, err := strconv.Atoi(args[3]); err == nil && n > 0 {
			maxInner = n
		}
	}
	// chdir 会使进程工作目录变为任务工作区，之后的相对路径（如 tokenizer 资产）
	// 会解析到错误位置。先把 tokenizer 资产路径解析为绝对路径并写入环境变量，
	// 保证 chdir 后 buildTokenizer 仍能读到。SIMPLEAGENT_TOKENIZER_PATH 优先于默认路径。
	if os.Getenv("SIMPLEAGENT_TOKENIZER_PATH") == "" {
		if repoRoot, werr := os.Getwd(); werr == nil {
			_ = os.Setenv("SIMPLEAGENT_TOKENIZER_PATH", filepath.Join(repoRoot, tokenizerAssetPath))
		}
	}
	// 先解析为绝对路径：chdir 之后相对路径会以任务目录为基准重新解释
	workspaceRoot, err := filepath.Abs(dir)
	if err != nil {
		return fmt.Errorf("解析任务工作区 %s 失败: %w", dir, err)
	}
	if err := os.Chdir(workspaceRoot); err != nil {
		return fmt.Errorf("进入任务工作区失败 %s: %w", workspaceRoot, err)
	}
	inst, err := os.ReadFile(instFile)
	if err != nil {
		return fmt.Errorf("读取任务说明 %s 失败: %w", instFile, err)
	}

	// 状态目录：评测 harness 用 SIMPLEAGENT_OFFICE_STATE_DIR 指定到工作区之外，
	// 避免会话库、审计与 final_answer.md 混入被校验的工作区。
	stateRoot := strings.TrimSpace(os.Getenv("SIMPLEAGENT_OFFICE_STATE_DIR"))
	if stateRoot == "" {
		stateRoot = workspaceRoot
	}
	if err := os.MkdirAll(stateRoot, 0o755); err != nil {
		return fmt.Errorf("创建状态目录失败 %s: %w", stateRoot, err)
	}

	cm, err := buildChatModel()
	if err != nil {
		return err
	}
	auditLog := openAudit(stateRoot)
	defer closeAudit(auditLog)
	ag := appagent.NewAgentWithOptions(cm, appagent.Options{WorkspaceRoot: workspaceRoot, StateRoot: stateRoot, Audit: auditLog})
	ag.SetTokenizer(buildTokenizer())
	defer func() {
		if cerr := ag.Close(); cerr != nil {
			fmt.Println("关闭 Agent 失败:", cerr)
		}
	}()
	fmt.Printf("[安全警告] Office 无人值守模式已启用全自动审批，工作区：%s\n", workspaceRoot)
	ag.SetApprover(audit.WrapApprover(dapproval.AutoApprover{}, auditLog, "office"))

	sid, res, err := runOfficeMode(ctx, ag, mode, string(inst), time.Now().Format("2006-01-02 15:04:05"), maxInner)
	if err != nil {
		writeOfficeResult(officeRunResult{State: "failed", SessionID: sid, Error: err.Error()})
		return err
	}
	fmt.Printf("[office:%s] state=%s\n", mode, res.State)
	fmt.Printf("[office:%s] metrics=rounds:%d entries:%d folded:%d dropped:%d maxWin:%d llmCalls:%d\n",
		mode, res.Metrics.CompactionRounds, res.Metrics.CompactedEntries, res.Metrics.FoldedToolResults,
		res.Metrics.DroppedWorkerEntries, res.Metrics.MaxWindowTokens, res.Metrics.LLMCalls)
	if res.State != "final" {
		writeOfficeResult(officeRunResult{State: res.State, SessionID: sid, Metrics: res.Metrics})
		if res.State == "tool" {
			return fmt.Errorf("office 任务暂停于外部工具 %s，参数=%s（需在 office 模式下用文件工具完成）", res.ToolName, res.ToolArg)
		}
		return fmt.Errorf("office 任务未正常收尾，state=%s", res.State)
	}
	writeOfficeResult(officeRunResult{State: res.State, SessionID: sid, Answer: res.Answer, Metrics: res.Metrics})
	answerPath := filepath.Join(stateRoot, "final_answer.md")
	if err := os.WriteFile(answerPath, []byte(res.Answer), 0o644); err != nil {
		return fmt.Errorf("写入 final_answer.md 失败: %w", err)
	}
	fmt.Printf("[office] 已写入 %s\n", answerPath)
	return nil
}

// officeRunResult 是 --office 输出给评测 harness 的结构化结果。
// 只有设置了 SIMPLEAGENT_OFFICE_RESULT 时才落盘，普通 CLI 运行不受影响。
type officeRunResult struct {
	State     string             `json:"state"`
	SessionID string             `json:"session_id"`
	Answer    string             `json:"answer,omitempty"`
	Metrics   appagent.WbMetrics `json:"metrics"`
	Error     string             `json:"error,omitempty"`
}

// writeOfficeResult 把结构化运行结果写入 SIMPLEAGENT_OFFICE_RESULT 指定的文件。
// 写入失败只打印告警：评测 harness 会回退解析 stdout 并记录 result_fallback。
func writeOfficeResult(res officeRunResult) {
	path := os.Getenv("SIMPLEAGENT_OFFICE_RESULT")
	if path == "" {
		return
	}
	data, err := json.MarshalIndent(res, "", "  ")
	if err != nil {
		fmt.Printf("[office 结果序列化失败]:%v\n", err)
		return
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		fmt.Printf("[office 结果写入失败 %s]:%v\n", path, err)
	}
}

// runOfficeMode 按模式分派到 Office 评测入口，并返回 workbench 会话 ID。
func runOfficeMode(ctx context.Context, ag *appagent.Agent, mode, task, datetime string, maxInner int) (string, appagent.WbResult, error) {
	switch mode {
	case "plan":
		return ag.StartOfficeWorkbench(ctx, task, nil, datetime, maxInner)
	case "planner_worker":
		return ag.StartOfficePlannerWorker(ctx, task, nil, datetime, maxInner)
	default:
		return "", appagent.WbResult{}, fmt.Errorf("未知 office 模式 %q，可选 plan、planner_worker", mode)
	}
}

// openAudit 创建审计日志。失败时打印告警并返回 nil，
// 保证审计存储故障不会阻断主流程（审计事件本身会丢失，但不影响业务可用性）。
func openAudit(workspaceRoot string) *audit.Logger {
	if strings.TrimSpace(workspaceRoot) == "" {
		return nil
	}
	path := filepath.Join(memory.WorkspaceDir(workspaceRoot), "audit", "audit.jsonl")
	logger, err := audit.Open(path)
	if err != nil {
		fmt.Printf("[审计日志不可用]:%v\n", err)
		return nil
	}
	return logger
}

// closeAudit 关闭审计日志；nil 安全。
func closeAudit(logger *audit.Logger) {
	if logger == nil {
		return
	}
	if err := logger.Close(); err != nil {
		fmt.Println("关闭审计日志失败:", err)
	}
}

// runServer 以 HTTP/WS 服务方式启动后端，供 Web 前端调用。
// 监听地址、工作区与安全开关全部来自集中配置，默认只监听本机回环。
func runServer(ctx context.Context, myAgent *appagent.Agent, cfg *Config, auditLog *audit.Logger) error {
	if cfg.GeneratedToken {
		// 只在控制台打印一次，不写入日志或磁盘；提示用户自行保存
		fmt.Printf("[安全提示] 本次启动生成了访问 Token（仅打印一次）：%s\n", cfg.AuthToken)
	}
	if cfg.AllowNoAuth {
		fmt.Println("[安全警告] 已开启本地免认证模式（仅 loopback 生效）")
	}
	if cfg.Headless {
		fmt.Println("[安全警告] 已开启无人值守模式，技能执行默认禁用")
	}

	srv, err := server.NewServer(myAgent, cfg.WorkspaceRoot, server.Options{
		AuthToken:       cfg.AuthToken,
		AllowedOrigins:  cfg.AllowedOrigins,
		AllowNoAuth:     cfg.AllowNoAuth,
		Loopback:        cfg.Loopback,
		Headless:        cfg.Headless,
		AllowSkillAdmin: cfg.AllowSkillAdmin,
		AllowSkillExec:  cfg.AllowSkillExec,
		RequestMaxBytes: cfg.RequestMaxBytes,
		SessionTTL:      cfg.SessionTTL,
		ApprovalTimeout: cfg.ApprovalTimeout,
		Audit:           auditLog,
	})
	if err != nil {
		return err
	}
	if err := srv.RegisterStoredSkills(); err != nil {
		return fmt.Errorf("加载持久化技能失败: %w", err)
	}
	return srv.Serve(ctx, cfg.Addr)
}

// printHistory 打印当前分支的历史。序号用于 --rollback / --fork 引用，
// 只有用户提问会带 → 标记，因为它是唯一合法的回滚点。
func printHistory(ctx context.Context, a *appagent.Agent, sessionID string) error {
	messages, err := a.LoadHistory(ctx, sessionID)
	if err != nil {
		return err
	}
	if len(messages) == 0 {
		fmt.Printf("会话 %s 还没有任何消息。\n", sessionID)
		return nil
	}

	fmt.Printf("会话 %s 当前分支共 %d 条消息：\n\n", sessionID, len(messages))
	for i, message := range messages {
		marker := "  "
		if message.Kind.Rollbackable() {
			marker = "→ "
		}
		fmt.Printf("%s[%2d] #%-4d %-11s %-9s %s\n",
			marker, i+1, message.ID, message.Kind, message.Role,
			session.Preview(message.Content, historyPreviewRunes))
	}
	fmt.Println("\n带 → 的是用户提问，可作为 --rollback / --fork 的目标")
	return nil
}

// printBranches 列出同一条对话树上的所有分支
func printBranches(ctx context.Context, a *appagent.Agent, sessionID string) error {
	branches, err := a.ListBranches(ctx, sessionID)
	if err != nil {
		return err
	}
	if len(branches) == 0 {
		fmt.Println("当前对话还没有任何分支。")
		return nil
	}

	fmt.Printf("会话 %s 所在对话树共有 %d 条分支：\n\n", sessionID, len(branches))
	for _, branch := range branches {
		marker := "  "
		if branch.SessionID == sessionID {
			marker = "→ "
		}
		label := branch.Label
		if label == "" {
			label = "（无说明）"
		}
		fmt.Printf("%s%s  [%d 条消息] %s\n", marker, branch.SessionID, branch.MessageCount, label)
		if branch.Preview != "" {
			fmt.Printf("     末尾：%s\n", branch.Preview)
		}
	}
	fmt.Println("\n带 → 的是当前会话；用 SESSION_ID=<分支 ID> 切换到其他分支")
	return nil
}

// doRollback 回滚到指定序号的消息
func doRollback(ctx context.Context, a *appagent.Agent, sessionID string, args []string) error {
	target, err := resolveTarget(ctx, a, sessionID, args)
	if err != nil {
		return err
	}
	archiveID, err := a.RollbackSession(ctx, sessionID, target.ID)
	if err != nil {
		return err
	}
	fmt.Printf("已回滚到 #%d（%s）\n", target.ID, session.Preview(target.Content, historyPreviewRunes))
	fmt.Printf("原路径已自动存档为分支：%s\n", archiveID)
	fmt.Println("继续提问会从该点长出一条新分支，旧路径不会丢失。")
	return nil
}

// doFork 从指定序号分叉出新分支；不带序号时复制当前 HEAD
func doFork(ctx context.Context, a *appagent.Agent, sessionID string, args []string) error {
	fromID := int64(0)
	description := "当前 HEAD"
	if len(args) > 0 {
		target, err := resolveTarget(ctx, a, sessionID, args)
		if err != nil {
			return err
		}
		fromID = target.ID
		description = fmt.Sprintf("#%d", target.ID)
	}

	newSessionID, err := a.ForkSession(ctx, sessionID, fromID)
	if err != nil {
		return err
	}
	fmt.Printf("已从 %s 分叉出新分支：%s\n", description, newSessionID)
	fmt.Printf("用 SESSION_ID=%s 继续对话即可在该分支上推进。\n", newSessionID)
	return nil
}

// resolveTarget 把 --history 显示的序号解析为消息。
//
// 只接受用户提问：一轮 Run 会在树上留下工具调用、工具结果等中间态，
// 回滚到这些节点会得到「模型说要调工具但结果凭空消失」的上下文。
func resolveTarget(ctx context.Context, a *appagent.Agent, sessionID string, args []string) (*session.Message, error) {
	if len(args) == 0 {
		return nil, errors.New("缺少序号，请先用 --history 查看，例如：--rollback 3")
	}
	index, err := strconv.Atoi(args[0])
	if err != nil {
		return nil, fmt.Errorf("序号必须是数字: %s", args[0])
	}

	messages, err := a.LoadHistory(ctx, sessionID)
	if err != nil {
		return nil, err
	}
	if index < 1 || index > len(messages) {
		return nil, fmt.Errorf("序号 %d 超出范围（当前分支共 %d 条消息）", index, len(messages))
	}

	target := messages[index-1]
	if !target.Kind.Rollbackable() {
		return nil, fmt.Errorf("第 %d 条是 %s，不能作为回滚点；请选带 → 标记的用户提问", index, target.Kind)
	}
	return &target, nil
}
