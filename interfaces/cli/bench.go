// --bench 子命令：命令行入口，负责参数解析、装配与结果展示。
//
// 评测编排逻辑全部在 application/eval，本文件不包含任何评分或文件操作规则。
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	appeval "simpleAgent/application/eval"
	"simpleAgent/infrastructure/bench"
	"simpleAgent/infrastructure/evalstore"
	"simpleAgent/infrastructure/office"
)

// defaultBenchCacheDir 未指定 --runs 时的默认运行根目录（用户缓存目录）。
func defaultBenchCacheDir() (string, error) {
	base, err := os.UserCacheDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(base, "simpleAgent-bench", "runs"), nil
}

// runBench 分派 --bench 子命令。
func runBench(ctx context.Context, args []string) error {
	if len(args) == 0 {
		return errors.New("--bench 用法：--bench run|report|validate [参数]")
	}
	switch args[0] {
	case "run":
		return runBenchRun(ctx, args[1:])
	case "report":
		return runBenchReport(ctx, args[1:])
	case "validate":
		return runBenchValidate(ctx, args[1:])
	default:
		return fmt.Errorf("未知 --bench 子命令 %q，可选 run、report、validate", args[0])
	}
}

// runBenchRun 执行一次评测运行。
func runBenchRun(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("bench run", flag.ContinueOnError)
	dataset := fs.String("dataset", os.Getenv("WB_BENCH_DATASET"), "任务集目录")
	runsRoot := fs.String("runs", os.Getenv("WB_BENCH_RUNS_ROOT"), "运行根目录（默认用户缓存目录）")
	only := fs.String("only", "", "只跑指定任务，逗号分隔")
	modes := fs.String("mode", appeval.ModePlan, "运行模式，逗号分隔：plan,planner_worker")
	maxTasks := fs.Int("max-tasks", 0, "最多运行多少个任务，0 表示不限制")
	concurrency := fs.Int("concurrency", 1, "并发任务数，1-4")
	maxInner := fs.Int("max-inner", 20, "Agent 最大内层步数")
	executor := fs.String("executor", "local", "执行器：local|docker")
	dockerImagePrefix := fs.String("docker-image-prefix", "simpleagent-bench/", "Docker 镜像名前缀")
	dockerNetwork := fs.String("docker-network", "", "容器网络，留空使用 Docker 默认 bridge")
	keepContainers := fs.Bool("keep-containers", false, "运行结束后保留容器，便于排查")
	harnessBin := fs.String("harness-bin", "", "预编译的 Linux harness 二进制路径，留空自动交叉编译")
	rebuildHarness := fs.Bool("rebuild-harness", false, "强制重新交叉编译 Linux harness")
	if err := fs.Parse(args); err != nil {
		return err
	}
	execImpl, err := newBenchExecutor(*executor, bench.DockerExecutorOptions{
		ImagePrefix:    *dockerImagePrefix,
		Network:        *dockerNetwork,
		TokenizerPath:  resolveTokenizerPath(),
		HarnessPath:    *harnessBin,
		RebuildHarness: *rebuildHarness,
		KeepContainers: *keepContainers,
	})
	if err != nil {
		return err
	}
	if strings.TrimSpace(*dataset) == "" {
		return errors.New("必须指定 --dataset")
	}
	root, err := resolveRunsRoot(*runsRoot)
	if err != nil {
		return err
	}

	cfg := appeval.Config{
		Dataset:           *dataset,
		RunsRoot:          root,
		Only:              splitCSV(*only),
		Modes:             splitCSV(*modes),
		MaxTasks:          *maxTasks,
		Concurrency:       *concurrency,
		MaxInner:          *maxInner,
		Model:             os.Getenv("OPENAI_MODEL"),
		ArtifactValidator: office.NewValidator(),
	}
	store := evalstore.New(root)
	runner := appeval.NewRunner(cfg, bench.Loader{}, execImpl, store, store)

	summary, runID, err := runner.Run(ctx)
	if err != nil {
		return err
	}
	printBenchSummary(summary, filepath.Join(root, runID))
	return nil
}

// runBenchValidate 校验任务集能否全部执行，并汇总交付能力缺口。
//
// 只读任务包、不调用模型，用于在动辄数小时的全量运行前先确认
// “50/50 可执行、0 静默跳过”，并暴露当前写能力不覆盖的产物类型。
func runBenchValidate(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("bench validate", flag.ContinueOnError)
	dataset := fs.String("dataset", os.Getenv("WB_BENCH_DATASET"), "任务集目录")
	writable := fs.String("writable", "xlsx,md,json,txt,csv,text", "当前支持写出的产物类型，逗号分隔")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if strings.TrimSpace(*dataset) == "" {
		return errors.New("必须指定 --dataset")
	}
	tasks, err := bench.Loader{}.LoadDataset(ctx, *dataset)
	if err != nil {
		return err
	}
	summary := appeval.AnalyzeCoverage(tasks, splitCSV(*writable))

	fmt.Printf("\n==== 任务集校验 %s ====\n", *dataset)
	fmt.Printf("任务总数: %d  可执行: %d  不可执行: %d\n",
		summary.Total, summary.Valid, summary.Invalid)
	if len(summary.Issues) > 0 {
		fmt.Println("不可执行任务：")
		for _, issue := range summary.Issues {
			fmt.Printf("  %s -> %s\n", issue.TaskID, issue.Reason)
		}
	}
	if len(summary.ArtifactTypes) > 0 {
		fmt.Printf("产物类型需求（按任务去重）: %s\n", formatTypeCounts(summary.ArtifactTypes))
	}
	if len(summary.MissingArtifactTypes) > 0 {
		fmt.Printf("写能力缺口: %s\n", strings.Join(summary.MissingArtifactTypes, ","))
	} else {
		fmt.Println("写能力缺口: 无")
	}
	if summary.Invalid > 0 {
		return fmt.Errorf("有 %d 个任务包不可执行", summary.Invalid)
	}
	return nil
}

// formatTypeCounts 把产物类型计数格式化为稳定顺序的 key=count 字符串。
func formatTypeCounts(counts map[string]int) string {
	kinds := make([]string, 0, len(counts))
	for kind := range counts {
		kinds = append(kinds, kind)
	}
	sort.Strings(kinds)
	parts := make([]string, 0, len(kinds))
	for _, kind := range kinds {
		parts = append(parts, fmt.Sprintf("%s=%d", kind, counts[kind]))
	}
	return strings.Join(parts, " ")
}

// newBenchExecutor 按名称装配执行器。
//
// local 用于本机快速调试（不计入正式分数）；docker 使用任务自带镜像，
// 是唯一可复现、可对标 WorkBuddy Bench 的执行方式。
func newBenchExecutor(name string, dockerOpts bench.DockerExecutorOptions) (appeval.Executor, error) {
	switch strings.ToLower(strings.TrimSpace(name)) {
	case "", "local":
		return bench.NewLocalExecutor(), nil
	case "docker":
		return bench.NewDockerExecutor(dockerOpts), nil
	default:
		return nil, fmt.Errorf("不支持的执行器 %q，可选 local、docker", name)
	}
}

// runBenchReport 读取并展示一次历史运行。
func runBenchReport(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("bench report", flag.ContinueOnError)
	runID := fs.String("run-id", "", "运行 ID（必填）")
	runsRoot := fs.String("runs", os.Getenv("WB_BENCH_RUNS_ROOT"), "运行根目录")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if strings.TrimSpace(*runID) == "" {
		return errors.New("必须指定 --run-id")
	}
	root, err := resolveRunsRoot(*runsRoot)
	if err != nil {
		return err
	}
	store := evalstore.New(root)
	summary, err := store.LoadSummary(ctx, *runID)
	if err != nil {
		return err
	}
	reports, err := store.List(ctx, *runID)
	if err != nil {
		return err
	}
	printBenchSummary(summary, filepath.Join(root, *runID))
	if len(reports) > 0 {
		fmt.Println("\n报告文件：")
		for _, r := range reports {
			fmt.Printf("  %s/%s -> report.json\n", r.TaskID, r.Mode)
		}
	}
	return nil
}

// resolveRunsRoot 解析运行根目录，拒绝写入仓库内部。
func resolveRunsRoot(flagValue string) (string, error) {
	root := flagValue
	if strings.TrimSpace(root) == "" {
		var err error
		root, err = defaultBenchCacheDir()
		if err != nil {
			return "", fmt.Errorf("无法确定默认运行目录: %w", err)
		}
	}
	abs, err := filepath.Abs(root)
	if err != nil {
		return "", err
	}
	if cwd, err := os.Getwd(); err == nil {
		rel, relErr := filepath.Rel(filepath.Clean(cwd), abs)
		if relErr == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			return "", fmt.Errorf("运行目录不能位于仓库内部: %s", abs)
		}
	}
	return abs, nil
}

// splitCSV 拆分逗号分隔参数并去除空白。
func splitCSV(v string) []string {
	if strings.TrimSpace(v) == "" {
		return nil
	}
	parts := strings.Split(v, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if s := strings.TrimSpace(p); s != "" {
			out = append(out, s)
		}
	}
	return out
}

// printBenchSummary 输出人类可读的运行汇总。
func printBenchSummary(summary appeval.Summary, runDir string) {
	fmt.Printf("\n==== 评测汇总 %s ====\n", summary.RunID)
	fmt.Printf("任务: %d  完成: %d  失败: %d  超步: %d  取消: %d  需人工: %d\n",
		summary.Total, summary.Completed, summary.Failed, summary.Limit, summary.Cancelled, summary.NeedsInput)
	fmt.Printf("平均通过率: %.2f%%  中位通过率: %.2f%%\n", summary.AvgRate*100, summary.MedianRate*100)
	fmt.Printf("产物总数: %d  LLM 调用: %d  平均耗时: %dms\n",
		summary.ArtifactCount, summary.TotalLLMCalls, summary.AvgDurationMS)
	if len(summary.FailureCodes) > 0 {
		fmt.Println("失败码分布:")
		for code, count := range summary.FailureCodes {
			fmt.Printf("  %-24s %d\n", code, count)
		}
	}
	fmt.Println("任务明细:")
	for _, t := range summary.Tasks {
		fmt.Printf("  %-40s %-12s 通过率=%6.2f%%  产物=%d  耗时=%dms\n",
			t.TaskID, t.Status, t.Rate*100, t.Artifacts, t.DurationMS)
		if len(t.Failures) > 0 {
			fmt.Printf("      失败码: %s\n", strings.Join(t.Failures, ", "))
		}
	}
	fmt.Printf("\n运行目录: %s\n", runDir)
}
