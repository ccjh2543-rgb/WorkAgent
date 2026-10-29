package eval

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"simpleAgent/domain/artifact"
	domaineval "simpleAgent/domain/eval"
)

// 默认运行参数。
const (
	defaultAgentTimeout    = 30 * time.Minute
	defaultVerifierTimeout = 30 * time.Minute
	defaultMaxInner        = 20
	harnessVersion         = "simpleagent-bench/0.1"
)

// Config 一次评测运行的全部配置。
type Config struct {
	Dataset     string   // 任务集目录
	RunsRoot    string   // 运行根目录
	Only        []string // 只跑指定任务
	Modes       []string // plan / planner_worker
	MaxTasks    int      // 最多跑多少个任务，0 表示不限制
	Concurrency int      // 并发任务数
	MaxInner    int      // Agent 最大内层步数
	Model       string   // 模型名，写入报告
	// ArtifactValidator 二进制交付物的格式校验器，nil 时跳过格式校验。
	ArtifactValidator artifact.FormatValidator
}

// TaskSummary 汇总中的单任务条目。
type TaskSummary struct {
	TaskID     string   `json:"task_id"`
	Mode       string   `json:"mode"`
	Status     string   `json:"status"`
	Rate       float64  `json:"rate"`
	Artifacts  int      `json:"artifacts"`
	DurationMS int64    `json:"duration_ms"`
	LLMCalls   int64    `json:"llm_calls"`
	Failures   []string `json:"failures,omitempty"`
}

// Summary 一次运行的总览。
type Summary struct {
	SchemaVersion string         `json:"schema_version"`
	RunID         string         `json:"run_id"`
	StartedAt     time.Time      `json:"started_at"`
	FinishedAt    time.Time      `json:"finished_at"`
	DurationMS    int64          `json:"duration_ms"`
	Dataset       string         `json:"dataset"`
	Executor      string         `json:"executor"`
	Model         string         `json:"model"`
	Total         int            `json:"total"`
	Completed     int            `json:"completed"`
	Failed        int            `json:"failed"`
	Limit         int            `json:"limit"`
	Cancelled     int            `json:"cancelled"`
	NeedsInput    int            `json:"needs_input"`
	AvgRate       float64        `json:"avg_rate"`
	MedianRate    float64        `json:"median_rate"`
	ArtifactCount int            `json:"artifact_count"`
	AvgDurationMS int64          `json:"avg_duration_ms"`
	TotalLLMCalls int64          `json:"total_llm_calls"`
	FailureCodes  map[string]int `json:"failure_codes"`
	Tasks         []TaskSummary  `json:"tasks"`
}

// SummaryStore 汇总落盘端口，由 infrastructure/evalstore 实现。
type SummaryStore interface {
	SaveSummary(ctx context.Context, runID string, summary Summary) error
}

// Runner 评测编排器。
//
// 它只依赖应用层端口（TaskSource / Executor / Repository），
// 不直接调用文件系统、Docker 或 Python。
type Runner struct {
	cfg      Config
	source   TaskSource
	executor Executor
	store    domaineval.Repository
	summary  SummaryStore
}

// NewRunner 装配评测编排器。
func NewRunner(cfg Config, source TaskSource, executor Executor, store domaineval.Repository, summary SummaryStore) *Runner {
	return &Runner{cfg: cfg, source: source, executor: executor, store: store, summary: summary}
}

// Run 执行一次评测并返回汇总。
//
// 单个任务失败不会取消其他任务；返回的 error 只表示运行级别无法继续
// （例如任务集不存在、运行目录非法）。
func (r *Runner) Run(ctx context.Context) (Summary, string, error) {
	if err := r.validate(); err != nil {
		return Summary{}, "", err
	}
	tasks, err := r.source.LoadDataset(ctx, r.cfg.Dataset)
	if err != nil {
		return Summary{}, "", fmt.Errorf("读取任务集失败: %w", err)
	}
	tasks = r.selectTasks(tasks)
	if len(tasks) == 0 {
		return Summary{}, "", errors.New("没有可执行的任务（检查 --only / --max-tasks）")
	}

	runID := newRunID(time.Now())
	start := time.Now()
	summary := Summary{
		SchemaVersion: domaineval.SchemaVersion,
		RunID:         runID,
		StartedAt:     start,
		Dataset:       r.cfg.Dataset,
		Executor:      r.executor.Mode(),
		Model:         r.cfg.Model,
		FailureCodes:  map[string]int{},
	}

	type job struct {
		task TaskSpec
		mode string
	}
	jobs := make([]job, 0, len(tasks)*len(r.modes()))
	for _, task := range tasks {
		for _, mode := range r.modes() {
			jobs = append(jobs, job{task: task, mode: mode})
		}
	}
	reports := make([]domaineval.Report, len(jobs))

	var wg sync.WaitGroup
	sem := make(chan struct{}, r.cfg.Concurrency)
	for i := range jobs {
		wg.Add(1)
		go func(idx int, j job) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			report := r.runOne(ctx, runID, j.task, j.mode)
			// 单任务完成即落盘：全量运行耗时长，进程中断时已完成的报告不能丢。
			if err := r.store.Save(ctx, report); err != nil {
				fmt.Printf("[报告落盘失败 %s/%s]: %v\n", report.TaskID, report.Mode, err)
			}
			reports[idx] = report
		}(i, jobs[i])
	}
	wg.Wait()

	for _, report := range reports {
		accumulate(&summary, report)
	}
	sort.SliceStable(summary.Tasks, func(i, j int) bool {
		if summary.Tasks[i].TaskID != summary.Tasks[j].TaskID {
			return summary.Tasks[i].TaskID < summary.Tasks[j].TaskID
		}
		return summary.Tasks[i].Mode < summary.Tasks[j].Mode
	})
	summary.FinishedAt = time.Now()
	summary.DurationMS = summary.FinishedAt.Sub(start).Milliseconds()
	Finalize(&summary)
	if err := r.summary.SaveSummary(ctx, runID, summary); err != nil {
		return summary, runID, fmt.Errorf("汇总落盘失败: %w", err)
	}
	return summary, runID, nil
}

// validate 校验运行参数，阻止危险或无效配置。
func (r *Runner) validate() error {
	if r.cfg.Dataset == "" {
		return errors.New("必须指定任务集目录 --dataset")
	}
	if info, err := os.Stat(r.cfg.Dataset); err != nil || !info.IsDir() {
		return fmt.Errorf("任务集目录不可用: %s", r.cfg.Dataset)
	}
	if r.cfg.RunsRoot == "" {
		return errors.New("运行根目录不能为空")
	}
	if r.cfg.Concurrency < 1 || r.cfg.Concurrency > 4 {
		return errors.New("并发数必须在 1-4 之间")
	}
	if r.cfg.MaxTasks < 0 {
		return errors.New("--max-tasks 不能为负数")
	}
	for _, m := range r.modes() {
		if m != ModePlan && m != ModePlannerWorker {
			return fmt.Errorf("不支持的运行模式 %q，可选 plan、planner_worker", m)
		}
	}
	if r.executor == nil || r.store == nil || r.summary == nil || r.source == nil {
		return errors.New("评测运行器缺少必要依赖")
	}
	return nil
}

// modes 返回去重后的运行模式，默认 plan。
func (r *Runner) modes() []string {
	if len(r.cfg.Modes) == 0 {
		return []string{ModePlan}
	}
	seen := map[string]bool{}
	out := make([]string, 0, len(r.cfg.Modes))
	for _, m := range r.cfg.Modes {
		if !seen[m] {
			seen[m] = true
			out = append(out, m)
		}
	}
	return out
}

// selectTasks 按 --only 与 --max-tasks 过滤任务，保持任务 ID 字典序。
func (r *Runner) selectTasks(tasks []TaskSpec) []TaskSpec {
	sort.SliceStable(tasks, func(i, j int) bool { return tasks[i].ID < tasks[j].ID })
	if len(r.cfg.Only) == 0 {
		if r.cfg.MaxTasks > 0 && len(tasks) > r.cfg.MaxTasks {
			return tasks[:r.cfg.MaxTasks]
		}
		return tasks
	}
	want := make(map[string]bool, len(r.cfg.Only))
	for _, id := range r.cfg.Only {
		want[id] = true
	}
	out := make([]TaskSpec, 0, len(want))
	for _, t := range tasks {
		if want[t.ID] {
			out = append(out, t)
		}
	}
	if r.cfg.MaxTasks > 0 && len(out) > r.cfg.MaxTasks {
		out = out[:r.cfg.MaxTasks]
	}
	return out
}

// runOne 执行单个任务的一个模式，返回统一报告。
func (r *Runner) runOne(ctx context.Context, runID string, task TaskSpec, mode string) (report domaineval.Report) {
	start := time.Now()
	report = domaineval.Report{
		SchemaVersion: domaineval.SchemaVersion,
		RunID:         runID,
		TaskID:        task.ID,
		Mode:          mode,
		Status:        domaineval.StatusFailed,
		StartedAt:     start,
		Artifacts:     []artifact.Artifact{},
		Failures:      []domaineval.Failure{},
		Environment: domaineval.EnvironmentInfo{
			ExecutorMode:   r.executor.Mode(),
			DatasetVersion: task.DatasetVersion,
			Model:          r.cfg.Model,
			HarnessVersion: harnessVersion,
		},
	}
	defer func() {
		report.FinishedAt = time.Now()
		report.DurationMS = report.FinishedAt.Sub(start).Milliseconds()
	}()

	if ctx.Err() != nil {
		report.Status = domaineval.StatusCancelled
		report.Failures = append(report.Failures, domaineval.Failure{
			Code: domaineval.FailCancelled, Scope: "environment", Message: "运行在开始前被取消",
		})
		return report
	}

	if task.LoadErr != "" {
		report.Failures = append(report.Failures, domaineval.Failure{
			Code: domaineval.FailDatasetInvalid, Scope: "environment", Message: task.LoadErr,
		})
		return report
	}

	runDir := filepath.Join(r.cfg.RunsRoot, runID, task.ID, mode)
	env, err := r.executor.Prepare(ctx, task, runDir)
	if err != nil {
		// 执行器可归因的环境错误（镜像构建/容器启动）优先采用自带失败码。
		code := domaineval.FailWorkspaceExtract
		if c := FailureCodeOf(err); c != "" {
			code = c
		}
		report.Failures = append(report.Failures, domaineval.Failure{
			Code: code, Scope: "environment",
			Message: err.Error(),
		})
		return report
	}
	defer func() {
		if cerr := r.executor.Cleanup(context.Background(), env); cerr != nil {
			fmt.Printf("[清理失败 %s/%s]: %v\n", task.ID, mode, cerr)
		}
	}()

	before, err := r.executor.SnapshotWorkspace(ctx, env.Workspace)
	if err != nil {
		// 快照失败不阻断执行，但报告会因此缺少“新增文件”证据。
		before = nil
		fmt.Printf("[快照失败 %s/%s]: %v\n", task.ID, mode, err)
	}

	agentTimeout := task.AgentTimeout
	if agentTimeout <= 0 {
		agentTimeout = defaultAgentTimeout
	}
	maxInner := r.cfg.MaxInner
	if maxInner <= 0 {
		maxInner = defaultMaxInner
	}
	agentCtx, cancel := context.WithTimeout(ctx, agentTimeout)
	agentResult, agentErr := r.executor.RunAgent(agentCtx, env, AgentRequest{
		Mode:            mode,
		InstructionPath: task.InstructionPath,
		MaxInner:        maxInner,
		ResultPath:      filepath.Join(env.Logs, "agent_result.json"),
	})
	cancel()

	report.TraceID = agentResult.SessionID
	report.Efficiency = domaineval.Efficiency{
		LLMCalls:        agentResult.Metrics.LLMCalls,
		MaxWindowTokens: agentResult.Metrics.MaxWindowTokens,
	}
	if agentResult.Fallback {
		report.Failures = append(report.Failures, domaineval.Failure{
			Code: domaineval.FailResultFallback, Scope: "agent",
			Message: "未读取到结构化 Agent 结果，已回退解析 stdout",
		})
	}
	switch {
	case agentErr != nil && errors.Is(agentCtx.Err(), context.DeadlineExceeded):
		report.Status = domaineval.StatusFailed
		report.Failures = append(report.Failures, domaineval.Failure{
			Code: domaineval.FailAgentTimeout, Scope: "agent",
			Message: fmt.Sprintf("Agent 超过任务超时 %s", agentTimeout),
		})
	case agentErr != nil && ctx.Err() != nil:
		report.Status = domaineval.StatusCancelled
		report.Failures = append(report.Failures, domaineval.Failure{
			Code: domaineval.FailCancelled, Scope: "environment", Message: "运行被取消",
		})
	case agentErr != nil:
		report.Status = domaineval.StatusFailed
		// 执行器可归因错误（如容器中途退出）使用自带失败码。
		code := domaineval.FailAgentError
		if c := FailureCodeOf(agentErr); c != "" {
			code = c
		}
		report.Failures = append(report.Failures, domaineval.Failure{
			Code: code, Scope: "agent", Message: agentErr.Error(),
		})
	default:
		switch agentResult.State {
		case "final":
			// 正常收尾，后续由评分器决定分数。
		case "limit":
			report.Status = domaineval.StatusLimit
			report.Failures = append(report.Failures, domaineval.Failure{
				Code: domaineval.FailAgentLimit, Scope: "agent",
				Message: "Agent 达到步数上限，未完成目标",
			})
		case "tool":
			report.Status = domaineval.StatusNeedsInput
			report.Failures = append(report.Failures, domaineval.Failure{
				Code: domaineval.FailNeedsInput, Scope: "agent",
				Message: "Agent 暂停于外部工具，需要人工继续",
			})
		default:
			report.Status = domaineval.StatusFailed
			report.Failures = append(report.Failures, domaineval.Failure{
				Code: domaineval.FailAgentError, Scope: "agent",
				Message: fmt.Sprintf("Agent 未正常收尾，state=%s err=%s", agentResult.State, agentResult.Err),
			})
		}
	}
	if report.Status == domaineval.StatusFailed && ctx.Err() != nil {
		report.Status = domaineval.StatusCancelled
	}

	// 最终答案索引默认写在 Agent 状态目录（与工作区隔离），
	// 兼容执行器未提供状态目录时回退到工作区。
	answerPath := filepath.Join(env.Workspace, "final_answer.md")
	if env.AgentState != "" {
		answerPath = filepath.Join(env.AgentState, "final_answer.md")
	}
	if _, statErr := os.Stat(answerPath); statErr != nil {
		report.Failures = append(report.Failures, domaineval.Failure{
			Code: domaineval.FailNoFinalAnswer, Scope: "agent",
			Message: "未产出 final_answer.md",
		})
	}

	after, snapErr := r.executor.SnapshotWorkspace(ctx, env.Workspace)
	if snapErr != nil {
		after = nil
		fmt.Printf("[运行后快照失败 %s/%s]: %v\n", task.ID, mode, snapErr)
	}

	verifierTimeout := task.VerifierTimeout
	if verifierTimeout <= 0 {
		verifierTimeout = defaultVerifierTimeout
	}
	verifyCtx, verifyCancel := context.WithTimeout(ctx, verifierTimeout)
	verifierResult, verifierErr := r.executor.RunVerifier(verifyCtx, env, task.Verifier)
	verifyCancel()
	report.Verifier = verifierResult
	if verifierErr != nil {
		report.Failures = append(report.Failures, domaineval.Failure{
			Code: domaineval.FailVerifierError, Scope: "verifier", Message: verifierErr.Error(),
		})
	}

	grade, gradeFailure := GradeFromVerifier(verifierResult)
	report.Grades.Rule = &grade
	report.Grades.Total = grade.Rate
	if gradeFailure != nil {
		report.Failures = append(report.Failures, *gradeFailure)
	}

	arts, artFailures := CollectArtifacts(env.Workspace, filepath.Join(env.Logs, "verifier"), task.Artifacts, before, after, r.cfg.ArtifactValidator)
	report.Artifacts = arts
	report.Failures = append(report.Failures, artFailures...)
	if len(arts) == 0 {
		report.Failures = append(report.Failures, domaineval.Failure{
			Code: domaineval.FailNoArtifact, Scope: "artifact",
			Message: "工作区没有任何新增或修改文件，也没有声明的产物",
		})
	}

	// Agent 正常收尾且评分器产出了用例，即视为完成（允许分数很低）。
	if agentErr == nil && agentResult.State == "final" && verifierResult.Tests > 0 && verifierErr == nil {
		report.Status = domaineval.StatusCompleted
	}
	return report
}

// accumulate 把单任务报告累加到汇总。
func accumulate(summary *Summary, report domaineval.Report) {
	summary.Total++
	switch report.Status {
	case domaineval.StatusCompleted:
		summary.Completed++
	case domaineval.StatusFailed:
		summary.Failed++
	case domaineval.StatusLimit:
		summary.Limit++
	case domaineval.StatusCancelled:
		summary.Cancelled++
	case domaineval.StatusNeedsInput:
		summary.NeedsInput++
	}
	summary.ArtifactCount += len(report.Artifacts)
	summary.TotalLLMCalls += report.Efficiency.LLMCalls
	summary.AvgDurationMS += report.DurationMS
	codes := make([]string, 0, len(report.Failures))
	for _, f := range report.Failures {
		summary.FailureCodes[string(f.Code)]++
		codes = append(codes, string(f.Code))
	}
	task := TaskSummary{
		TaskID:     report.TaskID,
		Mode:       report.Mode,
		Status:     string(report.Status),
		Rate:       report.Grades.Total,
		Artifacts:  len(report.Artifacts),
		DurationMS: report.DurationMS,
		LLMCalls:   report.Efficiency.LLMCalls,
		Failures:   codes,
	}
	summary.Tasks = append(summary.Tasks, task)
}

// Finalize 计算需要全部任务完成后的汇总指标（均值、中位数、平均耗时）。
func Finalize(summary *Summary) {
	if summary.Total > 0 {
		summary.AvgDurationMS /= int64(summary.Total)
	}
	rates := make([]float64, 0, len(summary.Tasks))
	var sum float64
	for _, t := range summary.Tasks {
		if t.Status == string(domaineval.StatusCompleted) {
			rates = append(rates, t.Rate)
			sum += t.Rate
		}
	}
	if len(rates) > 0 {
		summary.AvgRate = round4(sum / float64(len(rates)))
		sort.Float64s(rates)
		mid := len(rates) / 2
		if len(rates)%2 == 1 {
			summary.MedianRate = round4(rates[mid])
		} else {
			summary.MedianRate = round4((rates[mid-1] + rates[mid]) / 2)
		}
	}
}

// round4 保留四位小数，避免浮点尾差进入报告。
func round4(v float64) float64 {
	return float64(int64(v*10000+0.5)) / 10000
}

// newRunID 生成“时间戳-随机后缀”的运行 ID。
func newRunID(now time.Time) string {
	buf := make([]byte, 2)
	if _, err := rand.Read(buf); err != nil {
		return now.Format("20060102T150405") + "-0000"
	}
	return now.Format("20060102T150405") + "-" + hex.EncodeToString(buf)
}
