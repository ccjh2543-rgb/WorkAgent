// Package agent 实现 Agent 的应用编排层。
//
// 它把 domain 层的领域契约（agent 端口、tool 注册表、approval 审批、
// planner 规划、session 仓储）与 infrastructure 层的实现（eino 模型、
// 记忆索引、会话库、具体工具）组装成一个可运行的自负规划 Agent。
// 除 ChatModel 端口通过构造注入外，其余依赖在进程中显式装配。
package agent

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	domainagent "simpleAgent/domain/agent"
	dapproval "simpleAgent/domain/approval"
	"simpleAgent/domain/planner"
	"simpleAgent/domain/session"
	"simpleAgent/domain/tool"
	"simpleAgent/infrastructure/audit"
	"simpleAgent/infrastructure/memory"
	"simpleAgent/infrastructure/memory/embed"
	"simpleAgent/infrastructure/memory/sqliteidx"
	"simpleAgent/infrastructure/sqlitestore"
	"simpleAgent/infrastructure/tools"
)

// Agent 自负规划 Agent 的应用层装配体。
//
// 它承载一次 Run 的参数与运行期状态（会话、记忆、工具、审批），
// 通过注入的 ChatModel 端口驱动推理。刻意不并发安全：同一实例上的
// Run / WorkBench 任务必须串行调用。多会话场景请通过 Manager 获取
// 独立实例，不要复用同一个 Agent 并发执行。
type Agent struct {
	chatModel domainagent.ChatModel // 模型端口，由构造注入（eino 实现）
	// baseModel 未经计数装饰的模型端口。会话级克隆用它重建自己的计数装饰，
	// 避免多个会话把调用次数记到同一个指标上。
	baseModel domainagent.ChatModel
	// opts 装配选项（工作区、状态目录、路径例外、审计），供会话级克隆复用
	opts    Options
	memory  *domainagent.Memory // 本轮上下文的临时消息序列
	planner *planner.Planner    // 任务拆解与进度跟踪
	toolReg *tool.ToolRegistry  // 工具注册表
	maxLoop int                 // 最大循环次数防止死循环

	// approver 人工确认端口。默认 DenyApprover（无审批通道即拒绝），
	// 需要人在回路时显式替换为 Interactive/WS 审批器
	approver dapproval.Approver
	// maxToolFailures 连续工具失败/被拒的次数上限，达到后转人工
	maxToolFailures int

	// systemPrompt 基础系统提示词。它每次请求都会重新拼装，
	// 因此不进入会话历史，也不落盘
	systemPrompt string
	// sysTemplate 未渲染的系统提示词模板（含 {{TOOL_LIST}} 占位符），
	// 供 renderSystemPrompt 在工具集变化时重建 systemPrompt 使用
	sysTemplate string
	// model 主推理模型名
	model string
	// contextBudget 单次请求的上下文预算（估算 token），超预算触发压缩
	contextBudget int
	// tokenizer 精确 token 计数端口（DeepSeek 离线 BPE）。nil 时回退 EstimateTokens
	// 启发式估算，保证即使未装配分词器也能正常工作
	tokenizer domainagent.Tokenizer
	// foldHeadRunes / foldTailRunes 工具输出折叠时保留的首尾长度
	foldHeadRunes int
	foldTailRunes int
	// savedFoldHead / savedFoldTail 评估模式进入前保存的生产折叠阈值，退出时恢复
	savedFoldHead int
	savedFoldTail int
	// compaction 上下文压缩的内存缓存，回滚或历史被替换时必须失效
	compaction compactionState
	// persistedCompaction 已持久化到会话树的压缩覆盖条数，
	// 用于避免同一范围内每轮重复写入压缩记录
	persistedCompaction int
	// metrics 上下文压缩/折叠/裁剪的可观测计数（判断窗口阈值是否合理）
	metrics AgentMetrics
	// contextReport 最近一次上下文装配的分区用量快照。
	// 使用原子指针，允许指标接口在运行过程中并发读取而不加锁。
	contextReport atomic.Pointer[ContextReport]
	// onEvent 可选的运行事件回调，供上层（HTTP/WS 服务）实时上报进度。
	// 为 nil 时不产生任何额外行为，不影响原有调用方。
	onEvent func(domainagent.AgentEvent)

	// workbenchMode 评估模式开关：为 true 时系统提示词只渲染 WorkBench 注入的
	// 工具，屏蔽内置工具与生产指令噪音。
	workbenchMode bool
	// wbToolAllowSet 评估模式下允许出现在提示词里的工具白名单
	wbToolAllowSet map[string]bool

	// sessionID 当前会话标识，由 Run 设置
	sessionID string
	// sessStore 会话仓储，nil 表示对话记录不落盘
	sessStore session.Repository
	// maxHistoryMessages 从库里最多载入多少条历史
	maxHistoryMessages int
	// maxHistoryRunes 载入历史的总字符预算，超出部分从最旧的开始丢弃
	maxHistoryRunes int
	// maxMessageBytes 单条消息落盘的最大字节数
	maxMessageBytes int
	// cleanupOnce 保证进程内只做一次过期会话清理
	cleanupOnce sync.Once

	memStore *memory.WorkspaceStore // 工作区记忆仓储，nil 表示持久记忆不可用
	memLoad  *memory.Loader         // 记忆检索与装配器
	memBlock string                 // 本轮检索得到的记忆块，由 prepareContext 注入
	// workspace 文件工具可访问的目录范围，nil 表示文件工具不可用
	workspace *tools.Workspace
	// stateDir 运行状态根目录（会话库、记忆索引、工具输出留存）
	stateDir string
	closers  []io.Closer // 退出时需要关闭的资源（记忆索引、会话库）

	// nativeTools 注入的模型端口是否支持原生 tool calling。
	// 装配时按端口能力一次性确定：支持则在每次请求里声明工具（toolDecls），
	// 不支持则完全保持 !TOOL: 文本协议，行为与改造前一致。
	nativeTools bool
	// toolDecls 当前允许模型调用的原生工具声明，随系统提示词一起重建
	// （两者都由「当前工具集」派生，必须同步刷新，否则会出现提示词里有、
	// 原生接口里没有的割裂）。
	toolDecls []domainagent.ToolDecl

	// wbExec 非 nil 时处于 WorkBench 评估模式：工具交由外部执行，
	// 返回 errToolDeferred 触发 step 暂停；nil 时由 toolReg 本机执行。
	wbExec execFn
	// wbFileTools 在 Office 评测模式下需要「进程内直接执行」的本地文件工具名集合。
	// 生产 WorkBench（模拟邮箱/lead 工具）与 Office 文件工具共用 wbExec 通道：
	// 该集合内的工具放行到 toolReg.ExecuteTool 直执行，集合外仍由外部延迟执行。
	wbFileTools map[string]bool
	// wbMu 串行化 WorkBench 的 Start/Continue/Close，避免评估并发竞争。
	wbMu sync.Mutex

	// 以下字段仅用于「发给模型的完整消息 + 模型原始回答」的诊断落盘，
	// 由环境变量 SIMPLEAGENT_LLM_TRACE 指向的文件路径开启；未设置时全部为空操作。
	traceFile *os.File   // LLM 请求/响应 trace 文件，nil 表示未开启
	traceMu   sync.Mutex // 保护 trace 文件并发写入
	traceSeq  int        // 单调递增的 trace 序号
}

// 默认参数
const (
	// defaultModel 主推理模型名
	defaultModel = "deepseek-v4-flash"
	// officeToolHint 办公评测模式下可用的本地文件工具列表，
	// 用于把不可用工具（如 python、shell）的错误回注给模型。
	officeToolHint = "read_file、write_file、list_dir、search_files、read_xlsx、write_xlsx、read_docx、read_pptx、read_pdf"
	// sessionDBFileName 会话库文件名，位于工作区数据目录下
	sessionDBFileName = "sessions.db"
	// defaultMaxHistoryMessages 单次载入的历史消息条数上限
	defaultMaxHistoryMessages = 100
	// defaultMaxHistoryRunes 单次载入历史的总字符预算
	defaultMaxHistoryRunes = 200000
	// defaultSessionRetention 会话保留期，超过后由清理任务删除
	defaultSessionRetention = 30 * 24 * time.Hour
	// minContextBudget 允许的最小上下文预算。低于该值时压缩无法保留
	// 足够近的窗口，反而会制造更多摘要请求，因此直接拒绝。
	minContextBudget = 8192
)

// envContextBudget 上下文预算的环境变量覆盖名。
// 用于在不改代码的情况下适配不同模型的窗口大小。
const envContextBudget = "SIMPLEAGENT_CONTEXT_BUDGET"

// 记忆向量化服务的环境变量名，敏感值只从环境读取，不写入代码
const (
	envEmbeddingBaseURL = "MEMORY_EMBEDDING_BASE_URL"
	envEmbeddingAPIKey  = "MEMORY_EMBEDDING_API_KEY"
	envEmbeddingModel   = "MEMORY_EMBEDDING_MODEL"
)

// Options Agent 装配选项。
type Options struct {
	// WorkspaceRoot 工作区根目录。为空时使用进程当前工作目录；
	// 非空时文件工具的读写范围绑定到该目录。
	WorkspaceRoot string
	// StateRoot 运行状态目录（会话库、记忆索引、审计）。为空时跟随 WorkspaceRoot。
	// 评测场景指定工作区之外的目录，避免框架自身产物混入被校验的工作区。
	StateRoot string
	// PathAllow 路径拒绝清单的显式例外（相对工作区的路径）。
	// 命中例外时仍应记录审计，默认空表示不放开任何敏感路径。
	PathAllow []string
	// Audit 审计日志；nil 表示不记录。
	Audit *audit.Logger
	// DisableLLMTrace 关闭 LLM 请求/响应落盘。
	// 会话级克隆默认关闭：trace 是进程级诊断，多实例并发写同一文件没有意义。
	DisableLLMTrace bool
}

// NewAgent 使用默认选项装配 Agent（工作区为进程当前目录）。
//
// chatModel 由调用方注入（CLI 用 llm.NewEinoChatModel 装配），
// 它替代旧的直连 openai.Client，是领域层与基础设施层的接缝。
//
// 调用示例：
//
//	cm, _ := llm.NewEinoChatModel(llm.Config{BaseURL: ..., APIKey: ..., Model: ...})
//	a := agent.NewAgent(cm)
func NewAgent(chatModel domainagent.ChatModel) *Agent {
	return NewAgentWithOptions(chatModel, Options{})
}

// NewAgentWithOptions 按给定选项装配 Agent。
//
// 工作区根目录在装配时一次性确定，随后记忆、会话库与文件工具都使用它；
// 调用方通过 SIMPLEAGENT_WORKSPACE_ROOT 指定独立工作区，避免把仓库根当作可读写范围。
func NewAgentWithOptions(chatModel domainagent.ChatModel, opts Options) *Agent {
	mem := domainagent.NewMemory()
	agent := &Agent{
		chatModel:          chatModel,
		baseModel:          chatModel,
		opts:               opts,
		nativeTools:        supportsNativeTools(chatModel),
		memory:             mem,
		planner:            planner.NewPlanner(),
		toolReg:            tool.NewToolRegistry(),
		maxLoop:            8,
		approver:           dapproval.DenyApprover{},
		maxToolFailures:    defaultMaxToolFailures,
		model:              defaultModel,
		sysTemplate:        domainagent.AgentSys,
		contextBudget:      defaultContextBudget,
		maxHistoryMessages: defaultMaxHistoryMessages,
		maxHistoryRunes:    defaultMaxHistoryRunes,
		maxMessageBytes:    session.DefaultMaxMessageBytes,
	}
	// 折叠开关：AGENT_RAW_TOOL_OUTPUTS=1 时禁用工具输出折叠，
	// 只调 FoldToolResult 的阈值到极大值，不新增分支。
	foldHead, foldTail := session.DefaultFoldHeadRunes, session.DefaultFoldTailRunes
	if os.Getenv("AGENT_RAW_TOOL_OUTPUTS") == "1" {
		foldHead, foldTail = math.MaxInt/2, math.MaxInt/2
	}
	agent.foldHeadRunes = foldHead
	agent.foldTailRunes = foldTail
	// 上下文预算允许按部署环境覆盖：不同模型的窗口差异很大，
	// 写死常量会让小窗口模型频繁被拒、大窗口模型浪费预算。
	if raw := strings.TrimSpace(os.Getenv(envContextBudget)); raw != "" {
		if n, err := strconv.Atoi(raw); err == nil {
			agent.SetContextBudget(n)
		} else {
			fmt.Printf("[上下文预算配置无效，使用默认值]:%s\n", raw)
		}
	}
	// 注册内置示例工具：只读工具自动放行
	agent.toolReg.RegisterWithPolicy(&tools.CalculatorTool{}, tool.ToolPolicy{Risk: tool.RiskReadOnly, AutoApprove: true})
	agent.toolReg.RegisterWithPolicy(&tools.SearchMockTool{}, tool.ToolPolicy{Risk: tool.RiskReadOnly, AutoApprove: true})

	projectDir := strings.TrimSpace(opts.WorkspaceRoot)
	if projectDir == "" {
		var err error
		projectDir, err = os.Getwd()
		if err != nil {
			fmt.Printf("[取不到工作目录，持久化与文件工具全部关闭]:%v\n", err)
			projectDir = ""
		}
	}
	// 状态目录默认跟随工作区；显式指定时与工作区解耦，保证框架文件不落到工作区。
	stateDir := strings.TrimSpace(opts.StateRoot)
	if stateDir == "" {
		stateDir = projectDir
	}
	if projectDir != "" {
		agent.initFileTools(projectDir, opts.PathAllow, opts.Audit)
	}
	if stateDir != "" {
		agent.stateDir = stateDir
		agent.initMemory(stateDir)
		agent.initSession(stateDir)
		// 折叠工具输出时会把原文留存到状态目录，这里注册只读回取工具。
		// 目录按当前会话动态计算，切换会话后自动收紧到新会话的留存范围。
		agent.toolReg.RegisterWithPolicy(tools.NewToolOutputReader(agent.toolOutputDir), tool.ToolPolicy{
			Risk:        tool.RiskReadOnly,
			AutoApprove: true,
		})
	}

	// 工具列表必须在所有工具注册完成之后才渲染。
	agent.renderSystemPrompt()
	if !opts.DisableLLMTrace {
		agent.initLLMTrace()
	}
	// 记录已解析的目录，供会话级克隆直接复用；避免克隆时按当前工作目录重新推导
	agent.opts = Options{
		WorkspaceRoot:   projectDir,
		StateRoot:       stateDir,
		PathAllow:       opts.PathAllow,
		Audit:           opts.Audit,
		DisableLLMTrace: opts.DisableLLMTrace,
	}
	// 统一 LLM 调用计数：装饰包住注入模型，拦下所有 Generate（含 domain 决策器与摘要生成）
	agent.chatModel = &countingModel{inner: chatModel, ctr: &agent.metrics.llmCalls}
	return agent
}

// NewSessionAgent 创建一个运行态完全独立的会话实例。
//
// 实例共享同一套配置（模型、工作区、状态目录、审计与路径例外），
// 但拥有独立的模型消息序列、计划、压缩缓存、指标与工具注册表；
// 会话库与记忆索引各自持有连接。调用方负责在实例不再使用时 Close。
func (a *Agent) NewSessionAgent() (*Agent, error) {
	if a.baseModel == nil {
		return nil, errors.New("模型端口未初始化，无法创建会话实例")
	}
	opts := a.opts
	// trace 是进程级诊断，克隆实例不重复打开同一个文件
	opts.DisableLLMTrace = true
	clone := NewAgentWithOptions(a.baseModel, opts)
	clone.model = a.model
	clone.maxLoop = a.maxLoop
	clone.maxToolFailures = a.maxToolFailures
	clone.contextBudget = a.contextBudget
	clone.tokenizer = a.tokenizer
	clone.foldHeadRunes = a.foldHeadRunes
	clone.foldTailRunes = a.foldTailRunes
	clone.sysTemplate = a.sysTemplate
	clone.renderSystemPrompt()
	return clone, nil
}

// countingModel 给底层 ChatModel 包一层「调用次数」计数，统计单个任务累计的模型调用次数。
type countingModel struct {
	inner domainagent.ChatModel
	ctr   *atomic.Int64
}

func (m *countingModel) Generate(ctx context.Context, req domainagent.ChatRequest) (domainagent.ChatResponse, error) {
	m.ctr.Add(1)
	return m.inner.Generate(ctx, req)
}

// supportsNativeTools 询问模型端口是否支持原生 tool calling。
//
// 用可选接口而非配置项判断：能力属于适配器事实，不是运行期开关。
// 未声明该能力的适配器（含测试里的假模型）返回 false，
// 系统自动退回 !TOOL: 文本协议，无需它们改任何代码。
func supportsNativeTools(chatModel domainagent.ChatModel) bool {
	nt, ok := chatModel.(domainagent.NativeToolModel)
	return ok && nt.SupportsNativeTools()
}

// initLLMTrace 依环境变量 SIMPLEAGENT_LLM_TRACE 开启 LLM 请求/响应诊断落盘。
func (a *Agent) initLLMTrace() {
	path := os.Getenv("SIMPLEAGENT_LLM_TRACE")
	if path == "" {
		return
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		fmt.Printf("[LLM trace 开启失败]:%v\n", err)
		return
	}
	a.traceFile = f
	a.closers = append(a.closers, f)
	fmt.Printf("[LLM trace 已开启]:%s\n", path)
	a.traceLLM(nil, domainagent.ChatResponse{Content: "==== trace 开始 ===="})
}

// AgentMetrics 上下文压缩/折叠/裁剪的可观测计数（原子累加，供判断窗口阈值是否合理）。
type AgentMetrics struct {
	compactionRounds     atomic.Int64 // 单请求真正触发压缩的次数
	compactedEntries     atomic.Int64 // 累计并入摘要的条目数
	foldedToolResults    atomic.Int64 // 折叠过的工具输出条数
	droppedWorkerEntries atomic.Int64 // worker 上下文超预算丢弃的条目数
	maxWindowTokens      atomic.Int64 // 观测到的最大单请求发送 token
	llmCalls             atomic.Int64 // 累计模型调用次数（装饰器统一计数）
}

// Reset 清零计数（每个评估任务开始时调用，保证单任务独立上报）。
func (m *AgentMetrics) Reset() {
	m.compactionRounds.Store(0)
	m.compactedEntries.Store(0)
	m.foldedToolResults.Store(0)
	m.droppedWorkerEntries.Store(0)
	m.maxWindowTokens.Store(0)
	m.llmCalls.Store(0)
}

// WbMetrics 随 /api/workbench 响应回传的指标快照。
type WbMetrics struct {
	CompactionRounds     int64 `json:"compaction_rounds"`
	CompactedEntries     int64 `json:"compacted_entries"`
	FoldedToolResults    int64 `json:"folded_tool_results"`
	DroppedWorkerEntries int64 `json:"dropped_worker_entries"`
	MaxWindowTokens      int64 `json:"max_window_tokens"`
	LLMCalls             int64 `json:"llm_calls"`
	// Context 最近一次上下文装配的分区用量；未装配时为 null。
	Context *ContextReport `json:"context,omitempty"`
}

// metricsSnapshot 读取当前计数为不可变快照。
func (a *Agent) metricsSnapshot() WbMetrics {
	return WbMetrics{
		CompactionRounds:     a.metrics.compactionRounds.Load(),
		CompactedEntries:     a.metrics.compactedEntries.Load(),
		FoldedToolResults:    a.metrics.foldedToolResults.Load(),
		DroppedWorkerEntries: a.metrics.droppedWorkerEntries.Load(),
		MaxWindowTokens:      a.metrics.maxWindowTokens.Load(),
		LLMCalls:             a.metrics.llmCalls.Load(),
		Context:              a.contextReport.Load(),
	}
}

// traceLLM 把每次发给大模型的完整消息与模型原始回答追加写入 trace 文件。
// 参数用 ChatResponse 而非字符串，工具调用（含调用 ID）才能一并留痕，
// 排查「模型说了要调用但没动作」时能直接看到端口层拿到了什么。
func (a *Agent) traceLLM(messages []domainagent.ChatMessage, reply domainagent.ChatResponse) {
	a.traceMu.Lock()
	defer a.traceMu.Unlock()
	a.traceSeq++
	rec := map[string]any{
		"seq":        a.traceSeq,
		"session_id": a.sessionID,
		"messages":   messages,
		"reply":      reply.Content,
		"tool_calls": reply.ToolCalls,
		"context":    a.contextReport.Load(),
	}
	b, err := json.Marshal(rec)
	if err != nil {
		return
	}
	b = append(b, '\n')
	if _, werr := a.traceFile.Write(b); werr == nil {
		_ = a.traceFile.Sync()
	}
}

// renderSystemPrompt 依当前完整工具集重新渲染系统提示词与原生工具声明，
// 并失效上下文压缩缓存。两者必须一起刷新：都派生自「当前工具集」。
func (a *Agent) renderSystemPrompt() {
	var allowed map[string]bool
	if a.workbenchMode {
		allowed = a.wbToolAllowSet
	}
	a.systemPrompt = strings.ReplaceAll(a.sysTemplate, "{{TOOL_LIST}}", a.toolReg.GetAllToolsDescFiltered(allowed))
	a.toolDecls = a.buildToolDecls(allowed)
	a.resetCompaction()
}

// buildToolDecls 收集可声明给模型的原生工具。
//
// 只收录实现了 SpecProvider（即带参数契约）的工具：原生 tool calling 要求
// 每个工具给出参数 schema，没有契约的工具（WorkBench 外部延迟工具、示例工具）
// 声明的参数必然与它实际接受的参数不符，宁可让它继续走 !TOOL: 文本协议。
// 排序是为了让每次请求的工具声明顺序稳定，便于比对 trace 与复现问题。
func (a *Agent) buildToolDecls(allowed map[string]bool) []domainagent.ToolDecl {
	if !a.nativeTools {
		return nil
	}
	names := a.toolReg.Names()
	sort.Strings(names)
	decls := make([]domainagent.ToolDecl, 0, len(names))
	for _, name := range names {
		if allowed != nil && !allowed[name] {
			continue
		}
		spec, ok := a.toolReg.Spec(name)
		if !ok {
			continue
		}
		desc := ""
		if t := a.toolReg.Lookup(name); t != nil {
			desc = t.Description()
		}
		decls = append(decls, domainagent.ToolDecl{
			Name:        name,
			Description: desc,
			Parameters:  spec.JSONSchema(),
		})
	}
	return decls
}

// Close 释放 Agent 持有的资源（记忆索引、会话库的文件句柄）。
func (a *Agent) Close() error {
	var errs []error
	for _, closer := range a.closers {
		if err := closer.Close(); err != nil {
			errs = append(errs, err)
		}
	}
	a.closers = nil
	return errors.Join(errs...)
}

// SessionID 返回最近一次 Run 使用的会话 ID
func (a *Agent) SessionID() string {
	return a.sessionID
}

// SetOnEvent 注册运行事件回调，用于实时上报进度。传 nil 表示关闭。
func (a *Agent) SetOnEvent(fn func(domainagent.AgentEvent)) {
	a.onEvent = fn
}

// SetTokenizer 注入精确 token 计数实现；不设置或传 nil 时使用 EstimateTokens 估算。
func (a *Agent) SetTokenizer(t domainagent.Tokenizer) {
	a.tokenizer = t
	// 计数口径变化会影响预算判断，失效已做的压缩缓存
	a.resetCompaction()
}

// SetContextBudget 覆盖单次请求的上下文预算（估算 token）。
//
// 传非正数恢复默认预算；低于最小预算时按最小预算处理。
// 预算变化会让已有压缩判断失效，因此同时清空压缩缓存。
func (a *Agent) SetContextBudget(tokens int) {
	if tokens <= 0 {
		tokens = defaultContextBudget
	}
	if tokens < minContextBudget {
		tokens = minContextBudget
	}
	a.contextBudget = tokens
	a.resetCompaction()
}

// ContextSnapshot 返回最近一次上下文装配的分区用量快照；尚未装配时返回 nil。
func (a *Agent) ContextSnapshot() *ContextReport {
	return a.contextReport.Load()
}

// emit 触发一次事件上报；未注册回调时为空操作
func (a *Agent) emit(ev domainagent.AgentEvent) {
	if a.onEvent == nil {
		return
	}
	ev.SessionID = a.sessionID
	a.onEvent(ev)
}

// RegisterSkill 动态注册一个自定义工具/技能，并立即刷新系统提示词。
func (a *Agent) RegisterSkill(t tool.Tool, policy tool.ToolPolicy) error {
	if t == nil || t.Name() == "" {
		return errors.New("技能名不能为空")
	}
	if a.toolReg.Lookup(t.Name()) != nil {
		return fmt.Errorf("已存在同名工具或技能: %s", t.Name())
	}
	a.toolReg.RegisterWithPolicy(t, policy)
	a.renderSystemPrompt()
	return nil
}

// CreateSession 生成一个新的会话 ID。会话本身在第一条消息写入时才真正落库。
func (a *Agent) CreateSession() (string, error) {
	return newSessionID()
}

// ListTools 返回当前已注册的全部工具与技能，供前端展示与技能管理。
func (a *Agent) ListTools() []domainagent.ToolInfo {
	names := domainagent.SortedToolNames(a.toolReg.Names())
	infos := make([]domainagent.ToolInfo, 0, len(names))
	for _, name := range names {
		policy := a.toolReg.Policy(name)
		tool := a.toolReg.Lookup(name)
		desc := ""
		if tool != nil {
			desc = tool.Description()
		}
		infos = append(infos, domainagent.ToolInfo{
			Name: name, Description: desc,
			Risk: policy.Risk.String(), AutoApprove: policy.AutoApprove,
		})
	}
	return infos
}

// UnregisterSkill 运行时移除一个已注册的自定义技能并刷新系统提示词。
func (a *Agent) UnregisterSkill(name string) error {
	if a.toolReg.Lookup(name) == nil {
		return fmt.Errorf("未注册的工具或技能: %s", name)
	}
	a.toolReg.Unregister(name)
	a.renderSystemPrompt()
	return nil
}

// ListConversations 列出全部对话树（每棵树的最近活跃时间与全部分支）。
// 用于前端侧栏展示「创建过的会话」。
func (a *Agent) ListConversations(ctx context.Context) ([]session.Conversation, error) {
	if a.sessStore == nil {
		return nil, errors.New("会话持久化未启用，无法列出会话")
	}
	return a.sessStore.ListConversations(ctx)
}

// initSession 初始化会话持久化：库文件为 <项目根目录>/.simpleagent/sessions.db。
// 初始化失败时降级为不保存对话记录，进程内的多轮对话仍然可用。
func (a *Agent) initSession(projectDir string) {
	dbPath := filepath.Join(memory.WorkspaceDir(projectDir), sessionDBFileName)
	store, err := sqlitestore.New(dbPath)
	if err != nil {
		fmt.Printf("[会话持久化不可用，对话记录不会保存]:%v\n", err)
		return
	}
	a.sessStore = store
	a.closers = append(a.closers, store)
}

// SetApprover 设置人工确认端口。默认是 DenyApprover（无审批通道即拒绝）。
// 传入 nil 表示清除自定义审批器，回退到安全默认值，而不是回退到全放行。
func (a *Agent) SetApprover(approver dapproval.Approver) {
	if approver == nil {
		a.approver = dapproval.DenyApprover{}
		return
	}
	a.approver = approver
}

// initFileTools 注册文件类工具。
// pathAllow 是路径拒绝清单的显式例外，来自 SIMPLEAGENT_PATH_ALLOW。
func (a *Agent) initFileTools(projectDir string, pathAllow []string, auditLog *audit.Logger) {
	policy := tools.DefaultPathPolicy().WithAllow(pathAllow)
	workspace, err := tools.NewWorkspaceWithPolicy(projectDir, policy)
	if err != nil {
		fmt.Printf("[文件工具不可用]:%v\n", err)
		return
	}
	workspace.SetAudit(auditLog)
	a.workspace = workspace
	readOnly := tool.ToolPolicy{Risk: tool.RiskReadOnly, AutoApprove: true}
	a.wbFileTools = map[string]bool{
		"read_file":    true,
		"list_dir":     true,
		"search_files": true,
		"write_file":   true,
		"read_xlsx":    true,
		"write_xlsx":   true,
		"read_docx":    true,
		"read_pptx":    true,
		"read_pdf":     true,
		// 折叠工具输出的回取工具同样在进程内执行，不交给外部 harness
		"read_tool_output": true,
	}
	writeTool := tools.NewWriteFileTool(workspace)
	writeTool.SetAudit(auditLog)
	a.toolReg.RegisterWithPolicy(tools.NewReadFileTool(workspace), readOnly)
	a.toolReg.RegisterWithPolicy(tools.NewListDirTool(workspace), readOnly)
	a.toolReg.RegisterWithPolicy(tools.NewSearchFilesTool(workspace), readOnly)
	a.toolReg.RegisterWithPolicy(writeTool, tool.ToolPolicy{Risk: tool.RiskWriteLocal})

	// Office 读取工具与文件读取同级：只读、自动放行。
	a.toolReg.RegisterWithPolicy(tools.NewReadXLSXTool(workspace), readOnly)
	a.toolReg.RegisterWithPolicy(tools.NewReadDOCXTool(workspace), readOnly)
	a.toolReg.RegisterWithPolicy(tools.NewReadPPTXTool(workspace), readOnly)
	a.toolReg.RegisterWithPolicy(tools.NewReadPDFTool(workspace), readOnly)
	writeXLSXTool := tools.NewWriteXLSXTool(workspace)
	writeXLSXTool.SetAudit(auditLog)
	a.toolReg.RegisterWithPolicy(writeXLSXTool, tool.ToolPolicy{Risk: tool.RiskWriteLocal})
}

// initMemory 初始化工作区记忆。
func (a *Agent) initMemory(projectDir string) {
	store := memory.NewWorkspaceStore(projectDir)
	a.memStore = store
	//remember 会写本地记忆文件，属于副作用操作，需要人工确认
	a.toolReg.RegisterWithPolicy(tools.NewRememberTool(store), tool.ToolPolicy{Risk: tool.RiskWriteLocal})

	retriever, closer, err := buildRetriever(store)
	if err != nil {
		fmt.Printf("[记忆索引不可用，降级为内存关键词检索]:%v\n", err)
		retriever = memory.KeywordRetriever{}
		closer = nil
	}
	if closer != nil {
		a.closers = append(a.closers, closer)
	}
	a.memLoad = memory.NewLoader(store, retriever, 0, 0)
	// 运行中按需召回：只读、自动放行，避免把全部记忆一次性塞进上下文
	a.toolReg.RegisterWithPolicy(tools.NewMemorySearchTool(a.memLoad), tool.ToolPolicy{
		Risk:        tool.RiskReadOnly,
		AutoApprove: true,
	})
}

// buildRetriever 组装混合检索器：SQLite 索引 + 阿里云百炼 embedding。
func buildRetriever(store *memory.WorkspaceStore) (memory.Retriever, io.Closer, error) {
	index, err := sqliteidx.New(store.Dir())
	if err != nil {
		return nil, nil, err
	}
	embedder, err := embed.NewOpenAICompatibleEmbedder(embed.Config{
		BaseURL: os.Getenv(envEmbeddingBaseURL),
		APIKey:  os.Getenv(envEmbeddingAPIKey),
		Model:   os.Getenv(envEmbeddingModel),
	})
	if err != nil {
		// 索引已打开，必须显式关闭，否则文件句柄泄漏
		_ = index.Close()
		return nil, nil, err
	}
	retriever, err := memory.NewHybridRetriever(index, embedder, store.Path())
	if err != nil {
		_ = index.Close()
		return nil, nil, err
	}
	return retriever, index, nil
}

// Run 在指定会话中执行任务，返回最终答案。
//
// sessionID 为空时自动新建会话，实际使用的 ID 可由 SessionID() 获取。
func (a *Agent) Run(ctx context.Context, sessionID, userQuery string) (string, error) {
	a.cleanupOnce.Do(func() {
		if err := a.cleanupExpiredSessions(ctx); err != nil {
			fmt.Printf("[过期会话清理失败]:%v\n", err)
		}
	})

	a.sessionID = sessionID
	if a.sessionID == "" {
		generated, err := newSessionID()
		if err != nil {
			return "", err
		}
		a.sessionID = generated
	}
	if err := a.restoreHistory(ctx); err != nil {
		fmt.Printf("[会话历史载入失败，降级为单轮执行]:%v\n", err)
		a.memory.Replace(nil)
	}

	if err := a.refreshMemoryBlock(ctx, userQuery); err != nil {
		fmt.Printf("[记忆加载失败，跳过注入]:%v\n", err)
		a.memBlock = ""
	}

	a.appendMessage(ctx, session.KindPrompt, session.RoleUser, userQuery)

	p := &runProgress{loopLimit: a.maxLoop}
	for {
		outcome, err := a.step(ctx, p, false)
		if err != nil {
			return "", err
		}
		switch outcome {
		case domainagent.RunFinal:
			a.emit(domainagent.AgentEvent{Type: domainagent.EventFinal, Result: p.finalAnswer})
			return p.finalAnswer, nil
		case domainagent.RunPaused:
			// 生产模式 wbExec 为 nil，永不返回 errToolDeferred，此处为防御性兜底
			return "", errors.New("生产模式不应出现外部工具暂停")
		case domainagent.RunLimit:
			a.emit(domainagent.AgentEvent{Type: domainagent.EventFinal, Result: "任务已由人工中止，未产生最终答案。"})
			return "任务已由人工中止，未产生最终答案。", nil
		}
	}
}

// RollbackSession 把会话 HEAD 回退到指定消息，并返回为原路径自动建立的存档分支 ID。
func (a *Agent) RollbackSession(ctx context.Context, sessionID string, targetID int64) (string, error) {
	if a.sessStore == nil {
		return "", errors.New("会话持久化未启用，无法回滚")
	}
	return a.sessStore.Rollback(ctx, sessionID, targetID)
}

// ForkSession 从指定消息分叉出一条新分支，fromID 为 0 表示复制当前 HEAD。
func (a *Agent) ForkSession(ctx context.Context, sessionID string, fromID int64) (string, error) {
	if a.sessStore == nil {
		return "", errors.New("会话持久化未启用，无法分叉")
	}
	return a.sessStore.Fork(ctx, sessionID, fromID)
}

// ListBranches 列出同一条对话树上的所有分支。
func (a *Agent) ListBranches(ctx context.Context, sessionID string) ([]session.Branch, error) {
	if a.sessStore == nil {
		return nil, errors.New("会话持久化未启用，无法列出分支")
	}
	return a.sessStore.ListBranches(ctx, sessionID)
}

// LoadHistory 读取指定会话当前分支的完整历史，供人工挑选回滚点。
// 压缩记录属于内部元数据，不作为消息展示给用户，也不参与回滚点选择。
func (a *Agent) LoadHistory(ctx context.Context, sessionID string) ([]session.Message, error) {
	if a.sessStore == nil {
		return nil, errors.New("会话持久化未启用，无法读取历史")
	}
	messages, err := a.sessStore.LoadBranch(ctx, sessionID, 0)
	if err != nil {
		return nil, err
	}
	visible := make([]session.Message, 0, len(messages))
	for _, message := range messages {
		if message.Kind == session.KindCompaction {
			continue
		}
		visible = append(visible, message)
	}
	return visible, nil
}

// cleanupExpiredSessions 删除超过保留期的会话
func (a *Agent) cleanupExpiredSessions(ctx context.Context) error {
	if a.sessStore == nil {
		return nil
	}
	removed, err := a.sessStore.DeleteExpired(ctx, time.Now().Add(-defaultSessionRetention))
	if err != nil {
		return err
	}
	if removed > 0 {
		fmt.Printf("[已清理 %d 个超过保留期的会话]\n", removed)
	}
	return nil
}

// newSessionID 生成随机会话 ID
func newSessionID() (string, error) {
	buf := make([]byte, 8)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("生成会话 ID 失败: %w", err)
	}
	return "sess-" + hex.EncodeToString(buf), nil
}
