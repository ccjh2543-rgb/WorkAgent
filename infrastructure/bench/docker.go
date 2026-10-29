package bench

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"

	appeval "simpleAgent/application/eval"
	domaineval "simpleAgent/domain/eval"
)

// 容器内固定路径布局，与任务自带的评测脚本约定一致。
const (
	ctrWorkspace    = "/workspace"
	ctrTests        = "/tests"
	ctrLogs         = "/logs"
	ctrAgentState   = "/agentstate"
	ctrHarnessBin   = "/opt/simpleagent/simpleagent"
	ctrTokenizer    = "/opt/simpleagent/tokenizer.json"
	ctrInstruction  = "/opt/task/instruction.md"
	defaultImageTag = "simpleagent-bench/"
	harnessBinary   = "simpleagent-linux-amd64"
)

// 需要透传给容器内 Agent 的模型相关环境变量。
//
// 只透传白名单，避免把宿主机上的其他凭据带进不可信的任务镜像。
var passthroughEnvKeys = []string{
	"OPENAI_API_KEY",
	"OPENAI_BASE_URL",
	"OPENAI_MODEL",
	"MEMORY_EMBEDDING_BASE_URL",
	"MEMORY_EMBEDDING_MODEL",
	"MEMORY_EMBEDDING_API_KEY",
}

// DockerExecutorOptions Docker 执行器的可选配置。
type DockerExecutorOptions struct {
	DockerBin      string // docker CLI 路径，默认 docker
	ImagePrefix    string // 镜像名前缀，默认 simpleagent-bench/
	Network        string // 容器网络，默认空（使用 docker 默认 bridge）
	TokenizerPath  string // 宿主机 tokenizer.json 路径，挂载进容器
	HarnessPath    string // 宿主机 Linux 版 harness 二进制，为空则自动交叉编译
	RebuildHarness bool   // 强制重新交叉编译 harness
	KeepContainers bool   // 运行结束后保留容器，便于排查
}

// dockerSession 一次任务运行对应的容器记录。
type dockerSession struct {
	container string
	image     string
}

// DockerExecutor 在任务自带镜像的容器内执行 Agent 与评分器。
//
// 设计要点：
//   - 镜像来自任务包的 environment/Dockerfile，不额外定制，保证评测环境与官方一致；
//   - 工作区、测试目录、日志、Agent 状态目录全部以 bind mount 方式与宿主机共享，
//     产物与报告因此天然落在宿主机，无需 docker cp；
//   - Agent 以交叉编译出的 Linux harness 二进制只读挂载进容器，通过 docker exec 运行；
//   - 评分器命令直接使用 verifier.toml 的原文，交由容器内 bash 执行，不做路径翻译。
type DockerExecutor struct {
	opts DockerExecutorOptions

	mu       sync.Mutex
	sessions map[string]dockerSession // key: 宿主机工作区绝对路径

	harnessOnce sync.Once
	harnessPath string
	harnessErr  error
}

// NewDockerExecutor 创建 Docker 执行器。
func NewDockerExecutor(opts DockerExecutorOptions) *DockerExecutor {
	if strings.TrimSpace(opts.DockerBin) == "" {
		opts.DockerBin = "docker"
	}
	if strings.TrimSpace(opts.ImagePrefix) == "" {
		opts.ImagePrefix = defaultImageTag
	}
	return &DockerExecutor{opts: opts, sessions: map[string]dockerSession{}}
}

// Mode 返回执行器标识。
func (e *DockerExecutor) Mode() string { return "docker" }

// Prepare 准备工作区、构建任务镜像并启动常驻容器。
func (e *DockerExecutor) Prepare(ctx context.Context, task appeval.TaskSpec, runDir string) (appeval.Environment, error) {
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
	if out, err := e.docker(ctx, "version", "--format", "{{.Server.Version}}"); err != nil {
		// 带上 docker CLI 的原始输出，便于区分“未安装 daemon”与“daemon 未启动”。
		return env, appeval.NewExecutorError(domaineval.FailContainerStart, "docker-daemon",
			fmt.Errorf("%w: %s", err, tailText(out, 500)))
	}
	bin, err := e.ensureHarness(ctx)
	if err != nil {
		return env, appeval.NewExecutorError(domaineval.FailImageBuild, "build-harness", err)
	}
	image, err := e.ensureImage(ctx, task)
	if err != nil {
		return env, appeval.NewExecutorError(domaineval.FailImageBuild, "build-image", err)
	}
	container, err := e.createContainer(ctx, task, env, image, bin)
	if err != nil {
		return env, appeval.NewExecutorError(domaineval.FailContainerStart, "start-container", err)
	}
	e.mu.Lock()
	e.sessions[env.Workspace] = dockerSession{container: container, image: image}
	e.mu.Unlock()
	return env, nil
}

// SnapshotWorkspace 复用基础设施层的工作区快照实现。
func (e *DockerExecutor) SnapshotWorkspace(ctx context.Context, root string) (map[string]appeval.FileMeta, error) {
	return SnapshotWorkspace(ctx, root)
}

// RunAgent 在容器内运行 --office，并优先读取结构化结果文件。
func (e *DockerExecutor) RunAgent(ctx context.Context, env appeval.Environment, req appeval.AgentRequest) (appeval.AgentResult, error) {
	sess, err := e.session(env.Workspace)
	if err != nil {
		return appeval.AgentResult{State: "failed", Err: err.Error()},
			appeval.NewExecutorError(domaineval.FailContainerStart, "container-missing", err)
	}
	logFile, err := os.Create(filepath.Join(env.Logs, "agent.log"))
	if err != nil {
		return appeval.AgentResult{State: "failed", Err: err.Error()}, err
	}
	maxInner := req.MaxInner
	if maxInner <= 0 {
		maxInner = 20
	}
	// 结果文件写在挂载的 /logs 下，宿主机可直接读取同一路径。
	resultInContainer := ctrLogs + "/" + filepath.Base(req.ResultPath)
	args := []string{
		"exec", "-w", ctrWorkspace,
		"-e", "SIMPLEAGENT_OFFICE_RESULT=" + resultInContainer,
		"-e", "SIMPLEAGENT_OFFICE_STATE_DIR=" + ctrAgentState,
		"-e", "SIMPLEAGENT_BENCH_DOCKER=1",
	}
	if strings.TrimSpace(e.opts.TokenizerPath) != "" {
		args = append(args, "-e", "SIMPLEAGENT_TOKENIZER_PATH="+ctrTokenizer)
	}
	for _, kv := range passthroughEnv() {
		args = append(args, "-e", kv)
	}
	args = append(args, sess.container, ctrHarnessBin,
		"--office", req.Mode, ctrWorkspace, ctrInstruction, strconv.Itoa(maxInner))

	cmd := exec.CommandContext(ctx, e.opts.DockerBin, args...)
	cmd.Stdout = logFile
	cmd.Stderr = logFile
	runErr := cmd.Run()
	if closeErr := logFile.Close(); closeErr != nil {
		fmt.Printf("[Agent 日志关闭失败]: %v\n", closeErr)
	}
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return appeval.AgentResult{State: "failed", Err: "Agent 执行超时"},
			appeval.NewExecutorError(domaineval.FailAgentTimeout, "docker-exec-agent", ctx.Err())
	}
	return agentRunOutcome(req.ResultPath, runErr)
}

// RunVerifier 在容器内按 verifier.toml 原文执行评分命令并解析 JUnit。
func (e *DockerExecutor) RunVerifier(ctx context.Context, env appeval.Environment, spec appeval.VerifierSpec) (domaineval.VerifierResult, error) {
	result := domaineval.VerifierResult{Command: spec.Command, Cwd: spec.Cwd}
	sess, err := e.session(env.Workspace)
	if err != nil {
		return result, appeval.NewExecutorError(domaineval.FailContainerStart, "container-missing", err)
	}
	verifierDir := filepath.Join(env.Logs, "verifier")
	if err := os.MkdirAll(verifierDir, 0o755); err != nil {
		return result, err
	}
	execOut := filepath.Join(verifierDir, "exec_output.txt")
	logFile, err := os.Create(execOut)
	if err != nil {
		return result, err
	}
	defer func() { _ = logFile.Close() }()

	args := []string{"exec", "-w", spec.Cwd}
	for _, k := range sortedKeys(spec.Env) {
		args = append(args, "-e", k+"="+spec.Env[k])
	}
	args = append(args, sess.container, "bash", "-lc", spec.Command)

	cmd := exec.CommandContext(ctx, e.opts.DockerBin, args...)
	cmd.Stdout = logFile
	cmd.Stderr = logFile
	runErr := cmd.Run()

	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		result.TimedOut = true
		return result, appeval.NewExecutorError(domaineval.FailVerifierTimeout, "docker-exec-verifier", ctx.Err())
	}
	var exitErr *exec.ExitError
	if runErr != nil && !errors.As(runErr, &exitErr) {
		return result, fmt.Errorf("启动评分命令失败: %w", runErr)
	}
	if exitErr != nil {
		result.ExitCode = exitErr.ExitCode()
	}

	// 契约命令会把 stdout 重定向到 /logs/verifier/test_output.txt；
	// 未重定向时回退到 harness 捕获的 exec 输出。
	outputPath := filepath.Join(verifierDir, "test_output.txt")
	if _, statErr := os.Stat(outputPath); statErr != nil {
		outputPath = execOut
	}
	result.OutputPath = outputPath

	junitPath := filepath.Join(verifierDir, "results.xml")
	result.ResultXML = junitPath
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

// Cleanup 删除容器；默认保留运行目录，便于人工复核产物。
func (e *DockerExecutor) Cleanup(ctx context.Context, env appeval.Environment) error {
	e.mu.Lock()
	sess, ok := e.sessions[env.Workspace]
	delete(e.sessions, env.Workspace)
	e.mu.Unlock()
	if !ok || e.opts.KeepContainers {
		return nil
	}
	if _, err := e.docker(ctx, "rm", "-f", sess.container); err != nil {
		return fmt.Errorf("删除容器 %s 失败: %w", sess.container, err)
	}
	return nil
}

// session 返回工作区对应的容器记录。
func (e *DockerExecutor) session(workspace string) (dockerSession, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	sess, ok := e.sessions[workspace]
	if !ok {
		return dockerSession{}, fmt.Errorf("工作区 %s 没有对应的容器", workspace)
	}
	return sess, nil
}

// ensureHarness 确保宿主机存在 Linux 版 harness 二进制，必要时交叉编译一次。
//
// 容器内不能运行 Windows 可执行文件，因此这里统一按 linux/amd64 交叉编译；
// CGO 关闭，仓库依赖（含 modernc.org/sqlite）均为纯 Go 实现。
func (e *DockerExecutor) ensureHarness(ctx context.Context) (string, error) {
	e.harnessOnce.Do(func() {
		e.harnessPath, e.harnessErr = e.buildHarness(ctx)
	})
	return e.harnessPath, e.harnessErr
}

// buildHarness 返回可用的 harness 二进制路径，已存在且未要求重建时直接复用。
func (e *DockerExecutor) buildHarness(ctx context.Context) (string, error) {
	if p := strings.TrimSpace(e.opts.HarnessPath); p != "" {
		if _, err := os.Stat(p); err != nil {
			return "", fmt.Errorf("指定的 harness 二进制不可用: %w", err)
		}
		return p, nil
	}
	root := findRepoRoot()
	if root == "" {
		return "", errors.New("找不到仓库根目录（go.mod），无法交叉编译 harness")
	}
	cacheBase, err := os.UserCacheDir()
	if err != nil {
		return "", err
	}
	out := filepath.Join(cacheBase, "simpleAgent-bench", "bin", harnessBinary)
	if !e.opts.RebuildHarness {
		if info, statErr := os.Stat(out); statErr == nil && info.Size() > 0 {
			return out, nil
		}
	}
	if err := os.MkdirAll(filepath.Dir(out), 0o755); err != nil {
		return "", err
	}
	cmd := exec.CommandContext(ctx, "go", "build", "-trimpath", "-o", out, "./interfaces/cli")
	cmd.Dir = root
	cmd.Env = append(os.Environ(), "GOOS=linux", "GOARCH=amd64", "CGO_ENABLED=0")
	var buf bytes.Buffer
	cmd.Stdout = &buf
	cmd.Stderr = &buf
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("交叉编译 harness 失败: %w: %s", err, tailText(buf.String(), 2000))
	}
	return out, nil
}

// ensureImage 构建或复用任务镜像，返回镜像标签。
func (e *DockerExecutor) ensureImage(ctx context.Context, task appeval.TaskSpec) (string, error) {
	envDir := filepath.Join(task.Dir, "environment")
	dockerfile := filepath.Join(envDir, "Dockerfile")
	if _, err := os.Stat(dockerfile); err != nil {
		return "", fmt.Errorf("任务缺少 environment/Dockerfile: %w", err)
	}
	tag, err := e.imageTag(task.ID, dockerfile, task.WorkspaceArchive)
	if err != nil {
		return "", err
	}
	// Dockerfile 与工作区归档的内容哈希都进了标签，命中即可直接复用。
	if _, err := e.docker(ctx, "image", "inspect", tag); err == nil {
		return tag, nil
	}
	out, err := e.docker(ctx, "build", "-t", tag, "-f", dockerfile, envDir)
	if err != nil {
		return "", fmt.Errorf("构建镜像 %s 失败: %w: %s", tag, err, tailText(out, 2000))
	}
	return tag, nil
}

// imageTag 由任务 ID 与构建输入内容哈希组成镜像标签。
func (e *DockerExecutor) imageTag(taskID, dockerfile, archive string) (string, error) {
	h := sha256.New()
	for _, p := range []string{dockerfile, archive} {
		f, err := os.Open(p)
		if err != nil {
			return "", fmt.Errorf("读取构建输入失败: %w", err)
		}
		if _, err := io.Copy(h, f); err != nil {
			_ = f.Close()
			return "", fmt.Errorf("计算构建输入哈希失败: %w", err)
		}
		if err := f.Close(); err != nil {
			return "", err
		}
	}
	name := strings.ToLower(strings.ReplaceAll(taskID, "_", "-"))
	return e.opts.ImagePrefix + sanitizeImageName(name) + ":" + hex.EncodeToString(h.Sum(nil))[:12], nil
}

// createContainer 创建并启动常驻容器。
func (e *DockerExecutor) createContainer(ctx context.Context, task appeval.TaskSpec, env appeval.Environment, image, bin string) (string, error) {
	name := containerName(task.ID, env.Workspace)
	args := []string{
		"create", "--name", name, "-w", ctrWorkspace,
		"-v", env.Workspace + ":" + ctrWorkspace,
		"-v", env.Tests + ":" + ctrTests + ":ro",
		"-v", env.Logs + ":" + ctrLogs,
		"-v", env.AgentState + ":" + ctrAgentState,
		"-v", bin + ":" + ctrHarnessBin + ":ro",
	}
	if p := strings.TrimSpace(e.opts.TokenizerPath); p != "" {
		// bind mount 的源文件不存在会让 docker create 直接失败，这里先确认再挂载。
		if _, err := os.Stat(p); err == nil {
			args = append(args, "-v", p+":"+ctrTokenizer+":ro")
		}
	}
	if p := strings.TrimSpace(task.InstructionPath); p != "" {
		args = append(args, "-v", p+":"+ctrInstruction+":ro")
	}
	if net := strings.TrimSpace(e.opts.Network); net != "" {
		args = append(args, "--network", net)
	}
	args = append(args, image, "sleep", "infinity")

	if out, err := e.docker(ctx, args...); err != nil {
		return "", fmt.Errorf("创建容器失败: %w: %s", err, tailText(out, 2000))
	}
	if out, err := e.docker(ctx, "start", name); err != nil {
		// 启动失败时清理残留容器，避免同名容器阻塞后续运行。
		_, _ = e.docker(context.Background(), "rm", "-f", name)
		return "", fmt.Errorf("启动容器失败: %w: %s", err, tailText(out, 2000))
	}
	return name, nil
}

// docker 执行一条 docker 命令并返回合并输出。
func (e *DockerExecutor) docker(ctx context.Context, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, e.opts.DockerBin, args...)
	var buf bytes.Buffer
	cmd.Stdout = &buf
	cmd.Stderr = &buf
	err := cmd.Run()
	return buf.String(), err
}

// containerName 由任务 ID 与工作区路径哈希生成稳定且唯一的容器名。
func containerName(taskID, workspace string) string {
	sum := sha256.Sum256([]byte(workspace))
	name := sanitizeImageName(strings.ToLower(taskID))
	return "sa-bench-" + name + "-" + hex.EncodeToString(sum[:])[:6]
}

// sanitizeImageName 把任意任务 ID 转成合法镜像/容器名片段。
func sanitizeImageName(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '-', r == '_', r == '.':
			b.WriteRune(r)
		default:
			b.WriteByte('-')
		}
	}
	out := strings.Trim(b.String(), "-.")
	if out == "" {
		return "task"
	}
	return out
}

// passthroughEnv 收集需要透传的模型环境变量，格式为 KEY=VALUE。
func passthroughEnv() []string {
	out := make([]string, 0, len(passthroughEnvKeys))
	for _, k := range passthroughEnvKeys {
		if v := strings.TrimSpace(os.Getenv(k)); v != "" {
			out = append(out, k+"="+v)
		}
	}
	return out
}

// sortedKeys 返回 map 的稳定排序键，保证 docker 参数可复现。
func sortedKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// tailText 截取输出尾部，避免超长构建日志灌进报告。
func tailText(s string, limit int) string {
	s = strings.TrimSpace(s)
	if len(s) <= limit {
		return s
	}
	return "..." + s[len(s)-limit:]
}
