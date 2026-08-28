// 本文件实现异步任务编排（TaskHub）：主 agent 把任务分发给具名子 agent 后立即返回，
// 子 agent 在后台 goroutine 中各自执行，主 agent 继续自己的循环，之后再通过
// task_status / task_wait 收取结果——即"发射后不管"（fire-and-forget）模式。
//
// 与 SubagentTool（同步委派）的区别：
//   - SubagentTool：父 agent 阻塞等待子任务完成，结果当轮拿到，适合"先查再答"；
//   - TaskHub：分发后循环立刻继续，适合"并行调研多项、最后汇总"或"启动长任务、
//     下轮对话再收结果"。
//
// 并发机制（Go 原语在框架内部的落点）：
//   - 每个任务一个 goroutine，跑完即退出，无常驻协程；
//   - 每个任务一个 done channel 作为完成信号，task_wait 用它做 join；
//   - 一个带缓冲的 semaphore channel 限制同时运行的子 agent 数量，超出的任务排队（pending）。
//
// 任务的生命周期挂在 TaskHub 的 base ctx（应用级）上，而不是某次 Ask 的请求 ctx：
// 主 agent 本轮回答结束、HTTP 请求返回都不会打断后台任务；只有应用退出（base ctx 取消）
// 或显式 task_cancel 才会终止它们。
package agent

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/capyflow/vortexagent/agent/sessionstore"
)

// TaskState 是后台任务的状态。
type TaskState string

const (
	TaskPending  TaskState = "pending"  // 已入队，等待并发槽位
	TaskRunning  TaskState = "running"  // 子 agent 执行中
	TaskDone     TaskState = "done"     // 成功，Result 可用
	TaskFailed   TaskState = "failed"   // 失败，Err 说明原因
	TaskCanceled TaskState = "canceled" // 被取消（task_cancel 或应用退出）
)

func (s TaskState) terminal() bool {
	return s == TaskDone || s == TaskFailed || s == TaskCanceled
}

// maxPending 是排队任务数上限：防止模型无节制地分发任务把内存吃满。
const maxPending = 64

// maxTaskHistory 是任务记录保留上限：超过后从最旧的已结束任务开始淘汰，
// 防止长驻进程的 tasks 表无限增长（进行中的任务不会被淘汰）。
const maxTaskHistory = 256

// Task 是一次后台子 agent 任务的记录。
type Task struct {
	ID        string    // 任务 ID（task-000001），收取结果时使用
	AgentName string    // 执行任务的子 agent 名称
	Input     string    // 委派的任务描述
	State     TaskState // 当前状态
	Result    string    // 子 agent 的最终回答（done 时有效）
	Err       string    // 失败或取消原因（failed / canceled 时有效）
	SessionID string    // 子 agent 会话 ID（子 agent 配置了 Store 时可按此回查完整记录）
	StartedAt time.Time
	EndedAt   time.Time

	done     chan struct{}      // 完成信号：进入终态时关闭，task_wait 据此 join
	cancel   context.CancelFunc // running 状态下取消执行
	cancelCh chan struct{}      // pending 状态下取消排队（关闭即取消）
}

// copy 返回快照（字段读取需持锁，done channel 原样复制供外部等待）。
func (t *Task) copy() *Task {
	c := *t
	return &c
}

// TaskHub 管理具名子 agent 与后台任务，是对外的主入口。
//
// 用法：创建 hub → Register 各专员子 agent → 把 hub.Tools() 注册进主 agent 的
// Registry → 应用退出前 hub.WaitAll 收尾。
type TaskHub struct {
	baseCtx context.Context
	sem     chan struct{} // 并发槽位

	mu     sync.Mutex
	agents map[string]*Agent
	tasks  map[string]*Task
	seq    atomic.Int64
}

// NewTaskHub 创建任务中心。baseCtx 是任务的生命周期锚点，应传入应用级 ctx
// （如 signal.NotifyContext 的返回值），不要传某次请求的 ctx——请求结束会误杀后台任务。
// maxConcurrent 限制同时运行的子 agent 数量，<=0 时默认 4。
func NewTaskHub(baseCtx context.Context, maxConcurrent int) *TaskHub {
	if baseCtx == nil {
		baseCtx = context.Background()
	}
	if maxConcurrent <= 0 {
		maxConcurrent = 4
	}
	return &TaskHub{
		baseCtx: baseCtx,
		sem:     make(chan struct{}, maxConcurrent),
		agents:  make(map[string]*Agent),
		tasks:   make(map[string]*Task),
	}
}

// Register 注册一个具名子 agent，task_start 按 name 分发任务。
func (h *TaskHub) Register(name string, sub *Agent) error {
	if strings.TrimSpace(name) == "" {
		return fmt.Errorf("agent: 子 agent 名称不能为空")
	}
	if sub == nil {
		return fmt.Errorf("agent: 子 agent %q 未配置", name)
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if _, dup := h.agents[name]; dup {
		return fmt.Errorf("agent: 子 agent %q 已存在", name)
	}
	h.agents[name] = sub
	return nil
}

// Submit 分发一个后台任务，立即返回任务 ID，不等待执行。
// agentName 未注册或排队已满时返回错误（均不可重试）。
func (h *TaskHub) Submit(agentName, input string) (string, error) {
	input = strings.TrimSpace(input)
	if agentName == "" || input == "" {
		return "", &nonRetryableError{fmt.Errorf("agent 与 task 均不能为空")}
	}

	h.mu.Lock()
	sub, ok := h.agents[agentName]
	if !ok {
		h.mu.Unlock()
		return "", &nonRetryableError{fmt.Errorf("未知子 agent %q（可用: %s）", agentName, strings.Join(h.agentNames(), ", "))}
	}
	// 上限只统计未完成任务（pending+running）：已结束的任务只占查询记录，不占队列。
	active := 0
	for _, t := range h.tasks {
		if !t.State.terminal() {
			active++
		}
	}
	if active >= maxPending {
		h.mu.Unlock()
		return "", &nonRetryableError{fmt.Errorf("未完成任务已达上限 %d，请先收取已有任务的结果", maxPending)}
	}
	id := fmt.Sprintf("task-%06d", h.seq.Add(1))
	t := &Task{
		ID:        id,
		AgentName: agentName,
		Input:     input,
		State:     TaskPending,
		done:      make(chan struct{}),
		cancelCh:  make(chan struct{}),
	}
	h.tasks[id] = t
	h.mu.Unlock()

	go h.run(t, sub)
	return id, nil
}

// run 是单个后台任务的执行体：排队 → 执行 → 记录结果。
func (h *TaskHub) run(t *Task, sub *Agent) {
	defer close(t.done)

	// 等待并发槽位；排队期间可被 task_cancel 或应用退出打断。
	select {
	case h.sem <- struct{}{}:
		defer func() { <-h.sem }()
	case <-t.cancelCh:
		h.finish(t, TaskCanceled, "", errors.New("任务在排队时被取消"))
		return
	case <-h.baseCtx.Done():
		h.finish(t, TaskCanceled, "", h.baseCtx.Err())
		return
	}

	// 拿到槽位后再检查一次：Cancel 可能在排队与启动之间已把任务标记为取消。
	h.mu.Lock()
	if t.State == TaskCanceled {
		h.mu.Unlock()
		return
	}
	ctx, cancel := context.WithCancel(h.baseCtx)
	t.cancel = cancel
	t.State = TaskRunning
	t.StartedAt = time.Now()
	h.mu.Unlock()
	defer cancel()

	session := sessionstore.NewSession(sub.model)
	h.mu.Lock()
	t.SessionID = session.ID
	h.mu.Unlock()

	answer, err := sub.Ask(ctx, session, t.Input)
	if err != nil {
		state := TaskFailed
		if errors.Is(err, context.Canceled) {
			state = TaskCanceled
		}
		h.finish(t, state, "", err)
		return
	}
	h.finish(t, TaskDone, answer, nil)
}

// finish 记录任务终态（幂等：终态不会被覆盖），随后按需淘汰最旧的已结束任务。
func (h *TaskHub) finish(t *Task, state TaskState, result string, err error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if t.State.terminal() {
		return
	}
	t.State = state
	t.Result = result
	if err != nil {
		t.Err = err.Error()
	}
	t.EndedAt = time.Now()
	h.pruneLocked()
}

// pruneLocked 淘汰最旧的已结束任务，把记录数压回 maxTaskHistory 以内
// （按 ID 递增即提交顺序；进行中的任务不淘汰）。
// 调用方需持有 h.mu。
func (h *TaskHub) pruneLocked() {
	if len(h.tasks) <= maxTaskHistory {
		return
	}
	var terminal []string
	for id, t := range h.tasks {
		if t.State.terminal() {
			terminal = append(terminal, id)
		}
	}
	sort.Strings(terminal)
	for _, id := range terminal {
		if len(h.tasks) <= maxTaskHistory {
			return
		}
		delete(h.tasks, id)
	}
}

// Status 返回任务快照；id 不存在时第二个返回值为 false。
func (h *TaskHub) Status(id string) (*Task, bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	t, ok := h.tasks[id]
	if !ok {
		return nil, false
	}
	return t.copy(), true
}

// Tasks 返回所有任务快照（按 ID 排序）。
func (h *TaskHub) Tasks() []*Task {
	h.mu.Lock()
	defer h.mu.Unlock()
	out := make([]*Task, 0, len(h.tasks))
	for _, t := range h.tasks {
		out = append(out, t.copy())
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// Wait 阻塞等待给定任务全部进入终态；ids 为空时等待当前所有任务。
// ctx 取消或超时则提前返回错误（任务本身不受影响，仍在后台运行）。
func (h *TaskHub) Wait(ctx context.Context, ids []string) ([]string, error) {
	resolved, err := h.resolve(ids)
	if err != nil {
		return nil, err
	}
	for _, id := range resolved {
		t, ok := h.Status(id)
		if !ok {
			return resolved, fmt.Errorf("agent: 任务 %s 不存在", id)
		}
		select {
		case <-t.done:
		case <-ctx.Done():
			return resolved, ctx.Err()
		}
	}
	return resolved, nil
}

// WaitAll 等待当前所有任务进入终态（应用优雅退出时使用）。
func (h *TaskHub) WaitAll(ctx context.Context) error {
	_, err := h.Wait(ctx, nil)
	return err
}

// Cancel 取消任务：running 状态中断执行，pending 状态直接出队。已结束的任务返回错误。
func (h *TaskHub) Cancel(id string) error {
	h.mu.Lock()
	t, ok := h.tasks[id]
	if !ok {
		h.mu.Unlock()
		return &nonRetryableError{fmt.Errorf("agent: 任务 %s 不存在", id)}
	}
	switch {
	case t.State == TaskPending:
		// 标记终态并关闭 cancelCh 唤醒排队的 goroutine（它退出时 finish 为幂等空操作）。
		h.finishLocked(t, TaskCanceled, "", errors.New("任务在排队时被取消"))
		close(t.cancelCh)
		h.mu.Unlock()
		return nil
	case t.State.terminal():
		h.mu.Unlock()
		return &nonRetryableError{fmt.Errorf("任务 %s 已结束（%s），无需取消", id, t.State)}
	default:
		cancel := t.cancel
		h.mu.Unlock()
		if cancel != nil {
			cancel()
		}
		return nil
	}
}

// resolve 把 ids 解析为确定的任务 ID 列表（空表示全部）。
func (h *TaskHub) resolve(ids []string) ([]string, error) {
	if len(ids) > 0 {
		h.mu.Lock()
		defer h.mu.Unlock()
		var unknown []string
		for _, id := range ids {
			if _, ok := h.tasks[id]; !ok {
				unknown = append(unknown, id)
			}
		}
		if len(unknown) > 0 {
			return nil, &nonRetryableError{fmt.Errorf("agent: 任务不存在: %s", strings.Join(unknown, ", "))}
		}
		return ids, nil
	}
	tasks := h.Tasks()
	out := make([]string, len(tasks))
	for i, t := range tasks {
		out[i] = t.ID
	}
	return out, nil
}

func (h *TaskHub) agentNames() []string {
	names := make([]string, 0, len(h.agents))
	for name := range h.agents {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

func (h *TaskHub) finishLocked(t *Task, state TaskState, result string, err error) {
	if t.State.terminal() {
		return
	}
	t.State = state
	t.Result = result
	if err != nil {
		t.Err = err.Error()
	}
	t.EndedAt = time.Now()
}

// ---- 模型侧工具：主 agent 通过以下四个工具使用 TaskHub ----

// Tools 返回注册进主 agent Registry 的任务工具集：
// task_start（分发）/ task_status（查询与收取结果）/ task_wait（等待完成）/ task_cancel（取消）。
func (h *TaskHub) Tools() []Tool {
	return []Tool{
		&taskStartTool{hub: h},
		&taskStatusTool{hub: h},
		&taskWaitTool{hub: h},
		&taskCancelTool{hub: h},
	}
}

type taskStartTool struct{ hub *TaskHub }

func (t *taskStartTool) Name() string { return "task_start" }
func (t *taskStartTool) Description() string {
	return "把任务分发给指定的子 agent 在后台异步执行，立即返回任务 ID、不阻塞当前工作。分发后可继续处理其他事项，之后用 task_wait 等待完成或 task_status 查询进度。"
}

func (t *taskStartTool) Schema() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"agent": map[string]any{
				"type":        "string",
				"description": "子 agent 名称（未知的名称会报错并列出可用值）",
			},
			"task": map[string]any{
				"type":        "string",
				"description": "任务描述。子 agent 看不到当前对话历史，请把背景、目标与约束写成一条自包含的指令",
			},
		},
		"required": []string{"agent", "task"},
	}
}

func (t *taskStartTool) Call(_ context.Context, args map[string]any) (string, error) {
	agentName := strings.TrimSpace(asText(args["agent"]))
	task := strings.TrimSpace(asText(args["task"]))
	if agentName == "" || task == "" {
		return "", &nonRetryableError{fmt.Errorf("agent 与 task 均不能为空")}
	}
	id, err := t.hub.Submit(agentName, task)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("任务 %s 已提交（agent=%s），后台执行中。你可以继续处理其他事项；用 task_wait 等待完成，或用 task_status 查询进度。", id, agentName), nil
}

type taskStatusTool struct{ hub *TaskHub }

func (t *taskStatusTool) Name() string { return "task_status" }
func (t *taskStatusTool) Description() string {
	return "查询后台任务的状态与结果。不传 task_id 时列出全部任务；任务完成后结果会一并列出。"
}

func (t *taskStatusTool) Schema() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"task_id": map[string]any{"type": "string", "description": "要查询的任务 ID，留空则列出全部任务"},
		},
	}
}

func (t *taskStatusTool) Call(_ context.Context, args map[string]any) (string, error) {
	id := strings.TrimSpace(asText(args["task_id"]))
	if id != "" {
		task, ok := t.hub.Status(id)
		if !ok {
			return "", &nonRetryableError{fmt.Errorf("agent: 任务 %s 不存在", id)}
		}
		return formatTask(task), nil
	}
	tasks := t.hub.Tasks()
	if len(tasks) == 0 {
		return "当前没有后台任务", nil
	}
	var sb strings.Builder
	for _, task := range tasks {
		sb.WriteString(formatTask(task))
		sb.WriteString("\n")
	}
	return strings.TrimRight(sb.String(), "\n"), nil
}

type taskWaitTool struct{ hub *TaskHub }

func (t *taskWaitTool) Name() string { return "task_wait" }
func (t *taskWaitTool) Description() string {
	return "阻塞等待一批后台任务完成后返回全部结果（task_ids 留空表示等待所有任务）。超时未完成的任务会照常列出当前状态，可稍后再次等待。在给出最终回答前，若回答依赖后台任务的结果，应先调用本工具。"
}

func (t *taskWaitTool) Schema() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"task_ids": map[string]any{
				"type":        "array",
				"items":       map[string]any{"type": "string"},
				"description": "要等待的任务 ID 列表，留空表示等待所有任务",
			},
			"timeout_seconds": map[string]any{
				"type":        "number",
				"description": "最长等待秒数，默认 120",
			},
		},
	}
}

func (t *taskWaitTool) Call(ctx context.Context, args map[string]any) (string, error) {
	ids := asStringList(args["task_ids"])
	timeout := 120 * time.Second
	if n, ok := args["timeout_seconds"].(float64); ok && n > 0 {
		timeout = time.Duration(n * float64(time.Second))
		if timeout > time.Hour {
			timeout = time.Hour
		}
	}
	waitCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	resolved, err := t.hub.Wait(waitCtx, ids)
	timedOut := errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled)
	if err != nil && !timedOut {
		return "", err
	}

	tasks := t.hub.Tasks()
	byID := make(map[string]*Task, len(tasks))
	for _, task := range tasks {
		byID[task.ID] = task
	}
	var sb strings.Builder
	pendingCount := 0
	for _, id := range resolved {
		task, ok := byID[id]
		if !ok {
			continue
		}
		sb.WriteString(formatTask(task))
		sb.WriteString("\n")
		if !task.State.terminal() {
			pendingCount++
		}
	}
	if timedOut {
		sb.WriteString(fmt.Sprintf("\n等待超时，仍有 %d 个任务未完成（不影响后台执行），可稍后再次 task_wait 或 task_status 查询。", pendingCount))
	}
	return strings.TrimRight(sb.String(), "\n"), nil
}

type taskCancelTool struct{ hub *TaskHub }

func (t *taskCancelTool) Name() string        { return "task_cancel" }
func (t *taskCancelTool) Description() string { return "取消一个尚未完成的后台任务" }

func (t *taskCancelTool) Schema() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"task_id": map[string]any{"type": "string", "description": "要取消的任务 ID"},
		},
		"required": []string{"task_id"},
	}
}

func (t *taskCancelTool) Call(_ context.Context, args map[string]any) (string, error) {
	id := strings.TrimSpace(asText(args["task_id"]))
	if id == "" {
		return "", &nonRetryableError{fmt.Errorf("task_id 不能为空")}
	}
	if err := t.hub.Cancel(id); err != nil {
		return "", err
	}
	return fmt.Sprintf("任务 %s 已请求取消", id), nil
}

// formatTask 把任务快照格式化为给模型看的单行/多行文本。
func formatTask(t *Task) string {
	var sb strings.Builder
	fmt.Fprintf(&sb, "%s [%s] agent=%s", t.ID, t.State, t.AgentName)
	if !t.StartedAt.IsZero() && t.EndedAt.IsZero() {
		fmt.Fprintf(&sb, " 已运行 %ds", int(time.Since(t.StartedAt).Seconds()))
	}
	sb.WriteString("\n  任务: " + oneLine(t.Input))
	switch t.State {
	case TaskDone:
		sb.WriteString("\n  结果: " + t.Result)
	case TaskFailed:
		sb.WriteString("\n  错误: " + t.Err)
	case TaskCanceled:
		sb.WriteString("\n  取消原因: " + t.Err)
	}
	return sb.String()
}

// oneLine 把多行文本压成一行（列表展示用；完整结果仍保留原文）。
func oneLine(s string) string {
	return strings.Join(strings.Fields(s), " ")
}

// asStringList 把 JSON 数组参数（[]any / []string / 单个字符串）统一为字符串列表。
func asStringList(v any) []string {
	switch items := v.(type) {
	case []string:
		return items
	case []any:
		out := make([]string, 0, len(items))
		for _, item := range items {
			if s := strings.TrimSpace(asText(item)); s != "" {
				out = append(out, s)
			}
		}
		return out
	default:
		if s := strings.TrimSpace(asText(v)); s != "" && v != nil {
			return []string{s}
		}
		return nil
	}
}
