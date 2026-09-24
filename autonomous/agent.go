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
	MaxBatchSize  int                 // 单次唤醒执行目标上限（默认 3）
	MaxFailCount  int                 // 连续失败次数上限，超过即标记 failed（默认 3）
	EventChan     chan Event          // 外部事件通道（可选，nil 时内部创建）
	UserInputCh   chan string         // 用户输入通道（可选，nil 时内部创建）
	ReflectConfig ReflectConfig       // 反思配置（可选，默认轻量模式）
	Executor      Executor            // 目标执行器（可选，nil 时使用默认 LLMExecutor）
	// 以下四个回调均在框架 goroutine 内同步调用：不得阻塞（会推迟主循环），
	// 不得重入调用 AutonomousAgent 的加锁方法（如 PauseGoal/ResumeGoal，会死锁）；
	// 需要读取目标列表时建议在回调内使用 goal.Snapshot() 或异步分发。
	OnGoalExecute      func(*Goal, string)                 // 执行完成回调（可选）
	OnGoalDue          func(*Goal)                         // 目标即将执行回调（预检通过后，可选）
	OnGoalSkipped      func(*Goal, string)                 // 目标被预检跳过回调，第二个参数为原因（可选）
	OnGoalStatusChange func(*Goal, GoalStatus, GoalStatus) // 状态变更回调（goal, old, new，可选）
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
	executor    Executor              // 目标执行器（默认 LLMExecutor）
	maxSleep    time.Duration
	maxBatch    int
	maxFail     int         // 连续失败次数上限
	timer       *time.Timer // 可重置的睡眠计时器
	eventCh     chan Event
	userInputCh chan string
	wakeReset   chan struct{}
	reflectCfg  ReflectConfig
	onExecute      func(*Goal, string)
	onDue          func(*Goal)
	onSkipped      func(*Goal, string)
	onStatusChange func(*Goal, GoalStatus, GoalStatus)
}

// New 创建 AutonomousAgent。
func New(cfg Config) *AutonomousAgent {
	if cfg.MaxSleep <= 0 {
		cfg.MaxSleep = time.Hour
	}
	if cfg.MaxBatchSize <= 0 {
		cfg.MaxBatchSize = 3
	}
	if cfg.MaxFailCount <= 0 {
		cfg.MaxFailCount = 3
	}
	if cfg.EventChan == nil {
		cfg.EventChan = make(chan Event)
	}
	if cfg.UserInputCh == nil {
		cfg.UserInputCh = make(chan string)
	}

	executor := cfg.Executor
	if executor == nil {
		executor = NewLLMExecutor(cfg.Agent, cfg.Model)
	}

	a := &AutonomousAgent{
		agent:          cfg.Agent,
		model:          cfg.Model,
		registry:       cfg.Registry,
		goalStore:      cfg.GoalStore,
		executor:       executor,
		maxSleep:       cfg.MaxSleep,
		maxBatch:       cfg.MaxBatchSize,
		maxFail:        cfg.MaxFailCount,
		timer:          time.NewTimer(time.Hour),
		eventCh:        cfg.EventChan,
		userInputCh:    cfg.UserInputCh,
		wakeReset:      make(chan struct{}, 1),
		reflectCfg:     cfg.ReflectConfig,
		onExecute:      cfg.OnGoalExecute,
		onDue:          cfg.OnGoalDue,
		onSkipped:      cfg.OnGoalSkipped,
		onStatusChange: cfg.OnGoalStatusChange,
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

	// 4. 创建专用 session，并注入给需要会话的执行器（默认 LLMExecutor）
	a.session = sessionstore.NewSession(a.model)
	if s, ok := a.executor.(sessionSetter); ok {
		s.setSession(a.session)
	}

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

// PauseGoal 暂停目标：不再调度执行，但不删除，可随时 ResumeGoal 恢复。
// 已是终态（done/failed/canceled）或已是 paused 的目标返回错误。
func (a *AutonomousAgent) PauseGoal(id string) error {
	a.mu.Lock()
	defer a.mu.Unlock()

	goal, err := a.goalStore.Get(id)
	if err != nil {
		return err
	}

	if s := goal.GetStatus(); s != GoalStatusActive && s != GoalStatusPending {
		return fmt.Errorf("目标 %q 状态为 %s，不能暂停", id, s)
	}

	a.setStatus(goal, GoalStatusPaused)
	// scheduler 中的对象与 store 通常是同一指针，此处兜底同步（reload 后可能不同）
	a.forEachScheduled(id, func(g *Goal) { g.SetStatus(GoalStatusPaused) })
	a.saveGoal(goal)

	// 暂停后可睡更久
	select {
	case a.wakeReset <- struct{}{}:
	default:
	}
	return nil
}

// ResumeGoal 恢复暂停的目标：状态置回 active，并重算已过期或缺失的下次执行时间。
func (a *AutonomousAgent) ResumeGoal(id string) error {
	a.mu.Lock()
	defer a.mu.Unlock()

	goal, err := a.goalStore.Get(id)
	if err != nil {
		return err
	}

	if goal.GetStatus() != GoalStatusPaused {
		return fmt.Errorf("目标 %q 不是暂停状态，无法恢复", id)
	}

	a.setStatus(goal, GoalStatusActive)

	// 下次执行时间缺失或已过期则重算；仍在未来的保留原计划时刻
	if next := goal.GetNextRunAt(); next.IsZero() || !next.After(time.Now()) {
		trigger := newTrigger(goal.Schedule)
		next, err := trigger.NextRun(time.Now())
		if err != nil {
			return fmt.Errorf("重算执行时间失败: %w", err)
		}
		goal.SetNextRunAt(next)
	}

	a.forEachScheduled(id, func(g *Goal) { g.SetStatus(GoalStatusActive) })
	a.saveGoal(goal)

	// 恢复后可能需要提前唤醒
	select {
	case a.wakeReset <- struct{}{}:
	default:
	}
	return nil
}

// forEachScheduled 对 scheduler 中指定 ID 的目标执行 fn（a.mu 须已持有）。
func (a *AutonomousAgent) forEachScheduled(id string, fn func(*Goal)) {
	if a.scheduler == nil {
		return
	}
	for _, g := range a.scheduler.Goals() {
		if g.ID == id {
			fn(g)
		}
	}
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

// handleDueGoals 执行所有到期的目标（逐个顺序执行，每个目标独立产出结果）。
func (a *AutonomousAgent) handleDueGoals(ctx context.Context) {
	due := a.scheduler.DueGoals()

	var actionable []*Goal
	for _, g := range due {
		if ok, reason := a.preCheck(g); ok {
			actionable = append(actionable, g)
		} else {
			// 未通过预检：重排（可能标记终态）并持久化
			a.reschedule(g)
			a.saveGoal(g)
			if a.onSkipped != nil {
				a.onSkipped(g, reason)
			}
		}
	}

	if len(actionable) == 0 {
		return
	}

	// 限制单次唤醒的执行数量；未执行的目标 NextRunAt 仍停留在过去，
	// 下次唤醒会继续处理
	if len(actionable) > a.maxBatch {
		actionable = actionable[:a.maxBatch]
	}

	for _, g := range actionable {
		a.executeGoal(ctx, g)
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
// 返回是否通过，以及未通过时的原因（供 OnGoalSkipped 回调使用）。
func (a *AutonomousAgent) preCheck(goal *Goal) (bool, string) {
	goal.mu.RLock()
	status := goal.Status
	runCount := goal.RunCount
	failCount := goal.FailCount
	maxRuns := goal.Schedule.MaxRuns
	scheduleType := goal.Schedule.Type
	goal.mu.RUnlock()

	switch status {
	case GoalStatusDone:
		return false, "status_done"
	case GoalStatusCanceled:
		return false, "status_canceled"
	case GoalStatusFailed:
		return false, "status_failed"
	case GoalStatusPaused:
		return false, "status_paused"
	}

	// 检查最大执行次数
	if maxRuns > 0 && runCount >= maxRuns {
		a.setStatus(goal, GoalStatusDone)
		return false, "max_runs_reached"
	}

	// 检查连续失败次数
	if failCount >= a.maxFail {
		a.setStatus(goal, GoalStatusFailed)
		return false, "fail_limit_reached"
	}

	// 事件驱动型：没有事件就不执行
	if scheduleType == ScheduleEvent {
		return false, "event_driven"
	}

	return true, ""
}

// executeGoal 执行单个目标：执行器产出结果，框架负责状态推进、反思、持久化与回调。
func (a *AutonomousAgent) executeGoal(ctx context.Context, goal *Goal) {
	if a.onDue != nil {
		a.onDue(goal)
	}

	answer, err := a.executor.Execute(ctx, goal)

	// 更新目标状态
	if err != nil {
		goal.RecordRun(fmt.Sprintf("执行失败: %v", err), err)
	} else {
		goal.RecordRun(answer, nil)
	}

	// 反思
	goal.RecordReflect(a.reflect(ctx, goal, answer))

	// 持久化
	a.saveGoal(goal)

	// 回调通知
	if a.onExecute != nil {
		a.onExecute(goal, answer)
	}
}

// reschedule 重新计算目标的下次执行时间。
// 状态写入在 goal.mu 锁内完成，OnGoalStatusChange 回调在锁外触发，
// 保证回调可以安全读取 Goal 的任意字段。
func (a *AutonomousAgent) reschedule(goal *Goal) {
	trigger := newTrigger(goal.Schedule)

	goal.mu.Lock()
	oldStatus := goal.Status
	newStatus := GoalStatus("")

	switch goal.Schedule.Type {
	case ScheduleOneShot:
		newStatus = GoalStatusDone
		goal.NextRunAt = time.Time{}
	case ScheduleInterval:
		if goal.Schedule.Interval <= 0 {
			// 非法间隔：不设置 NextRunAt，标记失败避免紧循环
			newStatus = GoalStatusFailed
			break
		}
		if goal.Schedule.Anchored {
			// 锚定模式：以计划时刻（NextRunAt）为基准推进，执行耗时不会累积漂移；
			// 错过多个周期时追到最近一个未来槽位，不补跑
			baseTime := goal.NextRunAt
			if baseTime.IsZero() {
				baseTime = goal.LastRunAt
			}
			if baseTime.IsZero() {
				baseTime = time.Now()
			}
			next := baseTime.Add(goal.Schedule.Interval)
			now := time.Now()
			for !next.After(now) {
				next = next.Add(goal.Schedule.Interval)
			}
			goal.NextRunAt = next
			break
		}
		// 默认模式：以实际执行时刻为基准，等价于"上次完成后间隔 Interval 再执行"
		baseTime := goal.LastRunAt
		if baseTime.IsZero() {
			baseTime = time.Now()
		}
		next, err := trigger.NextRun(baseTime)
		if err != nil {
			newStatus = GoalStatusFailed
		} else {
			goal.NextRunAt = next
		}
	case ScheduleCron:
		next, err := trigger.NextRun(time.Now())
		if err != nil {
			newStatus = GoalStatusFailed
		} else {
			goal.NextRunAt = next
		}
	case ScheduleEvent:
		goal.NextRunAt = time.Time{}
	}

	if newStatus != "" && newStatus != oldStatus {
		goal.Status = newStatus
	}
	finalOld, finalNew := oldStatus, goal.Status
	goal.mu.Unlock()

	if finalOld != finalNew && a.onStatusChange != nil {
		a.onStatusChange(goal, finalOld, finalNew)
	}
}

// setStatus 统一的状态变更入口：写入 Goal 并触发 OnGoalStatusChange 回调。
// 调用方不得持有 goal.mu（回调期间回调方可能读取 Goal 字段）。
func (a *AutonomousAgent) setStatus(goal *Goal, s GoalStatus) {
	old := goal.GetStatus()
	if old == s {
		return
	}
	goal.SetStatus(s)
	if a.onStatusChange != nil {
		a.onStatusChange(goal, old, s)
	}
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
		a.saveGoal(g) // 与 handleDueGoals 保持一致：重排后必须落盘
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
