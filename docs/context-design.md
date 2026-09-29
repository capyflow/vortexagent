# 上下文治理体系设计（ContextEngine / textscore）

> 本文是设计依据与决策记录；使用说明见 [context.md](context.md)。

## 一、背景与目标

单会话长对话会把上下文撑爆；跨会话知识分属 memory 与 knowledge 两套工具。本设计交付：

1. **ContextEngine**（单会话，包 `ctxengine`）：活跃上下文有界 = 摘要区 + 最近原文 + 按需检索片段；原始历史不可变归档，异步压缩，检索工具按需取回。核心机制：**活跃上下文是稳态系统（扰动后自动回到预算内），不是累积系统**——会话 200 轮还是 2000 轮，每轮看到的上下文形态一致。
2. **检索内核统一**（跨会话，包 `internal/textscore`）：memory 与 knowledge **保持完全独立**（独立的包、工具、目录、配置、生命周期、写入权威），仅底层共用同一套评分检索实现。本质隔离，实现复用。

已确认的产品决策：

- 检索**纯被动**（无自动预取，模型主动调工具才检索）；
- 归档对模型**不暴露删除**（读路径 only，生命周期靠 TTL）；
- 摘要采用**状态交接式**结构（参考 ZCode compaction：压缩保留"接着干活所需的状态"，不是对话流水账）；
- 工具输出**源头节流**（大输出进历史前截断——工具结果占每轮上下文增量的绝大部分，是膨胀最大来源）；
- 微压缩、子代理指引、/compact 命令延后（见"后续可选"）。

## 二、旧方案的问题（为什么删除重建）

旧 `agent/context_manager.go` + `OffloadStore` 是死代码（零调用方），且存在设计缺陷：

- 构造时绑定固定 `*Session` 指针，与 serve 模式（共享 Agent、按 sessionID 取会话）和 TaskHub（每个任务全新会话）不兼容；
- `offloadStore == nil` 时仍执行 `ReplaceHistory`，原始消息被移出活跃历史却无处落盘（数据丢失路径）；
- Postgres 会话存储不持久化 Offloaded 块（与 JSON 存储不对称）；
- 摘要失败静默降级为"N 条消息"计数摘要，信息无声丢失；
- `ImportanceBasedStrategy` 按 role 挑消息，可能拆散 tool_calls/tool_result 配对（重放被 provider 拒 400）。

处置：整体删除，由 `ctxengine` 包替换。

## 三、ctxengine 完整运行流程

### 阶段 0：初始化

- cmd 构造 Archive 后端（FS：`~/.vortex/<group>/archive/`）+ Engine（全局单例，无会话指针）→ 注入 `agent.Options.ContextEngine`；
- 引擎内部 `map[sessionID]*engineState{校准系数, 归档中, 摘要缓存}`，首次见到某会话时惰性建条目；
- 会话恢复（REPL /switch、serve Store.Load、TaskHub 新任务）：活跃历史来自会话 Store；Archive 摘要在第一次 Build 时惰性加载；
- TTL 清理：`retentionDays > 0` 时启动扫描 + 每日惰性。

### 阶段 1：组装（同步路径）

1. `session.Add(用户消息)`；
2. `engine.Build(session)`：
   - 摘要区：取 engineState 摘要缓存；超预算（10% 窗口）→ 本轮只带 global 摘要 + 最近 chunk，同时触发异步合并（**Build 内绝不做 LLM 调用**）；
   - 最近块：活跃历史按块取尾部，K 由剩余预算反推（基准 `keepRecentBlocks=12`）；
3. L2 硬校验：估算 + maxTokens > 95% 窗口 → 被裁块先同步落 Archive，再重新组装；
4. 发送 LLM 请求。

### 阶段 2：工具循环

- 工具结果**进历史前源头节流**（exec > 8k 字符截断 + 标注；filesystem 读内容同理）；
- `session.Add(tool result)`，块自然累积；
- 每次响应的 `usage.prompt_tokens` → 校准系数滑动平均更新（L3）；
- 纯文本回复 → OnFinish → 回答返回用户（同步路径到此结束）。

### 阶段 3：轮末异步归档（OnTurnDone）

触发：使用率 ≥ `softRatio`(0.7) 或活跃块数 > `maxBlocks`(24)。singleflight 抢占后，后台 goroutine 执行流水线（严格按序，失败即止）：

| 步骤 | 动作 | 失败时的行为 |
|---|---|---|
| 1 选块 | 块划分；待归档 = 除最近 keepRecentBlocks 外的全部 | 无可归档 → 结束 |
| 2 原文落库 | `Archive.AppendBlocks`（全部消息原文 + seq + 指纹去重） | **中止本轮**，原文仍在活跃区，零丢失 |
| 3 交接摘要 | 一次 LLM 调用双输出 `{handoff, candidate_facts}` | 解析失败→重试→降级自由文本→再失败→**放弃归档**，下轮重试 |
| 4 写摘要 | `Archive.SaveSummary`（chunk 摘要 + 元数据） | 同上 |
| 5 替换活跃区 | `Session.ResetTo(保留块)`，锁内原子替换 | 前序已过，必然成功 |
| 6 收尾 | 刷新摘要缓存；候选事实回调（M2 起入 memory） | — |

崩溃安全：步骤 2 完成后任何时刻崩溃，原文已在 Archive；重试按块指纹去重，不重复落库。

### 检索（纯被动）

- `search_history(query, limit)`：textscore 对全部 chunk/global 摘要 + 各块首条用户消息评分，返回带出处（chunk ID / 时间 / 消息数）的命中；
- `expand_chunk(id, max_tokens)`：从 Archive 取回原文，格式化 `[时间] 角色: 内容` 纯文本记录，当轮租借，不持久进历史；
- 模型侧无删除；生命周期 `retentionDays`（默认 0 = 永久）。

### 摘要区自清理

摘要区超预算 → 最旧 N 个 chunk 摘要合并为一个 global 摘要（`merged_ids` 溯源）；被合并 chunk 标记 merged，不再进摘要区但仍可检索。两级结构（chunk → global）保证摘要区永有界。

### L4 兜底

收到疑似超窗错误 → 被裁块先落库 → 裁最旧 1/3 块（机械摘要保索引功能）→ 重试一次。

### 四层防线汇总

| 层 | 时机 | 动作 |
|---|---|---|
| L1 预算组装 | 每次 Build | 摘要区封顶 10% 窗口，最近块吃剩余 |
| L2 发送前硬校验 | 每次请求前 | 估算+maxTokens > 95% → 强制裁剪再发 |
| L3 usage 校准 | 每次响应后 | 真实 prompt_tokens / 估算 → 滑动平均修正系数 |
| L4 错误兜底 | 超窗错误 | 裁最旧 1/3 块重试一次 |

## 四、关键设计决策

1. **引擎不绑定 Session 指针**：方法以会话为参；per-session 运行态按 sessionID 键控（镜像 server 的 sessionLocks 模式）。会话归属经由 context 传递（`ctxengine.WithSession`），Ask 入口统一注入，工具自动取到当前会话。
2. **摘要区归 Archive 所有**：`Session.Offloaded` 字段及 5 个相关方法删除；会话 Store 只管活跃历史。
3. **块模型**：块 = 相邻两条用户消息之间的全部消息。工具循环内不会出现用户消息，因此 assistant(tool_calls) 与其全部 tool 结果天然同块，结构上杜绝配对被拆。
4. **状态交接式摘要**：固定结构（任务意图/已确认事实/决策记录/错误与修复/未完成事项/当前状态），压缩保留工作状态而非对话过程。
5. **源头节流**：exec/filesystem 输出进历史前截断（默认 8000 字符，0/未配 = 默认值，-1 = 不限），标注原始长度；完整内容在 Archive 可 expand 回读。
6. **删除语义边界（三口径）**：memory 可删（memory_delete/update）；knowledge agent 无删除（写入权威在人/git）；ctxengine 归档模型侧仅读路径，TTL 管生命周期。

## 五、后续可选（不在本次范围）

按优先级：微压缩（旧 tool result 占位化，让阈值前增长也亚线性）→ /compact 手动归档命令 → 子代理隔离指引 → 向量检索（触发条件：关键词检索出现真实召回不足，或数据量超单机扫描能力；实现为 Searcher 另一实现，三处接口不变）。
