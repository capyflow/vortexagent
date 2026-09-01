package autonomous

import (
	"context"
	"fmt"
	"log"
	"sync"
	"time"

	"github.com/capyflow/vortexagent/agent"
	"github.com/capyflow/vortexagent/agent/sessionstore"
)

// Event 是外部事件，用于触发事件驱动型目标。
type Event struct {
	Type      string         // "github.push" / "file_change" / "webhook" / "timer"
	Source    string         // 事件来源标识
	Data      map[string]any // 事件数据
	Timestamp time.Time
}

// ReflectionMode 反思模式。
type ReflectionMode int

const (
	ReflectionLight ReflectionMode = iota // 轻量模式：规则判断，零 token（默认）
	ReflectionDeep                        // 深度模式：LLM 推理，消耗 token
)

// ReflectConfig 反思配置。
type ReflectConfig struct {
	Mode ReflectionMode
}

// Config 是 AutonomousAgent 的配置。
type Config struct {
	Agent         *agent.Agent        // 基础 agent
	Model         string              // 模型名称（如 "deepseek-chat"，用于创建自治 session）
	Registry      *agent.Registry     // 工具注册表（用于注册自治工具）
	GoalStore     GoalStore           // 目标存储
	MaxSleep      time.Duration       // 最大睡眠间隔（默认 1h）
	MaxBatchSize  int                 // 单次批量执行目标上限（默认 3）
	EventChan     chan Event          // 外部事件通道（可选，nil 时内部创建）
	UserInputCh   chan string         // 用户输入通道（可选，nil 时内部创建）
	ReflectConfig ReflectConfig       // 反思配置（可选，默认轻量模式）
	OnGoalExecute func(*Goal, string) // 执行完成回调（可选）
}

// AutonomousAgent 是自治 agent 的主体。
//
// 它包装现有的 agent.Agent，添加目标管理与自主唤醒能力。
// 使用方通过 Run() 启动自治循环，循环会阻塞直到 ctx 取消。
type AutonomousAgent struct {
	mu          sync.RWMutex
	agent       *agent.Agent
	model       string
	registry    *agent.Registry
	goalStore   GoalStore
	scheduler   *Scheduler
	session     *sessionstore.Session // 自治专用 session
	maxSleep    time.Duration
	maxBatch    int
	timer       *time.Timer // 可重置的睡眠计时器
	eventCh     chan Event
	userInputCh chan string
	wakeReset   chan struct{}
	reflectCfg  ReflectConfig
	onExecute   func(*Goal, string)
}

// New 创建 AutonomousAgent。
func New(cfg Config) *AutonomousAgent {
	if cfg.MaxSleep <= 0 {
		cfg.MaxSleep = time.Hour
	}
	if cfg.MaxBatchSize <= 0 {
		cfg.MaxBatchSize = 3
	}
	if cfg.EventChan == nil {
		cfg.EventChan = make(chan Event)
	}
	if cfg.UserInputCh == nil {
		cfg.UserInputCh = make(chan string)
	}

	a := &AutonomousAgent{
		agent:       cfg.Agent,
		model:       cfg.Model,
		registry:    cfg.Registry,
		goalStore:   cfg.GoalStore,
		maxSleep:    cfg.MaxSleep,
		maxBatch:    cfg.MaxBatchSize,
		timer:       time.NewTimer(time.Hour),
		eventCh:     cfg.EventChan,
		userInputCh: cfg.UserInputCh,
		wakeReset:   make(chan struct{}, 1),
		reflectCfg:  cfg.ReflectConfig,
		onExecute:   cfg.OnGoalExecute,
	}
	a.timer.Stop() // 停止初始 timer，Run() 中再启动
	return a
}

// Run 启动自治循环，阻塞直到 ctx 取消。
//
// 循环逻辑：
//  1. 计算下次睡眠时间（精确到最近目标的 NextRunAt）
//  2. 睡眠等待（可被用户输入/事件/目标变更打断）
//  3. 醒来后执行所有到期目标
//  4. 重新计算并继续睡眠
func (a *AutonomousAgent) Run(ctx context.Context) error {
	// 1. 加载目标
	goals, err := a.goalStore.LoadAll()
	if err != nil {
		return fmt.Errorf("加载目标失败: %w", err)
	}

	// 2. 计算每个目标的下次执行时间
	for _, g := range goals {
		if g.GetNextRunAt().IsZero() {
			trigger := newTrigger(g.Schedule)
			next, err := trigger.NextRun(time.Now())
			if err != nil {
				return fmt.Errorf("计算目标 %q 首次执行时间失败: %w", g.Title, err)
			}
			g.SetNextRunAt(next)
		}
	}

	// 3. 初始化调度器
	a.mu.Lock()
	a.scheduler = NewScheduler(a.maxSleep)
	a.scheduler.ReplaceAll(goals)
	a.mu.Unlock()

	// 4. 创建专用 session
	a.session = sessionstore.NewSession(a.model)

	a.setupGoalTools()

	// 5. 进入主循环
	a.resetTimer(a.scheduler.SleepDuration())

	for {
		select {
		case <-ctx.Done():
			return nil

		// 时间到了
		case <-a.timer.C:
			a.handleDueGoals(ctx)
			a.resetTimer(a.scheduler.SleepDuration())

		// 用户输入触发重新调度
		case input := <-a.userInputCh:
			a.handleUserInput(ctx, input)
			a.resetTimer(a.scheduler.SleepDuration())

		// 外部事件触发
		case event := <-a.eventCh:
			a.handleEvent(ctx, event)
			a.resetTimer(a.scheduler.SleepDuration())

		// 内部重置（目标变更时）
		case <-a.wakeReset:
			a.resetTimer(a.scheduler.SleepDuration())
		}
	}
}

// resetTimer 安全重置睡眠计时器。
func (a *AutonomousAgent) resetTimer(d time.Duration) {
	if !a.timer.Stop() {
		select {
		case <-a.timer.C: // 排空已触发但未消费的值
		default:
		}
	}
	if d < 0 {
		d = 0
	}
	a.timer.Reset(d)
}

// AddGoal 安全添加目标（可被外部 goroutine 调用）。
func (a *AutonomousAgent) AddGoal(goal *Goal) error {
	a.mu.Lock()
	defer a.mu.Unlock()

	// 确保 goal 有 mutex
	if goal.mu == nil {
		goal.mu = &sync.RWMutex{}
	}

	// 计算首次执行时间
	if goal.GetNextRunAt().IsZero() && goal.Schedule.Type != ScheduleEvent {
		trigger := newTrigger(goal.Schedule)
		next, err := trigger.NextRun(time.Now())
		if err != nil {
			return fmt.Errorf("计算首次执行时间失败: %w", err)
		}
		goal.SetNextRunAt(next)
	}

	// 保存到存储（Run() 加载时会读取）
	if err := a.goalStore.Save(goal); err != nil {
		return fmt.Errorf("保存目标失败: %w", err)
	}

	// 如果 scheduler 已初始化，立即加入调度
	if a.scheduler != nil {
		a.scheduler.Add(goal)

		// 通知主循环重新计算睡眠
		select {
		case a.wakeReset <- struct{}{}:
		default:
		}
	}

	return nil
}

// RemoveGoal 安全移除目标。
func (a *AutonomousAgent) RemoveGoal(id string) error {
	a.mu.Lock()
	defer a.mu.Unlock()

	if a.scheduler == nil {
		return fmt.Errorf("scheduler 未初始化")
	}

	// 从 scheduler 中移除
	a.scheduler.Remove(id)

	// 从存储中移除
	if err := a.goalStore.Delete(id); err != nil {
		return err
	}

	// 通知主循环重新计算睡眠
	select {
	case a.wakeReset <- struct{}{}:
	default:
	}

	return nil
}

// Goals 返回所有目标的快照（值拷贝，调用方无需再加锁即可读取字段）。
func (a *AutonomousAgent) Goals() []Goal {
	a.mu.RLock()
	defer a.mu.RUnlock()

	if a.scheduler == nil {
		return nil
	}
	goals := a.scheduler.Goals()
	out := make([]Goal, len(goals))
	for i, g := range goals {
		out[i] = g.Snapshot()
	}
	return out
}

// setupGoalTools 注册自治工具到 registry。
func (a *AutonomousAgent) setupGoalTools() {
	if a.registry == nil {
		return
	}

	addTool := NewAddGoalTool(a.goalStore, a.registry, func(g *Goal) {
		a.AddGoal(g)
	})
	listTool := NewListGoalsTool(a.goalStore)
	removeTool := NewRemoveGoalTool(a.goalStore, a.registry, func(id string) {
		a.RemoveGoal(id)
	})

	// 忽略重复注册错误（Run() 多次调用时）
	_ = a.registry.Add(addTool)
	_ = a.registry.Add(listTool)
	_ = a.registry.Add(removeTool)
}

// EmitEvent 发送事件到自治循环（并发安全，尽力而为：超时会静默丢弃）。
func (a *AutonomousAgent) EmitEvent(event Event) {
	_ = a.EmitEventWait(event, 5*time.Second)
}

// EmitEventWait 发送事件并等待投递结果。
// 超时（如主循环正忙于长时间的 Ask 调用）时返回错误，调用方可据此感知事件被丢弃。
func (a *AutonomousAgent) EmitEventWait(event Event, timeout time.Duration) error {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	select {
	case a.eventCh <- event:
		return nil
	case <-ctx.Done():
		log.Printf("[vortex-autonomous] 发送事件超时: %s", event.Type)
		return fmt.Errorf("事件 %q 在 %s 内未被消费，已丢弃", event.Type, timeout)
	}
}

// ─── 内部方法 ───

// handleDueGoals 执行所有到期的目标。
func (a *AutonomousAgent) handleDueGoals(ctx context.Context) {
	due := a.scheduler.DueGoals()

	var actionable []*Goal
	for _, g := range due {
		if a.preCheck(g) {
			actionable = append(actionable, g)
		} else {
			// 未通过预检：重排（可能标记终态）并持久化
			a.reschedule(g)
			a.saveGoal(g)
		}
	}

	if len(actionable) == 0 {
		return
	}

	// 限制单次批量数量
	if len(actionable) > a.maxBatch {
		actionable = actionable[:a.maxBatch]
	}

	if len(actionable) == 1 {
		a.executeGoal(ctx, actionable[0])
	} else {
		a.executeBatch(ctx, actionable)
	}

	// 执行后必须重排并持久化：否则 NextRunAt 仍停留在过去，
	// 下次唤醒会立即重复执行同一目标，形成紧密执行循环
	for _, g := range actionable {
		a.reschedule(g)
		a.saveGoal(g)
	}
}

// saveGoal 持久化目标；落盘失败只记录日志，不中断调度。
func (a *AutonomousAgent) saveGoal(goal *Goal) {
	if err := a.goalStore.Save(goal); err != nil {
		log.Printf("[vortex-autonomous] 保存目标 %q(%s) 失败: %v", goal.Title, goal.ID, err)
	}
}

// preCheck 零 token 预检，过滤不需要执行的目标。
func (a *AutonomousAgent) preCheck(goal *Goal) bool {
	goal.mu.RLock()
	status := goal.Status
	runCount := goal.RunCount
	failCount := goal.FailCount
	maxRuns := goal.Schedule.MaxRuns
	scheduleType := goal.Schedule.Type
	goal.mu.RUnlock()

	switch status {
	case GoalStatusDone, GoalStatusCanceled, GoalStatusFailed:
		return false
	}

	// 检查最大执行次数
	if maxRuns > 0 && runCount >= maxRuns {
		goal.SetStatus(GoalStatusDone)
		return false
	}

	// 检查连续失败次数
	if failCount >= 3 {
		goal.SetStatus(GoalStatusFailed)
		return false
	}

	// 事件驱动型：没有事件就不执行
	if scheduleType == ScheduleEvent {
		return false
	}

	return true
}

// executeGoal 执行单个目标。
func (a *AutonomousAgent) executeGoal(ctx context.Context, goal *Goal) {
	ctxData := a.buildContext(goal)
	prompt := a.buildPrompt(goal, ctxData)

	answer, err := a.agent.Ask(ctx, a.session, prompt)

	// 更新目标状态
	goal.mu.Lock()
	goal.RunCount++
	goal.LastRunAt = time.Now()
	goal.LastResult = answer
	if err != nil {
		goal.FailCount++
		answer = fmt.Sprintf("执行失败: %v", err)
	} else {
		goal.FailCount = 0
	}
	goal.mu.Unlock()

	// 反思
	reflection := a.reflect(ctx, goal, answer)
	goal.mu.Lock()
	goal.LastReflect = reflection
	goal.mu.Unlock()

	// 持久化
	a.saveGoal(goal)

	// 回调通知
	if a.onExecute != nil {
		a.onExecute(goal, answer)
	}
}

// executeBatch 批量执行多个目标（合并为一次 Ask()）。
func (a *AutonomousAgent) executeBatch(ctx context.Context, goals []*Goal) {
	prompt := "## 以下目标已到期，请逐一执行\n\n"
	for i, g := range goals {
		prompt += fmt.Sprintf("### 目标 %d: %s\n%s\n\n", i+1, g.Title, g.Description)
	}

	answer, err := a.agent.Ask(ctx, a.session, prompt)

	// 批量更新：与 executeGoal 保持一致的状态推进
	now := time.Now()
	for _, g := range goals {
		g.mu.Lock()
		g.RunCount++
		g.LastRunAt = now
		g.LastResult = answer
		if err != nil {
			g.FailCount++
		} else {
			g.FailCount = 0
		}
		g.mu.Unlock()

		if a.onExecute != nil {
			a.onExecute(g, answer)
		}
	}
}

// reschedule 重新计算目标的下次执行时间。
func (a *AutonomousAgent) reschedule(goal *Goal) {
	trigger := newTrigger(goal.Schedule)

	goal.mu.Lock()
	defer goal.mu.Unlock()

	var baseTime time.Time
	switch goal.Schedule.Type {
	case ScheduleOneShot:
		goal.Status = GoalStatusDone
		goal.NextRunAt = time.Time{}
		return
	case ScheduleInterval:
		baseTime = goal.LastRunAt
		if baseTime.IsZero() {
			baseTime = time.Now()
		}
	case ScheduleCron:
		baseTime = time.Now()
	case ScheduleEvent:
		goal.NextRunAt = time.Time{}
		return
	}

	next, err := trigger.NextRun(baseTime)
	if err != nil {
		goal.Status = GoalStatusFailed
		return
	}
	goal.NextRunAt = next
}

// handleUserInput 处理用户输入（目标管理类指令）。
func (a *AutonomousAgent) handleUserInput(ctx context.Context, input string) {
	// 使用自治 session 处理目标管理
	answer, _ := a.agent.Ask(ctx, a.session, input)

	// 检查目标是否有变更
	a.reloadGoals()

	// 触发重新调度已在 Run() 主循环中处理
	_ = answer
}

// handleEvent 处理外部事件，匹配关联的目标并执行。
func (a *AutonomousAgent) handleEvent(ctx context.Context, event Event) {
	a.mu.RLock()
	goals := a.scheduler.goals
	a.mu.RUnlock()

	var matched []*Goal
	for _, g := range goals {
		g.mu.RLock()
		scheduleType := g.Schedule.Type
		eventType := g.Schedule.Event
		g.mu.RUnlock()

		if scheduleType != ScheduleEvent {
			continue
		}
		if eventType == event.Type || eventType == "" {
			matched = append(matched, g)
		}
	}

	for _, g := range matched {
		a.executeGoal(ctx, g)
		a.reschedule(g)
	}
}

// reloadGoals 从 GoalStore 重新加载并合并目标。
func (a *AutonomousAgent) reloadGoals() {
	a.mu.Lock()
	defer a.mu.Unlock()

	stored, err := a.goalStore.LoadAll()
	if err != nil {
		return
	}

	// 重建 goals map，保留运行时状态
	existing := make(map[string]*Goal)
	for _, g := range a.scheduler.goals {
		existing[g.ID] = g
	}

	var merged []*Goal
	for _, g := range stored {
		// 确保有 mutex
		if g.mu == nil {
			g.mu = &sync.RWMutex{}
		}

		if old, ok := existing[g.ID]; ok {
			// 保留运行时状态
			old.mu.RLock()
			runCount := old.RunCount
			failCount := old.FailCount
			lastRunAt := old.LastRunAt
			lastResult := old.LastResult
			lastReflect := old.LastReflect
			old.mu.RUnlock()

			g.mu.Lock()
			g.RunCount = runCount
			g.FailCount = failCount
			g.LastRunAt = lastRunAt
			g.LastResult = lastResult
			g.LastReflect = lastReflect
			g.mu.Unlock()
		}
		merged = append(merged, g)
	}

	a.scheduler.ReplaceAll(merged)
}

// ─── 上下文与 Prompt ───

// ContextData 是构建 prompt 所需的上下文信息。
type ContextData struct {
	CurrentTime time.Time
	Goal        *Goal
}

func (a *AutonomousAgent) buildContext(goal *Goal) ContextData {
	return ContextData{
		CurrentTime: time.Now(),
		Goal:        goal,
	}
}

func (a *AutonomousAgent) buildPrompt(goal *Goal, ctx ContextData) string {
	goal.mu.RLock()
	title := goal.Title
	description := goal.Description
	status := goal.Status
	runCount := goal.RunCount
	lastResult := goal.LastResult
	lastReflect := goal.LastReflect
	goal.mu.RUnlock()

	return fmt.Sprintf(`你是一个自治 agent。请根据以下信息判断并执行任务。

## 当前时间
%s

## 目标
- 标题: %s
- 描述: %s
- 状态: %s
- 已执行次数: %d

## 上次执行结果
%s

## 上次反思
%s

## 可用工具
你可以使用所有已注册的工具。

## 指令
1. 判断当前是否需要执行该目标描述的任务
2. 如果需要，说明具体步骤并执行（通过工具调用）
3. 如果不需要，说明原因
4. 执行完毕后，简要总结结果`,
		ctx.CurrentTime.Format("2006-01-02 15:04:05 MST"),
		title,
		description,
		status,
		runCount,
		lastResult,
		lastReflect,
	)
}

// ─── 反思 ───

func (a *AutonomousAgent) reflect(ctx context.Context, goal *Goal, result string) string {
	if a.reflectCfg.Mode == ReflectionDeep {
		return a.reflectDeep(ctx, goal, result)
	}
	return a.reflectLight(goal, result)
}

// reflectLight 轻量反思：零 token，规则判断。
func (a *AutonomousAgent) reflectLight(goal *Goal, result string) string {
	goal.mu.RLock()
	failCount := goal.FailCount
	runCount := goal.RunCount
	scheduleType := goal.Schedule.Type
	goal.mu.RUnlock()

	if failCount > 0 {
		return fmt.Sprintf("执行失败（连续第 %d 次），建议检查目标配置或依赖条件", failCount)
	}

	if scheduleType == ScheduleOneShot {
		return "一次性任务已完成"
	}

	if runCount > 10 && failCount == 0 {
		return "执行稳定，无需调整"
	}

	return "执行正常"
}

// reflectDeep 深度反思：消耗一次 Ask()，评估执行质量。
func (a *AutonomousAgent) reflectDeep(ctx context.Context, goal *Goal, result string) string {
	goal.mu.RLock()
	title := goal.Title
	runCount := goal.RunCount
	goal.mu.RUnlock()

	prompt := fmt.Sprintf(`
请评估以下任务的执行质量：

目标: %s
结果: %s
历史执行次数: %d

请回答：
1. 目标是否达成？
2. 结果质量如何？
3. 下次执行是否需要调整策略？
4. 有什么意外发现？
`, title, result, runCount)

	answer, err := a.agent.Ask(ctx, a.session, prompt)
	if err != nil {
		return fmt.Sprintf("深度反思失败: %v", err)
	}
	return answer
}
