#!/usr/bin/env python3
"""WorkBuddy Bench Office 评测 runner（simpleAgent 复用）。

原理：
  1. 对每个任务，把 environment/workspace.tar.gz 解压到一个干净的运行目录 work_dir。
  2. 用 simpleAgent 的 --office CLI（两种模式 plan / planner_worker）在该目录内
     全自动读取/写入真实文件，并把最终答案写入 work_dir/final_answer.md。
  3. 用任务自带的 tests/grading/eval_core.py 对 final_answer 做确定性打分
     （跳过 LLM judge，只取 deterministic pass_rate / passed / total 分）。

文本可行过滤：只跑 workspace 内不含 xlsx/pptx/pdf/docx 等二进制办公文件的任务，
因为 simpleAgent 的文件工具只支持文本读写。

用法：
  python scripts/office_run.py [--tasks <dir>] [--runs <dir>] [--modes plan,planner_worker]
       [--max-inner 20] [--only t1,t2] [--no-build] [--parallel N]
"""
from __future__ import annotations

import argparse
import json
import os
import re
import shutil
import subprocess
import sys
import tarfile
from concurrent.futures import ThreadPoolExecutor, as_completed
from pathlib import Path

# 说明：早期版本按后缀把 xlsx/pptx/pdf/docx 判定为“不可执行”并跳过。
# Go 侧已支持四种格式读取，这里不再做后缀过滤，缺能力应在运行中显式失败。

DEFAULT_TASKS = (
    r"F:\zhuomian\workbuddy-bench-main\datasets\wb-bench-office-v1.0\tasks"
)
DEFAULT_RUNS = os.path.join(os.path.dirname(os.path.abspath(__file__)), "..", "office_runs")
DEFAULT_MODES = ["plan", "planner_worker"]


def task_is_feasible(task_dir: Path) -> tuple[bool, list[str]]:
    """判断任务是否可执行：工作区归档可读，且 eval_core 为统一评分接口。

    仅检查“能不能跑、能不能评分”，不按产物格式过滤——
    否则含 xlsx/pptx/pdf/docx 的任务会被静默跳过。
    """
    reasons: list[str] = []
    tgz = task_dir / "environment" / "workspace.tar.gz"
    if not tgz.exists():
        return False, ["no workspace.tar.gz"]
    try:
        with tarfile.open(tgz, "r:gz") as tar:
            tar.getmembers()
    except Exception as exc:  # pragma: no cover - 仅在损坏归档时触发
        return False, [f"untar error: {exc}"]
    # 统一评分接口：eval_core.py 需支持 "--case-dir" 且 gold 可经
    # WB_BENCH_GOLD_PATH 环境变量覆盖（这是 WorkBuddy Office 的主流约定）。
    # 答案的传入方式（位置参数 answer_file 或工作区 root）在 run_one 里自适应尝试。
    eval_core = task_dir / "tests" / "grading" / "eval_core.py"
    if eval_core.exists():
        text = eval_core.read_text(encoding="utf-8", errors="replace")
        has_case_dir = '"--case-dir"' in text
        has_gold_env = "WB_BENCH_GOLD_PATH" in text
        if not (has_case_dir and has_gold_env):
            reasons.append("eval_core 非统一接口")
    else:
        reasons.append("no eval_core.py")
    return not reasons, reasons


def extract_workspace(tgz: Path, dest: Path) -> None:
    """解压 workspace.tar.gz 到 dest（清空重建）。"""
    if dest.exists():
        shutil.rmtree(dest)
    dest.mkdir(parents=True, exist_ok=True)
    with tarfile.open(tgz, "r:gz") as tar:
        # 防御性：拒绝解压时路径越界
        for m in tar.getmembers():
            target = (dest / m.name).resolve()
            if not target.is_relative_to(dest.resolve()):
                raise ValueError(f"suspicious path in archive: {m.name}")
        tar.extractall(dest)


def run_one(exe: Path, repo: Path, task: Path, work_dir: Path, mode: str, max_inner: int) -> dict:
    """在 work_dir 内驱动一次模式运行并评分。"""
    inst_file = task / "instruction.md"
    gold_path = task / "tests" / "gold" / "gold_answer.json"
    eval_core = task / "tests" / "grading" / "eval_core.py"
    record: dict = {"task": task.name, "mode": mode, "ok": False}

    if not exe.exists() or not inst_file.exists() or not gold_path.exists() or not eval_core.exists():
        record["error"] = "missing exe/instruction/gold/eval_core"
        return record

    work_dir.mkdir(parents=True, exist_ok=True)
    tgz = task / "environment" / "workspace.tar.gz"
    try:
        extract_workspace(tgz, work_dir)
    except Exception as exc:
        record["error"] = f"extract: {exc}"
        return record

    # 1) 跑 agent（cwd=repo，保证 .env / tokenizer 相对路径正确；CLI 内部会 chdir 到 work_dir）
    cmd = [str(exe), "--office", mode, str(work_dir), str(inst_file), str(max_inner)]
    try:
        proc = subprocess.run(
            cmd, cwd=repo, capture_output=True, text=True,
            timeout=3600, encoding="utf-8", errors="replace",
        )
    except subprocess.TimeoutExpired:
        record["error"] = "agent timeout"
        return record
    record["stdout"] = proc.stdout

    # 从 stdout 取 state 与 metrics（llmCalls 等）
    m_state = re.search(r"\[office:\w+\] state=(\w+)", proc.stdout)
    record["state"] = m_state.group(1) if m_state else "unknown"
    m_met = re.search(r"\[office:\w+\] metrics=rounds:\d+ entries:\d+ folded:\d+ dropped:\d+ maxWin:\d+ llmCalls:(\d+)", proc.stdout)
    record["llm_calls"] = int(m_met.group(1)) if m_met else None
    record["agent_exit"] = proc.returncode

    answer_file = work_dir / "final_answer.md"
    if not answer_file.exists():
        record["error"] = "no final_answer.md produced"
        return record

    # 2) 确定性评分。WorkBuddy 各任务 eval_core.py 接口不统一，这里自适应尝试：
    #    a) answer-file 型：位置参数是最终答案文件（api-usage 风格）；
    #    b) workspace-root 型：位置参数是工作区根目录，答案从工作区内文件读取
    #       （如 final_answer.md）。取首个能解析出 total_count>0 JSON 的调用。
    env = dict(os.environ)
    env["WB_BENCH_GOLD_PATH"] = str(gold_path)
    last = {}
    for score_args in (
        (str(answer_file), "--case-dir", str(work_dir)),
        (str(work_dir), "--case-dir", str(work_dir)),
    ):
        sc = [sys.executable, str(eval_core)] + list(score_args)
        try:
            sp = subprocess.run(sc, cwd=repo, capture_output=True, text=True, timeout=600, encoding="utf-8", errors="replace")
        except subprocess.TimeoutExpired:
            record["error"] = "eval timeout"
            return record
        try:
            result = json.loads(sp.stdout or sp.stderr)
        except json.JSONDecodeError:
            record["error"] = "eval non-json: " + (sp.stdout or sp.stderr)[-300:]
            continue
        last = result
        if isinstance(result, dict) and result.get("total_count", 0) > 0:
            break
    record["passed"] = last.get("passed_count")
    record["total"] = last.get("total_count")
    record["pass_rate"] = last.get("pass_rate")
    record["error"] = None if last.get("total_count") else (record.get("error") or "eval produced no score")
    record["ok"] = last.get("total_count", 0) > 0
    return record


def main() -> int:
    ap = argparse.ArgumentParser()
    ap.add_argument("--tasks", default=DEFAULT_TASKS)
    ap.add_argument("--runs", default=DEFAULT_RUNS)
    ap.add_argument("--modes", default=",".join(DEFAULT_MODES))
    ap.add_argument("--max-inner", type=int, default=20)
    ap.add_argument("--only", default="", help="逗号分隔的任务ID，尝跑用")
    ap.add_argument("--no-build", action="store_true")
    ap.add_argument("--parallel", type=int, default=1)
    args = ap.parse_args()

    tasks_dir = Path(args.tasks)
    if not tasks_dir.is_dir():
        print(f"任务目录不存在: {tasks_dir}", file=sys.stderr)
        return 2
    modes = [m for m in args.modes.split(",") if m in ("plan", "planner_worker")]
    runs_root = Path(args.runs)

    # 收集可执行任务（不按产物格式过滤）
    only_ids = {s.strip() for s in args.only.split(",") if s.strip()}
    feasible, skipped = [], []
    for d in sorted(tasks_dir.iterdir()):
        if not d.is_dir():
            continue
        if only_ids and d.name not in only_ids:
            continue
        ok, bad = task_is_feasible(d)
        if ok:
            feasible.append(d)
        else:
            skipped.append((d.name, bad[:2]))
    print(f"可执行任务 {len(feasible)} 个；跳过 {len(skipped)} 个：{skipped[:5]}……")

    repo = Path(__file__).resolve().parent.parent
    exe = runs_root.parent / "office-runner" / ("office-runner.exe")
    if not args.no_build:
        exe.parent.mkdir(parents=True, exist_ok=True)
        print("构建 office CLI ...")
        b = subprocess.run(
            ["go", "build", "-o", str(exe), "./interfaces/cli"], cwd=repo,
            capture_output=True, text=True,
        )
        if b.returncode != 0:
            print("构建失败:\n" + b.stdout + b.stderr, file=sys.stderr)
            return 1
        print("构建完成")

    jobs = [
        (mode, task)
        for task in feasible
        for mode in modes
    ]
    print(f"待执行评测任务 {len(jobs)}（{len(feasible)} 任务 × {len(modes)} 模式）")
    results = {}

    def worker(job):
        mode, task = job
        work_dir = runs_root / task.name / mode
        rec = run_one(exe, repo, task, work_dir, mode, args.max_inner)
        if rec["ok"]:
            print(
                f"  ✓ {task.name:<42} {mode:<14} pass_rate={rec['pass_rate']} "
                f"({rec['passed']}/{rec['total']}) llmCalls={rec['llm_calls']} state={rec['state']}"
            )
        else:
            print(f"  ✗ {task.name:<42} {mode:<14} {rec.get('error')} state={rec.get('state')}")
        return mode, task.name, rec

    if args.parallel > 1:
        with ThreadPoolExecutor(max_workers=args.parallel) as pool:
            for mode, name, rec in pool.map(worker, jobs):
                results.setdefault(name, {})[mode] = rec
    else:
        for job in jobs:
            mode, name, rec = worker(job)
            results.setdefault(name, {})[mode] = rec

    # 汇总
    print("\n======== 汇总 ========")
    header = f"{'task':<42} " + "".join(f"{m:>20}" for m in modes)
    print(header)
    summary = {m: {"pass": 0, "total": 0, "eval_total": 0} for m in modes}
    for name in sorted(results):
        row = f"{name:<42} "
        for m in modes:
            rec = results.get(name, {}).get(m)
            if not rec:
                row += f"{'NA':>20}"
                continue
            if rec.get("ok"):
                val = rec["pass_rate"]
                summary[m]["pass"] += rec["passed"]
                summary[m]["eval_total"] += rec["total"]
                summary[m]["total"] += 1
            else:
                val = "FAIL"
            row += f"{str(val):>20}"
        print(row)
    print("-" * len(header))
    print(f"{'AGG':<42} " + "".join(
        f"{summary[m]['pass']/max(summary[m]['total'],1):>20.2f}" for m in modes
    ))
    print(f"{'passed/total-tasks':<42} " + "".join(
        f"{summary[m]['pass']}/{summary[m]['total']:>15}" for m in modes
    ))

    # 落盘原始结果
    runs_root.mkdir(parents=True, exist_ok=True)
    out = runs_root / "summary.json"
    out.write_text(json.dumps(results, ensure_ascii=False, indent=2, sort_keys=True), encoding="utf-8")
    print(f"\n原始结果已写入: {out}")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())