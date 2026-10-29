// Package approver 提供人工确认的终端交互实现。
//
// 这是 domain/approval.Approver 的基础设施实现：把待确认的操作渲染到终端，
// 阻塞读取人工输入。领域层只依赖 Approver 接口，不关心这里的交互形式。
package approver

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync/atomic"
	"time"

	"simpleAgent/domain/approval"
)

// defaultInteractiveTimeout 终端人工确认的默认等待上限，超时按拒绝处理。
// 终端读取不可中断，必须靠超时避免无人值守时永久挂起。
const defaultInteractiveTimeout = 5 * time.Minute

// InteractiveApprover 在终端上阻塞等待人工决策
type InteractiveApprover struct {
	reader *bufio.Reader
	out    io.Writer
	// timeout 单次确认的等待上限，零值使用 defaultInteractiveTimeout
	timeout time.Duration
	// maxRetries 输入无法识别时的重试次数，超过后按拒绝处理（fail-safe）
	maxRetries int
	// pending 标记是否已有一次读取悬空未完成。
	// 终端读取本身无法中断，ctx 取消后旧协程仍会阻塞在 ReadString 上；
	// 此时若再发起读取，两个协程会争抢同一行输入，因此直接拒绝新读取。
	pending atomic.Bool
}

// NewInteractiveApprover 创建终端交互式审批器
func NewInteractiveApprover(in io.Reader, out io.Writer) *InteractiveApprover {
	return &InteractiveApprover{
		reader:     bufio.NewReader(in),
		out:        out,
		timeout:    defaultInteractiveTimeout,
		maxRetries: 3,
	}
}

// Approve 实现 Approver：渲染请求后阻塞读取人工输入。
//
// 读取放在独立协程中，以便 ctx 取消时立刻返回。协程会一直阻塞在 ReadString 上
// 直到用户输入或进程退出——这是终端读取不可中断导致的固有限制。
func (a *InteractiveApprover) Approve(ctx context.Context, req approval.ApprovalRequest) (approval.ApprovalResult, error) {
	if a == nil || a.reader == nil {
		return approval.ApprovalResult{}, errors.New("交互式审批器未初始化")
	}
	a.render(req)

	timeoutCtx, cancel := a.withTimeout(ctx)
	defer cancel()
	result, err := a.collect(timeoutCtx, req)
	if err != nil {
		// 超时属于预期内的安全结果：按拒绝返回，让模型自行调整而不是中止整次运行
		if errors.Is(err, context.DeadlineExceeded) {
			fmt.Fprintln(a.out, "审批超时，已按拒绝处理。")
			return approval.ApprovalResult{Decision: approval.DecisionReject, Reason: "审批超时，已拒绝"}, nil
		}
		fmt.Fprintln(a.out, "已取消本次确认。")
		return approval.ApprovalResult{}, err
	}
	return result, nil
}

// withTimeout 给没有显式截止时间的上下文补上默认审批超时。
func (a *InteractiveApprover) withTimeout(ctx context.Context) (context.Context, context.CancelFunc) {
	timeout := a.timeout
	if timeout <= 0 {
		timeout = defaultInteractiveTimeout
	}
	if _, ok := ctx.Deadline(); ok {
		return ctx, func() {}
	}
	return context.WithTimeout(ctx, timeout)
}

// collect 反复读取，直到拿到可识别的决策或耗尽重试次数
func (a *InteractiveApprover) collect(ctx context.Context, req approval.ApprovalRequest) (approval.ApprovalResult, error) {
	for attempt := 0; attempt < a.maxRetries; attempt++ {
		line, err := a.readLine(ctx)
		if errors.Is(err, io.EOF) {
			// 输入流已关闭，无法继续询问，按拒绝处理
			fmt.Fprintln(a.out, "输入已结束，按拒绝处理。")
			return approval.ApprovalResult{Decision: approval.DecisionReject, Reason: "输入流已关闭"}, nil
		}
		if err != nil {
			return approval.ApprovalResult{}, err
		}

		// 计划改写需要多行输入：输入 e 之后继续读到空行为止
		if req.Kind == approval.KindPlan && isModifyInput(line) {
			block, blockErr := a.readBlock(ctx, modifyContent(line))
			if blockErr != nil {
				return approval.ApprovalResult{}, blockErr
			}
			if block == "" {
				fmt.Fprintf(a.out, "新计划不能为空，请重新输入（%s）> ", promptHint(req.Kind))
				continue
			}
			return approval.ApprovalResult{Decision: approval.DecisionModify, Arguments: block, Reason: block}, nil
		}

		result, ok := parseDecision(line)
		if ok {
			return result, nil
		}
		fmt.Fprintf(a.out, "无法识别的输入，请重新输入（%s）> ", promptHint(req.Kind))
	}
	fmt.Fprintln(a.out, "连续无法识别输入，按拒绝处理。")
	return approval.ApprovalResult{Decision: approval.DecisionReject, Reason: "无法识别的输入"}, nil
}

// readLine 读取一行输入，支持 ctx 取消。
// 返回 io.EOF 表示输入流已结束且没有更多内容。
func (a *InteractiveApprover) readLine(ctx context.Context) (string, error) {
	if !a.pending.CompareAndSwap(false, true) {
		return "", errors.New("上一次人工确认尚未结束，无法发起新的确认")
	}
	// 带缓冲的通道保证读取协程总能退出，不会因为没人接收而永久阻塞
	lines := make(chan readLineResult, 1)
	go func() {
		line, err := a.reader.ReadString('\n')
		lines <- readLineResult{line: line, err: err}
	}()

	select {
	case <-ctx.Done():
		// 读取协程仍占用着输入流，此处故意不归还 pending，
		// 让后续确认直接报错，而不是与它争抢同一行输入
		return "", fmt.Errorf("等待人工确认被中断: %w", ctx.Err())
	case res := <-lines:
		a.pending.Store(false)
		if res.err != nil && !errors.Is(res.err, io.EOF) {
			return "", fmt.Errorf("读取人工确认失败: %w", res.err)
		}
		line := strings.TrimRight(res.line, "\r\n")
		// 最后一行可能没有换行符，此时 ReadString 返回内容 + io.EOF，内容仍需保留
		if line == "" && errors.Is(res.err, io.EOF) {
			return "", io.EOF
		}
		return line, nil
	}
}

// readLineResult 单行读取结果
type readLineResult struct {
	line string
	err  error
}

// readBlock 连续读取多行直到空行，用于人工改写计划。
// first 非空时会作为首行一并计入，以支持「e 第一行内容」这种写法。
func (a *InteractiveApprover) readBlock(ctx context.Context, first string) (string, error) {
	collected := make([]string, 0, 8)
	if first != "" {
		collected = append(collected, first)
	}
	for {
		line, err := a.readLine(ctx)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return "", err
		}
		if strings.TrimSpace(line) == "" {
			break
		}
		collected = append(collected, line)
	}
	return strings.TrimSpace(strings.Join(collected, "\n")), nil
}

// render 把待确认的操作呈现给用户
func (a *InteractiveApprover) render(req approval.ApprovalRequest) {
	fmt.Fprintln(a.out, "")
	fmt.Fprintln(a.out, "========== 需要人工确认 ==========")
	fmt.Fprintf(a.out, "类型：%s\n", kindLabel(req.Kind))
	if req.ToolName != "" {
		fmt.Fprintf(a.out, "工具：%s\n", req.ToolName)
	}
	fmt.Fprintf(a.out, "风险：%s\n", req.Risk)
	if req.Round > 0 {
		fmt.Fprintf(a.out, "轮次：第 %d 轮\n", req.Round)
	}
	if req.Summary != "" {
		fmt.Fprintf(a.out, "说明：%s\n", req.Summary)
	}
	if req.Arguments != "" {
		fmt.Fprintf(a.out, "内容：\n%s\n", indentLines(req.Arguments))
	}
	fmt.Fprintf(a.out, "请输入 %s > ", promptHint(req.Kind))
}

// kindLabel 返回确认点类型的中文名
func kindLabel(kind string) string {
	switch kind {
	case approval.KindPlan:
		return "执行计划确认"
	case approval.KindTool:
		return "工具执行审批"
	case approval.KindStuck:
		return "任务卡住，需要人工介入"
	default:
		return kind
	}
}

// promptHint 返回当前确认点支持的输入形式
func promptHint(kind string) string {
	switch kind {
	case approval.KindPlan:
		return "[y] 放行  [n 理由] 否决  [e] 另行输入新计划（空行结束）"
	case approval.KindStuck:
		return "[e 新指令] 继续执行  [n] 收工"
	default:
		return "[y] 放行  [n 理由] 拒绝  [e 新参数] 修改后放行"
	}
}

// isModifyInput 判断输入是否以修改指令开头
func isModifyInput(line string) bool {
	switch strings.ToLower(modifyVerb(line)) {
	case "e", "edit", "改":
		return true
	default:
		return false
	}
}

// modifyVerb 取出输入开头的动词
func modifyVerb(line string) string {
	head, _, _ := strings.Cut(strings.TrimSpace(line), " ")
	return head
}

// modifyContent 取出修改指令后面的内容部分
func modifyContent(line string) string {
	_, rest, _ := strings.Cut(strings.TrimSpace(line), " ")
	return strings.TrimSpace(rest)
}

// parseDecision 解析人工输入：
//
//	y / yes / 是         放行
//	n / no / 否 [理由]   拒绝，理由可选
//	e / edit / 改 [内容] 修改后执行，内容为要替换成的新值
//
// 无法识别时 ok 为 false，由调用方重新提示。
func parseDecision(line string) (approval.ApprovalResult, bool) {
	trimmed := strings.TrimSpace(line)
	if trimmed == "" {
		return approval.ApprovalResult{}, false
	}
	content := modifyContent(trimmed)

	switch strings.ToLower(modifyVerb(trimmed)) {
	case "y", "yes", "是":
		return approval.ApprovalResult{Decision: approval.DecisionApprove}, true
	case "n", "no", "否":
		return approval.ApprovalResult{Decision: approval.DecisionReject, Reason: content}, true
	case "e", "edit", "改":
		if content == "" {
			return approval.ApprovalResult{}, false
		}
		return approval.ApprovalResult{Decision: approval.DecisionModify, Arguments: content, Reason: content}, true
	default:
		return approval.ApprovalResult{}, false
	}
}

// indentLines 给多行文本统一加缩进，便于在终端里区分提示语与内容
func indentLines(text string) string {
	lines := strings.Split(text, "\n")
	for i, line := range lines {
		lines[i] = "  " + line
	}
	return strings.Join(lines, "\n")
}
