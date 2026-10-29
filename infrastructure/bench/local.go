package bench

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"

	appeval "simpleAgent/application/eval"
	domaineval "simpleAgent/domain/eval"
)

// 虚拟盘符分配器：同一进程内的并发任务各自占用一个盘符。
var (
	driveMu    sync.Mutex
	usedDrives = map[string]bool{}
)

// LocalExecutor 本地调试执行器。
//
// 它不使用容器，在宿主目录内准备 workspace/tests/logs，
// 通过重新执行当前二进制运行 --office，并在宿主机调用 Python pytest。
// 报告会标记 executor_mode=local，结果不能作为正式分数。
type LocalExecutor struct {
	// Python 是 pytest 使用的 Python 可执行文件，默认 python。
	Python string
}

// NewLocalExecutor 创建本地执行器。
func NewLocalExecutor() *LocalExecutor {
	return &LocalExecutor{Python: "python"}
}

// Mode 返回执行器标识。
func (e *LocalExecutor) Mode() string { return "local" }

// Prepare 解压工作区、复制测试目录并创建日志目录。
func (e *LocalExecutor) Prepare(ctx context.Context, task appeval.TaskSpec, runDir string) (appeval.Environment, error) {
	if err := ctx.Err(); err != nil {
		return appeval.Environment{}, err
	}
	if task.LoadErr != "" {
		return appeval.Environment{}, errors.New(task.LoadErr)
	}
	absRunDir, err := filepath.Abs(runDir)
	if err != nil {
		return appeval.Environment{}, err
	}
	env := appeval.Environment{
		Workspace:  filepath.Join(absRunDir, "workspace"),
		Tests:      filepath.Join(absRunDir, "tests"),
		Logs:       filepath.Join(absRunDir, "logs"),
		AgentState: filepath.Join(absRunDir, "logs", "agent_state"),
	}
	for _, dir := range []string{env.Workspace, env.Tests, filepath.Join(env.Logs, "verifier"), env.AgentState} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return appeval.Environment{}, fmt.Errorf("创建运行目录失败: %w", err)
		}
	}
	if err := ExtractTarGz(task.WorkspaceArchive, env.Workspace); err != nil {
		return appeval.Environment{}, err
	}
	if err := copyTree(task.TestsDir, env.Tests); err != nil {
		return appeval.Environment{}, fmt.Errorf("复制测试目录失败: %w", err)
	}
	if _, err := os.ReadFile(task.InstructionPath); err != nil {
		return appeval.Environment{}, fmt.Errorf("读取任务说明失败: %w", err)
	}
	// 评测脚本大量硬编码 /workspace、/tests、/logs；Windows 本地用临时盘符
	// 映射运行目录，使这些绝对路径在子进程里真实可解析。
	virtualRoot, err := mapVirtualRoot(absRunDir)
	if err != nil {
		return appeval.Environment{}, err
	}
	env.ContainerRoot = virtualRoot
	return env, nil
}

// SnapshotWorkspace 复用基础设施层的工作区快照实现。
func (e *LocalExecutor) SnapshotWorkspace(ctx context.Context, root string) (map[string]appeval.FileMeta, error) {
	return SnapshotWorkspace(ctx, root)
}

// RunAgent 在本地执行 --office，并优先读取结构化结果文件。
func (e *LocalExecutor) RunAgent(ctx context.Context, env appeval.Environment, req appeval.AgentRequest) (appeval.AgentResult, error) {
	exe, err := os.Executable()
	if err != nil {
		return appeval.AgentResult{State: "failed", Err: err.Error()}, fmt.Errorf("获取当前可执行文件失败: %w", err)
	}
	logFile, err := os.Create(filepath.Join(env.Logs, "agent.log"))
	if err != nil {
		return appeval.AgentResult{State: "failed", Err: err.Error()}, err
	}
	maxInner := req.MaxInner
	if maxInner <= 0 {
		maxInner = 20
	}
	cmd := exec.CommandContext(ctx, exe, "--office", req.Mode, env.Workspace, req.InstructionPath, strconv.Itoa(maxInner))
	cmd.Dir = findRepoRoot()
	cmd.Env = append(os.Environ(),
		"SIMPLEAGENT_OFFICE_RESULT="+req.ResultPath,
		"SIMPLEAGENT_BENCH_LOCAL=1",
		// 状态目录放在 logs 下，避免 .simpleagent 与会话库污染被校验的工作区。
		"SIMPLEAGENT_OFFICE_STATE_DIR="+env.AgentState,
	)
	if os.Getenv("SIMPLEAGENT_TOKENIZER_PATH") == "" {
		if root := findRepoRoot(); root != "" {
			asset := filepath.Join(root, "infrastructure", "tokenizer", "assets", "tokenizer.json")
			if _, statErr := os.Stat(asset); statErr == nil {
				cmd.Env = append(cmd.Env, "SIMPLEAGENT_TOKENIZER_PATH="+asset)
			}
		}
	}
	cmd.Stdout = logFile
	cmd.Stderr = logFile
	runErr := cmd.Run()
	closeErr := logFile.Close()
	if closeErr != nil {
		fmt.Printf("[Agent 日志关闭失败]: %v\n", closeErr)
	}

	// 结构化结果与进程退出码统一由 agentRunOutcome 判定，本地与 Docker 执行器共用。
	return agentRunOutcome(req.ResultPath, runErr)
}

// RunVerifier 按 verifier.toml 执行评分命令并解析 JUnit。
//
// 本地模式把容器路径 /workspace、/tests、/logs 翻译为运行目录路径，
// 并直接用 argv 调用 python -m pytest，避免依赖 POSIX shell。
func (e *LocalExecutor) RunVerifier(ctx context.Context, env appeval.Environment, spec appeval.VerifierSpec) (domaineval.VerifierResult, error) {
	result := domaineval.VerifierResult{Command: spec.Command, Cwd: spec.Cwd}
	outputPath := filepath.Join(env.Logs, "verifier", "test_output.txt")
	if err := os.MkdirAll(filepath.Dir(outputPath), 0o755); err != nil {
		return result, err
	}
	logFile, err := os.Create(outputPath)
	if err != nil {
		return result, err
	}
	defer func() { _ = logFile.Close() }()

	argv, junitPath, cmdEnv, err := e.buildVerifierCommand(env, spec)
	if err != nil {
		return result, err
	}
	result.OutputPath = outputPath
	result.ResultXML = junitPath

	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	cmd.Dir = verifierCwd(env, spec.Cwd)
	cmd.Env = append(os.Environ(), cmdEnv...)
	cmd.Stdout = logFile
	cmd.Stderr = logFile
	runErr := cmd.Run()

	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		result.TimedOut = true
		return result, fmt.Errorf("评分命令超时")
	}
	var exitErr *exec.ExitError
	if runErr != nil && !errors.As(runErr, &exitErr) {
		return result, fmt.Errorf("启动评分命令失败: %w", runErr)
	}
	if exitErr != nil {
		result.ExitCode = exitErr.ExitCode()
	}

	parsed, parseErr := ParseJUnit(junitPath)
	if parseErr != nil {
		// JUnit 缺失或损坏由上层记录 verifier_error，不在这里伪造通过。
		return result, nil
	}
	result.Tests = parsed.Tests
	result.Failures = parsed.Failures
	result.Errors = parsed.Errors
	result.Skipped = parsed.Skipped
	result.Cases = parsed.Cases
	return result, nil
}

// Cleanup 本地模式保留工作区与日志，便于人工复核；只解除虚拟盘符映射。
func (e *LocalExecutor) Cleanup(ctx context.Context, env appeval.Environment) error {
	if env.ContainerRoot != "" {
		unmapVirtualRoot(env.ContainerRoot)
	}
	return nil
}

// buildVerifierCommand 解析 verifier.toml 命令并翻译容器路径。
func (e *LocalExecutor) buildVerifierCommand(env appeval.Environment, spec appeval.VerifierSpec) ([]string, string, []string, error) {
	tokens, ok := splitShell(spec.Command)
	if !ok {
		return nil, "", nil, fmt.Errorf("无法解析 verifier 命令: %s", spec.Command)
	}
	// 丢弃 shell 重定向部分（> /logs/... 2>&1），输出统一由 harness 捕获。
	if idx := indexToken(tokens, ">"); idx >= 0 {
		tokens = tokens[:idx]
	}

	pythonIdx := -1
	for i, t := range tokens {
		if t == "python" || t == "python3" || strings.HasSuffix(strings.ToLower(t), "python.exe") {
			pythonIdx = i
			break
		}
	}
	if pythonIdx < 0 || len(tokens) < pythonIdx+3 || tokens[pythonIdx+1] != "-m" || tokens[pythonIdx+2] != "pytest" {
		return nil, "", nil, fmt.Errorf("本地执行器只支持 python -m pytest 形式的评分命令: %s", spec.Command)
	}

	envMap := map[string]string{}
	prefixEnv := map[string]string{}
	for i, t := range tokens {
		if i >= pythonIdx {
			break
		}
		name, value, found := strings.Cut(t, "=")
		if !found || name == "" || strings.ContainsAny(name, "/\\") {
			continue
		}
		prefixEnv[name] = value
	}

	python := e.Python
	if python == "" {
		python = "python"
	}
	argv := []string{python, "-m", "pytest"}
	junitPath := filepath.Join(env.Logs, "verifier", "results.xml")
	for _, t := range tokens[pythonIdx+3:] {
		if strings.HasPrefix(t, "--junitxml=") {
			raw := strings.TrimPrefix(t, "--junitxml=")
			// 文件落盘位置用真实路径（报告在解除盘符映射后仍可定位），
			// 传给 pytest 的参数用与工作目录同盘的虚拟路径。
			junitPath = filepath.FromSlash(translatePath(env, raw))
			argv = append(argv, "--junitxml="+translateForVerifier(env, raw))
			continue
		}
		argv = append(argv, translateForVerifier(env, t))
	}

	// 任务声明的环境变量：翻译容器路径后注入。
	for k, v := range spec.Env {
		if k == "PYTHONPATH" {
			continue
		}
		envMap[k] = translateForVerifier(env, v)
	}
	for k, v := range prefixEnv {
		if k == "PYTHONPATH" {
			continue
		}
		envMap[k] = translateForVerifier(env, v)
	}
	// PYTHONPATH 只翻译一次：任务声明优先于命令前缀，避免 Windows 盘符被二次拆分。
	pyRaw := prefixEnv["PYTHONPATH"]
	if v, ok := spec.Env["PYTHONPATH"]; ok {
		pyRaw = v
	}
	if pyRaw == "" {
		pyRaw = "/workspace"
	}
	envMap["PYTHONPATH"] = translatePythonPath(env, pyRaw)

	envList := make([]string, 0, len(envMap))
	for k, v := range envMap {
		envList = append(envList, k+"="+v)
	}
	return argv, junitPath, envList, nil
}

// translatePath 把容器内路径前缀翻译为本地运行目录（真实路径）。
func translatePath(env appeval.Environment, s string) string {
	replacer := strings.NewReplacer(
		"/workspace", filepath.ToSlash(env.Workspace),
		"/tests", filepath.ToSlash(env.Tests),
		"/logs", filepath.ToSlash(env.Logs),
	)
	return replacer.Replace(s)
}

// translateForVerifier 优先翻译到虚拟盘符内的容器根目录。
//
// Windows 上 pytest 的工作目录与测试参数必须位于同一个盘符，
// 否则跨盘收集会把系统盘根目录当成收集目标。
func translateForVerifier(env appeval.Environment, s string) string {
	if env.ContainerRoot != "" {
		replacer := strings.NewReplacer(
			"/workspace", filepath.ToSlash(filepath.Join(env.ContainerRoot, "workspace")),
			"/tests", filepath.ToSlash(filepath.Join(env.ContainerRoot, "tests")),
			"/logs", filepath.ToSlash(filepath.Join(env.ContainerRoot, "logs")),
		)
		return replacer.Replace(s)
	}
	return translatePath(env, s)
}

// translatePythonPath 处理 PYTHONPATH 前缀：按容器分隔符拆分后逐项翻译，
// 再用宿主分隔符拼接，避免 Windows 盘符冒号被误拆。
func translatePythonPath(env appeval.Environment, raw string) string {
	parts := strings.Split(raw, ":")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		if strings.Contains(p, "$") {
			if v := os.Getenv("PYTHONPATH"); v != "" {
				out = append(out, v)
			}
			continue
		}
		out = append(out, filepath.FromSlash(translateForVerifier(env, p)))
	}
	return strings.Join(out, string(os.PathListSeparator))
}

// verifierCwd 返回评分命令的工作目录。
//
// Windows 本地模式下优先使用虚拟盘符内的 /workspace，
// 使评测脚本里硬编码的 /tests、/workspace 在没有翻译时也能解析。
func verifierCwd(env appeval.Environment, specCwd string) string {
	if env.ContainerRoot != "" {
		rel := strings.TrimPrefix(specCwd, "/")
		return filepath.Join(env.ContainerRoot, filepath.FromSlash(rel))
	}
	return filepath.FromSlash(translatePath(env, specCwd))
}

// splitShell 极简 shell 分词：支持单双引号与反斜杠续行，返回 ok=false 表示引号未闭合。
func splitShell(s string) ([]string, bool) {
	var tokens []string
	var cur strings.Builder
	inSingle, inDouble := false, false
	flush := func() {
		if cur.Len() > 0 {
			tokens = append(tokens, cur.String())
			cur.Reset()
		}
	}
	runes := []rune(s)
	for i := 0; i < len(runes); i++ {
		c := runes[i]
		switch {
		case inSingle:
			if c == '\'' {
				inSingle = false
				continue
			}
			cur.WriteRune(c)
		case inDouble:
			if c == '"' {
				inDouble = false
				continue
			}
			cur.WriteRune(c)
		case c == '\'':
			inSingle = true
		case c == '"':
			inDouble = true
		case c == '\\':
			// 续行：反斜杠 + 换行直接丢弃
			if i+1 < len(runes) && (runes[i+1] == '\n' || runes[i+1] == '\r') {
				continue
			}
			cur.WriteRune('\\')
		case c == ' ' || c == '\t' || c == '\n' || c == '\r':
			flush()
		default:
			cur.WriteRune(c)
		}
	}
	if inSingle || inDouble {
		return nil, false
	}
	flush()
	return tokens, true
}

// indexToken 返回 token 首次出现的下标。
func indexToken(tokens []string, want string) int {
	for i, t := range tokens {
		if t == want {
			return i
		}
	}
	return -1
}

// agentRunOutcome 依据结构化结果文件与进程退出码判定 Agent 运行结果。
//
// 结构化结果优先：只要结果文件可读且 state 非 final，就以结果文件为准；
// 结果文件缺失时视为框架故障，标记 Fallback 交由上层记录 result_fallback。
func agentRunOutcome(resultPath string, runErr error) (appeval.AgentResult, error) {
	res, readErr := readAgentResult(resultPath)
	if readErr == nil {
		if res.State == "final" {
			return res, nil
		}
		if runErr != nil {
			res.Err = runErr.Error()
			return res, runErr
		}
		return res, nil
	}
	if runErr == nil {
		runErr = errors.New("未产出结构化结果文件")
	}
	return appeval.AgentResult{State: "failed", Fallback: true, Err: runErr.Error()}, runErr
}

// readAgentResult 读取 --office 输出的结构化结果。
func readAgentResult(path string) (appeval.AgentResult, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return appeval.AgentResult{}, err
	}
	var res appeval.AgentResult
	if err := json.Unmarshal(data, &res); err != nil {
		return appeval.AgentResult{}, err
	}
	return res, nil
}

// mapVirtualRoot 用 subst 把运行目录映射为临时盘符，返回形如 "X:\\" 的根。
//
// 仅 Windows 支持；其他平台返回空串，由路径翻译兜底。
func mapVirtualRoot(dir string) (string, error) {
	if runtime.GOOS != "windows" {
		return "", nil
	}
	driveMu.Lock()
	defer driveMu.Unlock()
	for c := 'P'; c <= 'Z'; c++ {
		letter := string(c) + ":"
		if usedDrives[letter] {
			continue
		}
		if err := exec.Command("subst", letter, dir).Run(); err == nil {
			usedDrives[letter] = true
			return letter + string(filepath.Separator), nil
		}
	}
	return "", errors.New("无法分配虚拟盘符，请检查 subst 是否可用")
}

// unmapVirtualRoot 解除虚拟盘符映射。
func unmapVirtualRoot(root string) {
	if runtime.GOOS != "windows" || root == "" {
		return
	}
	letter := strings.TrimSuffix(root, string(filepath.Separator))
	if letter == "" {
		return
	}
	driveMu.Lock()
	defer driveMu.Unlock()
	if !usedDrives[letter] {
		return
	}
	_ = exec.Command("subst", letter, "/d").Run()
	delete(usedDrives, letter)
}

// findRepoRoot 从当前目录向上查找 go.mod，用于定位 tokenizer 资产与 .env。
func findRepoRoot() string {
	cwd, err := os.Getwd()
	if err != nil {
		return ""
	}
	dir := cwd
	for {
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
