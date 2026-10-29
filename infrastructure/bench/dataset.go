package bench

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"

	appeval "simpleAgent/application/eval"
)

// Loader 读取 WorkBuddy Office 任务集。
type Loader struct{}

// LoadDataset 遍历任务集目录，返回全部任务（含无效任务）。
//
// 单个任务包损坏不会中断整个任务集：对应 TaskSpec 的 LoadErr 非空，
// 由 Runner 记录 dataset_invalid 失败码，保证“0 静默跳过”。
func (Loader) LoadDataset(ctx context.Context, root string) ([]appeval.TaskSpec, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		return nil, fmt.Errorf("读取任务集目录失败: %w", err)
	}
	version := filepath.Base(filepath.Clean(root))
	tasks := make([]appeval.TaskSpec, 0, len(entries))
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		tasks = append(tasks, loadTask(filepath.Join(root, entry.Name()), version))
	}
	sort.SliceStable(tasks, func(i, j int) bool { return tasks[i].ID < tasks[j].ID })
	return tasks, nil
}

// loadTask 读取单个任务包并解析契约；任何错误写入 LoadErr。
func loadTask(taskDir, datasetVersion string) appeval.TaskSpec {
	spec := appeval.TaskSpec{
		ID:              filepath.Base(taskDir),
		Dir:             taskDir,
		DatasetVersion:  datasetVersion,
		AgentTimeout:    defaultTaskTimeout,
		VerifierTimeout: defaultTaskTimeout,
	}
	instruction := filepath.Join(taskDir, "instruction.md")
	archive := filepath.Join(taskDir, "environment", "workspace.tar.gz")
	verifierPath := filepath.Join(testsDir(taskDir), "verifier.toml")
	judgePath := filepath.Join(testsDir(taskDir), "judge.yaml")

	if err := requireFile(instruction, "instruction.md"); err != nil {
		spec.LoadErr = err.Error()
		return spec
	}
	if err := requireFile(archive, "environment/workspace.tar.gz"); err != nil {
		spec.LoadErr = err.Error()
		return spec
	}
	verifier, err := ParseVerifierTOML(verifierPath)
	if err != nil {
		spec.LoadErr = err.Error()
		return spec
	}
	artifacts, err := ParseJudgeYAML(judgePath)
	if err != nil {
		spec.LoadErr = err.Error()
		return spec
	}
	agentTimeout, verifierTimeout, warnings, err := ParseTaskTOML(filepath.Join(taskDir, "task.toml"))
	if err != nil {
		spec.LoadErr = err.Error()
		return spec
	}

	spec.InstructionPath = instruction
	spec.WorkspaceArchive = archive
	spec.TestsDir = testsDir(taskDir)
	spec.Verifier = verifier
	spec.Artifacts = artifacts
	spec.AgentTimeout = agentTimeout
	spec.VerifierTimeout = verifierTimeout
	spec.Warnings = warnings
	return spec
}
