# Vortex Autonomous Agent 设计文档

## 1. 概述

### 1.1 目标

将 Vortex 从**被动应答式 agent** 升级为**主动自治式 agent**，使其具备：
- 自主唤醒并执行任务的能力
- 目标管理与分解能力
- 基于时间和事件的调度能力
- 自我反思与调整能力

### 1.2 设计原则

| 原则 | 说明 |
|------|------|
| **最小侵入** | 仅新增 `autonomous/` 包；`agent/` 完全不动；`server/` 和 `cmd/` 仅新增可选字段/路由，向后兼容 |
| **纯叠加** | 作为上层包装层，组合现有 `Agent`，不继承不修改 |
| **可选功能** | 不开启 `autonomous` 配置，行为与现在完全一致 |
| **Token 经济** | 精确睡眠 + 预检过滤，杜绝空转浪费 |

### 1.3 非目标

- 不实现多 agent 间的协作通信（现有 TaskHub 已覆盖）
- 不实现长期记忆/知识图谱（现有知识库已覆盖）
- 不改动 LLM 协议层、工具接口层、会话存储层

---

## 2. 架构

### 2.1 分层关系

```
┌─────────────────────────────────────────────────────────────┐
│                     应用层（可选）                           │
│  cmd/vortex/          cmd/vortex-serve/       自定义应用     │
│  (REPL CLI)           (HTTP Server)                         │
├─────────────────────────────────────────────────────────────┤
│                   ┌──────────────────────┐                  │
│  新增 ───────────→│  autonomous.Agent    │                  │
│                   │  (自治循环 + 调度器)  │                  │
│                   └──────────┬───────────┘                  │
│                              │ 组合调用                      │
├──────────────────────────────┼──────────────────────────────┤
│  现有框架（不动）            ▼                              │
│  agent.Agent.Ask()  ←  被 autonomous.Agent 调用            │
│  agent.TaskHub      ←  可选，用于复杂子任务                │
│  sessionstore       ←  会话持久化                          │
│  tools/*            ←  工具扩展                            │
├─────────────────────────────────────────────────────────────┤
│  LLM Provider（openai / anthropic / gemini）                │
└─────────────────────────────────────────────────────────────┘
```

### 2.2 包结构

```
autonomous/
├── agent.go           # AutonomousAgent 主体 + Run() 主循环
├── goal.go            # Goal 结构体 + GoalStore 接口
├── goal_store.go      # GoalStore 实现（JSON 文件持久化）
├── scheduler.go       # 调度器（精确睡眠 + 最大间隔兜底）
├── trigger.go         # 触发器接口（时间/事件/条件）
├── context.go         # 上下文收集（时间/历史/事件）
├── reflect.go         # 反思评估
├── tool.go            # 自治工具：add_goal / list_goals / remove_goal
└── errors.go          # 错误类型
```

> **依赖**：`CronTrigger` 需要 `github.com/robfig/cron/v3`，需在 `go.mod` 中添加。

### 2.3 集成点

| 现有组件 | 集成方式 | 是否修改 |
|---------|---------|---------|
| `agent.Agent` | 组合：`AutonomousAgent` 持有 `*agent.Agent` | ❌ |
| `agent.Tool` | 新增 3 个自治工具，实现 `Tool` 接口 | ❌ |
| `sessionstore.Session` | 自治 session 与用户 session 独立创建 | ❌ |
| `sessionstore.Store` | Goal 复用 JSON 存储实现 | ❌ |
| `cmd/vortex-serve` | 新增 `autonomous` 配置段 + 启动 goroutine | ✅ 向后兼容 |
| `server.Server` | 新增可选 `Autonomous` 字段 + webhook 路由 | ✅ 向后兼容 |

---

## 3. 核心组件

### 3.1 Goal（目标）

```go
type GoalStatus string

const (
    GoalStatusPending  GoalStatus = "pending"   // 等待执行
    GoalStatusActive   GoalStatus = "active"    // 活跃（定期执行）
    GoalStatusDone     GoalStatus = "done"      // 已完成（一次性任务）
    GoalStatusFailed   GoalStatus = "failed"    // 失败超限
    GoalStatusCanceled GoalStatus = "canceled"  // 被用户取消
)

type ScheduleType string

const (
    ScheduleOneShot  ScheduleType = "oneshot"   // 一次性：延迟执行
    ScheduleInterval ScheduleType = "interval"  // 周期性：间隔执行
    ScheduleCron     ScheduleType = "cron"      // 定时：cron 表达式
    ScheduleEvent    ScheduleType = "event"     // 事件驱动
)

type Schedule struct {
    Type     ScheduleType    // 调度类型
    Delay    time.Duration   // 一次性延迟（30 分钟后）
    Interval time.Duration   // 周期间隔（每 30 分钟）
    Cron     string          // cron 表达式（"0 8 * * *"）
    Event    string          // 事件类型（"github.push"）
    MaxRuns  int             // 最大执行次数（0=无限）
}

type Goal struct {
    mu          *sync.RWMutex `json:"-"`  // 保护可变字段，指针避免 JSON 序列化问题
    ID          string            // 唯一 ID
    Title       string            // 标题（"每天早上查天气"）
    Description string            // 详细描述
    Status      GoalStatus        // 当前状态
    Priority    int               // 优先级（越高越先执行）
    Schedule    Schedule          // 调度规则
    CreatedAt   time.Time
    UpdatedAt   time.Time
    NextRunAt   time.Time         // 下次执行时间（调度器计算）
    LastRunAt   time.Time         // 上次执行时间
    RunCount    int               // 已执行次数
    FailCount   int               // 连续失败次数
    LastResult  string            // 上次执行结果摘要
    LastReflect string            // 上次反思结论
}

// NewGoal 创建 Goal 并初始化 mutex
func NewGoal() *Goal {
    return &Goal{
        mu: &sync.RWMutex{},
    }
}
```

### 3.2 GoalStore（目标存储）

```go
type GoalStore interface {
    LoadAll() ([]*Goal, error)
    Save(goal *Goal) error
    Delete(id string) error
    Get(id string) (*Goal, error)
}

// 内置实现：JSON 文件持久化
type JSONGoalStore struct {
    mu    sync.RWMutex
    file  string
    goals map[string]*Goal
}

// 可选实现：复用 sessionstore.Postgres
```

### 3.3 Trigger（触发器）

```go
type Trigger interface {
    // NextRun 返回下次执行时间
    NextRun(lastRun time.Time) (time.Time, error)
    
    // Type 返回触发器类型
    Type() ScheduleType
}

// ─── 具体实现 ───

// OneShotTrigger：一次性延迟
type OneShotTrigger struct {
    Delay time.Duration
}

func (t *OneShotTrigger) NextRun(now time.Time) (time.Time, error) {
    return now.Add(t.Delay), nil
}

// IntervalTrigger：固定间隔
type IntervalTrigger struct {
    Interval time.Duration
}

func (t *IntervalTrigger) NextRun(lastRun time.Time) (time.Time, error) {
    return lastRun.Add(t.Interval), nil
}

// CronTrigger：cron 表达式
type CronTrigger struct {
    Expr string
}

func (t *CronTrigger) NextRun(lastRun time.Time) (time.Time, error) {
    sched, err := cron.ParseStandard(t.Expr)
    if err != nil {
        return time.Time{}, err
    }
    return sched.Next(lastRun), nil
}

// newTrigger 根据 Schedule 创建对应的触发器
func newTrigger(s Schedule) Trigger {
    switch s.Type {
    case ScheduleOneShot:
        return &OneShotTrigger{Delay: s.Delay}
    case ScheduleInterval:
        return &IntervalTrigger{Interval: s.Interval}
    case ScheduleCron:
        return &CronTrigger{Expr: s.Cron}
    case ScheduleEvent:
        return &EventTrigger{EventType: s.Event}
    default:
        return &OneShotTrigger{}
    }
}
```

### 3.4 Scheduler（调度器）

```go
type Scheduler struct {
    goals      []*Goal
    mu         sync.RWMutex
    maxSleep   time.Duration // 最大睡眠间隔（默认 1 小时）
}

// NewScheduler 创建调度器
func NewScheduler(maxSleep time.Duration) *Scheduler {
    if maxSleep <= 0 {
        maxSleep = time.Hour
    }
    return &Scheduler{
        goals:    make([]*Goal, 0),
        maxSleep: maxSleep,
    }
}

// NextWakeTime 计算下次应该醒来的时间
func (s *Scheduler) NextWakeTime() time.Time {
    s.mu.RLock()
    defer s.mu.RUnlock()
    
    var earliest time.Time
    for _, g := range s.goals {
        g.mu.RLock()
        status := g.Status
        nextRun := g.NextRunAt
        g.mu.RUnlock()
        
        if status != GoalStatusActive && status != GoalStatusPending {
            continue
        }
        if nextRun.IsZero() {
            continue
        }
        if earliest.IsZero() || nextRun.Before(earliest) {
            earliest = nextRun
        }
    }
    
    if earliest.IsZero() {
        // 没有活跃目标，使用最大间隔
        return time.Now().Add(s.maxSleep)
    }
    
    return earliest
}

// SleepDuration 计算当前应该睡眠多久（受最大间隔限制）
func (s *Scheduler) SleepDuration() time.Duration {
    nextWake := s.NextWakeTime()
    wait := time.Until(nextWake)
    if wait > s.maxSleep {
        return s.maxSleep
    }
    if wait < 0 {
        return 0 // 已经过期，立即执行
    }
    return wait
}

// DueGoals 返回所有到期的目标
func (s *Scheduler) DueGoals() []*Goal {
    s.mu.RLock()
    defer s.mu.RUnlock()
    
    var due []*Goal
    now := time.Now()
    for _, g := range s.goals {
        g.mu.RLock()
        status := g.Status
        nextRun := g.NextRunAt
        g.mu.RUnlock()
        
        if status != GoalStatusActive && status != GoalStatusPending {
            continue
        }
        if !nextRun.IsZero() && !nextRun.After(now) {
            due = append(due, g)
        }
    }
    // 按优先级排序
    sort.Slice(due, func(i, j int) bool {
        return due[i].Priority > due[j].Priority
    })
    return due
}
```

### 3.5 AutonomousAgent（自治主体）

```go
type Config struct {
    Agent        *agent.Agent       // 基础 agent
    Model        string             // 模型名称（如 "deepseek-chat"，用于创建自治 session）
    Registry     *agent.Registry    // 工具注册表（用于注册自治工具）
    GoalStore    GoalStore          // 目标存储
    MaxSleep     time.Duration      // 最大睡眠间隔（默认 1h）
    MaxBatchSize int                // 单次批量执行目标上限（默认 3）
    EventChan    chan Event         // 外部事件通道（可选，nil 时内部创建）
    UserInputCh  chan string        // 用户输入通道（可选，nil 时内部创建）
    ReflectConfig ReflectConfig     // 反思配置（可选，默认轻量模式）
    OnGoalExecute func(*Goal, string) // 执行完成回调（可选）
}

type AutonomousAgent struct {
    mu          sync.RWMutex
    agent       *agent.Agent
    model       string
    registry    *agent.Registry
    goalStore   GoalStore
    scheduler   *Scheduler
    session     *sessionstore.Session  // 自治专用 session
    maxSleep    time.Duration
    maxBatch    int
    timer       *time.Timer            // 可重置的睡眠计时器
    eventCh     chan Event
    userInputCh chan string
    wakeReset   chan struct{}
    reflectCfg  ReflectConfig
    onExecute   func(*Goal, string)
}

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
        timer:       time.NewTimer(time.Hour), // 初始值，Run() 中会重置
        eventCh:     cfg.EventChan,
        userInputCh: cfg.UserInputCh,
        wakeReset:   make(chan struct{}, 1),
        reflectCfg:  cfg.ReflectConfig,
        onExecute:   cfg.OnGoalExecute,
    }
    a.timer.Stop() // 停止初始 timer，Run() 中再启动
    return a
}
```

---

## 4. 主循环与唤醒机制

### 4.1 Run() 主循环

```go
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
            next, _ := trigger.NextRun(time.Now())
            g.SetNextRunAt(next)
        }
    }
    
    // 3. 初始化调度器
    a.mu.Lock()
    a.scheduler = NewScheduler(a.maxSleep)
    a.scheduler.goals = goals
    a.mu.Unlock()
    
    // 4. 创建专用 session
    a.session = sessionstore.NewSession(a.model)
    
    // 5. 注册自治工具到 agent 的 registry（让用户可以通过对话管理目标）
    a.setupGoalTools()
    
    // 6. 进入主循环
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

// resetTimer 安全重置睡眠计时器
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

// AddGoal 安全添加目标（可被外部 goroutine 调用）
func (a *AutonomousAgent) AddGoal(goal *Goal) error {
    a.mu.Lock()
    defer a.mu.Unlock()
    
    if a.scheduler == nil {
        return fmt.Errorf("scheduler 未初始化")
    }
    
    // 确保 goal 有 mutex
    if goal.mu == nil {
        goal.mu = &sync.RWMutex{}
    }
    
    // 计算首次执行时间
    goal.mu.RLock()
    nextRunAt := goal.NextRunAt
    goal.mu.RUnlock()
    
    if nextRunAt.IsZero() {
        trigger := newTrigger(goal.Schedule)
        next, err := trigger.NextRun(time.Now())
        if err != nil {
            return fmt.Errorf("计算首次执行时间失败: %w", err)
        }
        goal.mu.Lock()
        goal.NextRunAt = next
        goal.mu.Unlock()
    }
    
    a.scheduler.goals = append(a.scheduler.goals, goal)
    a.goalStore.Save(goal)
    
    // 通知主循环重新计算睡眠
    select {
    case a.wakeReset <- struct{}{}:
    default:
    }
    
    return nil
}

// reloadGoals 从 GoalStore 重新加载并合并目标
func (a *AutonomousAgent) reloadGoals() error {
    a.mu.Lock()
    defer a.mu.Unlock()
    
    stored, err := a.goalStore.LoadAll()
    if err != nil {
        return err
    }
    
    // 重建 goals map，保留运行时状态（RunCount/FailCount）
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
            // 保留运行时状态，更新调度字段
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
    
    a.scheduler.goals = merged
    return nil
}
```

### 4.2 执行到期目标

```go
func (a *AutonomousAgent) handleDueGoals(ctx context.Context) {
    due := a.scheduler.DueGoals()
    
    for _, goal := range due {
        // 预检：零 token 消耗的快速判断
        if !a.preCheck(goal) {
            a.reschedule(goal)
            continue
        }
        
        // 执行
        a.executeGoal(ctx, goal)
        
        // 重新调度
        a.reschedule(goal)
    }
}

// preCheck 零 token 预检，过滤不需要执行的目标
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
        goal.mu.Lock()
        goal.Status = GoalStatusDone
        goal.mu.Unlock()
        return false
    }
    
    // 检查连续失败次数
    if failCount >= 3 {
        goal.mu.Lock()
        goal.Status = GoalStatusFailed
        goal.mu.Unlock()
        return false
    }
    
    // 事件驱动型：没有事件就不执行
    if scheduleType == ScheduleEvent {
        return false // 事件驱动只在 handleEvent 中执行
    }
    
    return true
}
```

### 4.3 执行单个目标

```go
func (a *AutonomousAgent) executeGoal(ctx context.Context, goal *Goal) {
    // 1. 收集上下文
    ctxData := a.buildContext(goal)
    
    // 2. 构建 prompt
    prompt := a.buildPrompt(goal, ctxData)
    
    // 3. 调用 Ask()（现有推理引擎）
    answer, err := a.agent.Ask(ctx, a.session, prompt)
    
    // 4. 更新目标状态（加锁）
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
    
    // 5. 反思
    reflection := a.reflect(ctx, goal, answer)
    
    goal.mu.Lock()
    goal.LastReflect = reflection
    goal.mu.Unlock()
    
    // 6. 持久化
    a.goalStore.Save(goal)
    
    // 7. 回调通知
    if a.onExecute != nil {
        a.onExecute(goal, answer)
    }
}

type ContextData struct {
    CurrentTime  time.Time
    Goal         *Goal
    RecentEvents []Event
    History      []RunRecord
}

func (a *AutonomousAgent) buildContext(goal *Goal) ContextData {
    return ContextData{
        CurrentTime:  time.Now(),
        Goal:         goal,
        RecentEvents: a.recentEvents(goal.ID, 5),
        History:      goal.RecentRuns(3),
    }
}

func (a *AutonomousAgent) buildPrompt(goal *Goal, ctx ContextData) string {
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

## 最近事件
%s

## 可用工具
你可以使用所有已注册的工具。

## 指令
1. 判断当前是否需要执行该目标描述的任务
2. 如果需要，说明具体步骤并执行（通过工具调用）
3. 如果不需要，说明原因
4. 执行完毕后，简要总结结果`,
        ctx.CurrentTime.Format("2006-01-02 15:04:05 MST"),
        goal.Title,
        goal.Description,
        goal.Status,
        goal.RunCount,
        goal.LastResult,
        goal.LastReflect,
        formatEvents(ctx.RecentEvents),
    )
}
```

### 4.4 反思机制（双模式）

```go
// ReflectionMode 反思模式
type ReflectionMode int

const (
    ReflectionLight ReflectionMode = iota  // 轻量模式：规则判断，零 token（默认）
    ReflectionDeep                         // 深度模式：LLM 推理，消耗 token
)

type ReflectConfig struct {
    Mode ReflectionMode
}

func (a *AutonomousAgent) reflect(ctx context.Context, goal *Goal, result string) string {
    if a.reflectConfig.Mode == ReflectionDeep {
        return a.reflectDeep(ctx, goal, result)
    }
    return a.reflectLight(goal, result)
}

// reflectLight 轻量反思：零 token，规则判断
func (a *AutonomousAgent) reflectLight(goal *Goal, result string) string {
    if goal.FailCount > 0 {
        return fmt.Sprintf("执行失败（连续第 %d 次），建议检查目标配置或依赖条件", goal.FailCount)
    }
    
    if goal.Schedule.Type == ScheduleOneShot {
        return "一次性任务已完成"
    }
    
    if goal.RunCount > 10 && goal.FailCount == 0 {
        return "执行稳定，无需调整"
    }
    
    return "执行正常"
}

// reflectDeep 深度反思：消耗一次 Ask()，评估执行质量
func (a *AutonomousAgent) reflectDeep(ctx context.Context, goal *Goal, result string) string {
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
`, goal.Title, result, goal.RunCount)
    
    answer, err := a.agent.Ask(ctx, a.session, prompt)
    if err != nil {
        return fmt.Sprintf("深度反思失败: %v", err)
    }
    return answer
}

// 类型转换辅助函数
func asString(v any) string {
    if s, ok := v.(string); ok {
        return s
    }
    return ""
}

func asFloat(v any) float64 {
    if f, ok := v.(float64); ok {
        return f
    }
    return 0
}
```

### 4.5 重新调度

```go
func (a *AutonomousAgent) reschedule(goal *Goal) {
    trigger := newTrigger(goal.Schedule)
    
    goal.mu.Lock()
    defer goal.mu.Unlock()
    
    var baseTime time.Time
    switch goal.Schedule.Type {
    case ScheduleOneShot:
        // 一次性任务：执行后标记完成
        goal.Status = GoalStatusDone
        goal.NextRunAt = time.Time{}
        return
    case ScheduleInterval:
        // 间隔型：从上次执行时间计算
        baseTime = goal.LastRunAt
        if baseTime.IsZero() {
            baseTime = time.Now()
        }
    case ScheduleCron:
        // Cron 型：从当前时间计算
        baseTime = time.Now()
    case ScheduleEvent:
        // 事件型：不主动调度
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
```

---

## 5. 用户输入处理

### 5.1 用户输入路由方案

用户输入由现有 handler 处理，不经过自治循环。自治 agent 只处理两类输入：
1. **目标管理指令**：用户说"30 分钟后提醒我开会" → 现有 Ask() 调用 `add_goal` 工具 → 工具通过 `AddGoal()` 方法通知自治循环
2. **直接目标操作**：通过 HTTP API `/goals` 或 `/webhook` 触发

```go
func (a *AutonomousAgent) handleUserInput(ctx context.Context, input string) {
    // 自治循环不直接处理用户对话。
    // 用户对话由 server/handler.go 或 cmd/vortex/main.go 的现有 Ask() 处理。
    // 这里只处理通过 userInputCh 传入的"目标管理"类输入。
    
    // 1. 使用自治 session 处理目标管理（非用户对话）
    answer, _ := a.agent.Ask(ctx, a.session, input)
    
    // 2. 检查目标是否有变更（工具调用可能已修改 GoalStore）
    a.reloadGoals()
    
    // 3. 触发重新调度
    select {
    case a.wakeReset <- struct{}{}:
    default:
    }
    
    // 4. 回答通过回调返回
    _ = answer
}

// handleEvent 处理外部事件，匹配关联的目标并执行
func (a *AutonomousAgent) handleEvent(ctx context.Context, event Event) {
    a.mu.RLock()
    goals := a.scheduler.goals
    a.mu.RUnlock()
    
    var matched []*Goal
    for _, g := range goals {
        if g.Schedule.Type != ScheduleEvent {
            continue
        }
        if g.Schedule.Event == event.Type || g.Schedule.Event == "" {
            matched = append(matched, g)
        }
    }
    
    for _, g := range matched {
        if !a.preCheck(g) {
            continue
        }
        a.executeGoal(ctx, g)
        a.reschedule(g)
    }
}

### 5.2 自治工具（用户通过对话管理目标）

```go
// add_goal 工具
type AddGoalTool struct {
    store    GoalStore
    onUpdate func() // 通知调度器重置
}

func (t *AddGoalTool) Name() string { return "add_goal" }
func (t *AddGoalTool) Description() string {
    return "添加一个长期目标，agent 会自动调度执行"
}
func (t *AddGoalTool) Schema() map[string]any {
    return map[string]any{
        "type": "object",
        "properties": map[string]any{
            "title": map[string]any{
                "type": "string", "description": "目标标题",
            },
            "description": map[string]any{
                "type": "string", "description": "目标详细描述",
            },
            "schedule_type": map[string]any{
                "type": "string",
                "enum": []string{"oneshot", "interval", "cron", "event"},
                "description": "调度类型",
            },
            "delay_minutes": map[string]any{
                "type": "number", "description": "延迟分钟数（oneshot 类型）",
            },
            "interval_minutes": map[string]any{
                "type": "number", "description": "间隔分钟数（interval 类型）",
            },
            "cron": map[string]any{
                "type": "string", "description": "cron 表达式（cron 类型）",
            },
            "priority": map[string]any{
                "type": "number", "description": "优先级（1-10，默认 5）",
            },
        },
        "required": []string{"title", "schedule_type"},
    }
}

func (t *AddGoalTool) Call(ctx context.Context, args map[string]any) (string, error) {
    goal := &Goal{
        ID:        generateID(),
        Title:     asString(args["title"]),
        Status:    GoalStatusActive,
        CreatedAt: time.Now(),
    }
    
    // 解析 schedule_type
    switch asString(args["schedule_type"]) {
    case "oneshot":
        goal.Schedule.Type = ScheduleOneShot
        minutes := asFloat(args["delay_minutes"])
        goal.Schedule.Delay = time.Duration(minutes) * time.Minute
        goal.NextRunAt = time.Now().Add(goal.Schedule.Delay)
    case "interval":
        goal.Schedule.Type = ScheduleInterval
        minutes := asFloat(args["interval_minutes"])
        goal.Schedule.Interval = time.Duration(minutes) * time.Minute
        goal.NextRunAt = time.Now().Add(goal.Schedule.Interval)
    case "cron":
        goal.Schedule.Type = ScheduleCron
        goal.Schedule.Cron = asString(args["cron"])
        // 计算下次执行时间
        trigger := &CronTrigger{Expr: goal.Schedule.Cron}
        next, _ := trigger.NextRun(time.Now())
        goal.NextRunAt = next
    }
    
    if err := t.store.Save(goal); err != nil {
        return "", err
    }
    
    // 通知调度器重新计算
    if t.onUpdate != nil {
        t.onUpdate()
    }
    
    return fmt.Sprintf("目标已添加（ID: %s），下次执行: %s", 
        goal.ID, goal.NextRunAt.Format("2006-01-02 15:04")), nil
}

// list_goals 工具
type ListGoalTool struct {
    store GoalStore
}

func (t *ListGoalTool) Name() string { return "list_goals" }
func (t *ListGoalTool) Description() string { return "列出所有长期目标" }
func (t *ListGoalTool) Schema() map[string]any {
    return map[string]any{"type": "object", "properties": map[string]any{}}
}

func (t *ListGoalTool) Call(ctx context.Context, args map[string]any) (string, error) {
    goals, err := t.store.LoadAll()
    if err != nil {
        return "", err
    }
    // 格式化输出...
}

// remove_goal 工具
type RemoveGoalTool struct {
    store    GoalStore
    onUpdate func()
}

func (t *RemoveGoalTool) Name() string { return "remove_goal" }
func (t *RemoveGoalTool) Description() string { return "移除一个长期目标" }
func (t *RemoveGoalTool) Schema() map[string]any {
    return map[string]any{
        "type": "object",
        "properties": map[string]any{
            "goal_id": map[string]any{
                "type": "string", "description": "要移除的目标 ID",
            },
        },
        "required": []string{"goal_id"},
    }
}
```

---

## 6. Token 优化策略

### 6.1 精确睡眠

```
不采用：每 N 分钟轮询一次（空转浪费 token）
采用：  计算下次目标时间，精确睡眠到那个时刻
兜底：  最大睡眠间隔 1 小时，防止睡过头
```

### 6.2 两层检查

```
第一层：预检（零 token）
  - 目标状态是否 active？
  - 是否超过最大执行次数？
  - 连续失败是否超限？
  → 不通过则跳过，不调 Ask()

第二层：推理（消耗 token）
  - 只有预检通过才调 Ask()
  - 每次 Ask() 都有明确目标，不做"该做什么"的空泛推理
```

### 6.3 批量执行

多个目标同时到期时，合并为一次 Ask() 调用：

```go
func (a *AutonomousAgent) handleDueGoals(ctx context.Context) {
    due := a.scheduler.DueGoals()
    
    // 预检过滤
    var actionable []*Goal
    for _, g := range due {
        if a.preCheck(g) {
            actionable = append(actionable, g)
        } else {
            a.reschedule(g)
        }
    }
    
    if len(actionable) == 0 {
        return
    }
    
    // 限制单次批量数量，防止 Ask() 的 MaxIterations 超限
    if len(actionable) > a.maxBatch {
        // 优先级高的先执行，剩余留到下次循环
        actionable = actionable[:a.maxBatch]
    }
    
    if len(actionable) == 1 {
        a.executeGoal(ctx, actionable[0])
    } else {
        a.executeBatch(ctx, actionable)
    }
}

func (a *AutonomousAgent) executeBatch(ctx context.Context, goals []*Goal) {
    prompt := "## 以下目标已到期，请逐一执行\n\n"
    for i, g := range goals {
        prompt += fmt.Sprintf("### 目标 %d: %s\n%s\n\n", i+1, g.Title, g.Description)
    }
    
    answer, err := a.agent.Ask(ctx, a.session, prompt)
    
    // 批量执行结果写入每个 goal 的 LastResult
    // 注意：Ask() 的单次返回难以精确拆分每个 goal 的结果
    // 保守做法：每个 goal 都记录完整 answer，由反思阶段再判断
    for _, g := range goals {
        g.mu.Lock()
        g.LastResult = answer
        if err != nil {
            g.FailCount++
        } else {
            g.FailCount = 0
        }
        g.mu.Unlock()
    }
}
```

### 6.4 Token 消耗估算

| 场景 | 每天唤醒次数 | 每次 token | 日消耗 |
|------|-----------|-----------|--------|
| 3 个目标（天气+CPU+周报） | ~18 次 | ~500 | ~9K |
| 10 个目标 | ~50 次 | ~500 | ~25K |
| 固定 1 分钟轮询（对比） | 1440 次 | ~200 | ~288K |

---

## 7. 事件系统

### 7.1 事件定义

```go
type Event struct {
    Type      string         // "github.push" / "file_change" / "webhook" / "timer"
    Source    string         // 事件来源标识
    Data      map[string]any // 事件数据
    Timestamp time.Time
}

type EventHandler interface {
    EventType() string
    Handle(ctx context.Context, event Event, agent *AutonomousAgent) error
}
```

### 7.2 Webhook 集成

```go
// 在 server 层添加 webhook 端点
func (s *Server) handleWebhook(w http.ResponseWriter, r *http.Request) {
    eventType := r.PathValue("type")
    
    var payload map[string]any
    json.NewDecoder(r.Body).Decode(&payload)
    
    // 转发到自治 agent
    if s.autonomous != nil {
        s.autonomous.EmitEvent(Event{
            Type:      eventType,
            Source:    r.RemoteAddr,
            Data:      payload,
            Timestamp: time.Now(),
        })
    }
    
    w.WriteHeader(http.StatusOK)
}
```

### 7.3 文件监听

```go
type FileWatchTrigger struct {
    Pattern string              // glob 模式
    GoalID  string              // 关联目标
    watcher *fsnotify.Watcher
}

func (t *FileWatchTrigger) Watch(ctx context.Context, emit func(Event)) {
    for {
        select {
        case <-ctx.Done():
            return
        case event := <-t.watcher.Events:
            if matched, _ := filepath.Match(t.Pattern, event.Name); matched {
                emit(Event{
                    Type:      "file_change",
                    Source:    event.Name,
                    Data:      map[string]any{"op": event.Op.String()},
                    Timestamp: time.Now(),
                })
            }
        }
    }
}
```

---

## 8. 配置

### 8.1 vortex.json 新增配置段

```json
{
  "provider": { "name": "openai", "model": "deepseek-chat" },
  "knowledge": ["./docs"],
  "tools": { "exec": { "enabled": false } },
  
  "autonomous": {
    "enabled": true,
    "max_sleep_minutes": 60,
    "goal_store": {
      "type": "json",
      "file": "~/.vortex/goals.json"
    },
    "system_prompt": "你是一个自治 agent...",
    "goals": [
      {
        "title": "每日天气",
        "description": "每天早上 8 点查询北京天气，如果下雨提醒带伞",
        "schedule_type": "cron",
        "cron": "0 8 * * *",
        "priority": 8
      },
      {
        "title": "服务器监控",
        "description": "每 30 分钟检查服务器 CPU 和内存，超过 80% 报警",
        "schedule_type": "interval",
        "interval_minutes": 30,
        "priority": 5
      }
    ],
    "webhooks": [
      {
        "path": "/webhook/github",
        "event_type": "github.push",
        "goal_id": "review-pr"
      }
    ]
  }
}
```

### 8.2 配置结构体

```go
type AutonomousConfig struct {
    Enabled        bool              `json:"enabled"`
    MaxSleepMin    int               `json:"max_sleep_minutes"`
    GoalStore      GoalStoreConfig   `json:"goal_store"`
    SystemPrompt   string            `json:"system_prompt"`
    Goals          []GoalConfig      `json:"goals"`
    Webhooks       []WebhookConfig   `json:"webhooks"`
}

type GoalStoreConfig struct {
    Type string `json:"type"` // "json"
    File string `json:"file"`
}

type GoalConfig struct {
    Title       string  `json:"title"`
    Description string  `json:"description"`
    ScheduleType string `json:"schedule_type"`
    Cron        string  `json:"cron,omitempty"`
    IntervalMin float64 `json:"interval_minutes,omitempty"`
    DelayMin    float64 `json:"delay_minutes,omitempty"`
    Priority    int     `json:"priority"`
}

type WebhookConfig struct {
    Path      string `json:"path"`
    EventType string `json:"event_type"`
    GoalID    string `json:"goal_id"`
}
```

---

## 9. 与现有系统的集成

### 9.1 vortex-serve 集成

```go
// cmd/vortex-serve/main.go 改动（最小化）

func main() {
    // ... 现有代码不变 ...
    
    ag := agent.New(agent.Options{...})
    
    // 新增：可选的自治 agent
    var autoAgent *autonomous.AutonomousAgent
    if cfg.Autonomous != nil && cfg.Autonomous.Enabled {
        goalStore, err := autonomous.NewJSONGoalStore(expandPath(cfg.Autonomous.GoalStore.File))
        // ... 初始化 ...
        
        autoAgent = autonomous.New(autonomous.Config{
            Agent:     ag,
            GoalStore: goalStore,
            MaxSleep:  time.Duration(cfg.Autonomous.MaxSleepMin) * time.Minute,
        })
        
        // 加载配置中的目标
        for _, g := range cfg.Autonomous.Goals {
            autoAgent.AddGoal(goalFromConfig(g))
        }
        
        // 启动自治循环（后台 goroutine）
        go autoAgent.Run(ctx)
    }
    
    srv := server.New(server.Config{
        Addr:        *addr,
        Agent:       ag,
        Autonomous:  autoAgent,  // 可选注入
    })
    
    // ... 现有代码不变 ...
}
```

### 9.2 server 层 webhook 集成

```go
// server/server.go 新增
type Server struct {
    // ... 现有字段 ...
    autonomous *autonomous.AutonomousAgent
}

// 新增路由
mux.HandleFunc("POST /webhook/{type}", s.handleWebhook)

func (s *Server) handleWebhook(w http.ResponseWriter, r *http.Request) {
    if s.autonomous == nil {
        http.Error(w, "autonomous not enabled", http.StatusNotFound)
        return
    }
    // ... 转发事件 ...
}
```

---

## 10. 错误处理与边界情况

### 10.1 目标执行失败

```go
// 连续失败 3 次 → 标记为 failed，停止调度
if goal.FailCount >= 3 {
    goal.Status = GoalStatusFailed
    // 可选：通知用户
    a.notifyUser(fmt.Sprintf("目标 %q 连续失败 3 次，已暂停", goal.Title))
}
```

### 10.2 Ask() 超时

```go
// 使用带超时的 context
ctx, cancel := context.WithTimeout(ctx, 5*time.Minute)
defer cancel()
answer, err := a.agent.Ask(ctx, a.session, prompt)
```

### 10.3 目标冲突

```go
// 同一时间多个目标到期 → 按优先级排序
sort.Slice(due, func(i, j int) bool {
    return due[i].Priority > due[j].Priority
})
```

### 10.4 应用退出

```go
// ctx 取消 → 优雅退出
case <-ctx.Done():
    // 保存所有目标状态
    for _, g := range a.scheduler.Goals() {
        a.goalStore.Save(g)
    }
    return nil
```

---

## 11. 实现阶段

### Phase 0：基础调度（~1 天）

- [ ] `go get github.com/robfig/cron/v3` — 添加 cron 依赖
- [ ] `goal.go` — Goal 结构体 + GoalStore 接口 + 并发安全访问器
- [ ] `goal_store.go` — JSON 文件持久化
- [ ] `trigger.go` — 触发器接口 + 三种触发器实现
- [ ] `scheduler.go` — 调度器（精确睡眠 + 最大间隔）
- [ ] 单元测试

### Phase 1：自治循环（~1 天）

- [ ] `agent.go` — AutonomousAgent + Run() 主循环 + `time.Timer` 可中断睡眠 + `AddGoal` / `reloadGoals`
- [ ] `context.go` — 上下文收集
- [ ] `reflect.go` — 反思评估（轻量+深度双模式）
- [ ] 集成测试

### Phase 2：用户交互（~1 天）

- [ ] `tool.go` — add_goal / list_goals / remove_goal 工具
- [ ] 系统提示词注入
- [ ] 用户输入触发重新调度

### Phase 3：事件系统（~1 天）

- [ ] Webhook 端点集成
- [ ] 文件监听触发器
- [ ] 事件与目标匹配

### Phase 4：生产化（~1 天）

- [ ] 配置加载
- [ ] vortex-serve 集成
- [ ] 错误处理完善
- [ ] 文档 + 示例

---

## 12. 示例：完整使用流程

### 12.1 作为库使用

```go
package main

import (
    "context"
    "github.com/capyflow/vortexagent/agent"
    "github.com/capyflow/vortexagent/llm"
    "github.com/capyflow/vortexagent/autonomous"
)

func main() {
    provider, _ := llm.NewProvider(llm.ProviderConfig{
        Name: "openai", APIKey: "sk-...", Model: "deepseek-chat",
    })
    
    registry := agent.NewRegistry()
    registry.Add(&myTool{})
    
    ag := agent.New(agent.Options{
        Provider: provider,
        Registry: registry,
    })
    
    // 创建自治 agent
    goalStore, _ := autonomous.NewJSONGoalStore("./goals.json")
    autoAgent := autonomous.New(autonomous.Config{
        Agent:     ag,
        GoalStore: goalStore,
        MaxSleep:  time.Hour,
    })
    
    // 添加目标
    autoAgent.AddGoal(&autonomous.Goal{
        Title:       "每日天气",
        Description: "每天早上 8 点查询天气",
        Schedule: autonomous.Schedule{
            Type: ScheduleCron,
            Cron: "0 8 * * *",
        },
        Priority: 8,
    })
    
    // 启动（阻塞）
    ctx := context.Background()
    autoAgent.Run(ctx)
}
```

### 12.2 通过 HTTP API

```bash
# 添加目标
curl -X POST http://localhost:8080/goals \
  -d '{"title":"30分钟后提醒开会","schedule_type":"oneshot","delay_minutes":30}'

# 列出目标
curl http://localhost:8080/goals

# 删除目标
curl -X DELETE http://localhost:8080/goals/{id}

# 触发 webhook
curl -X POST http://localhost:8080/webhook/github \
  -d '{"event":"push","repo":"my-repo"}'
```

---

## 13. 风险与权衡

| 风险 | 缓解措施 |
|------|---------|
| Token 消耗超预期 | 精确睡眠 + 预检过滤 + 最大间隔兜底 |
| 目标膨胀（太多目标） | 最大活跃目标数限制（默认 50） |
| 执行失败累积 | 连续失败 3 次自动暂停 |
| 时间漂移 | 每次执行后重新计算下次时间，不依赖固定间隔 |
| 与用户对话冲突 | 独立 session，互不污染 |
| 睡眠无法被中断 | `time.Timer` + `Reset()`，支持动态调整唤醒时间 |
| Goal 并发访问竞态 | `sync.RWMutex` 保护可变字段，Scheduler 只通过访问器读写 |
| 批量执行超限 | `MaxBatchSize` 限制单次处理目标数（默认 3） |
| 新目标添加后睡眠不更新 | `AddGoal()` 通过 `wakeReset` 通道强制中断当前睡眠并重算 |

---

## 14. 未来扩展（不在本期范围）

- **自适应间隔**：根据历史执行频率动态调整唤醒间隔
- **目标依赖**：B 目标依赖 A 目标完成后再执行
- **多 agent 协作**：自治 agent 之间互相分配任务
- **执行统计面板**：可视化目标执行历史与 token 消耗
- **自然语言调度**："每当我收到邮件就..." 自动解析为事件触发
