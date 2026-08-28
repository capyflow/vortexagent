// 本文件测试异步任务编排（TaskHub）：分发后立即返回、后台执行、
// 状态流转、并发上限排队、取消、等待超时，以及主 agent 通过工具端到端使用。
package agent

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/capyflow/vortexagent/agent/sessionstore"
	"github.com/capyflow/vortexagent/llm"
)

// gateProvider 的 Chat 会阻塞直到 gate 关闭（或 ctx 取消），用于构造"仍在运行"的任务。
type gateProvider struct {
	gate  chan struct{}
	calls int
}

func (p *gateProvider) Name() string       { return "gate" }
func (p *gateProvider) ContextWindow() int { return 128000 }

func (p *gateProvider) Chat(ctx context.Context, _ *llm.ChatRequest, _ func(llm.Delta) error) (*llm.ChatResponse, error) {
	p.calls++
	select {
	case <-p.gate:
		resp := textResp("闸门后的回答")
		return &resp, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// TestTaskHub_SubmitAndWait 校验完整异步链路：提交立即返回，等待后收取结果。
func TestTaskHub_SubmitAndWait(t *testing.T) {
	subProv := &scriptProvider{name: "sub", script: []llm.ChatResponse{textResp("库存还有 32 件")}}
	sub := New(Options{Provider: subProv, Model: "m"})

	hub := NewTaskHub(context.Background(), 2)
	if err := hub.Register("stock", sub); err != nil {
		t.Fatal(err)
	}

	id, err := hub.Submit("stock", "查询库存")
	if err != nil {
		t.Fatalf("Submit 失败: %v", err)
	}
	if !strings.HasPrefix(id, "task-") {
		t.Errorf("任务 ID = %q, 应以 task- 开头", id)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := hub.WaitAll(ctx); err != nil {
		t.Fatalf("WaitAll 失败: %v", err)
	}

	task, ok := hub.Status(id)
	if !ok {
		t.Fatal("任务应存在")
	}
	if task.State != TaskDone {
		t.Fatalf("状态 = %s, 期望 done（错误: %s）", task.State, task.Err)
	}
	if task.Result != "库存还有 32 件" {
		t.Errorf("结果 = %q", task.Result)
	}
	if task.SessionID == "" {
		t.Error("应记录子 agent 会话 ID")
	}
	if task.StartedAt.IsZero() || task.EndedAt.IsZero() {
		t.Error("应记录开始与结束时间")
	}
}

// TestTaskHub_SubmitReturnsImmediately 校验分发不阻塞：子 agent 卡住时，
// Submit / task_start 也应在毫秒级返回。
func TestTaskHub_SubmitReturnsImmediately(t *testing.T) {
	gate := make(chan struct{})
	sub := New(Options{Provider: &gateProvider{gate: gate}, Model: "m"})
	hub := NewTaskHub(context.Background(), 2)
	if err := hub.Register("slow", sub); err != nil {
		t.Fatal(err)
	}

	start := time.Now()
	id, err := hub.Submit("slow", "长任务")
	if err != nil {
		t.Fatalf("Submit 失败: %v", err)
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Errorf("Submit 耗时 %v, 应立即返回", elapsed)
	}

	task, _ := hub.Status(id)
	if task.State != TaskRunning && task.State != TaskPending {
		t.Errorf("状态 = %s, 期望 running 或 pending", task.State)
	}

	// 收尾：放行并等待，避免泄漏 goroutine
	close(gate)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_ = hub.WaitAll(ctx)
}

// TestTaskHub_WaitTimeout 校验等待超时：Wait 提前返回但任务继续在后台运行，可再取消。
func TestTaskHub_WaitTimeout(t *testing.T) {
	gate := make(chan struct{})
	sub := New(Options{Provider: &gateProvider{gate: gate}, Model: "m"})
	hub := NewTaskHub(context.Background(), 2)
	if err := hub.Register("slow", sub); err != nil {
		t.Fatal(err)
	}

	id, err := hub.Submit("slow", "长任务")
	if err != nil {
		t.Fatal(err)
	}

	waitCtx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	if _, err := hub.Wait(waitCtx, []string{id}); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("期望等待超时，实际: %v", err)
	}

	// 超时后任务应仍在后台运行
	task, _ := hub.Status(id)
	if task.State != TaskRunning {
		t.Errorf("超时后状态 = %s, 期望仍为 running", task.State)
	}

	// 取消后应进入 canceled 终态
	if err := hub.Cancel(id); err != nil {
		t.Fatalf("Cancel 失败: %v", err)
	}
	ctx, cancel2 := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel2()
	if err := hub.WaitAll(ctx); err != nil {
		t.Fatalf("取消后 WaitAll 失败: %v", err)
	}
	task, _ = hub.Status(id)
	if task.State != TaskCanceled {
		t.Errorf("取消后状态 = %s, 期望 canceled", task.State)
	}
}

// waitTaskState 轮询等待任务进入期望状态（goroutine 调度无固定顺序，不能假设先提交先运行）。
func waitTaskState(t *testing.T, hub *TaskHub, id string, want TaskState) *Task {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if task, ok := hub.Status(id); ok && task.State == want {
			return task
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("任务 %s 未在限时内进入 %s 状态", id, want)
	return nil
}

// TestTaskHub_ConcurrencyLimit 校验并发槽位：达到上限后的任务排队（pending），
// 槽位释放后依次执行。
func TestTaskHub_ConcurrencyLimit(t *testing.T) {
	gate := make(chan struct{})
	sub := New(Options{Provider: &gateProvider{gate: gate}, Model: "m"})
	hub := NewTaskHub(context.Background(), 1)
	if err := hub.Register("worker", sub); err != nil {
		t.Fatal(err)
	}

	id1, _ := hub.Submit("worker", "任务一")
	id2, _ := hub.Submit("worker", "任务二")

	// 并发上限为 1：稳定后必然一个 running、一个 pending
	// （谁先抢到槽位由 goroutine 调度决定，不能假设提交顺序）
	deadline := time.Now().Add(2 * time.Second)
	for {
		s1, _ := hub.Status(id1)
		s2, _ := hub.Status(id2)
		one := s1.State == TaskRunning && s2.State == TaskPending
		other := s1.State == TaskPending && s2.State == TaskRunning
		if one || other {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("任务未稳定为一运行一排队: %s / %s", s1.State, s2.State)
		}
		time.Sleep(5 * time.Millisecond)
	}

	close(gate)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := hub.WaitAll(ctx); err != nil {
		t.Fatalf("WaitAll 失败: %v", err)
	}
}

// TestTaskHub_CancelPending 校验取消排队中的任务：立即出队，不占用槽位。
func TestTaskHub_CancelPending(t *testing.T) {
	gate := make(chan struct{})
	sub := New(Options{Provider: &gateProvider{gate: gate}, Model: "m"})
	hub := NewTaskHub(context.Background(), 1)
	if err := hub.Register("worker", sub); err != nil {
		t.Fatal(err)
	}

	id1, _ := hub.Submit("worker", "占用槽位")
	waitTaskState(t, hub, id1, TaskRunning)
	id2, _ := hub.Submit("worker", "排队中")
	waitTaskState(t, hub, id2, TaskPending)

	if err := hub.Cancel(id2); err != nil {
		t.Fatalf("Cancel 失败: %v", err)
	}
	s2, _ := hub.Status(id2)
	if s2.State != TaskCanceled {
		t.Errorf("排队任务取消后状态 = %s, 期望 canceled", s2.State)
	}

	close(gate)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := hub.WaitAll(ctx); err != nil {
		t.Fatalf("WaitAll 失败: %v", err)
	}
	s1, _ := hub.Status(id1)
	if s1.State != TaskDone {
		t.Errorf("任务一状态 = %s, 期望 done", s1.State)
	}
}

// TestTaskHub_UnknownAgent 校验分发到未注册的子 agent 时报不可重试错误，并列出可用值。
func TestTaskHub_UnknownAgent(t *testing.T) {
	hub := NewTaskHub(context.Background(), 1)
	if err := hub.Register("known", New(Options{Provider: &scriptProvider{name: "s"}, Model: "m"})); err != nil {
		t.Fatal(err)
	}

	_, err := hub.Submit("nope", "任务")
	var nr *nonRetryableError
	if err == nil || !errors.As(err, &nr) {
		t.Fatalf("未知子 agent 应返回不可重试错误，实际: %v", err)
	}
	if !strings.Contains(err.Error(), "known") {
		t.Errorf("错误信息 = %v, 应列出已注册的子 agent 名称", err)
	}
}

// TestTaskHub_AgentEndToEnd 校验主 agent 通过工具使用 TaskHub 的完整流程：
// task_start 分发 → task_wait 收取 → 汇总回答，分发后主循环不被阻塞。
func TestTaskHub_AgentEndToEnd(t *testing.T) {
	subProv := &scriptProvider{name: "sub", script: []llm.ChatResponse{textResp("库存 32 件")}}
	sub := New(Options{Provider: subProv, Model: "m"})

	hub := NewTaskHub(context.Background(), 2)
	if err := hub.Register("stock", sub); err != nil {
		t.Fatal(err)
	}

	parentProv := &scriptProvider{name: "parent", script: []llm.ChatResponse{
		toolCallResp("task_start", map[string]any{"agent": "stock", "task": "查询库存"}),
		toolCallResp("task_wait", map[string]any{"task_ids": []any{}}),
		textResp("汇总：库存 32 件"),
	}}
	reg := NewRegistry()
	for _, tool := range hub.Tools() {
		if err := reg.Add(tool); err != nil {
			t.Fatal(err)
		}
	}
	parent := New(Options{Provider: parentProv, Registry: reg, Model: "m"})
	session := sessionstore.NewSession("m")

	ans, err := parent.Ask(context.Background(), session, "帮我盘点库存")
	if err != nil {
		t.Fatalf("Ask 失败: %v", err)
	}
	if ans != "汇总：库存 32 件" {
		t.Errorf("最终回答 = %q", ans)
	}
	if subProv.callCount() != 1 {
		t.Errorf("子 agent 调用次数 = %d, 期望 1", subProv.callCount())
	}
	// task_wait 的结果应把子 agent 的回答带回父历史
	found := false
	for _, msg := range session.Messages() {
		if msg.Role == llm.RoleTool && strings.Contains(msg.Content[0].Text, "库存 32 件") {
			found = true
		}
	}
	if !found {
		t.Error("父历史中应包含 task_wait 带回的子任务结果")
	}
}

// TestTaskHub_HistoryPruned 校验任务记录淘汰：超过 maxTaskHistory 后
// 最旧的已结束任务被清理，最新任务保留。
func TestTaskHub_HistoryPruned(t *testing.T) {
	subProv := &scriptProvider{name: "sub", script: []llm.ChatResponse{{}}}
	// 脚本耗尽时返回固定文本，见 scriptProvider
	sub := New(Options{Provider: subProv, Model: "m"})
	hub := NewTaskHub(context.Background(), 8)
	if err := hub.Register("worker", sub); err != nil {
		t.Fatal(err)
	}

	const total = maxTaskHistory + 40
	ids := make([]string, 0, total)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	for i := 0; i < total; i++ {
		id, err := hub.Submit("worker", "任务")
		if err != nil {
			t.Fatalf("第 %d 个任务提交失败: %v", i, err)
		}
		ids = append(ids, id)
		// 分批等待：Submit 快于完成，避免撞上 maxPending 排队上限
		if (i+1)%50 == 0 {
			if err := hub.WaitAll(ctx); err != nil {
				t.Fatalf("批量等待失败: %v", err)
			}
		}
	}
	if err := hub.WaitAll(ctx); err != nil {
		t.Fatalf("WaitAll 失败: %v", err)
	}

	tasks := hub.Tasks()
	if len(tasks) > maxTaskHistory {
		t.Errorf("任务记录数 = %d, 应不超过 %d", len(tasks), maxTaskHistory)
	}
	// 最新任务必须仍在（最旧的被淘汰）
	if _, ok := hub.Status(ids[len(ids)-1]); !ok {
		t.Error("最新的任务记录不应被淘汰")
	}
}

// TestTaskHub_WaitToolTimeoutContinues 校验 task_wait 超时返回文本而非报错：
// 循环继续，模型可稍后再收。
func TestTaskHub_WaitToolTimeoutContinues(t *testing.T) {
	gate := make(chan struct{})
	sub := New(Options{Provider: &gateProvider{gate: gate}, Model: "m"})
	hub := NewTaskHub(context.Background(), 1)
	if err := hub.Register("slow", sub); err != nil {
		t.Fatal(err)
	}
	id, _ := hub.Submit("slow", "长任务")

	waitTool := &taskWaitTool{hub: hub}
	start := time.Now()
	out, err := waitTool.Call(context.Background(), map[string]any{
		"task_ids":        []any{id},
		"timeout_seconds": float64(0.2),
	})
	if err != nil {
		t.Fatalf("task_wait 不应返回错误: %v", err)
	}
	if elapsed := time.Since(start); elapsed < 150*time.Millisecond {
		t.Errorf("task_wait 返回过快（%v）, 应等待了超时时长", elapsed)
	}
	if !strings.Contains(out, "未完成") {
		t.Errorf("输出 = %q, 应说明仍有任务未完成", out)
	}

	close(gate)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_ = hub.WaitAll(ctx)
}
