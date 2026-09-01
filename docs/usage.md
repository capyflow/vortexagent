# VortexAgent 使用说明

> 一份面向使用者的完整介绍：这个项目是什么、它好在哪、以及每个功能怎么用。
> 想理解设计原理请读 [architecture.md](architecture.md)，想从零学会请读 [tutorial.md](tutorial.md)。

---

## 一、VortexAgent 是什么

VortexAgent 是一个用 **Go** 编写的**通用 Agent 框架**。

它提供的就是一个 agent 的全部发动机：**统一的多厂商 LLM 接入 + 工具循环 + 会话管理 + 生命周期钩子 + 持久化**。框架本身与任何业务领域无关——你注入什么工具、写什么提示词，它就是什么 agent：

| 你注入的东西 | 得到的 agent |
|--------------|--------------|
| 知识库检索工具 | 文档问答机器人 |
| 订单/退款工具 + 子 agent 编排 | 智能客服系统 |
| shell 与文件读写工具 | 本地编码助手 |
| MCP 生态的任意工具 | 接入数百个现成工具的通用助手 |

一个 agent 的本质只有一句话：**LLM 负责决策，工具负责执行，循环把两者串起来**。VortexAgent 把这个循环做对、做稳，把其余一切都做成可插拔的扩展点。

```
用户提问 ──▶ Agent 循环 ──▶ 调用 LLM（OpenAI / Anthropic / Gemini / 兼容厂商）
                │               │ 返回工具调用？
                │               ▼
                │           执行工具（钩子可拦截、panic 有防护、可并行）
                │               │ 结果回传
                └───────────────┘
                    直到给出最终回答（轮数上限兜底）
```

---

## 二、它有什么优点

### 1. 换厂商只改一行配置
OpenAI 兼容（DeepSeek / Qwen / 智谱 / Moonshot）、Anthropic、Gemini 三套协议适配完毕，业务代码只依赖统一的 `llm.Provider` 接口。换模型 = 改一个字符串，循环与工具零改动。

### 2. 两种多 agent 编排原语，覆盖"总-分"结构
- **同步委派**（`SubagentTool`）：把派生好的 Agent 包装成一个工具，父 agent 委派任务、只回收最终答复，中间过程不污染父上下文。
- **异步编排**（`TaskHub`）：`task_start` 分发后台任务后主 agent 立即继续自己的工作，子 agent 在 goroutine 里并行执行，`task_wait` 一次性收结果——并行调研多项再汇总，时间直接减半。

智能客服、编码 agent 的"总机 + 专员"结构由此衍生，且有嵌套深度上限防递归失控。

### 3. 安全边界是内建的，不是口头约定
- filesystem 工具三重路径校验（绝对路径不放行 / `filepath.Rel` 判越界 / `EvalSymlinks` 防符号链接逃逸），prompt injection 读不了 root 外的文件；
- 任意工具 panic 都会被框架 recover 转为错误文本回传模型，一个有 bug 的第三方工具打不崩进程；
- 权限钩子 `OnBeforeToolCall` 可按工具名 + 参数拦截任何调用（含对子 agent 的委派）。

### 4. 可观测性开箱即用
`OnLLMCall` 钩子透出每次 LLM 调用的**模型名、轮次、耗时、token 消耗（含缓存命中）、错误**——按客户、按会话核算成本只需几行代码。工具级钩子（`OnBefore/AfterToolCall`）与消息钩子覆盖其余观测面。

### 5. 会话与上下文管理是完整的
- 存储：内存 / JSON 文件 / PostgreSQL（含跨设备会话锁），重启续聊、多会话切换开箱可用；
- 上下文卸载（compaction）：历史超长时自动摘要归档，token 估算对**中文按 1 字 1 token** 校准，不会在中文场景下过早撑爆窗口；
- 失败自动回滚：任何一步出错，半截工具轮次不会残留在会话里污染下一次提问。

### 6. 结构化输出 + Skill 系统
`WithJSONMode()` 一行开启 JSON 模式（意图分类、工单字段抽取的基座）；SKILL.md 技能的 `allowed-tools` 白名单双重生效（模型看不到 + 执行前拦截）、`model` / `temperature` 按技能覆盖。

### 7. 部署形态齐全
库（import 即用）、REPL CLI（配置驱动）、HTTP + SSE 服务（`/chat`、`/chat/stream`，会话持久化、按会话串行化）三种形态，从本地玩具到线上服务是同一套代码。

### 8. 纯 Go、依赖极少、测试扎实
纯 Go 实现，第三方依赖只有三个且各司其职（YAML 解析、Postgres 驱动、MCP 协议库）；
全仓 `go vet` 干净，测试全部通过且含 `-race` 并发检测，示例均可运行。

---

## 三、快速开始

### 3.1 作为库使用（推荐）

```go
package main

import (
    "context"
    "fmt"
    "os"

    "github.com/capyflow/vortexagent/agent"
    "github.com/capyflow/vortexagent/agent/sessionstore"
    "github.com/capyflow/vortexagent/llm"
)

func main() {
    // 1. LLM provider（换厂商只改 Name）
    provider, _ := llm.NewProvider(llm.ProviderConfig{
        Name:   "openai", // openai（含兼容厂商）/ anthropic / gemini
        APIKey: os.Getenv("OPENAI_API_KEY"),
    })

    // 2. 工具注册表：实现 agent.Tool 接口即可
    registry := agent.NewRegistry()
    registry.Add(&myTool{})

    // 3. 创建 Agent
    ag := agent.New(agent.Options{
        Provider: provider,
        Registry: registry,
        OnDelta:  func(d llm.Delta) { fmt.Print(d.Text) }, // 打字机效果
    })

    // 4. 提问
    session := sessionstore.NewSession("default")
    answer, err := ag.Ask(context.Background(), session, "你的问题")
}
```

### 3.2 运行参考 CLI

```bash
export OPENAI_API_KEY=sk-...        # 或 ANTHROPIC_API_KEY / GEMINI_API_KEY
cp vortex.json.example vortex.json   # 修改 provider 与模型配置
go run ./cmd/vortex
```

REPL 内置命令：`/help` 帮助、`/tools` 工具列表、`/clear` 清空历史、
`/sessions` `/new` `/switch ID` `/delete ID` 会话管理、`/exit` 退出。

### 3.3 跑示例

```bash
go run ./examples/minimal-agent "现在几点了？"              # 最小 agent
go run ./examples/knowledge-agent "框架支持哪些厂商？"       # 文档问答
go run ./examples/customer-service-agent "数据线坏了要退款"  # 智能客服（同步多 agent）
go run ./examples/async-agent "采购 50 台打印机做评估"       # 异步编排（并行调研）
```

---

## 四、功能使用说明

### 4.1 工具：agent 能力的唯一来源

实现 4 个方法即成为一个工具（Go 接口是结构化的，无需 import 框架）：

```go
type WeatherTool struct{}

func (t *WeatherTool) Name() string        { return "get_weather" }
func (t *WeatherTool) Description() string { return "查询指定城市的实时天气" }
func (t *WeatherTool) Schema() map[string]any {
    return map[string]any{
        "type": "object",
        "properties": map[string]any{
            "city": map[string]any{"type": "string", "description": "城市名"},
        },
        "required": []string{"city"},
    }
}
func (t *WeatherTool) Call(ctx context.Context, args map[string]any) (string, error) {
    city, _ := args["city"].(string)
    return city + "：晴，28°C", nil
}
```

> 提示：`Description` 写得越清楚，模型用得越准。工具执行出错不会中断循环——
> 错误会作为结果文本回传，模型会自己调整。

### 4.2 内置工具与 MCP

`vortex.json` 按需启用，全部默认关闭：

```json
{
  "tools": {
    "exec":       { "enabled": true, "workdir": "." },
    "filesystem": { "enabled": true, "root": "./project" },
    "memory":     { "enabled": true, "dir": "~/.vortex/memory" }
  },
  "knowledge": ["./docs"],
  "mcpServers": [
    { "name": "fetch", "command": "uvx", "args": ["mcp-server-fetch"] }
  ]
}
```

- `exec`：shell 命令执行（超时封顶 10 分钟、输出安全截断）。**建议配合权限钩子做命令白名单**；
- `filesystem`：read/write/edit/list 四件套，路径被限制在 `root` 内；
- `memory`：长期记忆六件套（`memory_save` / `memory_update` / `memory_delete` / `memory_get` / `memory_search` / `memory_list`），agent 运行中自己积累的跨会话事实记忆（偏好 / 决策 / 教训），JSON 文件持久化，与单会话历史相互独立；
- `knowledge`：本地文档检索（`search_knowledge` / `read_document`）；
- `mcpServers`：任意语言编写的 MCP server，工具自动发现注册，Python/Node 生态的现成 server 直接可用。

### 4.3 多 agent：同步委派（SubagentTool）

适合"先问专员、拿到答复再继续"的场景。每个专员 = 独立提示词 + 独立工具箱：

```go
aftersales := agent.New(agent.Options{
    Provider:     provider,
    Registry:     售后工具箱,
    SystemPrompt: "你是售后专员，处理退款前先查订单……",
})

registry.Add(agent.NewSubagentTool("aftersales", "处理退款/换货等售后问题", aftersales))
```

父 agent 把它当普通工具调用；每次委派使用全新会话（上下文隔离），父级权限钩子原样生效。

### 4.4 多 agent：异步编排（TaskHub）

适合"并行调研多项、最后汇总"或"启动长任务、稍后收结果"：

```go
hub := agent.NewTaskHub(appCtx, 2) // 应用级 ctx；并发上限 2
hub.Register("pricing", pricingAgent)
hub.Register("stock", stockAgent)
for _, t := range hub.Tools() {
    registry.Add(t) // task_start / task_status / task_wait / task_cancel
}
```

主 agent 的行为流：`task_start` 并行分发（`ParallelTools: true` 时同一轮真正并发）
→ **继续做自己的事** → `task_wait` 收取 → 汇总回答。任务挂在应用级 ctx 上，
本轮对话结束任务继续跑，下轮还能收；应用退出前用 `hub.WaitAll(ctx)` 优雅收尾。

> 选型经验：**先查再答用 SubagentTool（同步），并行/长任务用 TaskHub（异步）**，两者可混用。

### 4.5 结构化输出

```go
answer, err := ag.Ask(ctx, session, "把这句话分类：'我要退款'", agent.WithJSONMode())
// answer 为合法 JSON，如 {"intent":"refund","order_id":null}
```

OpenAI / Gemini 走原生 JSON 模式；Anthropic 通过系统提示约束（协议无统一字段）。解析失败建议重试或降级。

### 4.6 Skill 系统

`.vortex/skills/<name>/SKILL.md`（YAML frontmatter + Markdown 正文）：

```markdown
---
name: code-review
description: "按团队规范审查代码"
allowed-tools: ["read_file", "list_files"]
model: "deepseek-chat"
temperature: 0.2
---

审查代码时遵循：安全 > 可读 > 性能……
```

```go
mgr := agent.NewSkillManager(".")
mgr.Discover()
skill, _ := mgr.Get("code-review")
answer, err := ag.Ask(ctx, session, input, agent.WithSkill(skill))
```

`allowed-tools` 双重生效：模型只看到白名单内的工具声明，越权调用在执行前被拦截。

### 4.7 钩子：观测、审计与权限

```go
ag := agent.New(agent.Options{
    Hooks: &agent.Hooks{
        // 每轮 LLM 调用：成本统计与延迟监控
        OnLLMCall: func(ctx context.Context, info agent.LLMCallInfo) {
            log.Printf("model=%s round=%d tokens=%d+%d cost=%v err=%v",
                info.Model, info.Round,
                info.Usage.InputTokens, info.Usage.OutputTokens,
                info.Duration, info.Err)
        },
        // 权限拦截：返回错误则工具不执行，原因回传模型
        OnBeforeToolCall: func(ctx context.Context, name string, args map[string]any) error {
            if name == "exec_command" {
                cmd, _ := args["command"].(string)
                if !allowed(cmd) {
                    return fmt.Errorf("命令不在白名单内")
                }
            }
            return nil
        },
        OnFinish: func(ctx context.Context, answer string) { /* 落库/告警 */ },
        OnError:  func(ctx context.Context, err error)     { /* 上报 */ },
    },
})
```

### 4.8 会话持久化

```go
// JSON 文件：重启后继续上次对话
store, _ := sessionstore.NewJSON("~/.vortex/sessions.json")
prev, _ := store.LoadLatest(ctx) // 恢复最近会话
if prev != nil { session = prev }

ag := agent.New(agent.Options{ ..., Store: store }) // 每次 Ask 自动保存
```

PostgreSQL 存储含跨设备会话锁（`sessionstore.NewPostgres`），适合多端场景。
接入其他存储只需实现 `Save / Load / Delete / List` 四个方法。

### 4.9 HTTP / SSE 服务模式

```go
srv := server.New(server.Config{
    Addr:  ":8080",
    Agent: ag,
    Store: store, // 可选：会话持久化，重启后按 session_id 恢复
})
srv.Start()
```

| 接口 | 说明 |
|------|------|
| `POST /chat` | `{session_id, message}` → 同步回答 |
| `POST /chat/stream` | SSE 流式：`delta`（增量）→ `done`（完整回答）/ `error` |
| `GET /sessions` / `POST /sessions` / `DELETE /sessions/{id}` | 会话管理 |
| `GET /health` | 健康检查 |

同一会话的并发请求自动串行化（避免历史交错污染），不同会话互不影响。

---

## 五、典型场景配方

### 智能客服（总-分结构）
总机 agent（提示词：识别意图、委派、友好汇总）+ `SubagentTool` 挂售前/售后专员
（各自业务工具）+ `OnLLMCall` 做按会话成本核算 + `Store` 落库审计。意图分类可用
`WithJSONMode()` 先行结构化。参考 `examples/customer-service-agent`。

### 本地编码助手
CLI 配置启用 `tools.filesystem` + `tools.exec`，`OnBeforeToolCall` 做 exec 命令
白名单，路径边界已由框架保证；用 TaskHub 把"全库检索调研"分发成后台任务，
主 agent 边等边整理。参考 `examples/async-agent`。

### 文档问答
注册 `knowledge.NewKBTools`，零其他配置。参考 `examples/knowledge-agent`。

---

## 六、注意事项

1. **TaskHub 的 ctx 必须是应用级的**：传入某次 HTTP 请求的 ctx 会在请求结束时误杀后台任务；
2. **exec 工具默认无命令限制**：风险自担，务必配权限钩子做白名单；
3. **工具描述即接口文档**：模型据此决定是否调用、怎么传参，含糊的描述是错误调用的第一原因；
4. **轮数上限**：Ask 默认最多 10 轮（`MaxIterations` 可调），防模型死循环烧钱；
5. **并行钩子**：开启 `ParallelTools` 时钩子可能被并发调用，实现里别用非线程安全的共享状态。

---

## 七、下一步

- 想**动手学**：[tutorial.md](tutorial.md)（10 节课从零搭 agent）
- 想**懂原理**：[architecture.md](architecture.md)（分层、设计决策、踩坑记录）
- 想**速查 API**：[guide.md](guide.md)（开发指南）
- 想**看能力全景**：[../README.md](../README.md)
