// Package eval 定义评测结果（EvalReport）的领域模型与报告仓储接口。
//
// 它只描述“一次任务运行留下了什么证据、得到了什么分数、失败原因是什么”，
// 不依赖 Docker、Python、TOML 或任何具体执行方式。
package eval

import (
	"context"
	"time"

	"simpleAgent/domain/artifact"
)

// SchemaVersion 报告 schema 版本，落盘后供下游校验。
const SchemaVersion = "simpleagent.eval.v1"

// Status 一次任务运行的终态。
type Status string

const (
	StatusCompleted  Status = "completed"   // 正常收尾且评分器执行完成
	StatusFailed     Status = "failed"      // 失败码导致无法得到有效评分
	StatusLimit      Status = "limit"       // Agent 达到步数上限
	StatusCancelled  Status = "cancelled"   // 用户中断
	StatusNeedsInput Status = "needs_input" // 暂停于外部工具，需要人工继续
)

// IsTerminal 判断状态是否为终态。当前枚举全部是终态。
func (s Status) IsTerminal() bool {
	switch s {
	case StatusCompleted, StatusFailed, StatusLimit, StatusCancelled, StatusNeedsInput:
		return true
	}
	return false
}

// FailureCode 失败归因码，覆盖环境、Agent、产物与评分器四类问题。
type FailureCode string

const (
	FailDatasetInvalid   FailureCode = "dataset_invalid"
	FailWorkspaceExtract FailureCode = "workspace_extract_failed"
	FailImageBuild       FailureCode = "image_build_failed"
	FailContainerStart   FailureCode = "container_start_failed"
	FailAgentTimeout     FailureCode = "agent_timeout"
	FailAgentError       FailureCode = "agent_error"
	FailAgentLimit       FailureCode = "agent_limit"
	FailNoFinalAnswer    FailureCode = "no_final_answer"
	FailNoArtifact       FailureCode = "no_artifact"
	FailArtifactMissing  FailureCode = "artifact_missing"
	FailArtifactInvalid  FailureCode = "artifact_invalid"
	FailVerifierTimeout  FailureCode = "verifier_timeout"
	FailVerifierError    FailureCode = "verifier_error"
	FailNeedsInput       FailureCode = "needs_input"
	FailCancelled        FailureCode = "cancelled"
	FailResultFallback   FailureCode = "result_fallback"
)

// Failure 一条可归因的失败记录。
type Failure struct {
	Code    FailureCode `json:"code"`
	Message string      `json:"message"`
	Scope   string      `json:"scope,omitempty"` // agent / artifact / verifier / environment
}

// Grade 一个维度的评分。
type Grade struct {
	Passed int     `json:"passed"`
	Total  int     `json:"total"`
	Rate   float64 `json:"rate"`
}

// Grades 汇总规则评分与（尚未接入的）Judge 评分。
type Grades struct {
	Rule  *Grade  `json:"rule"`
	Judge *Grade  `json:"judge"`
	Total float64 `json:"total"`
}

// Efficiency 单任务效率指标。
type Efficiency struct {
	LLMCalls        int64  `json:"llm_calls"`
	MaxWindowTokens int64  `json:"max_window_tokens"`
	DurationMS      int64  `json:"duration_ms"`
	TokenUsage      *int64 `json:"token_usage"`
}

// CaseResult 评分器中的单个用例结果。
type CaseResult struct {
	Name   string `json:"name"`
	Status string `json:"status"` // passed / failure / error / skipped
	Detail string `json:"detail,omitempty"`
}

// VerifierResult 官方评分器一次执行的原始结果。
type VerifierResult struct {
	Command    string       `json:"command"`
	Cwd        string       `json:"cwd"`
	ExitCode   int          `json:"exit_code"`
	OutputPath string       `json:"output_path,omitempty"`
	ResultXML  string       `json:"result_xml,omitempty"`
	Tests      int          `json:"tests"`
	Failures   int          `json:"failures"`
	Errors     int          `json:"errors"`
	Skipped    int          `json:"skipped"`
	Cases      []CaseResult `json:"cases,omitempty"`
	TimedOut   bool         `json:"timed_out,omitempty"`
}

// EnvironmentInfo 记录本次运行的环境标识，便于跨版本比较。
type EnvironmentInfo struct {
	ExecutorMode   string `json:"executor_mode"`
	DatasetVersion string `json:"dataset_version"`
	Model          string `json:"model"`
	HarnessVersion string `json:"harness_version"`
}

// Report 单个任务的统一评测报告。
type Report struct {
	SchemaVersion string              `json:"schema_version"`
	RunID         string              `json:"run_id"`
	TaskID        string              `json:"task_id"`
	Mode          string              `json:"mode"`
	Status        Status              `json:"status"`
	StartedAt     time.Time           `json:"started_at"`
	FinishedAt    time.Time           `json:"finished_at"`
	DurationMS    int64               `json:"duration_ms"`
	Artifacts     []artifact.Artifact `json:"artifacts"`
	Grades        Grades              `json:"grades"`
	Efficiency    Efficiency          `json:"efficiency"`
	Failures      []Failure           `json:"failures"`
	Verifier      VerifierResult      `json:"verifier"`
	TraceID       string              `json:"trace_id,omitempty"`
	Environment   EnvironmentInfo     `json:"environment"`
}

// Repository 评测报告仓储端口。
//
// 实现位于 infrastructure/evalstore，领域层只依赖该接口。
type Repository interface {
	// Save 持久化单任务报告。
	Save(ctx context.Context, report Report) error
	// Load 读取指定运行的单个任务报告。
	Load(ctx context.Context, runID, taskID, mode string) (Report, error)
	// List 列出指定运行下的全部任务报告。
	List(ctx context.Context, runID string) ([]Report, error)
}
