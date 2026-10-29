// Package eval 是评测 harness 的应用编排层。
//
// 它定义任务描述、执行器端口与报告汇总规则，负责把
// “准备 → 运行 → 采集 → 评分 → 报告”串起来；
// 具体文件、Docker、Python 调用等外部细节由 infrastructure/bench 实现。
package eval

import (
	"context"
	"errors"
	"fmt"
	"time"

	domaineval "simpleAgent/domain/eval"
)

// 运行模式常量。
const (
	ModePlan          = "plan"
	ModePlannerWorker = "planner_worker"
)

// DeclaredArtifact judge.yaml 中声明的一条交付物契约。
type DeclaredArtifact struct {
	ID       string `json:"id"`
	Path     string `json:"path"`
	Type     string `json:"type"`
	Required bool   `json:"required"`
}

// VerifierSpec 官方评分入口（来自 tests/verifier.toml）。
type VerifierSpec struct {
	Cwd     string            `json:"cwd"`
	Command string            `json:"command"`
	Env     map[string]string `json:"env"`
}

// TaskSpec 一个可执行任务包的完整描述。
type TaskSpec struct {
	ID               string
	Dir              string
	InstructionPath  string
	WorkspaceArchive string
	TestsDir         string
	Verifier         VerifierSpec
	Artifacts        []DeclaredArtifact
	DatasetVersion   string
	AgentTimeout     time.Duration
	VerifierTimeout  time.Duration
	Warnings         []string
	// LoadErr 非空表示任务包读取失败（缺少必填文件或契约非法），
	// Runner 会记录 dataset_invalid，而不是静默跳过。
	LoadErr string
}

// Environment 一次任务运行的工作目录布局。
type Environment struct {
	Workspace string // 工作区（对应容器内 /workspace）
	Tests     string // 任务测试目录副本（对应容器内 /tests）
	Logs      string // 运行日志目录（对应容器内 /logs）
	// AgentState Agent 运行状态目录，存放会话库、审计与 final_answer.md。
	// 与工作区隔离，避免框架自身文件混入被评分器校验的工作区。
	AgentState string
	// ContainerRoot 本地模式下模拟容器根目录的虚拟盘符根（如 X:\）。
	// 为空表示不做容器路径模拟，仅依赖路径翻译。
	ContainerRoot string
}

// Metrics 单任务效率指标。
type Metrics struct {
	CompactionRounds     int64 `json:"compaction_rounds"`
	CompactedEntries     int64 `json:"compacted_entries"`
	FoldedToolResults    int64 `json:"folded_tool_results"`
	DroppedWorkerEntries int64 `json:"dropped_worker_entries"`
	MaxWindowTokens      int64 `json:"max_window_tokens"`
	LLMCalls             int64 `json:"llm_calls"`
}

// AgentRequest 一次 Agent 运行请求。
type AgentRequest struct {
	Mode            string
	InstructionPath string
	MaxInner        int
	ResultPath      string
}

// AgentResult Agent 运行结果；状态取值与 --office 的 state 一致。
type AgentResult struct {
	State     string  `json:"state"` // final / limit / tool / failed
	SessionID string  `json:"session_id"`
	Answer    string  `json:"answer"`
	Metrics   Metrics `json:"metrics"`
	Err       string  `json:"error"`
	Fallback  bool    `json:"-"` // 结构化结果缺失时为 true
}

// FileMeta 工作区快照中的单个文件元数据。
type FileMeta struct {
	Size    int64     `json:"size"`
	ModTime time.Time `json:"mod_time"`
	Hash    string    `json:"sha256"`
}

// ExecutorError 执行器上报的、可归因到领域失败码的错误。
//
// 执行器（本地 / Docker）用它在基础设施层把“镜像构建失败”“容器启动失败”
// 这类环境问题与普通错误区分开，Runner 据此写入准确的 failure code，
// 避免所有 Prepare 错误都被笼统记成 workspace_extract_failed。
type ExecutorError struct {
	Code domaineval.FailureCode // 领域失败码
	Op   string                 // 出错的操作，如 build-image / start-container
	Err  error                  // 原始错误
}

// Error 实现 error 接口，保留操作上下文便于日志定位。
func (e *ExecutorError) Error() string {
	return fmt.Sprintf("%s: %v", e.Op, e.Err)
}

// Unwrap 暴露原始错误，支持 errors.Is / errors.As 链式判断。
func (e *ExecutorError) Unwrap() error { return e.Err }

// NewExecutorError 构造可归因的执行器错误；err 为 nil 时返回 nil。
func NewExecutorError(code domaineval.FailureCode, op string, err error) error {
	if err == nil {
		return nil
	}
	return &ExecutorError{Code: code, Op: op, Err: err}
}

// FailureCodeOf 提取执行器错误携带的失败码；不是 ExecutorError 时返回空串。
func FailureCodeOf(err error) domaineval.FailureCode {
	var execErr *ExecutorError
	if errors.As(err, &execErr) {
		return execErr.Code
	}
	return ""
}

// Executor 评测执行器端口。
//
// 本地与 Docker 执行器都实现该接口；应用层不直接调用 exec.Command。
type Executor interface {
	// Prepare 准备工作区（解压、复制测试目录），返回目录布局。
	Prepare(ctx context.Context, task TaskSpec, runDir string) (Environment, error)
	// SnapshotWorkspace 对工作区做一次只读快照。
	SnapshotWorkspace(ctx context.Context, root string) (map[string]FileMeta, error)
	// RunAgent 执行被测 Agent。
	RunAgent(ctx context.Context, env Environment, req AgentRequest) (AgentResult, error)
	// RunVerifier 按官方契约执行评分器。
	RunVerifier(ctx context.Context, env Environment, spec VerifierSpec) (domaineval.VerifierResult, error)
	// Cleanup 清理执行环境；本地执行器保留工作区以便复核。
	Cleanup(ctx context.Context, env Environment) error
	// Mode 返回执行器标识，写入报告 environment.executor_mode。
	Mode() string
}

// TaskSource 任务包读取端口，由 infrastructure/bench 实现。
type TaskSource interface {
	LoadDataset(ctx context.Context, root string) ([]TaskSpec, error)
}
