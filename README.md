# SimpleAgent

一个按 DDD 分层组织的 Go 语言 AI Agent 运行时：支持多轮工具调用、会话分支与回滚、上下文压缩、Office 文件读写，
并内置一套可复现的 Office 评测 harness（本地执行器 / Docker 执行器）。

## 能力概览

- **推理循环**：原生 tool calling（含 `tool_call_id` 配对回放）+ JSON Schema 参数校验，模型不支持时回退文本协议。
- **工具集**：文件读写与检索、Office 读取（xlsx / docx / pptx / pdf）、xlsx 写入、命令与技能调用。
- **上下文治理**：真实 tokenizer 计数、预算裁剪、历史压缩与折叠。
- **会话**：SQLite 持久化，支持历史、分支、回滚、fork。
- **交付模型**：Artifact-first，产物带 sha256 / 产生者 / 来源 / 必需标记与校验结果。
- **安全**：默认只监听 `127.0.0.1`、Token 鉴权、Origin 白名单、敏感路径拒绝、审批 fail-closed、技能接口鉴权。
- **评测**：`--bench run` 批量跑任务，产出统一 EvalReport（状态、产物、评分、失败归因、轨迹、耗时）。

## 架构

严格 DDD 分层，依赖只能由外向内：

```
interfaces/         CLI 与 HTTP/WS 入口，只做参数解析与装配
application/        Agent 编排、工具执行、上下文治理、评测编排
domain/             实体、值对象、领域事件、端口接口（不依赖任何外部包）
infrastructure/     LLM 适配、SQLite、文件与 Office 读写、评测执行器、审计
```

- `domain` 只定义 `ChatModel`、`Tool`、`Repository` 等端口，不出现数据库或网络细节。
- 数据库读写集中在 `infrastructure`，领域实体只承载业务行为。
- 接口返回统一包装，入参统一校验；SQL 全部参数化。

## 目录

```
application/agent/        主循环、步骤执行、上下文压缩、planner-worker
application/eval/         评测编排、产物比对、报告汇总（Executor 端口定义在此）
domain/agent|eval|...     领域模型与端口
infrastructure/bench/     任务加载、本地执行器、Docker 执行器
infrastructure/office/    xlsx/docx/pptx/pdf 读写与结构校验
interfaces/cli/           main、--serve、--office、--bench
interfaces/http/          REST + WebSocket 服务
```

## 快速开始

要求 Go 1.25+。

```bash
# 1. 配置（.env 会被自动加载，但不会入库）
OPENAI_API_KEY=sk-xxx
OPENAI_BASE_URL=https://api.deepseek.com   # 可选
OPENAI_MODEL=deepseek-v4-flash             # 可选

# 2. 单次问答
go run ./interfaces/cli "帮我总结当前目录的结构"

# 3. 交互式 HTTP 服务（默认 127.0.0.1:8080）
go run ./interfaces/cli --serve
```

启动时若未配置 `SIMPLEAGENT_AUTH_TOKEN`，会生成一次性 Token 并只在控制台打印一次。

### 常用子命令

| 命令 | 说明 |
|---|---|
| `--history` | 查看当前分支历史（带 → 的是可回滚的用户提问） |
| `--branches` | 列出同一对话树的所有分支 |
| `--rollback <序号>` | 回滚到某条提问，原路径自动存档为分支 |
| `--fork [序号]` | 从指定位置分叉出新分支 |
| `--planner-worker <目标>` | 规划-执行两段式运行 |
| `--office <mode> <workspace> <instruction> [maxInner]` | 无人值守跑单个 Office 任务 |
| `--bench run\|report` | 评测 harness |

## 评测 harness

```bash
# 本地执行器（快速调试，结果不作为正式口径）
go run ./interfaces/cli --bench run \
  --dataset <任务集目录> --executor local --mode plan --max-tasks 1

# Docker 执行器（可复现口径）
go run ./interfaces/cli --bench run \
  --dataset <任务集目录> --executor docker --mode plan --max-tasks 1

# 查看历史运行
go run ./interfaces/cli --bench report --run-id <RUN_ID>

# 只校验任务集（不调用模型，秒级）
go run ./interfaces/cli --bench validate --dataset <任务集目录>
```

`--bench validate` 会在全量运行前确认“任务是否都能执行、是否存在静默跳过”：输出任务总数、可执行/不可执行数量及原因、
产物类型需求，以及当前写能力不覆盖的类型（`--writable` 可覆盖默认能力集）。任一任务包不可执行时以非零码退出。

Docker 执行器行为：

- 用任务包自带的 `environment/Dockerfile` 构建镜像，标签含内容哈希，命中缓存不会重复构建；
- 工作区、测试、日志、Agent 状态目录以 bind mount 挂载，产物直接落在宿主机运行目录；
- harness 交叉编译为 `linux/amd64` 后只读挂载进容器，通过 `docker exec` 运行；
- verifier 直接执行 `verifier.toml` 原文，JUnit 结果从 `/logs/verifier/results.xml` 解析；
- 只透传白名单内的模型环境变量，其余宿主机凭据不会进入任务镜像。

失败会归因到统一失败码（`image_build_failed`、`container_start_failed`、`agent_timeout`、
`verifier_timeout` 等），而不是笼统的“执行失败”。运行目录默认在系统缓存目录，禁止写入仓库内部。

## 配置项

| 变量 | 默认值 | 说明 |
|---|---|---|
| `OPENAI_API_KEY` | 无（必填） | 模型密钥 |
| `OPENAI_BASE_URL` | 无 | 兼容 OpenAI 协议的地址 |
| `OPENAI_MODEL` | `deepseek-v4-flash` | 模型名 |
| `SESSION_ID` | `cli-local` | 默认会话/分支 ID |
| `SIMPLEAGENT_ADDR` | `127.0.0.1:8080` | 服务监听地址 |
| `SIMPLEAGENT_AUTH_TOKEN` | 启动时生成 | 访问 Token，至少 32 字符 |
| `SIMPLEAGENT_ALLOWED_ORIGINS` | 空 | Origin 白名单（逗号分隔） |
| `SIMPLEAGENT_WORKSPACE_ROOT` | 当前目录 | Agent 工作区根目录 |
| `SIMPLEAGENT_PATH_ALLOW` | 空 | 敏感路径拒绝清单的例外 |
| `SIMPLEAGENT_ALLOW_NO_AUTH` | `false` | 本地免认证调试，仅 loopback 可用 |
| `SIMPLEAGENT_HEADLESS` | `false` | 无人值守模式，技能执行默认禁用 |
| `SIMPLEAGENT_ALLOW_SKILL_ADMIN` | `false` | 是否允许登录用户管理技能 |
| `SIMPLEAGENT_ALLOW_SKILL_EXEC` | `false` | Headless 下是否允许执行命令技能 |
| `SIMPLEAGENT_REQUEST_MAX_BYTES` | `1048576` | 请求体上限 |
| `SIMPLEAGENT_SESSION_TTL` | `24h` | 登录会话有效期 |
| `SIMPLEAGENT_APPROVAL_TIMEOUT` | `2m` | 审批超时，超时按拒绝处理 |
| `SIMPLEAGENT_TOKENIZER_PATH` | 仓库内资产 | 离线 tokenizer.json 路径 |
| `SIMPLEAGENT_CONTEXT_BUDGET` | `400000` | 单次请求上下文预算（估算 token），用于适配不同模型的窗口大小 |

## 测试

```bash
gofmt -l .
go build ./...
go vet ./...
go test ./...
```

CI（`.github/workflows/ci.yml`）在 `windows-latest` 上执行同样的步骤：`go mod tidy -diff` → `gofmt` →
`build` → `vet` → `test`。选择 Windows 是因为部分路径用例依赖 Windows 语义。

## 说明

- `.env`、运行日志、会话数据库、trace、exe 产物、笔记类 `txt`/`md` 均在 `.gitignore` 中；
- 任务集不随仓库分发，评测时通过 `--dataset` 指向外部任务包目录。
