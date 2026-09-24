# 自治 Agent：运行机制与使用指南

`autonomous` 包让 Vortex agent 具备**长期自主运行**能力：你给它注册一批目标（Goal），
它自己计算何时醒来、执行任务、记录结果、失败退避，并支持通过 Webhook / 对话在运行期动态增删目标。

> 设计动机与取舍的完整讨论见 [autonomous-agent-design.md](autonomous-agent-design.md)；
> 本文描述的是**当前实现的真实行为**，作为使用参考。

---

## 1. 核心概念

### 1.1 Goal（目标）

Goal 是自治 agent 的基本工作单元，一个 Goal 描述"要做什么、何时做"：

| 字段 | 说明 |
|------|------|
| `ID` | 唯一标识（UUID 前缀 `goal-`，重启不会撞 ID） |
| `Title` / `Description` | 标题与详细描述，会进入执行 prompt |
| `Status` | 生命周期状态（见下） |
| `Priority` | 优先级，数字越大越先执行（默认 5） |
| `Schedule` | 调度规则（见 1.2） |
| `NextRunAt` | 下次执行时间（调度器计算并维护） |
| `RunCount` / `FailCount` | 累计执行次数 / 连续失败次数 |
| `LastRunAt` / `LastResult` / `LastReflect` | 上次执行时间、结果摘要、反思结论 |
| `Meta` | 业务自定义元数据（`map[string]string`，如 `agent`/`channel` 标记），随目标持久化，快照深拷贝 |

**状态机**：

```
pending ──┐
          ├──▶ active ──▶ done     （oneshot 执行完毕，或达到 MaxRuns）
          │   │    ├──▶ failed   （连续失败 ≥ MaxFailCount，默认 3）
          │   │    └──▶ paused ──▶ active（PauseGoal / ResumeGoal，暂停不删除）
          └─────────┴──▶ canceled（预留状态，当前 remove_goal 直接删除目标）
```

终态（`done` / `failed`）会持久化到存储，**重启后不会重新执行**；
`paused` 不是终态，恢复后按计划继续。

> 包外更新 Goal 的运行状态请使用 `RecordRun(result, err)` / `RecordReflect(s)` /
> `SetMeta(k, v)` 等原子方法——直接裸写 `RunCount` 等字段会产生 data race。

### 1.2 Schedule（调度规则）

| 类型 | 说明 | 关键参数 |
|------|------|----------|
| `oneshot` | 一次性：延迟后执行一次 | `Delay`（相对）或 `At`（绝对时刻，优先于 `Delay`；跨重启不漂移，已过期的 `At` 会立即补跑一次） |
| `interval` | 周期性：固定间隔重复执行 | `Interval`；`Anchored=true` 时按计划时刻推进（见下） |
| `cron` | 定时：cron 表达式（robfig 标准格式） | `Cron`，如 `"0 8 * * *"` |
| `event` | 事件驱动：只在收到匹配事件时执行 | `Event`（事件类型，如 `"github.push"`） |

所有类型共用可选参数 `MaxRuns`（最大执行次数，0 = 不限）。

**interval 的两种推进模式**：

- 默认（非锚定）：以**实际执行时刻**为基准（`LastRunAt + Interval`），执行耗时会长间隔，逐次累积漂移；
- 锚定（`Anchored: true`）：以**计划时刻**（`NextRunAt`）为基准推进，执行多慢都不漂移；
  错过多个周期时追到最近一个未来槽位，**不补跑**。

---

## 2. 运行机制

### 2.1 主循环（Run）

`AutonomousAgent.Run(ctx)` 是一个阻塞循环，直到 ctx 取消才返回：

```
启动
  │
  ├─ 1. 从 GoalStore 加载全部目标，补算缺失的 NextRunAt
  ├─ 2. 初始化 Scheduler，创建自治专用 session（与对话 session 隔离）
  ├─ 3. 向工具注册表注册 add_goal / list_goals / remove_goal
  │
  └─ 4. 循环：
       睡眠 min(最近目标的 NextRunAt, MaxSleep)    ← 精确睡眠，不空转轮询
       │
       ├─ timer 到点 ────▶ 执行到期目标 → 重排 → 重新计算睡眠
       ├─ 用户输入 ──────▶ 处理输入 → 重载目标 → 重新计算睡眠
       ├─ 外部事件 ──────▶ 匹配 event 型目标并执行 → 重新计算睡眠
       └─ 目标变更 ──────▶ 重新计算睡眠（Add/RemoveGoal 时触发）
```

`MaxSleep`（默认 1 小时）是睡眠上限：即使没有目标到期，agent 也会定期醒来检查。

### 2.2 到期目标的执行流程

每次唤醒后，`handleDueGoals` 按以下顺序处理：

1. **收集到期目标**：`Status` 为 active/pending 且 `NextRunAt` 已到，按 `Priority` 降序排列；
2. **零 token 预检（preCheck）**，过滤掉：
   - 已是终态（done / failed / canceled）或 paused 的目标；
   - `RunCount ≥ MaxRuns` 的目标（标记 done）；
   - 连续失败 ≥ `MaxFailCount` 次（默认 3）的目标（标记 failed，防止无限烧 token）；
   - event 型目标（它们不走时间调度）；
3. **限制批量**：单次唤醒最多执行 `MaxBatchSize` 个（默认 3），剩余目标下次唤醒继续；
4. **逐个执行**：每个目标独立经 `Executor` 执行（默认 LLMExecutor），
   结果归属各自目标——`LastResult` / `FailCount` / 反思 / 回调互不串扰；
5. **重排并落盘**：执行完毕后统一计算 `NextRunAt` 并持久化——
   oneshot 标记 done，interval 按锚定/非锚定模式推进，cron 取下一个触发时刻。

> 执行后必须重排是正确性的关键：否则 `NextRunAt` 留在过去，下一次唤醒会立即重复执行，
> 形成紧密循环（这正是 1.0 版本的实际 bug，已由回归测试覆盖）。

**单目标执行（executeGoal）**：

```
触发 OnGoalDue 回调（如配置）
  → Executor.Execute(ctx, goal)（产出结果；默认构建自治 prompt 并走 agent.Ask）
  → RecordRun：更新 RunCount / LastRunAt / LastResult / FailCount
  → 反思（见 2.3）
  → 持久化到 GoalStore
  → 触发 OnGoalExecute 回调（如配置）
```

**可插拔执行器（Executor）**：执行方式不绑定 LLM。`Config.Executor` 为 nil 时使用默认
`LLMExecutor`（经自治 session 走 `agent.Ask`）；业务方可注入自定义实现，例如零 token 的
"到点直投文本"：

```go
type DirectExecutor struct{}

func (e *DirectExecutor) Execute(ctx context.Context, g *autonomous.Goal) (string, error) {
    return g.Description, nil // 普通提醒直投文本，零 token
}

auto := autonomous.New(autonomous.Config{ ..., Executor: &DirectExecutor{} })
```

框架负责唤醒、预检、状态推进、反思、持久化与回调；Executor 只需把目标变成结果。
`LLMExecutor` 也可经 `NewLLMExecutor` 显式创建，作为自定义执行器的兜底复用。

**观测钩子**：`Config` 支持四个回调——`OnGoalDue`（即将执行）、`OnGoalExecute`（执行完成）、
`OnGoalSkipped`（预检跳过，附原因：`status_done` / `max_runs_reached` / `fail_limit_reached` /
`event_driven` 等）、`OnGoalStatusChange`（状态变更，含 old/new）。回调由框架 goroutine 同步
调用：不得阻塞（会推迟主循环），不得重入 `PauseGoal` / `ResumeGoal` 等加锁方法（会死锁）。

### 2.3 反思机制（Reflection）

| 模式 | 开销 | 行为 |
|------|------|------|
| `ReflectionLight`（默认） | 零 token | 规则判断：失败提示 / oneshot 完成 / 稳定运行 |
| `ReflectionDeep` | 额外一次 LLM 调用 | 评估目标是否达成、结果质量、下次策略调整建议 |

反思结论写入 `Goal.LastReflect`，并注入下次执行的 prompt，形成"执行 → 反思 → 改进"闭环。
深度模式目前仅能通过编程 API（`Config.ReflectConfig`）开启。

### 2.4 事件系统

**Webhook**（已接入 `vortex-serve`）：

```
POST /webhook/{事件类型}
```

- 请求体为 JSON，作为 `Event.Data` 传递给目标执行 prompt；
- **鉴权**：配置了密钥时，请求头 `X-Webhook-Secret` 必须匹配（constant-time 比较）；
  **未配置密钥时仅接受本机（loopback）请求**，启动时会打印警告；
- 请求体上限 1MB，超出返回 `413`；
- 密钥错误 / 非本机来源返回 `401`；
- 自治主循环正忙（如正在执行一次长任务）导致事件投递超时（2 秒）时，如实返回 `503`——
  调用方可以据此重试，而不是误以为事件已被消费。

**事件匹配规则**：只有 event 型目标会被事件触发；目标的 `Event` 类型与事件类型相同即匹配
（目标 `Event` 为空时匹配任意事件）。

**FileWatcher**（文件监听）：基于 mtime 轮询的实现已存在（`event.go`），**但尚未接入
vortex-serve 启动流程**，当前生产路径只有 Webhook 事件（代码中有 TODO 标注）。

### 2.5 持久化与容错

- 存储实现为 `JSONGoalStore`：每次保存全量落盘；写入采用**临时文件 + rename 原子替换**，
  进程崩溃不会损坏数据文件；
- 每次执行后（含预检标记的终态变更）都会落盘，重启后：
  - active 目标按 `NextRunAt` 继续调度（已过期则立即执行一次）；
  - done / failed 目标不会重新执行；
  - paused 目标保持暂停，恢复后继续；
- 失败退避：`Ask` 出错时 `FailCount++`（成功则清零），连续 `MaxFailCount` 次（默认 3）
  失败目标进入 failed 终态。

### 2.6 并发模型

- 每个 Goal 内嵌一把 `RWMutex` 保护可变字段；
- Scheduler 的目标列表由其内部锁保护，外部增删必须走 `Scheduler.Add/Remove/ReplaceAll`
  （AutonomousAgent 已统一走这些方法）；
- `Goals()` 返回**值快照**，调用方无需再加锁即可安全读取；
- 事件/用户输入通道为无缓冲通道，投递带超时：`EmitEvent` 尽力而为（超时静默丢弃），
  `EmitEventWait` 返回投递结果供调用方处理。

---

## 3. 使用方式

### 3.1 配置文件启用（vortex-serve）

在 `vortex-serve` 的配置文件（由 `-config` 指定，必填）中加入 `autonomous` 段：

```json
{
  "provider": {
    "name": "deepseek",
    "apiKeyEnv": "DEEPSEEK_API_KEY",
    "model": "deepseek-chat"
  },
  "session": { "type": "json", "file": "~/.vortex/sessions.json" },
  "autonomous": {
    "enabled": true,
    "max_sleep_minutes": 30,
    "goal_store": { "type": "json", "file": "~/.vortex/goals.json" },
    "webhook_secret": "change-me-to-a-random-secret",
    "goals": [
      {
        "title": "每日晨报",
        "description": "汇总知识库中昨天的新增内容，生成一份简报",
        "schedule_type": "cron",
        "cron": "0 8 * * *",
        "priority": 8
      },
      {
        "title": "服务巡检",
        "description": "检查目标服务健康状态，发现异常时给出处置建议",
        "schedule_type": "interval",
        "interval_minutes": 30
      },
      {
        "title": "一次性提醒",
        "description": "提醒我出席 15:00 的评审会议",
        "schedule_type": "oneshot",
        "delay_minutes": 45
      }
    ]
  }
}
```

字段说明：

| 字段 | 说明 | 默认值 |
|------|------|--------|
| `enabled` | 是否启用自治 agent | `false` |
| `max_sleep_minutes` | 最大睡眠间隔（分钟） | 60 |
| `goal_store.file` | 目标存储 JSON 文件路径（支持 `~`） | 必填 |
| `webhook_secret` | Webhook 鉴权密钥；也可用环境变量 `VORTEX_WEBHOOK_SECRET` | 空 = 仅允许本机调用 |
| `goals[].schedule_type` | `oneshot` / `interval` / `cron` | 必填 |
| `goals[].delay_minutes` | oneshot 延迟分钟数 | 60 |
| `goals[].interval_minutes` | interval 间隔分钟数 | 60 |
| `goals[].cron` | cron 表达式（5 段标准格式） | `"0 8 * * *"` |
| `goals[].priority` | 优先级，越大越先执行 | 5 |

> ⚠️ **注意**：配置文件目前**不支持 `event` 型目标**——事件驱动目标请通过对话
> `add_goal` 工具在运行期创建（见 3.4）。配置中无效的目标（缺字段 / cron 非法）
> 会在启动时打印警告并跳过，不会阻断其余目标。

启动：

```bash
vortex-serve -config ~/.vortex/agent.json -addr :8080
```

启动后自治循环随服务运行，SIGINT/SIGTERM 优雅退出。

### 3.2 Webhook API

向自治 agent 投递外部事件（GitHub webhook、CI 通知、任意脚本均可）：

```bash
curl -X POST http://localhost:8080/webhook/github.push \
  -H "Content-Type: application/json" \
  -H "X-Webhook-Secret: change-me-to-a-random-secret" \
  -d '{"repo": "capyflow/vortexagent", "ref": "main"}'
```

成功响应：

```json
{"status": "ok", "event": "github.push"}
```

| 状态码 | 含义 |
|--------|------|
| `200` | 事件已投递到自治循环 |
| `400` | 路径未指定事件类型（需 `POST /webhook/{type}`） |
| `401` | 密钥错误，或未配置密钥时来源非本机 |
| `405` | 非 POST 请求 |
| `413` | 请求体超过 1MB |
| `503` | 自治循环繁忙，事件投递超时被丢弃——**请稍后重试** |

### 3.3 对话管理目标（运行期）

自治 agent 启动时会向工具注册表注册三个工具，因此你可以像平常一样与 agent 对话来管理目标：

| 工具 | 作用 | 关键参数 |
|------|------|----------|
| `add_goal` | 添加目标（四种调度类型都支持） | `title`、`description`、`schedule_type`、`delay_minutes` / `interval_minutes` / `cron` / `event_type`、`priority` |
| `list_goals` | 列出全部目标及状态、下次执行时间 | 无 |
| `remove_goal` | 移除目标 | `goal_id` |

对话示例：

```
用户：以后每天早上 8 点帮我生成一份晨报，汇总知识库新增内容。
agent：（调用 add_goal：schedule_type=cron, cron="0 8 * * *"）
agent：好的，已创建目标 goal-a1b2c3d4，下次执行：明天 08:00。

用户：当收到 github.push 事件时，检查提交信息是否符合规范。
agent：（调用 add_goal：schedule_type=event, event_type=github.push）
agent：已创建事件驱动目标，收到 github.push 事件后自动执行。
```

### 3.4 编程接口（Go）

把自治能力嵌入自己的程序：

```go
goalStore, err := autonomous.NewJSONGoalStore("~/.vortex/goals.json")
if err != nil {
    log.Fatal(err)
}

auto := autonomous.New(autonomous.Config{
    Agent:     ag,               // 已构建的 *agent.Agent
    Model:     "deepseek-chat",  // 自治 session 使用的模型名
    Registry:  registry,         // 注册 add_goal 等自治工具
    GoalStore: goalStore,
    MaxSleep:  30 * time.Minute, // 睡眠上限，默认 1h
    MaxBatchSize: 3,             // 单次唤醒最多执行的目标数，默认 3
    // Executor: &myExecutor{},  // 自定义执行器，nil 时默认走 LLM
    // ReflectConfig: autonomous.ReflectConfig{Mode: autonomous.ReflectionDeep},
    // OnGoalDue:          func(g *autonomous.Goal) { ... },                          // 即将执行
    // OnGoalExecute:      func(g *autonomous.Goal, result string) { ... },           // 执行完成
    // OnGoalSkipped:      func(g *autonomous.Goal, reason string) { ... },           // 预检跳过
    // OnGoalStatusChange: func(g *autonomous.Goal, old, new autonomous.GoalStatus) { ... },
})

// 编程方式添加目标
g := autonomous.NewGoal()
g.Title = "每小时检查收件箱"
g.Description = "检查新邮件并汇总"
g.Status = autonomous.GoalStatusActive
g.Schedule = autonomous.Schedule{Type: autonomous.ScheduleInterval, Interval: time.Hour}
g.SetMeta("channel", "dm") // 业务自定义元数据，随目标持久化
if err := auto.AddGoal(g); err != nil {
    log.Fatal(err)
}

// 绝对时刻的一次性提醒（跨重启不漂移）
g2 := autonomous.NewGoal()
g2.Title = "15:00 的评审会议提醒"
g2.Status = autonomous.GoalStatusActive
g2.Schedule = autonomous.Schedule{Type: autonomous.ScheduleOneShot, At: meetingTime}

// 锚定 interval：按计划时刻推进，长执行不累积漂移
g3 := autonomous.NewGoal()
g3.Schedule = autonomous.Schedule{
    Type: autonomous.ScheduleInterval, Interval: time.Hour, Anchored: true,
}

// 暂停 / 恢复（删除重建不再必要）
_ = auto.PauseGoal(g.ID)
_ = auto.ResumeGoal(g.ID)

// 阻塞运行（ctx 取消后优雅退出）
go func() {
    if err := auto.Run(ctx); err != nil {
        log.Printf("自治 agent 退出: %v", err)
    }
}()

// 投递事件（带投递结果）
err = auto.EmitEventWait(autonomous.Event{
    Type: "github.push",
    Data: map[string]any{"repo": "my-repo"},
}, 2*time.Second)

// 读取目标快照（无需加锁）
for _, g := range auto.Goals() {
    fmt.Printf("%s: %s, 已执行 %d 次\n", g.Title, g.Status, g.RunCount)
}
```

### 3.5 目标存储文件格式

`JSONGoalStore` 的文件是 `ID → Goal` 的 JSON map（字段名与 Go 结构体一致）：

```json
{
  "goal-a1b2c3d4": {
    "ID": "goal-a1b2c3d4",
    "Title": "每日晨报",
    "Status": "active",
    "Priority": 8,
    "Schedule": { "Type": "cron", "Cron": "0 8 * * *", "MaxRuns": 0 },
    "RunCount": 3,
    "FailCount": 0,
    "NextRunAt": "2026-09-02T08:00:00+08:00",
    "Meta": { "channel": "dm" }
  }
}
```

> `Schedule.Delay` / `Schedule.Interval` 以纳秒整数存储（`time.Duration` 的 JSON 序列化）。
> 文件由程序自动维护，**不建议手工编辑**；管理目标请走对话工具或编程接口。

---

## 4. Token 消耗控制

自治 agent 天然适合长期运行，成本控制手段：

1. **精确睡眠**：只睡到最近目标时刻，不轮询、不空耗；
2. **零 token 预检**：终态 / 暂停 / 超限 / 连续失败的目标不产生任何 LLM 调用；
3. **可插拔执行器**：普通提醒类目标注入 `DirectExecutor` 直投文本，完全零 token；
4. **失败熔断**：连续失败 `MaxFailCount` 次（默认 3）自动进入 failed 终态，
   避免对故障目标反复重试烧 token；
5. **轻量反思默认开启**：规则判断零 token，深度反思按需开启。

## 5. 已知限制

- `FileWatcher`（文件监听）已实现但未接入启动流程，事件目前只有 Webhook 一种来源；
- 配置文件不能声明 event 型目标（用对话 `add_goal` 创建）；
- 深度反思、`MaxBatchSize`、自定义执行器、观测钩子等高级配置仅暴露编程 API，未进配置文件；
- 自治执行使用独立的内存 session，执行历史不持久化（目标执行结果在 `LastResult` 中保留一份摘要）；
- `Executor.Execute` 由主循环同步调用，长阻塞的自定义执行器会推迟后续目标与下一次唤醒。
