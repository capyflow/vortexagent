# Vortex Agent 架构指南（初学者向）

> 这份文档从零讲解 agent 是什么、vortex 这个项目怎么分层、每层代码在做什么，
> 以及如果你要自己写一个 agent 或给 vortex 加功能，应该从哪下手。
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

---

## 1. 整体架构总览

vortex 借鉴了 [earendil-works/pi](https://github.com/earendil-works/pi)（一个 91k stars 的 TS 编码 agent）的分层思想，用 Go 实现。核心原则：**分层、单向依赖**。

```
┌─────────────────────────────────────────────────┐
│  cmd/vortex/    CLI 组装层（入口，把下面所有层拼起来） │
├─────────────────────────────────────────────────┤
│  agent/         Agent 运行时（循环调度 LLM 和工具）  │
├───────────────┬─────────────────────────────────┤
│  llm/          │  tools/mcp/  +  knowledge/       │
│  统一 LLM API  │  工具层（MCP 客户端 / 知识库检索）   │
├───────────────┴─────────────────────────────────┤
│  OpenAI / Anthropic / Gemini / 任意 MCP server     │
└─────────────────────────────────────────────────┘
```

对应 pi 项目（如果你感兴趣可以对照学习）：

| vortex | pi 项目 | 职责 |
|--------|---------|------|
| `llm/` | `packages/ai` | 统一多厂商 LLM API |
| `agent/` | `packages/agent` | Agent 运行时（循环、会话） |
| `tools/mcp/` | extensions 体系 | 可插拔工具（pi 用 TS 扩展，我们用 MCP 标准） |
| `knowledge/` | （pi 没有，我们自研） | 知识库检索 |
| `cmd/vortex/` | `packages/coding-agent` | CLI 入口 |

**依赖方向是单向的**（上层依赖下层，下层不依赖上层）：

```
cmd → agent → llm
           → tools/mcp → llm
           → knowledge →（不依赖任何上层）
```

为什么这么设计？**下层不知道上层的存在**，所以可以单独测试、单独替换。比如换一个 LLM 厂商只动 `llm/` 里一个文件，agent 循环完全不用改。

---

## 2. 第一层：统一 LLM API（llm/）

### 2.1 为什么需要"统一"层？

市面上有几十家 LLM 厂商：OpenAI、Anthropic、Google、DeepSeek、Qwen、智谱……它们的 HTTP API 长得都不一样：

| 差异点 | OpenAI 兼容 | Anthropic | Gemini |
|--------|------------|-----------|--------|
| 接口路径 | `/chat/completions` | `/v1/messages` | `:generateContent` |
| 鉴权 | `Authorization: Bearer` | `x-api-key` 头 | URL query `key=` |
| 工具调用 | `tool_calls` 顶层字段 | `tool_use` 内容块 | `function_call` part |
| 工具结果 | `role: "tool"` | 放 `user` 消息里 | `function_response` part |
| 角色 | system/user/assistant/tool | 同左 | 只有 user/model |

如果业务代码直接调厂商 API，换厂商就得改所有业务代码。所以 vortex 定义了一套**与厂商无关的协议**（`llm/protocol.go`），每个厂商写一个"翻译器"（adapter）把自家协议翻译成统一协议。

### 2.2 核心协议类型（llm/protocol.go）

这是全项目最重要的文件，所有层都依赖它。几个关键类型：

```go
// 一条消息（system 系统提示 / user 用户 / assistant 模型 / tool 工具结果）
type Message struct {
    Role       Role        // system | user | assistant | tool
    Content    []Content   // 内容块列表（一条消息可以有多块内容）
    ToolCalls  []ToolCall  // 模型发起的工具调用（assistant 消息用）
    ToolCallID string      // 工具结果对应的调用 ID（tool 消息用）
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

**流式设计的一个关键决策**（初学者容易踩坑）：流式增量 `Delta` **只用于实时展示**（打字机效果），最终的结构化结果（工具调用、完整文本）等流结束由 provider 拼好统一返回。这样 agent 循环不需要处理"半截 JSON 的工具调用"，逻辑简单很多。

### 2.4 工厂方法

```go
llm.NewProvider(llm.ProviderConfig{Name: "openai", APIKey: "...", ...})
```

按名字返回对应 provider，CLI 层从配置文件读一个字符串就能切换厂商。

---

## 3. 第二层：Agent 运行时（agent/）—— 这是 agent 的心脏

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
func (r *Registry) Params() []llm.ToolParam  // 给模型看的工具清单
func (r *Registry) Call(ctx, name, args) (string, error)
```

**Session 会话（agent/session.go）**

```go
type Session struct {
    Model   string
    History []llm.Message  // 完整对话历史
}
```

会话就是"记忆"。每次提问，agent 把完整历史发给模型，模型才能"记得"之前聊了什么。

### 3.2 Agent 循环（agent/loop.go）—— 全项目最重要的逻辑

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

**为什么历史必须包含完整的三条消息？** 因为模型看到 `tool` 消息才能知道工具执行结果，从而组织回答。如果你只把结果"拼进 prompt"，模型可能不认；标准做法就是保持 `user → assistant(工具调用) → tool(结果) → assistant(最终)` 这个序列。

**为什么限制循环轮数？** 模型可能陷入"无限调用工具"的死循环（比如一直查天气但不回答）。10 轮上限是安全阀。

### 3.3 Agent 自己不"知道"任何厂商差异

注意 `agent/loop.go` 只依赖 `llm.Provider` 接口和 `llm.Message` 类型——它不关心背后是 OpenAI 还是 Gemini。**这就是分层的好处**：循环逻辑写一次，所有厂商通吃。

---

## 4. 第三层：工具层（tools/ + knowledge/）

### 4.1 内置工具：知识库检索（knowledge/）

`search_knowledge`：在配置的目录里按关键词搜索，返回"文件路径 + 行号 + 匹配行 + 上下文"。
`read_document`：读取指定文档全文（有路径安全校验，只能读知识库目录内的文件）。

这是给 LLM 的"长期记忆"：模型不知道你项目里写了什么文档，但能通过搜索工具"查"到。

实现要点（学习价值很高）：

- **文件扫描**：`filepath.WalkDir` 递归，跳过 `.git`/`node_modules` 等目录、跳过二进制文件、限制单文件大小
- **检索**：大小写不敏感子串匹配，按行返回，带前后各 1 行上下文
- **懒加载**：不建索引，每次现读（第一版够用；文档多了再升级 bleve/向量检索）
- **路径安全**：`read_document` 用 `filepath.Rel` 校验，拒绝 `../` 越界——**给 LLM 的工具必须有权限边界**，否则模型可能被 prompt injection 诱导读取任意文件

### 4.2 自定义工具：MCP 客户端（tools/mcp/）

**MCP（Model Context Protocol）** 是一个开放标准，类似"USB 接口"：任何厂商实现 MCP server（Python/Node/Go 都行），agent 作为 MCP client 就能自动发现并调用它的工具。

vortex 的 MCP 客户端工作流程（`tools/mcp/client.go`）：

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

**为什么用 MCP 而不是自己发明工具格式？** 生态。现成的 MCP server 有几百个（文件系统、浏览器、数据库、Slack……），用户写一个 server 就能接入，agent 侧零改动。这是"用户自定义功能"的入口：`examples/mcp-server` 就是一个 40 行的最小模板。

---

## 5. CLI 组装层（cmd/vortex/）—— 组合根

`cmd/vortex/main.go` 不写业务逻辑，只做**组装**（Go 里叫 composition root）：

```go
func main() {
    // 1. 读配置（vortex.json）：provider、知识库目录、MCP server 列表
    cfg, _ := LoadConfig(*configPath)

    // 2. 从环境变量取 API 密钥 → 创建 provider
    provider, _ := llm.NewProvider(llm.ProviderConfig{...})

    // 3. 建工具注册表，注册知识库工具
    registry := agent.NewRegistry()
    registry.Add(knowledge.NewKBTools(kb)...)

    // 4. 逐个连接 MCP server，把它们的工具也注册进去
    for _, srv := range cfg.MCPServers {
        client, _ := mcp.Connect(ctx, srv)
        registry.Add(client.Tools()...)
    }

    // 5. 创建 agent（注入 provider + 注册表 + 流式回调）
    ag := agent.New(agent.Options{
        Provider: provider,
        Registry: registry,
        OnDelta: func(d llm.Delta) { fmt.Print(d.Text) },  // 打字机效果
    })

    // 6. REPL 循环：读一行 → agent.Ask → 打印回答
    session := agent.NewSession(cfg.Provider.Model)
    for { fmt.Print("> "); scanner.Scan()
          line := ...
          ag.Ask(ctx, session, line) }
}
```

注意依赖注入的方向：**agent 不自己创建任何东西**，所有依赖（用哪个模型、有哪些工具）都是外面塞进来的。这样测试时可以注入 fake provider 和假工具，不用真调 API——这就是 `agent/e2e_test.go` 能端到端测试的原因。

---

## 6. 端到端走读：一次真实请求的全链路

以"Vortex 是什么？"为例（`agent/e2e_test.go` 验证的正是这条链路）：

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
   → knowledge 包扫描目录 → 返回 "docs/vortex.md 第1行: Vortex 是一个基于 Go 的智能文档助手..."
7. session.Add(tool "docs/vortex.md: ...")
8. provider.Chat({ Messages: [..., assistant(工具调用), tool(结果)], Tools: [...] })
9. 模型基于检索结果回答: "根据知识库：Vortex 是一个智能文档助手..."
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
| 流式增量只用于展示，结果等流结束统一返回 | 避免处理"半截 JSON 工具参数"的复杂度 |
| 工具结果用标准 `tool` 消息回传 | 各厂商协议要求，模型才能正确理解 |
| Agent 循环上限 10 轮 | 防模型死循环烧钱 |
| 知识库 `read_document` 做路径校验 | 防 prompt injection 导致任意文件读取 |
| 统一协议 + 厂商适配器 | 换厂商只改一个文件 |
| MCP 标准做自定义工具 | 复用生态，用户零成本接入 |
| 会话历史不存 system 提示词 | system 每次请求时动态插入，方便换提示词 |

**踩过的坑**（写代码时真实遇到的）：

1. **Go 包名坑**：`llm/` 目录里声明 `package vllm` 会导致引用方写 `llm.Provider` 报 "undefined: llm"——Go 的导入绑定名是声明的包名而非目录名。已统一为 `package llm`。
2. **Anthropic 的 tool_result 必须放在 `role: "user"` 的消息里**，和 OpenAI 的 `role: "tool"` 完全不同，适配时最容易错。
3. **Gemini 没有 system 角色**，要用顶层 `systemInstruction` 字段；角色只有 `user`/`model`。
4. **流式工具参数是分片 JSON**（`{"city":` + `"北京"}`），必须等流结束拼接后再反序列化。

---

## 8. 读代码顺序（初学者路线）

如果这是你第一次读 agent 项目，按这个顺序：

1. **`llm/protocol.go`** —— 先认识所有类型（30 分钟，这是全项目的"字母表"）
2. **`agent/loop.go`** —— agent 的心脏，先看 `Ask` 函数（30 分钟）
3. **`agent/tool.go`** —— 工具接口和注册表（10 分钟）
4. **`llm/openai.go`** —— 看一个适配器怎么把统一协议翻译成厂商协议（1 小时，最难也最有收获）
5. **`knowledge/knowledge.go`** —— 看一个真实工具的实现（30 分钟）
6. **`tools/mcp/client.go`** —— MCP 客户端（1 小时）
7. **`cmd/vortex/main.go`** —— 最后看怎么组装（10 分钟）
8. 回过头看测试：`agent/loop_test.go`（fake provider 测循环）、`agent/e2e_test.go`（全链路）、`tools/mcp/client_test.go`（真实子进程）

**给初学者的三个核心心法：**

1. **Agent 的本质是循环，不是模型**：模型只负责生成文本，是循环把"决策→执行→反馈"串起来
2. **消息历史是 agent 的"内存"**：一切信息（用户问题、工具结果）都要变成 `Message` 进历史，模型才能"看到"
3. **接口是分层的粘合剂**：`Provider` 接口屏蔽厂商差异，`Tool` 接口屏蔽工具差异，上层代码永远只依赖接口

---

## 9. 如何扩展（实践练习）

| 想做的事 | 改哪里 | 难度 |
|----------|--------|------|
| 加一个 LLM 厂商 | `llm/` 加一个文件实现 `Provider` 接口 + 工厂注册 | ⭐⭐ |
| 加一个内置工具 | 实现 `agent.Tool` 接口 4 个方法，注册进 `Registry` | ⭐ |
| 加一个自定义工具 | 写个 MCP server（参考 `examples/mcp-server`），配置加一行 | ⭐ |
| 让 agent 更聪明 | 改进 `DefaultSystemPrompt`（提示词工程） | ⭐ |
| 会话持久化 | 把 `Session.History` 存到 SQLite/JSON | ⭐⭐ |
| 上下文压缩 | 历史太长时摘要旧消息（compaction，pi 有成熟实现可借鉴） | ⭐⭐⭐ |
| 知识库升级 RAG | 换向量检索（chromem-go 等），`knowledge` 包内部替换 | ⭐⭐⭐ |

**进阶学习资源**：本项目借鉴的 [earendil-works/pi](https://github.com/earendil-works/pi)（TS，91k stars，分层和这里的几乎一一对应）；MCP 官方文档 [modelcontextprotocol.io](https://modelcontextprotocol.io)；Anthropic 的 Agent 设计指南（building effective agents）。
