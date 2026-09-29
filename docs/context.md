# 上下文组件：ctxengine / memory / knowledge

> 本文介绍 vortexagent 框架中的三种上下文组件：它们各管什么、边界在哪、
> 以及重点——**ctxengine（单会话上下文引擎）如何让长会话不被历史撑爆**。
> 设计依据与决策记录见 [context-design.md](context-design.md)。

## 一句话总览

| 组件 | 回答的问题 | 范围 | 谁写 | 生命周期 |
|------|-----------|------|------|---------|
| **ctxengine** | "这个会话里之前发生了什么？" | 单会话 | 自动归档（LLM 压缩） | 会话内存活，TTL 可配 |
| **memory** | "这个用户/项目定过什么？" | 跨会话 | agent 显式读写 | 持久，显式更新/删除 |
| **knowledge** | "文档里怎么说的？" | agent 级 | 人（git），agent 只读 | 跟随文档版本 |

三者的写入权威与可信度不同，这是边界的本质：

- **ctxengine 归档**：对话过程的压缩表示 + 完整原文，自动产生、自动检索，
  永远不会跨会话泄漏；
- **memory**：对话中蒸馏出的结论（偏好/决策/教训），以模型显式保存为主，
  归档时自动抽取的候选事实为辅（低置信、带来源，确认后转正）；
- **knowledge**：磁盘上的原始文档，答案可验证到 文件：行号，agent 不可写。

判断一条信息该去哪，问一个问题：**换一个会话还需要吗？**
需要且是结论 → memory；只有本会话内可能被追问 → ctxengine 归档；
有正式文档承载 → knowledge。

---

# ctxengine：单会话上下文引擎

## 问题

没有治理时，每轮 Ask 把完整历史发给模型：上下文随对话轮数**线性膨胀**，
长会话必然超窗；即便不超窗，巨大的历史也拖慢请求、干扰模型注意力。

## 机制总览

ctxengine 让活跃上下文变成**稳态系统**——扰动后自动回到预算内，
而不是累积系统。会话跑 200 轮还是 2000 轮，每轮发给模型的上下文形态一致：

```
活跃上下文（每轮组装，有界）＝ 摘要区 + 最近 N 块原文 + 当前输入
                                 │
                 原始历史不可变外部化归档（Archive）
                                 │
                 模型按需检索（search_history → expand_chunk）
```

### 块（Block）：一切操作的原子单位

块 = 相邻两条用户消息之间的全部消息。工具循环内不会出现用户消息，
因此 `assistant(tool_calls)` 与其全部 `tool` 结果**结构上永不拆散**——
归档、裁剪、检索都不会产生被 provider 拒收的半截配对。

### 单轮完整时序

```
用户输入
 │
 ▼
① Build 组装（同步，轻量）
 │   [system+交接摘要] + [最近 K 块] + [当前输入]
 │   摘要区超预算 → 本轮降级只带 global 摘要，异步触发合并
 │
 ▼
② L2 硬校验：估算+maxTokens > 95% 窗口 → 先裁剪（原文先落库）再发
 │
 ▼
③ LLM ⇄ 工具循环
 │   工具结果进历史前【源头节流】：exec/filesystem 超过 8000 字符
 │   截断并标注原始长度（可配，-1 不限）
 │   每次响应的真实 usage 回写 → 校准估算系数（L3）
 │
 ▼
④ 回答返回用户 —— 同步路径到此结束，以下全部异步
 │
 ▼
⑤ OnTurnDone 评估：使用率 ≥ 70% 或活跃块数 > 24 ？
 │   否 → 结束，等下一轮
 ▼
⑥ 归档流水线（后台 goroutine，每会话 singleflight）
     a. 选块：除最近 keepRecentBlocks(12) 块外的全部
     b. 原文先落库（含指纹，崩溃/重试天然去重）   ← 从此不可变
     c. 一次 LLM 调用，双输出：
        { handoff: 状态交接文档, candidate_facts: 候选事实 }
        失败 → 重试一次 → 降级自由文本摘要 → 再失败【放弃归档】
     d. 写摘要 → e. 活跃区原子替换为保留块 → f. 刷新缓存 + 候选事实回调
```

### 状态交接式摘要（不是流水账）

归档摘要固定结构——压缩保留的是"接着干活所需的状态"：

```
【任务意图】用户的任务/意图
【已确认事实】已确认的关键事实
【决策记录】已做的决策与约定
【错误与修复】遇到的错误与解决方式
【未完成事项】待办
【当前状态】进行到哪一步
```

这是压缩后 agent 仍能无缝继续任务的关键：它压缩的是**工作状态**，
对话过程本身交给归档原文按需取回。

### 四层防线

| 层 | 时机 | 动作 |
|---|---|---|
| L1 预算组装 | 每次 Build | 摘要区封顶 10% 窗口，最近块吃剩余空间 |
| L2 发送前硬校验 | 每次请求前 | 估算+maxTokens > 95% → 强制裁剪再发 |
| L3 usage 校准 | 每次响应后 | 真实 prompt_tokens / 估算 → 滑动平均修正系数 |
| L4 超窗兜底 | 疑似超窗错误 | 落库并裁最旧 1/3 块，重试一次 |

### 摘要区自清理

每个归档摘要约几百 token，长会话会积累。摘要区超预算时把最旧的
若干 chunk 摘要合并为一个 global 摘要（记录 `merged_ids` 溯源），
被合并的 chunk 不再进摘要区但仍可检索。两级结构保证摘要区永有界。

### 检索：纯被动，租借不搬回

模型发现需要早期细节时（系统提示词与工具描述双重指引）：

```
search_history("postgres 连接报错")
  → 按相关度返回：ID | 类型 | 时间 | 消息数 | 内容摘要
expand_chunk("chunk-xxx", max_tokens=2000)
  → "[01-02 14:32] 用户: … / [01-02 14:33] 工具 exec: …" 纯文本记录
```

- 检索结果必带出处（ID + 时间 + 消息数），抑制幻觉；
- `expand_chunk` 返回纯文本记录而非消息数组——绝不破坏 tool 配对；
- 展开内容只占当轮上下文，之后随滑动窗口自然淡出。

检索内核是 `internal/textscore`（CJK 二元组分词 + 加权评分 + 新鲜度衰减），
与 memory、knowledge 共享同一套实现。

### 失败路径全部无害

| 故障 | 结果 |
|---|---|
| 摘要 LLM 失败 | 不卸载，原文不动，下轮重试 |
| 摘要写库失败 | 同上（force 场景降级机械摘要保索引） |
| 归档中进程崩溃 | 原文已落库（步骤 b 在前），重试按指纹去重 |
| 真实超窗 | L4 裁剪重试一次 |

**不变量：任何情况下原始消息都不会丢。**

### 配置

```json
"context": {
  "enabled": true,
  "softRatio": 0.7,
  "hardRatio": 0.95,
  "keepRecentBlocks": 12,
  "summaryBudget": 0.10,
  "archive": { "backend": "fs", "dir": "" },
  "retrieval": { "limit": 5 },
  "retentionDays": 0
}
```

`enabled=false` 时引擎不注入，行为与未引入本组件完全一致。
归档目录默认 `~/.vortex/<group>/archive/`（group 隔离）。

### 编程接口

```go
archive, _ := ctxengine.NewFileSystemArchive(dir)
engine := ctxengine.New(archive,
    ctxengine.NewLLMSummarizer(provider, model),
    ctxengine.Config{Model: model, RetentionDays: 30})
engine.StartRetention(ctx) // 每日 TTL 清理

ag := agent.New(agent.Options{
    Provider:      provider,
    ContextEngine: engine, // Ask 循环自动接入组装/校准/兜底/轮末归档
    ...
})
for _, t := range ctxengine.NewTools(engine) {
    registry.Add(t) // search_history / expand_chunk
}
```

引擎不绑定会话指针：同一实例可同时服务 REPL、serve 多会话与
subagent/TaskHub 的全新会话（运行态按 sessionID 键控）。

### 删除语义

ctxengine 归档对模型**只读**（无删除工具）——防止模型"删掉对自己不利的
上下文"。生命周期由 `retentionDays` 管理（默认永久）。

---

# memory：跨会话长期记忆

`tools/memory`：agent 运行中自己写入、跨会话跨进程存活的事实库，
六件套工具 `memory_save / memory_update / memory_delete / memory_get /
memory_search / memory_list`。每条记忆是自包含的一句话事实
（fact / preference / decision / lesson 四类 + 标签 + 重要级），
JSON 文件存储（原子写），`KeywordSearcher` 评分检索（委托 textscore）。

与 ctxengine 的桥：归档时抽取的**候选事实**自动写入 memory，
`Importance=2`（低置信）并带 `Source`（来源 chunk ID）标记；
模型可用 `memory_update` 转正或 `memory_delete` 删除。
显式 `memory_save` 始终是主通道，保存时会提示与既有记忆的近似重复。

存储目录默认 `~/.vortex/<group>/memory/memories.json`；
CLI 与 serve 模式均可通过 `tools.memory.enabled` 启用。

# knowledge：文档知识库

`tools/knowledge`：本地文档全文检索（`search_knowledge`）+ 按路径读全文
（`read_document`，带目录穿越/符号链接双重防护）。内容来自磁盘文件，
人维护、走 git，agent 只读——答案可验证到 文件：行号。

配置 `knowledge: ["./docs", ...]` 指定根目录；文件级排序使用与
memory/ctxengine 相同的 textscore 评分内核（区分"顺带提到"与"核心主题"）。

---

## 三种上下文组件的协作示例

一轮长会话里三者各司其职：

1. 会话进行到 80 轮，使用率过 70% → ctxengine 后台归档旧对话，
   生成状态交接摘要；抽取的候选事实（"生产库连接池上限 200"）入 memory
   （低置信带来源）；
2. 第 150 轮用户问"上次那个 postgres 报错怎么解决的" → 模型
   `search_history` 命中归档片段 → `expand_chunk` 展开原文 → 凭原文作答；
3. 新会话开始 → 模型 `memory_search("连接池")` 召回已转正的记忆；
   项目约定类问题 → `search_knowledge` 查文档；
   两套工具、两份数据完全独立，仅共享同一套评分内核。
