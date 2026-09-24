package autonomous

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/capyflow/vortexagent/agent"
	"github.com/capyflow/vortexagent/agent/sessionstore"
	"github.com/capyflow/vortexagent/llm"
)

// fakeProvider 是内存中的假 provider，用于集成测试。
type fakeProvider struct {
	name      string
	rounds    int
	maxRounds int // 前 maxRounds 轮返回工具调用，之后返回最终回答
	mu        sync.Mutex
	lastReq   *llm.ChatRequest
}

func (f *fakeProvider) Name() string       { return f.name }
func (f *fakeProvider) ContextWindow() int { return 128000 }

func (f *fakeProvider) Chat(_ context.Context, req *llm.ChatRequest, onDelta func(llm.Delta) error) (*llm.ChatResponse, error) {
	f.mu.Lock()
	f.lastReq = req
	f.rounds++
	round := f.rounds
	f.mu.Unlock()

	if onDelta != nil {
		_ = onDelta(llm.Delta{Text: "thinking..."})
		_ = onDelta(llm.Delta{Done: true})
	}

	if round <= f.maxRounds {
		return &llm.ChatResponse{
			Message: llm.Message{
				Role: llm.RoleAssistant,
				ToolCalls: []llm.ToolCall{{
					ID:        "call-1",
					Name:      "echo",
					Arguments: map[string]any{"text": "autonomous test"},
				}},
			},
			FinishReason: "tool_calls",
		}, nil
	}

	return &llm.ChatResponse{
		Message:      llm.NewTextMessage(llm.RoleAssistant, "任务已完成"),
		FinishReason: "stop",
	}, nil
}

// echoTool 是测试工具：把 text 参数原样返回。
type echoTool struct {
	calls atomic.Int32
}

func (t *echoTool) Name() string        { return "echo" }
func (t *echoTool) Description() string { return "回显测试工具" }
func (t *echoTool) Schema() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"text": map[string]any{"type": "string", "description": "要回显的文本"},
		},
	}
}

func (t *echoTool) Call(_ context.Context, args map[string]any) (string, error) {
	t.calls.Add(1)
	if text, ok := args["text"].(string); ok {
		return "echo: " + text, nil
	}
	return "echo: (empty)", nil
}

// TestAutonomousAgent_Run 是端到端测试：启动自治循环，验证目标执行。
func TestAutonomousAgent_Run(t *testing.T) {
	// 创建假 provider 和工具
	provider := &fakeProvider{name: "fake", maxRounds: 0}
	echo := &echoTool{}

	registry := agent.NewRegistry()
	if err := registry.Add(echo); err != nil {
		t.Fatal(err)
	}

	// 创建 agent
	ag := agent.New(agent.Options{
		Provider: provider,
		Registry: registry,
		Model:    "fake-model",
	})

	// 创建目标存储
	store, err := NewJSONGoalStore(t.TempDir() + "/goals.json")
	if err != nil {
		t.Fatal(err)
	}

	// 创建自治 agent（使用很短的睡眠间隔以便测试）
	autoAgent := New(Config{
		Agent:     ag,
		Model:     "fake-model",
		GoalStore: store,
		MaxSleep:  100 * time.Millisecond,
	})

	// 添加一个已到期的目标（立即执行）
	g := NewGoal()
	g.ID = "test-1"
	g.Title = "测试目标"
	g.Description = "执行 echo 工具"
	g.Status = GoalStatusActive
	g.NextRunAt = time.Now().Add(-1 * time.Second) // 已到期
	g.Schedule = Schedule{Type: ScheduleOneShot}
	autoAgent.AddGoal(g)

	// 启动自治循环
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	done := make(chan error, 1)
	go func() {
		done <- autoAgent.Run(ctx)
	}()

	// 等待目标执行
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		goals := autoAgent.Goals()
		if len(goals) > 0 && goals[0].RunCount > 0 {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}

	// 验证目标被执行
	goals := autoAgent.Goals()
	if len(goals) != 1 {
		t.Fatalf("应有 1 个目标, got %d", len(goals))
	}
	if goals[0].RunCount == 0 {
		t.Error("目标应被执行至少一次")
	}
	if goals[0].LastResult == "" {
		t.Error("目标应有执行结果")
	}

	// 取消上下文，验证优雅退出
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("Run() 不应返回错误, got %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Error("Run() 应在 ctx 取消后快速退出")
	}
}

// TestAutonomousAgent_EventDriven 测试事件驱动目标。
func TestAutonomousAgent_EventDriven(t *testing.T) {
	provider := &fakeProvider{name: "fake", maxRounds: 0}
	echo := &echoTool{}

	registry := agent.NewRegistry()
	registry.Add(echo)

	ag := agent.New(agent.Options{
		Provider: provider,
		Registry: registry,
		Model:    "fake-model",
	})

	store, err := NewJSONGoalStore(t.TempDir() + "/goals.json")
	if err != nil {
		t.Fatal(err)
	}

	autoAgent := New(Config{
		Agent:     ag,
		Model:     "fake-model",
		GoalStore: store,
		MaxSleep:  time.Hour,
	})

	// 添加事件驱动目标
	g := NewGoal()
	g.ID = "event-1"
	g.Title = "事件驱动目标"
	g.Description = "当 github.push 事件发生时执行"
	g.Status = GoalStatusActive
	g.Schedule = Schedule{Type: ScheduleEvent, Event: "github.push"}
	autoAgent.AddGoal(g)

	// 启动自治循环
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	done := make(chan error, 1)
	go func() {
		done <- autoAgent.Run(ctx)
	}()

	time.Sleep(100 * time.Millisecond)

	// 发送事件
	autoAgent.EmitEvent(Event{
		Type:      "github.push",
		Source:    "test",
		Data:      map[string]any{"repo": "test-repo"},
		Timestamp: time.Now(),
	})

	// 等待执行
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		goals := autoAgent.Goals()
		if len(goals) > 0 && goals[0].RunCount > 0 {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}

	goals := autoAgent.Goals()
	if len(goals) != 1 {
		t.Fatalf("应有 1 个目标, got %d", len(goals))
	}
	if goals[0].RunCount == 0 {
		t.Error("事件应触发目标执行")
	}

	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Error("Run() 应快速退出")
	}
}

// TestAutonomousAgent_AddGoalAtRuntime 测试运行时添加目标。
func TestAutonomousAgent_AddGoalAtRuntime(t *testing.T) {
	provider := &fakeProvider{name: "fake", maxRounds: 0}
	echo := &echoTool{}

	registry := agent.NewRegistry()
	registry.Add(echo)

	ag := agent.New(agent.Options{
		Provider: provider,
		Registry: registry,
		Model:    "fake-model",
	})

	store, err := NewJSONGoalStore(t.TempDir() + "/goals.json")
	if err != nil {
		t.Fatal(err)
	}

	autoAgent := New(Config{
		Agent:     ag,
		Model:     "fake-model",
		GoalStore: store,
		MaxSleep:  time.Hour,
	})

	// 启动自治循环
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	done := make(chan error, 1)
	go func() {
		done <- autoAgent.Run(ctx)
	}()

	time.Sleep(100 * time.Millisecond)

	// 运行时添加一个已到期的目标
	g := NewGoal()
	g.ID = "runtime-1"
	g.Title = "运行时添加的目标"
	g.Description = "测试运行时添加"
	g.Status = GoalStatusActive
	g.NextRunAt = time.Now().Add(-1 * time.Second)
	g.Schedule = Schedule{Type: ScheduleOneShot}
	if err := autoAgent.AddGoal(g); err != nil {
		t.Fatalf("AddGoal 不应报错: %v", err)
	}

	// 等待执行
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		goals := autoAgent.Goals()
		if len(goals) > 0 && goals[0].RunCount > 0 {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}

	goals := autoAgent.Goals()
	if len(goals) != 1 {
		t.Fatalf("应有 1 个目标, got %d", len(goals))
	}
	if goals[0].RunCount == 0 {
		t.Error("运行时添加的目标应被执行")
	}

	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Error("Run() 应快速退出")
	}
}

// TestAutonomousAgent_WebhookIntegration 测试 webhook 端到端流程。
func TestAutonomousAgent_WebhookIntegration(t *testing.T) {
	provider := &fakeProvider{name: "fake", maxRounds: 0}
	echo := &echoTool{}
	registry := agent.NewRegistry()
	registry.Add(echo)

	ag := agent.New(agent.Options{
		Provider: provider,
		Registry: registry,
		Model:    "fake-model",
	})

	store, err := NewJSONGoalStore(t.TempDir() + "/goals.json")
	if err != nil {
		t.Fatal(err)
	}

	autoAgent := New(Config{
		Agent:     ag,
		Model:     "fake-model",
		GoalStore: store,
		MaxSleep:  time.Hour,
	})

	// 添加事件驱动目标
	g := NewGoal()
	g.ID = "webhook-1"
	g.Title = "Webhook 目标"
	g.Description = "当 deploy 事件发生时执行"
	g.Status = GoalStatusActive
	g.Schedule = Schedule{Type: ScheduleEvent, Event: "deploy"}
	autoAgent.AddGoal(g)

	// 启动自治循环
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	done := make(chan error, 1)
	go func() {
		done <- autoAgent.Run(ctx)
	}()

	time.Sleep(100 * time.Millisecond)

	// 通过 webhook 发送事件
	handler := NewWebhookHandler(func(e Event) error {
		return autoAgent.EmitEventWait(e, 2*time.Second)
	}, "")

	req := httptest.NewRequest(http.MethodPost, "/webhook/deploy", strings.NewReader(`{"env":"prod"}`))
	req.RemoteAddr = "127.0.0.1:1234" // 未配置密钥时仅允许本机来源
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("webhook 状态码应为 200, got %d", w.Code)
	}

	// 等待执行
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		goals := autoAgent.Goals()
		if len(goals) > 0 && goals[0].RunCount > 0 {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}

	goals := autoAgent.Goals()
	if len(goals) != 1 {
		t.Fatalf("应有 1 个目标, got %d", len(goals))
	}
	if goals[0].RunCount == 0 {
		t.Error("webhook 事件应触发目标执行")
	}

	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Error("Run() 应快速退出")
	}
}

// TestAutonomousAgent_OneShotExecutesOnce 回归：oneshot 目标只执行一次并进入 done，
// 执行后必须重排（标记终态）且持久化，不得陷入重复执行循环。
func TestAutonomousAgent_OneShotExecutesOnce(t *testing.T) {
	provider := &fakeProvider{name: "fake", maxRounds: 0}
	registry := agent.NewRegistry()
	registry.Add(&echoTool{})

	ag := agent.New(agent.Options{
		Provider: provider,
		Registry: registry,
		Model:    "fake-model",
	})

	store, err := NewJSONGoalStore(t.TempDir() + "/goals.json")
	if err != nil {
		t.Fatal(err)
	}

	autoAgent := New(Config{
		Agent:     ag,
		Model:     "fake-model",
		GoalStore: store,
		MaxSleep:  50 * time.Millisecond,
	})

	g := NewGoal()
	g.ID = "once-1"
	g.Title = "只执行一次的目标"
	g.Status = GoalStatusActive
	g.NextRunAt = time.Now().Add(-1 * time.Second) // 已到期
	g.Schedule = Schedule{Type: ScheduleOneShot}
	if err := autoAgent.AddGoal(g); err != nil {
		t.Fatalf("AddGoal 不应报错: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	go func() { _ = autoAgent.Run(ctx) }()

	// 等待首次执行完成
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		goals := autoAgent.Goals()
		if len(goals) == 1 && goals[0].RunCount >= 1 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}

	// 给潜在的重复执行留出观察窗口
	time.Sleep(300 * time.Millisecond)

	goals := autoAgent.Goals()
	if len(goals) != 1 {
		t.Fatalf("应有 1 个目标, got %d", len(goals))
	}
	if goals[0].RunCount != 1 {
		t.Errorf("oneshot 目标应只执行 1 次, got %d", goals[0].RunCount)
	}
	if goals[0].Status != GoalStatusDone {
		t.Errorf("oneshot 目标执行后状态应为 done, got %s", goals[0].Status)
	}

	// 终态必须持久化：重启后按存储内容不应再执行
	persisted, err := store.Get("once-1")
	if err != nil {
		t.Fatalf("目标应已落盘: %v", err)
	}
	persisted.mu.RLock()
	status := persisted.Status
	runCount := persisted.RunCount
	persisted.mu.RUnlock()
	if status != GoalStatusDone || runCount != 1 {
		t.Errorf("落盘状态应为 done/1 次, got %s/%d", status, runCount)
	}
}

// TestAutonomousAgent_BatchUpdatesState 回归：批量执行必须与单目标一致地
// 推进 RunCount/LastRunAt、重排 NextRunAt 并落盘。
func TestAutonomousAgent_BatchUpdatesState(t *testing.T) {
	provider := &fakeProvider{name: "fake", maxRounds: 0}
	registry := agent.NewRegistry()
	registry.Add(&echoTool{})

	ag := agent.New(agent.Options{
		Provider: provider,
		Registry: registry,
		Model:    "fake-model",
	})

	store, err := NewJSONGoalStore(t.TempDir() + "/goals.json")
	if err != nil {
		t.Fatal(err)
	}

	autoAgent := New(Config{
		Agent:     ag,
		Model:     "fake-model",
		GoalStore: store,
		MaxSleep:  time.Hour,
	})

	// 白盒：不经 Run()，直接初始化 scheduler 并注入自治会话后触发多目标执行路径
	autoAgent.mu.Lock()
	autoAgent.scheduler = NewScheduler(time.Hour)
	autoAgent.session = sessionstore.NewSession("fake-model")
	if s, ok := autoAgent.executor.(sessionSetter); ok {
		s.setSession(autoAgent.session)
	}
	autoAgent.mu.Unlock()

	past := time.Now().Add(-1 * time.Second)
	for i, id := range []string{"batch-1", "batch-2"} {
		g := NewGoal()
		g.ID = id
		g.Title = "批量目标 " + id
		g.Status = GoalStatusActive
		g.NextRunAt = past
		g.Schedule = Schedule{Type: ScheduleInterval, Interval: time.Hour}
		if err := autoAgent.AddGoal(g); err != nil {
			t.Fatalf("AddGoal %d 不应报错: %v", i, err)
		}
	}

	autoAgent.handleDueGoals(context.Background())

	for _, id := range []string{"batch-1", "batch-2"} {
		g, err := store.Get(id)
		if err != nil {
			t.Fatalf("目标 %s 应已落盘: %v", id, err)
		}
		g.mu.RLock()
		runCount, lastRunAt, nextRunAt, status := g.RunCount, g.LastRunAt, g.NextRunAt, g.Status
		g.mu.RUnlock()

		if runCount != 1 {
			t.Errorf("%s: RunCount 应为 1, got %d", id, runCount)
		}
		if lastRunAt.IsZero() {
			t.Errorf("%s: LastRunAt 不应为零值", id)
		}
		if status != GoalStatusActive {
			t.Errorf("%s: interval 目标执行后应为 active, got %s", id, status)
		}
		if !nextRunAt.After(time.Now()) {
			t.Errorf("%s: 执行后 NextRunAt 应被推进到未来, got %v", id, nextRunAt)
		}
	}
}

// 确保接口满足
var _ llm.Provider = (*fakeProvider)(nil)
var _ agent.Tool = (*echoTool)(nil)
var _ sessionstore.Store = (*nilStore)(nil)

type nilStore struct{}

func (s *nilStore) Save(ctx context.Context, sess *sessionstore.Session) error { return nil }
func (s *nilStore) Load(ctx context.Context, id string) (*sessionstore.Session, error) {
	return nil, nil
}
func (s *nilStore) Delete(ctx context.Context, id string) error               { return nil }
func (s *nilStore) List(ctx context.Context) ([]*sessionstore.Session, error) { return nil, nil }
