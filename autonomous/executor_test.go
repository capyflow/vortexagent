package autonomous

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/capyflow/vortexagent/agent"
)

// stubExecutor 测试桩：直接返回固定结果，不消耗 token。
type stubExecutor struct {
	result string
	err    error
	calls  atomic.Int32
}

func (e *stubExecutor) Execute(_ context.Context, _ *Goal) (string, error) {
	e.calls.Add(1)
	return e.result, e.err
}

// TestLLMExecutor_NilSessionReturnsError 确保 session 缺失时返回明确错误而非 panic。
func TestLLMExecutor_NilSessionReturnsError(t *testing.T) {
	e := NewLLMExecutor(nil, "fake-model")
	g := NewGoal()
	g.Title = "测试"
	if _, err := e.Execute(context.Background(), g); err == nil {
		t.Fatal("session 未注入时应返回错误")
	}
}

// TestAutonomousAgent_ExecutorInjection 验证自定义 Executor 被使用，
// 且默认 LLM 路径完全不被调用（零 token 直投）。
func TestAutonomousAgent_ExecutorInjection(t *testing.T) {
	provider := &fakeProvider{name: "fake", maxRounds: 0}
	ag := agent.New(agent.Options{
		Provider: provider,
		Registry: agent.NewRegistry(),
		Model:    "fake-model",
	})

	store, err := NewJSONGoalStore(t.TempDir() + "/goals.json")
	if err != nil {
		t.Fatal(err)
	}

	exec := &stubExecutor{result: "direct-result"}
	autoAgent := New(Config{
		Agent:     ag,
		Model:     "fake-model",
		GoalStore: store,
		MaxSleep:  50 * time.Millisecond,
		Executor:  exec,
	})

	g := NewGoal()
	g.ID = "exec-1"
	g.Title = "直投目标"
	g.Status = GoalStatusActive
	g.NextRunAt = time.Now().Add(-1 * time.Second)
	g.Schedule = Schedule{Type: ScheduleOneShot}
	if err := autoAgent.AddGoal(g); err != nil {
		t.Fatalf("AddGoal 不应报错: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- autoAgent.Run(ctx) }()

	// 等待执行
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if exec.calls.Load() > 0 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}

	cancel()
	<-done // 等待 Run 退出后再读 provider，避免 data race

	if exec.calls.Load() != 1 {
		t.Errorf("自定义执行器应被调用 1 次, got %d", exec.calls.Load())
	}
	snap := g.Snapshot()
	if snap.LastResult != "direct-result" {
		t.Errorf("LastResult 应来自自定义执行器, got %q", snap.LastResult)
	}
	if snap.RunCount != 1 {
		t.Errorf("RunCount 应为 1, got %d", snap.RunCount)
	}
	if n := provider.rounds; n != 0 {
		t.Errorf("LLM 不应被调用, got %d 次", n)
	}
}

// TestObservabilityHooks 白盒验证 OnGoalDue / OnGoalExecute / OnGoalSkipped /
// OnGoalStatusChange 的触发时机与参数。
func TestObservabilityHooks(t *testing.T) {
	store, err := NewJSONGoalStore(t.TempDir() + "/goals.json")
	if err != nil {
		t.Fatal(err)
	}

	var (
		dueCalled     atomic.Int32
		execCalled    atomic.Int32
		skippedReason atomic.Value // string
		statusOld     atomic.Value // GoalStatus
		statusNew     atomic.Value // GoalStatus
	)

	autoAgent := New(Config{
		Model:     "fake-model",
		GoalStore: store,
		Executor:  &stubExecutor{result: "ok"},
		OnGoalDue: func(*Goal) {
			dueCalled.Add(1)
		},
		OnGoalExecute: func(*Goal, string) {
			execCalled.Add(1)
		},
		OnGoalSkipped: func(_ *Goal, reason string) {
			skippedReason.Store(reason)
		},
		OnGoalStatusChange: func(_ *Goal, old, new GoalStatus) {
			statusOld.Store(old)
			statusNew.Store(new)
		},
	})

	// 白盒初始化：不经 Run() 直接驱动 handleDueGoals
	autoAgent.mu.Lock()
	autoAgent.scheduler = NewScheduler(time.Hour)
	autoAgent.mu.Unlock()

	due := NewGoal()
	due.ID = "due-1"
	due.Title = "到期目标"
	due.Status = GoalStatusActive
	due.NextRunAt = time.Now().Add(-1 * time.Second)
	due.Schedule = Schedule{Type: ScheduleOneShot}
	autoAgent.scheduler.Add(due)

	skipMe := NewGoal()
	skipMe.ID = "skip-1"
	skipMe.Title = "次数用尽目标"
	skipMe.Status = GoalStatusActive
	skipMe.RunCount = 1
	skipMe.NextRunAt = time.Now().Add(-1 * time.Second)
	skipMe.Schedule = Schedule{Type: ScheduleOneShot, MaxRuns: 1} // 预检标记 done 并跳过
	autoAgent.scheduler.Add(skipMe)

	autoAgent.handleDueGoals(context.Background())

	if dueCalled.Load() != 1 {
		t.Errorf("OnGoalDue 应触发 1 次, got %d", dueCalled.Load())
	}
	if execCalled.Load() != 1 {
		t.Errorf("OnGoalExecute 应触发 1 次, got %d", execCalled.Load())
	}
	if r, ok := skippedReason.Load().(string); !ok || r != "max_runs_reached" {
		t.Errorf("OnGoalSkipped 原因应为 max_runs_reached, got %v", skippedReason.Load())
	}
	// oneshot 执行后经 reschedule 标记 done，状态变更回调 old=active new=done
	if old, ok := statusOld.Load().(GoalStatus); !ok || old != GoalStatusActive {
		t.Errorf("状态变更 old 应为 active, got %v", statusOld.Load())
	}
	if new, ok := statusNew.Load().(GoalStatus); !ok || new != GoalStatusDone {
		t.Errorf("状态变更 new 应为 done, got %v", statusNew.Load())
	}
}

// TestAnchoredReschedule 锚定模式：以计划时刻为基准推进，错过多个周期时
// 追到最近一个未来槽位，不补跑。
func TestAnchoredReschedule(t *testing.T) {
	store, err := NewJSONGoalStore(t.TempDir() + "/goals.json")
	if err != nil {
		t.Fatal(err)
	}
	autoAgent := New(Config{Model: "fake-model", GoalStore: store})

	interval := time.Hour

	t.Run("锚定基准是计划时刻", func(t *testing.T) {
		g := NewGoal()
		g.ID = "anchored-1"
		g.Status = GoalStatusActive
		g.Schedule = Schedule{Type: ScheduleInterval, Interval: interval, Anchored: true}
		// 计划时刻 30 分钟前，实际执行刚刚完成
		g.NextRunAt = time.Now().Add(-30 * time.Minute)
		g.LastRunAt = time.Now().Add(-1 * time.Second)

		autoAgent.reschedule(g)

		next := g.GetNextRunAt()
		// 应为 计划时刻+1h = 未来 30 分钟，而非 LastRunAt+1h = 未来 59 分钟
		if !next.After(time.Now()) {
			t.Fatalf("NextRunAt 应在未来, got %v", next)
		}
		if d := time.Until(next); d > 40*time.Minute {
			t.Errorf("锚定推进应基于计划时刻（约 30 分钟后）, got %v", d)
		}
	})

	t.Run("错过多个周期追到最近未来槽位", func(t *testing.T) {
		g := NewGoal()
		g.ID = "anchored-2"
		g.Status = GoalStatusActive
		g.Schedule = Schedule{Type: ScheduleInterval, Interval: interval, Anchored: true}
		// 计划时刻 3.5 小时前：3.5h+1h=now-2.5h → now-1.5h → now-0.5h → now+0.5h
		g.NextRunAt = time.Now().Add(-3*time.Hour - 30*time.Minute)

		autoAgent.reschedule(g)

		next := g.GetNextRunAt()
		d := time.Until(next)
		if d <= 0 || d > interval {
			t.Errorf("应追到最近一个未来槽位（0 < d <= 1h）, got %v", d)
		}
	})

	t.Run("非锚定基准是实际执行时刻", func(t *testing.T) {
		g := NewGoal()
		g.ID = "unanchored-1"
		g.Status = GoalStatusActive
		g.Schedule = Schedule{Type: ScheduleInterval, Interval: interval}
		g.NextRunAt = time.Now().Add(-30 * time.Minute)
		g.LastRunAt = time.Now().Add(-1 * time.Second)

		autoAgent.reschedule(g)

		// LastRunAt+1h ≈ 未来 59 分钟
		d := time.Until(g.GetNextRunAt())
		if d < 50*time.Minute {
			t.Errorf("非锚定应基于 LastRunAt（约 59 分钟后）, got %v", d)
		}
	})

	t.Run("非法间隔标记失败", func(t *testing.T) {
		g := NewGoal()
		g.ID = "bad-interval"
		g.Status = GoalStatusActive
		g.Schedule = Schedule{Type: ScheduleInterval, Interval: 0, Anchored: true}

		autoAgent.reschedule(g)

		if g.GetStatus() != GoalStatusFailed {
			t.Errorf("Interval=0 应标记 failed, got %s", g.GetStatus())
		}
	})
}

// TestPauseResume 验证暂停/恢复：状态持久化、Resume 重算过期时间、非法转换报错。
func TestPauseResume(t *testing.T) {
	store, err := NewJSONGoalStore(t.TempDir() + "/goals.json")
	if err != nil {
		t.Fatal(err)
	}
	autoAgent := New(Config{Model: "fake-model", GoalStore: store})

	g := NewGoal()
	g.ID = "pause-1"
	g.Title = "可暂停目标"
	g.Status = GoalStatusActive
	g.Schedule = Schedule{Type: ScheduleInterval, Interval: time.Hour}
	g.NextRunAt = time.Now().Add(30 * time.Minute)
	if err := store.Save(g); err != nil {
		t.Fatal(err)
	}

	// 非暂停状态不能 Resume
	if err := autoAgent.ResumeGoal("pause-1"); err == nil {
		t.Error("active 状态 Resume 应报错")
	}

	// 暂停
	if err := autoAgent.PauseGoal("pause-1"); err != nil {
		t.Fatalf("PauseGoal 不应报错: %v", err)
	}
	paused, err := store.Get("pause-1")
	if err != nil {
		t.Fatal(err)
	}
	if paused.GetStatus() != GoalStatusPaused {
		t.Fatalf("暂停后 store 状态应为 paused, got %s", paused.GetStatus())
	}

	// 暂停状态下重复 Pause 报错
	if err := autoAgent.PauseGoal("pause-1"); err == nil {
		t.Error("paused 状态重复 Pause 应报错")
	}

	// 恢复：NextRunAt 仍在未来，保留原计划时刻
	if err := autoAgent.ResumeGoal("pause-1"); err != nil {
		t.Fatalf("ResumeGoal 不应报错: %v", err)
	}
	resumed, err := store.Get("pause-1")
	if err != nil {
		t.Fatal(err)
	}
	if resumed.GetStatus() != GoalStatusActive {
		t.Errorf("恢复后状态应为 active, got %s", resumed.GetStatus())
	}
	if !resumed.GetNextRunAt().Equal(g.NextRunAt) {
		t.Errorf("未来计划时刻应保留, want %v got %v", g.NextRunAt, resumed.GetNextRunAt())
	}

	// 再次暂停后 NextRunAt 过期：恢复时应重算到未来
	if err := autoAgent.PauseGoal("pause-1"); err != nil {
		t.Fatal(err)
	}
	expired := time.Now().Add(-1 * time.Second)
	resumed.SetNextRunAt(expired)
	if err := autoAgent.ResumeGoal("pause-1"); err != nil {
		t.Fatal(err)
	}
	next := resumed.GetNextRunAt()
	if !next.After(time.Now()) {
		t.Errorf("过期时刻恢复后应重算到未来, got %v", next)
	}

	// 不存在的目标
	if err := autoAgent.PauseGoal("no-such"); err == nil {
		t.Error("暂停不存在的目标应报错")
	}
}

// TestGoalMeta 验证 Meta 的并发访问、快照深拷贝与 JSON 持久化往返。
func TestGoalMeta(t *testing.T) {
	g := NewGoal()
	g.ID = "meta-1"
	g.SetMeta("agent", "friday")
	g.SetMeta("channel", "dm")

	if got := g.GetMeta("agent"); got != "friday" {
		t.Errorf("GetMeta(agent) = %q, want friday", got)
	}
	if got := g.GetMeta("missing"); got != "" {
		t.Errorf("不存在的 key 应返回空串, got %q", got)
	}

	// 快照深拷贝：改快照不影响原目标
	snap := g.Snapshot()
	snap.Meta["agent"] = "mutated"
	if got := g.GetMeta("agent"); got != "friday" {
		t.Errorf("快照修改不应影响原目标, got %q", got)
	}
	g.SetMeta("channel", "group")
	if snap.Meta["channel"] != "dm" {
		t.Errorf("原目标修改不应影响既有快照, got %q", snap.Meta["channel"])
	}

	// JSON 持久化往返
	store, err := NewJSONGoalStore(t.TempDir() + "/goals.json")
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Save(g); err != nil {
		t.Fatal(err)
	}
	loaded, err := store.Get("meta-1")
	if err != nil {
		t.Fatal(err)
	}
	if got := loaded.GetMeta("agent"); got != "friday" {
		t.Errorf("持久化后 Meta 应保留, got %q", got)
	}
}

// TestRecordRun 验证原子执行记录：成功清零失败计数，失败递增。
func TestRecordRun(t *testing.T) {
	g := NewGoal()
	g.ID = "run-1"

	g.RecordRun("结果 A", nil)
	snap := g.Snapshot()
	if snap.RunCount != 1 || snap.FailCount != 0 {
		t.Errorf("成功后 RunCount=1/FailCount=0, got %d/%d", snap.RunCount, snap.FailCount)
	}
	if snap.LastResult != "结果 A" || snap.LastRunAt.IsZero() {
		t.Error("成功后应记录结果与执行时刻")
	}

	g.RecordRun("", errors.New("boom"))
	snap = g.Snapshot()
	if snap.RunCount != 2 || snap.FailCount != 1 {
		t.Errorf("失败后 RunCount=2/FailCount=1, got %d/%d", snap.RunCount, snap.FailCount)
	}

	g.RecordRun("恢复", nil)
	if snap = g.Snapshot(); snap.FailCount != 0 {
		t.Errorf("成功后 FailCount 应清零, got %d", snap.FailCount)
	}

	// RecordReflect
	g.RecordReflect("一切正常")
	if got := g.Snapshot().LastReflect; got != "一切正常" {
		t.Errorf("RecordReflect 未生效, got %q", got)
	}
}
