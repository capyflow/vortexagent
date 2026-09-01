# Vortex Agent 框架架构指南（初学者向）

> 这份文档从零讲解 agent 是什么、vortex 这个**通用 agent 框架**怎么分层、每层代码在做什么，
> 以及如何基于它构建你自己的 agent。
> 建议配合代码阅读：先看本文的"读代码顺序"，再逐文件对照。

---

## 0. 先搞懂：什么是 Agent？

**LLM（大语言模型）本身不是 agent。** 模型只会"生成文本"——你给它一段话，它回一段话。

**Agent = LLM + 工具 + 循环。**

举个生活中的例子：

- 你问一个纯 LLM："今天北京天气怎么样？" —— 它只能凭训练记忆瞎编，因为它**无法获取实时数据**。
- 你问一个 agent："今天北京天气怎么样？" —— 它的工作方式是：

```
1. 把问题发给 LLM
2. LLM 说："我需要调用 get_weather 工具，参数 city=北京"
3. agent 替 LLM 执行工具，拿到"晴，28°C"
4. 把结果回传给 LLM
5. LLM 基于真实数据组织回答："北京今天晴，28°C"
```

关键点：**工具是 LLM 的"手"**，LLM 负责**决策**（用哪个工具、传什么参数），agent 负责**执行**（真正去调用工具、把结果喂回去）。这个"决策→执行→再决策"的循环，就是 agent 的心脏。

**"通用框架"意味着什么？** 框架只提供"决策→执行→反馈"这台发动机，不规定你的 agent 是干什么的。
"文档问答"也好、"代码助手"也好，区别只在于**你注入了哪些工具、写了什么提示词**。

---

## 1. 整体架构总览

vortex 借鉴了 [earendil-works/pi](https://github.com/earendil-works/pi)（一个 91k stars 的 TS 编码 agent）
的分层思想，用 Go 实现。核心原则：**分层、单向依赖**。分三层：

```
┌────────────────────────────────────────────────────────────┐
│ 应用层（不属于框架，是按需使用框架的人写的）                      │
│   cmd/vortex/   参考 CLI：通用 REPL agent（配置驱动）            │
│   examples/     用法示例：minimal / knowledge-agent / mcp-server│
├────────────────────────────────────────────────────────────┤
│ 框架核心（你的应用 import 的就是这些包）                        │
│   agent/        Agent 运行时：循环 / 工具 / 会话 / 钩子 / 存储    │
│   llm/          统一 LLM API：协议 + 多厂商适配器                │
├────────────────────────────────────────────────────────────┤
│ 内置扩展（可选，按需注册，不注册就是纯通用 agent）                │
│   tools/mcp/          MCP 客户端（任意语言的自定义工具）          │
│   tools/knowledge/    本地文档知识库工具（文档问答场景才需要）      │
│   tools/memory/       长期记忆工具（跨会话事实记忆，按需启用）      │
├────────────────────────────────────────────────────────────┤
│ 外部世界：OpenAI / Anthropic / Gemini / 任意 MCP server        │
└────────────────────────────────────────────────────────────┘
```

对应 pi 项目（如果你感兴趣可以对照学习）：

| vortex | pi 项目 | 职责 |
|--------|---------|------|
| `llm/` | `packages/ai` | 统一多厂商 LLM API |
| `agent/` | `packages/agent` | Agent 运行时（循环、会话） |
| `tools/mcp/` | extensions 体系 | 可插拔工具（pi 用 TS 扩展，我们用 MCP 标准） |
| `tools/knowledge/` | （pi 没有，我们的扩展示例） | 文档知识库检索 |
| `cmd/vortex/` | `packages/coding-agent` | 参考 CLI 应用 |

**依赖方向是单向的**（上层依赖下层，下层不依赖上层）：

```
应用层 → agent → llm
              → tools/mcp
              → tools/knowledge
              → tools/memory
```

为什么这么设计？**下层不知道上层的存在**，所以可以单独测试、单独替换。
比如换一个 LLM 厂商只动 `llm/` 里一个文件，agent 循环完全不用改；
你的应用不 import `tools/knowledge`，它就不存在。

---

## 2. 核心层一：统一 LLM API（llm/）

### 2.1 为什么需要"统一"层？

市面上有几十家 LLM 厂商：OpenAI、Anthropic、Google、DeepSeek、Qwen、智谱……它们的 HTTP API 长得都不一样：

| 差异点 | OpenAI 兼容 | Anthropic | Gemini |
|--------|------------|-----------|--------|
| 接口路径 | `/chat/completions` | `/v1/messages` | `:generateContent` |
| 鉴权 | `Authorization: Bearer` | `x-api-key` 头 | URL query `key=` |
| 工具调用 | `tool_calls` 顶层字段 | `tool_use` 内容块 | `function_call` part |
| 工具结果 | `role: "tool"` | 放 `user` 消息里 | `function_response` part |
| 角色 | system/user/assistant/tool | 同左 | 只有 user/model |

如果业务代码直接调厂商 API，换厂商就得改所有业务代码。所以框架定义了一套**与厂商无关的协议**
（`llm/protocol.go`），每个厂商写一个"翻译器"（adapter）把自家协议翻译成统一协议。

### 2.2 核心协议类型（llm/protocol.go）

这是框架最重要的文件，所有层都依赖它。几个关键类型：

```go
// 一条消息（system 系统提示 / user 用户 / assistant 模型 / tool 工具结果）
type Message struct {
    Role       Role       // system | user | assistant | tool
    Content    []Content  // 内容块列表（一条消息可以有多块内容）
    ToolCalls  []ToolCall // 模型发起的工具调用（assistant 消息用）
    ToolCallID string     // 工具结果对应的调用 ID（tool 消息用）
}

// 内容块：文本 / 图片 / 思考过程
type Content struct {
    Type     ContentType // text | image | thinking
    Text     string
    Data     []byte      // 图片原始字节
    URL      string
    MIME     string
    Thinking string      // 思考内容（如 DeepSeek R1 的 reasoning）
}

// 工具调用：模型说"我要调用 get_weather，参数是 {city: 北京}"
type ToolCall struct {
    ID        string         // 调用 ID（执行完要带着它回传结果）
    Name      string         // 工具名
    Arguments map[string]any // 参数（JSON 对象）
}

// 发给模型的工具声明（模型据此知道有哪些工具可用、怎么传参）
type ToolParam struct {
    Name        string
    Description string         // 描述越清楚，模型越会正确使用
    Schema      map[string]any // 参数 JSON Schema
}

// 一次完整的对话请求
type ChatRequest struct {
    Model       string
    Messages    []Message     // 完整历史
    Tools       []ToolParam   // 可用工具
    Temperature *float64
    MaxTokens   int
    Thinking    bool          // 思考模式
}

// 模型完整回复（流式结束后由 provider 汇总）
type ChatResponse struct {
    Message      Message
    Usage        Usage         // token 消耗统计
    FinishReason string        // stop | tool_calls | length ...
}

// 流式增量（用于实时展示，不是最终结果）
type Delta struct {
    Text     string // 增量文本
    Thinking string // 增量思考
    Done     bool   // 流结束标记
}
```

**理解这些类型就理解了 agent 对话的本质**：对话 = 一串 `Message` 的数组（历史），模型每次基于完整历史生成新的 `Message`。

### 2.3 Provider 接口（统一协议的抽象）

```go
type Provider interface {
    Name() string
    // 发送请求；onDelta 非 nil 时走流式，返回的 ChatResponse 始终是完整消息
    Chat(ctx context.Context, req *ChatRequest, onDelta func(Delta) error) (*ChatResponse, error)
}
```

每个厂商一个实现：`openai.go` / `anthropic.go` / `gemini.go`。它们内部做的事完全一样：

```
统一 Message 数组  ──翻译──▶  厂商的 HTTP JSON  ──发送──▶  厂商服务器
统一 ChatResponse ◀──翻译──  厂商的响应 JSON  ◀──解析──
```

**流式设计的一个关键决策**（初学者容易踩坑）：流式增量 `Delta` **只用于实时展示**（打字机效果），
最终的结构化结果（工具调用、完整文本）等流结束由 provider 拼好统一返回。
这样 agent 循环不需要处理"半截 JSON 的工具调用"，逻辑简单很多。

### 2.4 工厂方法

```go
llm.NewProvider(llm.ProviderConfig{Name: "openai", APIKey: "...", ...})
```

按名字返回对应 provider，应用层从配置读一个字符串就能切换厂商。

---

## 3. 核心层二：Agent 运行时（agent/）—— agent 的心脏

### 3.0 包内结构一览

`agent/` 包按职责拆成 6 个文件，读代码前先记住这张图：

```
agent/ 包
├── agent.go    Agent 本体与配置：Options（怎么创建）、New（构造）、默认提示词
├── ask.go      Ask 主循环：用户输入 → LLM ↔ 工具 往返调度 → 最终回答 ★心脏
├── tool.go     Tool 接口（agent 能力的来源）+ Registry 注册表
├── session.go  Session 会话：对话历史（agent 的"内存"）
├── hooks.go    Hooks 生命周期钩子：观察 / 拦截扩展点（日志、权限）
├── subagent.go 子 agent 原语：把 Agent 包装成 Tool，供父 agent 委派任务
├── taskhub.go  异步任务编排：后台并发执行子 agent 任务，分发后不阻塞主 agent
└── store.go    SessionStore 会话存储：持久化抽象（内存 / JSON 文件实现）

运行时协作关系（一次 Ask 的调用链）：
                        ┌───────────────────────────┐
   用户输入 ──▶ Ask 循环 │  chat(): 组装请求          │
                        │   历史 ← session.History   │
                        │   工具声明 ← registry.Params│
                        │   系统提示词 ← systemPrompt │
                        │   ──▶ provider.Chat()      │──▶ OpenAI/Anthropic/Gemini
                        │   ◀── 回复（文本 / 工具调用）│
                        │                            │
                        │   execTool(): 执行工具      │──▶ registry.Call() ──▶ 具体 Tool
                        │     执行前 hooks.OnBeforeToolCall（可拦截）
                        │     执行后 hooks.OnAfterToolCall
                        │   结果追加回 session.History │
                        └───────────────────────────┘
                      退出前：OnFinish / OnError 钩子、store.Save() 自动持久化
```

核心思想：**Ask 循环是唯一的总指挥**，Session 是它的记忆，Registry 是它的工具箱，
Provider 是它的嘴，Hooks 和 Store 是它的"外接仪表与硬盘"——这些组件互不认识，全靠 Ask 串起来。

### 3.1 两个基础组件

**Tool 接口 + 注册表（agent/tool.go）**

```go
// 任何工具只需要实现这 4 个方法（Go 接口是结构化的，实现方甚至不用 import 本包）
type Tool interface {
    Name() string                    // 工具名（模型调用时用）
    Description() string             // 工具说明（模型据此决定是否调用）
    Schema() map[string]any          // 参数 JSON Schema
    Call(ctx context.Context, args map[string]any) (string, error) // 执行
}

// 注册表：维护"名字 → 工具"的映射，重名报错
type Registry struct{ ... }
func (r *Registry) Add(t Tool) error
func (r *Registry) Params() []llm.ToolParam  // 给模型看的工具清单（按名字排序，顺序确定）
func (r *Registry) Call(ctx, name, args) (string, error)
```

**Session 会话（agent/session.go）**

```go
type Session struct {
    ID      string
    Model   string
    History []llm.Message  // 完整对话历史
}
```

会话就是"记忆"。每次提问，agent 把完整历史发给模型，模型才能"记得"之前聊了什么。

### 3.2 Agent 循环（agent/ask.go）—— 全框架最重要的逻辑

```go
func (a *Agent) Ask(ctx context.Context, session *Session, userInput string) (string, error) {
    // ① 把用户输入追加到历史
    session.Add(llm.NewTextMessage(llm.RoleUser, userInput))

    // ② 循环：最多 maxIterations（默认 10）轮
    for iter := 1; iter <= a.maxIter; iter++ {
        // ③ 带完整历史 + 工具清单调用 LLM
        resp, err := a.chat(ctx, session)
        session.Add(resp.Message)

        // ④ 模型没要求调用工具 → 这就是最终回答，返回
        if len(resp.Message.ToolCalls) == 0 {
            return textOf(resp.Message), nil
        }

        // ⑤ 模型要求调用工具 → 逐个执行，结果追加回历史
        for _, call := range resp.Message.ToolCalls {
            result, _ := a.execTool(ctx, call)
            session.Add(llm.NewToolResultMessage(call.ID, result))
        }
        // ⑥ 回到 ③，把"工具结果"交给模型继续思考
    }
    return "", fmt.Errorf("工具调用超过 %d 轮仍未结束", a.maxIter)
}
```

对应的时序图：

```
用户 "北京天气？"
  │
  ▼
[user] "北京天气？"
  │
  ▼ 发给 LLM（带 get_weather 工具声明）
LLM: 我要调用 get_weather{city:"北京"}   ← FinishReason: tool_calls
  │
  ▼
[assistant(工具调用)] 加入历史
  │
  ▼ 执行工具（真实查天气）
[tool] "晴，28°C" 加入历史
  │
  ▼ 再发给 LLM（历史现在有 user+assistant+tool 三条）
LLM: "北京今天晴，28°C"                    ← FinishReason: stop
  │
  ▼
返回文本给用户
```

**为什么历史必须包含完整的三条消息？** 因为模型看到 `tool` 消息才能知道工具执行结果，从而组织回答。
如果你只把结果"拼进 prompt"，模型可能不认；标准做法就是保持 `user → assistant(工具调用) → tool(结果) → assistant(最终)` 这个序列。

**为什么限制循环轮数？** 模型可能陷入"无限调用工具"的死循环（比如一直查天气但不回答）。10 轮上限是安全阀。

**失败时历史自动回滚**：任何一步出错（LLM 调用失败、空回答、超轮数），Ask 都会把历史回滚到提问前，
未回答的问题与半截工具轮次不会残留在会话里污染下一次提问。

### 3.3 扩展点一：Hooks 生命周期钩子（agent/hooks.go）

通用框架必须让使用者能**观察和干预** agent 的运行。Hooks 提供了 5 个钩子：

```go
type Hooks struct {
    OnMessage         func(ctx, msg llm.Message)                       // 每条 LLM 回复后
    OnBeforeToolCall  func(ctx, name string, args map[string]any) error // 工具执行前，返回错误可拦截
    OnAfterToolCall   func(ctx, name string, args, result string, err error) // 工具执行后
    OnError           func(ctx, err error)                              // 任何出错路径
    OnFinish          func(ctx, answer string)                          // 成功返回回答后
}
```

用途举例：

- **日志与遥测**：`OnMessage` / `OnAfterToolCall` 记录每轮交互，统计 token 消耗与工具成功率
- **权限控制**：`OnBeforeToolCall` 检查工具名与参数，拦截危险调用（错误文本会作为工具结果回传，
  模型可改用其他工具或直接回答）
- **监控告警**：`OnError` 上报失败

### 3.4 扩展点二：SessionStore 会话存储（agent/store.go）

会话默认只存在于内存。框架定义了存储抽象：

```go
type SessionStore interface {
    Save(ctx context.Context, s *Session) error
    Load(ctx context.Context, id string) (*Session, error) // 不存在返回 (nil, nil)
    Delete(ctx context.Context, id string) error
}
```

内置两个实现：

- `MemorySessionStore`：进程内存储，框架默认行为
- `JSONSessionStore`：单文件持久化，进程重启后恢复会话（"继续上次对话"），
  还提供了 `LoadLatest()` 取最近会话的便利方法

接入方式：`agent.New(Options{Store: myStore})`，每次 Ask 结束自动保存。
接 SQLite / Redis 只需实现这 3 个方法。参考 CLI 用 `sessionFile` 配置即可启用。

### 3.5 Agent 自己不"知道"任何厂商差异

注意 `agent/ask.go` 只依赖 `llm.Provider` 接口和 `llm.Message` 类型——它不关心背后是
OpenAI 还是 Gemini。**这就是分层的好处**：循环逻辑写一次，所有厂商通吃。

### 3.6 扩展点三：子 agent（agent/subagent.go）

`NewSubagentTool` 把一个构建好的 Agent 包装成 Tool，是框架衍生**多 agent 系统**的核心原语：

```go
aftersales := agent.New(agent.Options{Provider: p, Registry: 售后工具, SystemPrompt: "你是售后专员…"})
parentRegistry.Add(agent.NewSubagentTool("aftersales", "处理退款/换货等售后问题", aftersales))
```

对父 agent 而言子 agent 就是一个普通工具，因此框架机制原样生效：

- **上下文隔离**：每次委派用全新会话，子 agent 的思考与工具轮次不进入父上下文，
  只有最终回答作为一条 tool 消息回传——父 agent 的上下文不会被子任务撑爆
- **权限继承**：父 agent 的 `OnBeforeToolCall` 钩子可以拦截某次委派（子 agent 根本不会启动）
- **独立配置**：子 agent 可以用不同的模型、提示词、工具箱、轮数上限
- **递归防护**：委派链路深度有上限（`MaxSubagentDepth`），防止 A 委派 B、B 又委派 A 的死循环

典型衍生场景见 `examples/customer-service-agent`：总机 agent 按意图把问题委派给
售前 / 售后专员子 agent，每个专员带自己的业务工具。

### 3.7 扩展点四：异步任务编排（agent/taskhub.go）

`SubagentTool` 是**同步**委派（父 agent 阻塞等结果）。`TaskHub` 补上**异步**模式：
主 agent 分发任务后循环立刻继续，子 agent 在后台各自执行，之后再收结果——
即"并行调研多项、最后汇总"或"启动长任务、下轮对话再收结果"。

```go
hub := agent.NewTaskHub(appCtx, 2)          // 应用级 ctx 锚定任务生命周期，并发上限 2
hub.Register("pricing", pricingAgent)       // 具名子 agent
registry.Add(hub.Tools()...)                // task_start / task_status / task_wait / task_cancel
```

Go 并发原语都在框架内部，对模型暴露的只是四个普通工具：

| Go 机制 | 落点 |
|---------|------|
| goroutine | 每个 `task_start` 起一个任务 goroutine，跑完即退，无常驻协程 |
| channel（done） | 每个任务一个完成信号 channel，`task_wait` 据此 join |
| channel（semaphore） | 带缓冲 channel 限制同时运行的子 agent 数，超出的任务排队（pending） |

关键决策：

- **任务挂在 Hub 的 base ctx 上，而不是某次 Ask 的请求 ctx**——主 agent 本轮回答结束、
  HTTP 请求返回都不会误杀后台任务；应用退出（base ctx 取消）或 `task_cancel` 才终止
- **结果靠轮询收取，而不是推送**：Ask 循环是同步的，"完成后唤醒父 agent"需要常驻
  收件箱机制，v1 用 `task_status` / `task_wait`（语义对模型更清晰），推送留作后续演进
- **排队上限**：未完成任务数有上限，防止模型无节制分发

完整示例见 `examples/async-agent`。

---

## 4. 扩展层（tools/）：框架的"可插拔能力"

框架核心不内置任何业务工具。所有领域能力以扩展包的形式存在，按需注册。

### 4.1 内置扩展：MCP 客户端（tools/mcp/）

**MCP（Model Context Protocol）** 是一个开放标准，类似"USB 接口"：任何厂商实现 MCP server
（Python/Node/Go 都行），agent 作为 MCP client 就能自动发现并调用它的工具。

MCP 客户端工作流程（`tools/mcp/client.go`）：

```
Connect(配置)
  → 以子进程方式拉起 MCP server（stdio 管道）
  → initialize 握手（协议版本、客户端信息，30s 超时兜底）
  → tools/list 拉取工具清单
  → 每个工具包装成 Tool 接口（Name/Description/Schema/Call）
  → 注册进 agent 的 Registry

对话中：
  LLM 决定调用某工具 → Tool.Call() → tools/call 请求 → 结果文本回传
```

**为什么用 MCP 而不是自己发明工具格式？** 生态。现成的 MCP server 有几百个
（文件系统、浏览器、数据库、Slack……），用户写一个 server 就能接入，agent 侧零改动。
`examples/mcp-server` 就是一个 40 行的最小模板。

### 4.2 内置扩展：知识库工具（tools/knowledge/）

`search_knowledge`：在配置的目录里按关键词搜索，返回"文件路径 + 行号 + 匹配行 + 上下文"。
`read_document`：读取指定文档全文（有路径安全校验，只能读知识库目录内的文件）。

这是"文档问答 agent"的长期记忆：模型不知道你项目里写了什么文档，但能通过搜索工具"查"到。
**注意它的定位**：这是扩展，不是核心。不注册 `knowledge.NewKBTools(kb)`，
你的 agent 就与文档毫无关系——`examples/minimal-agent` 演示的正是这种"纯通用"形态。

实现要点（学习价值很高）：

- **文件扫描**：`filepath.WalkDir` 递归，跳过 `.git`/`node_modules` 等目录、跳过二进制文件、限制单文件大小
- **检索**：大小写不敏感子串匹配，按行返回，带前后各 1 行上下文
- **懒加载**：不建索引，每次现读（第一版够用；文档多了再升级 bleve/向量检索）
- **路径安全**：`read_document` 用 `filepath.Rel` + `EvalSymlinks` 双重校验，拒绝 `../` 越界——
  **给 LLM 的工具必须有权限边界**，否则模型可能被 prompt injection 诱导读取任意文件

### 4.3 内置扩展：长期记忆工具（tools/memory/）

与 4.2 的知识库不同，这是 **agent 在运行中自己写入、跨会话存活的记忆**。框架里三类
已有"记忆"机制的分工：

| 机制 | 生命周期 | 粒度 |
|------|---------|------|
| `Session.History` | 单次会话内 | 对话原文 |
| ContextManager 卸载 | 会话内压缩归档（按 sessionID 隔离） | 历史摘要 |
| `tools/knowledge` | 人工维护的静态语料，只读 | 文档 |
| `tools/memory`（本节） | **跨会话，agent 自己增删改查** | 一条记忆一个事实 |

暴露六个工具：`memory_save` / `memory_update` / `memory_delete` / `memory_get` /
`memory_search` / `memory_list`。每条记忆是自包含的事实（偏好 / 决策 / 教训等，
带 kind、tags、importance），不存对话原文。

实现要点：

- **两个抽象接口**：`Store`（持久化，内置 JSON 文件实现，可换 Postgres）、
  `Searcher`（检索，内置零依赖的关键词评分实现——CJK 二元组分词 + 标签加权 +
  新鲜度/重要级加分）。记忆量大或需要语义泛化时，把 `Searcher` 换成向量检索
  （RAG）即可，工具层零改动
- **近似重复提示**：save 时计算词元 Jaccard 相似度，发现高度相似的旧记忆只提示
  不阻止——保存是显式意图，是否合并/清理由模型决定
- **工具结果带 ID**：所有输出把 ID 放在最前面，模型引用它做 update / delete

---

## 5. 应用层：参考 CLI 与示例

### 5.1 参考 CLI（cmd/vortex/）—— 组合根

`cmd/vortex/main.go` 不写业务逻辑，只做**组装**（Go 里叫 composition root）：

```go
func main() {
    // 1. 读配置（vortex.json）：provider、知识库目录、MCP server 列表、会话文件
    cfg, _ := LoadConfig(*configPath)

    // 2. 从环境变量取 API 密钥 → 创建 provider
    provider, _ := llm.NewProvider(llm.ProviderConfig{...})

    // 3. 建工具注册表，注册知识库扩展工具（可选）
    registry := agent.NewRegistry()
    if len(cfg.Knowledge) > 0 {
        kb := knowledge.NewKB(cfg.Knowledge)
        registry.Add(knowledge.NewKBTools(kb)...)
    }

    // 4. 逐个连接 MCP server，把它们的工具也注册进去（可选）
    for _, srv := range cfg.MCPServers {
        client, _ := mcp.Connect(ctx, srv)
        registry.Add(client.Tools()...)
    }

    // 5. 会话持久化（可选）：启动时恢复上次会话，Ask 后自动保存
    if cfg.SessionFile != "" { store = agent.NewJSONSessionStore(cfg.SessionFile); ... }

    // 6. 创建 agent（注入 provider + 注册表 + 存储 + 流式回调）
    ag := agent.New(agent.Options{
        Provider: provider, Registry: registry, Store: store,
        OnDelta: func(d llm.Delta) { fmt.Print(d.Text) },  // 打字机效果
    })

    // 7. REPL 循环：读一行 → agent.Ask → 打印回答
    session := agent.NewSession(cfg.Provider.Model)
    for { fmt.Print("> "); scanner.Scan()
          line := ...
          ag.Ask(ctx, session, line) }
}
```

注意依赖注入的方向：**agent 不自己创建任何东西**，所有依赖（用哪个模型、有哪些工具）都是外面塞进来的。
这样测试时可以注入 fake provider 和假工具，不用真调 API——这就是 `agent/e2e_test.go` 能端到端测试的原因。

### 5.2 示例（examples/）

| 示例 | 演示内容 |
|------|---------|
| `minimal-agent` | 最小编程模型：4 步搭起一个带"时间工具"的通用 agent，不依赖任何扩展 |
| `knowledge-agent` | 在 minimal 基础上多注册两个知识库工具，即变成文档问答 agent |
| `mcp-server` | MCP server 模板：任何语言的自定义工具如何暴露给 agent |

**对比 minimal 与 knowledge-agent 两个示例**，就能直观理解"框架与领域无关"：
它们唯一的差别是注册的工具和系统提示词不同。

---

## 6. 端到端走读：一次真实请求的全链路

以"Vortex 是什么？"为例（`agent/e2e_test.go` 验证的正是这条链路，mock 了 OpenAI + 知识库扩展）：

```
1. CLI 收到输入 "Vortex 是什么？"
2. session.Add(user "Vortex 是什么？")
3. provider.Chat({
       Messages: [system提示词, user "Vortex 是什么？"],
       Tools: [search_knowledge, read_document, ...],
   })
4. OpenAI 兼容服务器返回: assistant(ToolCalls: [search_knowledge{query:"Vortex"}])
5. session.Add(assistant 消息)
6. registry.Call("search_knowledge", {query:"Vortex"})
   → knowledge 扩展扫描目录 → 返回 "docs/vortex.md 第1行: Vortex 是一个基于 Go 的通用 agent 框架..."
7. session.Add(tool "docs/vortex.md: ...")
8. provider.Chat({ Messages: [..., assistant(工具调用), tool(结果)], Tools: [...] })
9. 模型基于检索结果回答: "根据知识库：Vortex 是一个基于 Go 的通用 agent 框架..."
10. 无工具调用 → 返回文本 → CLI 打印
```

消息历史随流程演变：

```
[user] "Vortex 是什么？"
[user] "Vortex 是什么？" [assistant] "我要调用 search_knowledge"
[user] "Vortex 是什么？" [assistant] "调用工具" [tool] "docs/vortex.md: Vortex 是一个..."
[user] "Vortex 是什么？" [assistant] "调用工具" [tool] "检索结果" [assistant] "根据知识库：..."
```

---

## 7. 关键设计决策（含踩坑记录）

| 决策 | 原因 |
|------|------|
| 框架核心不内置业务工具，能力全部来自扩展 | 保证框架与领域无关，任何 agent 场景复用同一套核心 |
| 流式增量只用于展示，结果等流结束统一返回 | 避免处理"半截 JSON 工具参数"的复杂度 |
| 工具结果用标准 `tool` 消息回传 | 各厂商协议要求，模型才能正确理解 |
| Agent 循环上限 10 轮 | 防模型死循环烧钱 |
| 知识库 `read_document` 做双重路径校验 | 防 prompt injection 导致任意文件读取 |
| 统一协议 + 厂商适配器 | 换厂商只改一个文件 |
| MCP 标准做自定义工具 | 复用生态，用户零成本接入 |
| 会话历史不存 system 提示词 | system 每次请求时动态插入，方便换提示词 |
| Hooks / SessionStore 用接口而非硬编码 | 日志、权限、持久化都是"使用方的事"，框架只留扩展点 |
| 权限拦截、未知工具等永久性错误不重试 | 重试同样的调用不会成功，只会白等退避时间；错误文本回传模型让它改路 |
| 异步任务靠轮询工具收取结果，不做推送 | Ask 循环是同步的；模型主动 status/wait 语义清晰，推送需要常驻收件箱（留作演进） |
| JSON 模式在 Anthropic 用系统提示约束 | Anthropic 协议没有统一的 response_format 字段，提示约束是对所有模型生效的通用兜底 |
| 给 LLM 的工具必须有路径/权限边界 | filesystem 工具限制在 root 内（Rel + EvalSymlinks 双重校验）；工具 panic 由框架 recover 转为错误文本 |
| Ask 失败自动回滚历史 | 未回答的问题与半截工具轮次不残留，避免污染下次提问 |

**踩过的坑**（写代码时真实遇到的）：

1. **Go 包名坑**：`llm/` 目录里声明 `package vllm` 会导致引用方写 `llm.Provider` 报
   "undefined: llm"——Go 的导入绑定名是声明的包名而非目录名。已统一为 `package llm`。
2. **Anthropic 的 tool_result 必须放在 `role: "user"` 的消息里**，和 OpenAI 的
   `role: "tool"` 完全不同，适配时最容易错。
3. **Gemini 没有 system 角色**，要用顶层 `systemInstruction` 字段；角色只有 `user`/`model`。
4. **流式工具参数是分片 JSON**（`{"city":` + `"北京"}`），必须等流结束拼接后再反序列化。
5. **工具执行出错不能中断循环**：错误要转成文本作为工具结果回传（"工具执行出错: ..."），
   让模型知道发生了什么，而不是让整个 Ask 失败。

---

## 8. 读代码顺序（初学者路线）

如果这是你第一次读 agent 框架项目，按这个顺序：

1. **`llm/protocol.go`** —— 先认识所有类型（30 分钟，这是全项目的"字母表"）
2. **`agent/agent.go` → `agent/ask.go`** —— agent 的心脏：先看 `Options` 与 `New`（配置），再看 `Ask` 函数（循环，30 分钟）
3. **`agent/tool.go`** —— 工具接口和注册表（10 分钟）
4. **`agent/hooks.go` + `agent/store.go`** —— 框架的扩展点长什么样（15 分钟）
5. **`llm/openai.go`** —— 看一个适配器怎么把统一协议翻译成厂商协议（1 小时，最难也最有收获）
6. **`tools/knowledge/knowledge.go`** —— 看一个真实扩展工具的实现（30 分钟）
7. **`tools/mcp/client.go`** —— MCP 客户端（1 小时）
8. **`cmd/vortex/main.go`** —— 最后看怎么把框架组装成应用（10 分钟）
9. 回过头看测试：`agent/loop_test.go`（fake provider 测循环）、`agent/hooks_test.go`（钩子）、
   `agent/store_test.go`（会话存储）、`agent/e2e_test.go`（全链路）、`tools/mcp/client_test.go`（真实子进程）

**给初学者的三个核心心法：**

1. **Agent 的本质是循环，不是模型**：模型只负责生成文本，是循环把"决策→执行→反馈"串起来
2. **消息历史是 agent 的"内存"**：一切信息（用户问题、工具结果）都要变成 `Message` 进历史，模型才能"看到"
3. **接口是分层的粘合剂**：`Provider` 接口屏蔽厂商差异，`Tool` 接口屏蔽工具差异，
   `SessionStore` 接口屏蔽存储差异，上层代码永远只依赖接口

---

## 9. 如何基于框架构建你自己的 agent（实践练习）

| 想做的事 | 改哪里 | 难度 |
|----------|--------|------|
| 加一个 LLM 厂商 | `llm/` 加一个文件实现 `Provider` 接口 + 工厂注册 | ⭐⭐ |
| 给 agent 加一个工具 | 实现 `agent.Tool` 接口 4 个方法，注册进 `Registry` | ⭐ |
| 接入现成 MCP 工具 | 配置 `mcpServers` 加一行（Python/Node/Go 生态现成 server 均可） | ⭐ |
| 记录对话日志 / 埋点 | 用 `agent.Hooks` 的 `OnMessage` / `OnAfterToolCall` | ⭐ |
| 禁止 agent 调用某工具 | 用 `agent.Hooks` 的 `OnBeforeToolCall` 拦截 | ⭐ |
| 会话持久化到数据库 | 实现 `agent.SessionStore` 3 个方法 | ⭐⭐ |
| 总-分式多 agent（智能客服 / 编码 agent） | 为每个专员场景建独立 Agent，`NewSubagentTool` 注册进父 Registry（见 `examples/customer-service-agent`） | ⭐⭐ |
| 并行分发后台任务 | `TaskHub` + `task_start` / `task_wait`，主 agent 分发后继续自己的工作（见 `examples/async-agent`） | ⭐⭐ |
| 让 agent 更聪明 | 改进 `Options.SystemPrompt`（提示词工程） | ⭐ |
| 上下文压缩 | 历史太长时摘要旧消息（compaction，pi 有成熟实现可借鉴） | ⭐⭐⭐ |
| 跨会话长期记忆 | 注册 `tools/memory` 六件套（`memory_save` / `memory_search` 等），或自定义 `Store` / `Searcher` 接入向量检索 | ⭐ |
| 文档问答升级 RAG | 换向量检索（chromem-go 等），`tools/knowledge` 包内部替换 | ⭐⭐⭐ |

**进阶学习资源**：本项目借鉴的 [earendil-works/pi](https://github.com/earendil-works/pi)
（TS，91k stars，分层和这里的几乎一一对应）；MCP 官方文档
[modelcontextprotocol.io](https://modelcontextprotocol.io)；Anthropic 的 Agent 设计指南
（building effective agents）。
