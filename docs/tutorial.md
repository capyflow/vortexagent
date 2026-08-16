# 从零学会 Agent：Vortex 框架初学者教程

> 本教程面向**没写过 agent、甚至没听说过 agent 的初学者**。不需要任何 AI 经验，
> 只需要会一点 Go。全程动手：每一课都有可运行的代码，建议跟着敲一遍。
>
> 配套文档：[架构参考](architecture.md)（讲"框架为什么这么设计"）、
> [项目 README](../README.md)（快速开始与配置）。

---

## 目录

- [第 0 课：Agent 是什么？](#第-0-课agent-是什么)
- [第 1 课：跑起你的第一个 agent](#第-1-课跑起你的第一个-agent)
- [第 2 课：拆解最小 agent 的每一行](#第-2-课拆解最小-agent-的每一行)
- [第 3 课：写你的第一个工具（动手）](#第-3-课写你的第一个工具动手)
- [第 4 课：看懂工具循环——agent 的心脏](#第-4-课看懂工具循环agent-的心脏)
- [第 5 课：会话——agent 的记忆](#第-5-课会话agent-的记忆)
- [第 6 课：钩子——给 agent 装日志和门禁](#第-6-课钩子给-agent-装日志和门禁)
- [第 7 课：持久化——重启不丢记忆](#第-7-课持久化重启不丢记忆)
- [第 8 课：接入现成能力（知识库 / MCP）](#第-8-课接入现成能力知识库--mcp)
- [第 9 课：读源码的路线图](#第-9-课读源码的路线图)
- [常见坑与练习](#常见坑与练习)

---

## 第 0 课：Agent 是什么？

**LLM（大语言模型）本身不是 agent。** 模型只会"生成文本"——你给它一段话，它回一段话。
它没有手，查不了天气、读不了文件、算不了账。

**Agent = LLM + 工具 + 循环。**

- 你问一个纯 LLM："现在几点了？" —— 它只能瞎编（它训练时没有"现在"这个概念）。
- 你问一个 agent："现在几点了？" —— 它会：

```
1. 把问题发给 LLM
2. LLM 说："我需要调用 current_time 工具"
3. agent 替你执行工具，拿到 "2025-08-17T09:30:00+08:00"
4. 把结果回传给 LLM
5. LLM 组织回答："现在是 2025 年 8 月 17 日 09:30。"
```

**分工**：LLM 负责**决策**（用哪个工具、传什么参数），agent 负责**执行**（真正调用工具、
把结果喂回去）。这个"决策 → 执行 → 再决策"的循环，就是 agent 的心脏。

Vortex 就是一个把这件事做好的**通用框架**：它不规定你的 agent 是干什么的——
文档问答、代码助手、天气查询……区别只在于你**注入了哪些工具、写了什么提示词**。

---

## 第 1 课：跑起你的第一个 agent

### 前置条件

- Go 1.23+（`go version` 确认）
- 一个 LLM 的 API 密钥。本仓库默认支持三种，任选其一：

| 厂商 | 环境变量 | 说明 |
|------|---------|------|
| OpenAI 兼容 | `OPENAI_API_KEY` | DeepSeek / Qwen / 智谱等都可填这里 |
| Anthropic | `ANTHROPIC_API_KEY` | Claude |
| Gemini | `GEMINI_API_KEY` | Google |

没有密钥？先用 DeepSeek（国内可注册，OpenAI 兼容协议，几块钱够玩很久）。

### 运行

在仓库根目录：

```bash
export OPENAI_API_KEY=sk-你的密钥
go run ./examples/minimal-agent "现在几点了？"
```

你会看到模型先"调用工具"，然后给出回答。如果一切正常，你已经在运行一个真正的 agent 了。

> 这个示例故意**不依赖任何扩展**（没有知识库、没有 MCP）——它就是框架的最小编程模型。

---

## 第 2 课：拆解最小 agent 的每一行

打开 [`examples/minimal-agent/main.go`](../examples/minimal-agent/main.go)，全项目
加起来约 70 行，核心只有 **四步**：

```go
// ① 创建 LLM 服务（provider）—— 框架的"嘴"
provider, _ := llm.NewProvider(llm.ProviderConfig{
    Name:   "openai",   // 换成 anthropic / gemini 即可切换厂商
    APIKey: apiKey,     // 从环境变量读
})

// ② 创建工具注册表并注册工具 —— 框架的"工具箱"
registry := agent.NewRegistry()
registry.Add(&timeTool{})   // timeTool 是示例里实现的一个工具

// ③ 创建 Agent —— 把"嘴"和"工具箱"装进引擎
ag := agent.New(agent.Options{
    Provider: provider,          // LLM（必填）
    Registry: registry,          // 工具（可空）
    OnDelta:  func(d llm.Delta) { fmt.Print(d.Text) }, // 流式增量 → 打字机效果
})

// ④ 提问
session := agent.NewSession("default")
answer, err := ag.Ask(context.Background(), session, "现在几点了？")
fmt.Println(answer)
```

**记住这个四步模型，它是一切 agent 的骨架**：嘴（Provider）→ 工具箱（Registry）→
引擎（Agent）→ 提问（Ask）。后面每一课都是在这四步上做加法。

---

## 第 3 课：写你的第一个工具（动手）

工具是 agent 能力的**唯一来源**——框架不内置任何业务工具。写一个工具只需要实现
`agent.Tool` 接口的 **4 个方法**：

```go
// 任意类型，实现这 4 个方法即成为工具。
// 注意：Go 接口是结构化的，你的代码甚至不用 import agent 包！
type timeTool struct{}

// Name      ：工具名，模型在"决定调用哪个工具"时使用这个名字
func (t *timeTool) Name() string { return "current_time" }

// Description：工具说明！描述越清楚，模型越知道什么时候该用它
func (t *timeTool) Description() string { return "获取当前时间" }

// Schema    ：参数定义（JSON Schema），没有参数就返回 {"type": "object"}
func (t *timeTool) Schema() map[string]any {
    return map[string]any{"type": "object"}
}

// Call      ：真正执行工具，返回给模型看的文本结果
func (t *timeTool) Call(_ context.Context, _ map[string]any) (string, error) {
    return time.Now().Format(time.RFC3339), nil
}
```

**动手练习**：把这个工具改成"带时区参数"的版本——在 `Schema()` 里声明一个
`timezone` 字符串参数，在 `Call()` 里从 `args` 取出：

```go
func (t *timeTool) Schema() map[string]any {
    return map[string]any{
        "type": "object",
        "properties": map[string]any{
            "timezone": map[string]any{
                "type":        "string",
                "description": "IANA 时区名，如 Asia/Shanghai",
            },
        },
    }
}

func (t *timeTool) Call(_ context.Context, args map[string]any) (string, error) {
    tz, _ := args["timezone"].(string) // 模型没传时是空串
    loc := time.Local
    if tz != "" {
        var err error
        loc, err = time.LoadLocation(tz)
        if err != nil {
            return "", fmt.Errorf("未知时区 %q", tz)
        }
    }
    return time.Now().In(loc).Format(time.RFC3339), nil
}
```

改完 `go run ./examples/minimal-agent "东京现在几点？"`——模型会传 `timezone` 参数了。

> **为什么模型会"自动学会"用新参数？** 因为每次请求，框架都会把你的 `Name`、
> `Description`、`Schema` 作为"工具说明书"发给模型。写清楚说明书，模型就会正确使用。

---

## 第 4 课：看懂工具循环——agent 的心脏

打开 [`agent/ask.go`](../agent/ask.go)，`Ask` 函数就是循环本体。伪代码：

```
Ask(会话, 用户输入):
    把用户输入追加进会话历史
    循环（最多 10 轮）:
        带"完整历史 + 工具清单"调用 LLM        ← chat()
        把 LLM 回复追加进历史
        如果回复里没有工具调用:
            返回回答文本                        ← 结束！
        否则:
            逐个执行工具调用，结果追加进历史      ← execTool()
    报错：工具调用超过最大轮数
```

**核心机密：消息历史（Message）是 agent 唯一的记忆通道。** 一切信息——用户问题、
模型决策、工具结果——都必须变成 `Message` 进历史，模型才能"看见"。

看一次提问的历史演变（`role` 视角）：

```
[user]      "东京现在几点？"
[assistant] "我要调用 current_time，参数 {timezone: Asia/Tokyo}"
[tool]      "2025-08-17T09:30:00+09:00"
[assistant] "东京现在是 9 点 30 分。"
```

注意中间**三条消息缺一不可**：模型看到 `assistant(工具调用)` 才知道自己说过要调工具，
看到 `tool(结果)` 才知道结果是什么，最后才能组织回答。如果你只把结果"拼进提示词"，
很多模型会不认。

**为什么限制 10 轮？** 模型可能陷入死循环（一直调工具不回答）。10 轮是安全阀，
可通过 `agent.Options{MaxIterations: n}` 调整。

---

## 第 5 课：会话——agent 的记忆

`Session`（[`agent/session.go`](../agent/session.go)）就是一次对话的完整历史：

```go
session := agent.NewSession("default")  // 创建新会话
session.Add(llm.NewTextMessage(llm.RoleUser, "你好"))      // 加一条用户消息
session.Messages()                       // 读完整历史
session.Clear()                          // 清空历史
session.Trim(20)                         // 只保留最近 20 条
```

**为什么 agent 能"记得"上下文？** 因为每次 `Ask` 都把完整历史发给模型。
问一句"我叫小明"，再问一句"我叫什么？"——模型看到历史里的"我叫小明"，就能答上来。
删掉历史（`session.Clear()`），agent 就失忆了。

**动手练习**：在最小示例里连续问两次：

```go
ag.Ask(ctx, session, "我叫小明，请记住。")     // 第一问
answer, _ := ag.Ask(ctx, session, "我叫什么？") // 第二问：模型从历史里找到答案
```

---

## 第 6 课：钩子——给 agent 装日志和门禁

有时你想**观察** agent 在干什么（日志、遥测），或者**拦截**它的危险行为（权限控制）。
`agent.Hooks`（[`agent/hooks.go`](../agent/hooks.go)）提供 5 个钩子：

| 钩子 | 时机 | 典型用途 |
|------|------|---------|
| `OnMessage` | 每条 LLM 回复后 | 记录日志、统计 token |
| `OnBeforeToolCall` | 工具执行**前** | 权限拦截（返回 error 即阻止） |
| `OnAfterToolCall` | 工具执行后 | 记录工具结果、成功率 |
| `OnError` | 任一出错路径 | 告警、上报 |
| `OnFinish` | 成功返回回答后 | 收尾、计时 |

```go
ag := agent.New(agent.Options{
    Provider: provider,
    Registry: registry,
    Hooks: &agent.Hooks{
        OnMessage: func(ctx context.Context, m llm.Message) {
            log.Printf("[对话] role=%s 工具调用数=%d", m.Role, len(m.ToolCalls))
        },
        OnBeforeToolCall: func(ctx context.Context, name string, args map[string]any) error {
            if name == "dangerous_tool" {
                return errors.New("该工具已被禁用") // 返回错误 = 阻止执行
            }
            return nil
        },
        OnError: func(ctx context.Context, err error) {
            log.Printf("[错误] %v", err)
        },
    },
})
```

**拦截之后会发生什么？** 工具不会执行，错误文本会作为工具结果回传给模型，
模型看到"工具执行出错：该工具已被禁用"后，可以改用其他工具或直接回答——循环不会中断。

---

## 第 7 课：持久化——重启不丢记忆

默认情况下，会话只存在于内存：进程一退出，agent 就"失忆"了。
框架的 `SessionStore`（[`agent/store.go`](../agent/store.go)）解决这个问题——
**只需实现 3 个方法**（`Save` / `Load` / `Delete`），内置了内存和 JSON 文件两种实现。

```go
// 创建（或打开）一个 JSON 文件存储
store, _ := agent.NewJSONSessionStore("./sessions.json")

// 注入 Agent：每次 Ask 结束（成功或失败）自动保存
ag := agent.New(agent.Options{
    Provider: provider,
    Store:    store,
})

// ---- 下次启动时，恢复最近一次会话，继续上次对话 ----
prev, _ := store.LoadLatest(context.Background())
session := prev
if session == nil {
    session = agent.NewSession("default")
}
```

**动手练习**：跑两次最小示例（把 store 代码加进去），第一次问"我叫小明"，
退出重启后再问"我叫什么？"——agent 记得你。

想换 SQLite / Redis？自己实现 `SessionStore` 的 3 个方法即可，agent 循环一行不用改。

---

## 第 8 课：接入现成能力（知识库 / MCP）

### 8.1 知识库扩展：让 agent 能"查文档"

注册两个工具，agent 就获得了文档检索能力（这就是"文档问答 agent"的全部秘密）：

```go
import "github.com/capyflow/vortexagent/tools/knowledge"

kb := knowledge.NewKB([]string{"./docs"})           // 知识库根目录
for _, tool := range knowledge.NewKBTools(kb) {     // search_knowledge + read_document
    registry.Add(tool)
}
```

完整示例见 [`examples/knowledge-agent`](../examples/knowledge-agent/main.go)。

### 8.2 MCP 扩展：接入任意语言的现成工具

**MCP（Model Context Protocol）** 是工具生态的开放标准——别人写好的 MCP server
（文件系统、浏览器、数据库、Slack……几百个现成的），你的 agent 直接就能用：

```go
import "github.com/capyflow/vortexagent/tools/mcp"

// 以子进程方式拉起一个 MCP server（任意语言实现）
client, err := mcp.Connect(ctx, mcp.ServerConfig{
    Name:    "my-tools",
    Command: "go",
    Args:    []string{"run", "./examples/mcp-server"},
})
defer client.Close()

for _, tool := range client.Tools() {   // 自动发现并注册它的全部工具
    registry.Add(tool)
}
```

自己写一个 MCP server 也很简单——`examples/mcp-server` 是个 40 行的模板，
任何语言都能写，写完配置加一行即可接入。

---

## 第 9 课：读源码的路线图

学完上面的课，你已经会用框架了。想真正"懂"它，按这个顺序读源码
（每个文件都是单一职责，见 `agent/agent.go` 顶部的包文档）：

```
1. llm/protocol.go        全项目的"字母表"：Message / ToolCall / Provider 接口（30 分钟）
2. agent/agent.go         Options 与 New：一个 Agent 需要什么（10 分钟）
3. agent/ask.go           Ask 循环：agent 的心脏（30 分钟）★
4. agent/tool.go          工具接口与注册表（10 分钟）
5. agent/session.go       会话与回滚（10 分钟）
6. agent/hooks.go         钩子（10 分钟）
7. agent/store.go         会话存储（10 分钟）
8. llm/openai.go          一个厂商适配器：统一协议 → OpenAI 协议（1 小时，最难也最有收获）
9. tools/knowledge/       一个真实扩展工具的实现（30 分钟）
10. tools/mcp/client.go   MCP 客户端（1 小时）
11. cmd/vortex/main.go    最后看：怎么把框架组装成一个完整应用（10 分钟）
```

**三个心法：**

1. **Agent 的本质是循环，不是模型**——模型只负责生成文本，循环把"决策→执行→反馈"串起来。
2. **消息历史是 agent 的内存**——一切信息都要变成 `Message` 进历史，模型才能"看见"。
3. **接口是分层的粘合剂**——`Provider` 屏蔽厂商差异、`Tool` 屏蔽工具差异、
   `SessionStore` 屏蔽存储差异，上层永远只依赖接口。

---

## 常见坑与练习

### 常见坑

| 症状 | 原因 | 解法 |
|------|------|------|
| 报错"未找到 API 密钥" | 环境变量没设置或拼错 | `export OPENAI_API_KEY=...` 后重开终端 |
| 模型说"我没有这个工具" | 工具没注册进 Registry | `registry.Add(tool)` 后检查 `registry.Names()` |
| 模型总是不用你的工具 | 描述写得太含糊 | 把 `Description` 写成"什么时候该用它" |
| 模型反复调用同一个工具 | 工具返回的结果不足以回答 | 让工具返回更完整的信息，或换更强模型 |
| agent 一直不回答 | 工具循环死循环 | 设置更小的 `MaxIterations`，检查工具结果 |
| 工具被调用了但结果为空 | `Call` 返回了空字符串 | 空结果模型无法利用，返回有信息量的文本 |

### 练习清单（按难度）

- ⭐ 给时间工具加一个 `format` 参数（如 `unix` / `rfc3339`）
- ⭐ 写一个 `roll_dice` 工具（掷骰子），问 agent "掷两个骰子，点数之和是多少？"
- ⭐⭐ 用 `OnBeforeToolCall` 实现"每天只能调用某工具 5 次"的限流
- ⭐⭐ 把会话持久化换成你自己的 `SessionStore`（比如存到 SQLite）
- ⭐⭐⭐ 写一个"代码审查 agent"：注册 `read_file` 工具，提示词让它读代码并挑毛病
- ⭐⭐⭐ 给 `llm/` 加一个厂商适配器（比如 Moonshot / 本地 Ollama）

### 下一步

- 读完本教程，去看[架构参考](architecture.md)，理解每个设计决策背后的原因
- 看 `agent/e2e_test.go`：一个不依赖真实 API 的端到端测试，能帮你把整条链路串起来
- 动手改框架：加钩子、加存储、加厂商……框架就是拿来改的
