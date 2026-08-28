# Vortex — 通用 Agent 框架（Go）

用 Go 编写的**通用 agent 框架**：提供统一 LLM 接入、agent 循环、工具系统、生命周期钩子与会话存储，
可以用它构建任意领域的 agent——文档问答、代码助手、运维机器人……领域能力全部来自可插拔的
**工具（Tool）** 与提示词，框架本身与任何具体领域无关。

- **框架核心**：`llm/`（统一 LLM 协议与多厂商适配）、`agent/`（运行时：循环 / 工具 / 会话 / 钩子 / 存储）
- **内置扩展**：`tools/mcp/`（MCP 客户端，接入任意语言编写的自定义工具）、`tools/knowledge/`（本地文档知识库工具）、`tools/exec/` 与 `tools/filesystem/`（本机执行与文件读写，可经 CLI 的 `tools` 配置启用）
- **参考应用**：`cmd/vortex/`（通用 REPL CLI）、`examples/`（框架用法示例）

架构设计借鉴了 [earendil-works/pi](https://github.com/earendil-works/pi)（91k stars 的 TS 编码 agent 工具包）的分层思想。

> **想学 agent 开发？** 两条路：
> - 想快速了解与上手：[docs/usage.md](docs/usage.md) —— 项目介绍、优点、各功能使用说明
> - 初学者：从 [docs/tutorial.md](docs/tutorial.md) 开始 —— 10 节课动手搭起自己的 agent
> - 想理解设计：读 [docs/architecture.md](docs/architecture.md) —— 框架分层原理与设计决策
> - 文档总入口：[docs/README.md](docs/README.md)

## 快速开始

### 方式一：作为库使用（推荐）

你的应用直接 import 框架包，四步搭起一个 agent（完整代码见 `examples/minimal-agent`）：

```go
provider, _ := llm.NewProvider(llm.ProviderConfig{Name: "openai", APIKey: apiKey})

registry := agent.NewRegistry()
registry.Add(&myTool{})          // 实现 agent.Tool 接口即可

ag := agent.New(agent.Options{
    Provider: provider,
    Registry: registry,
    OnDelta:  func(d llm.Delta) { fmt.Print(d.Text) },
})

answer, _ := ag.Ask(ctx, agent.NewSession("default"), "你的问题")
```

### 方式二：运行参考 CLI

```bash
# 1. 配置 API 密钥（三选一，对应 vortex.json 中的 provider.name）
#    也可以把密钥写进项目根目录的 .env 文件（每行一条 KEY=VALUE，已存在的环境变量优先）
export OPENAI_API_KEY=sk-...        # OpenAI 兼容（DeepSeek/Qwen/智谱等）
# export ANTHROPIC_API_KEY=sk-ant-...
# export GEMINI_API_KEY=...

# 2. 复制配置模板并修改
cp vortex.json.example vortex.json

# 3. 运行（知识库、MCP 工具、会话持久化均为可选配置）
go run ./cmd/vortex
```

交互界面：直接输入问题回车，`/tools` 查看可用工具，`/clear` 清空历史，`/exit`（或 `/quit`）退出。

### 方式三：跑示例

```bash
export OPENAI_API_KEY=sk-...
go run ./examples/minimal-agent "现在几点了？"      # 最小 agent（内置时间工具）
go run ./examples/knowledge-agent "框架支持哪些厂商？" # 文档问答 agent（知识库扩展）
go run ./examples/customer-service-agent "数据线坏了要退款" # 智能客服 agent（子 agent 编排）
go run ./examples/async-agent "采购 50 台打印机做评估"    # 异步任务编排（并行分发 + 收取结果）
go run ./examples/mcp-server                        # MCP server 模板（自定义工具）
```

## 架构

```
cmd/vortex/        参考应用：通用 REPL CLI（配置驱动，组装框架全部能力）
examples/          框架用法示例（minimal / knowledge / customer-service / async / mcp-server）
├──────────────────────── 框架核心（你的应用依赖的就是这些包）────────────────────────
llm/               统一 LLM API
  protocol.go      统一消息/工具/流式协议
  openai.go        OpenAI 兼容层（DeepSeek/Qwen/智谱等）
  anthropic.go     Anthropic Messages API（含思考模式）
  gemini.go        Google Gemini API
agent/             Agent 运行时
  agent.go         Agent 本体与配置（Options / New / 默认提示词）
  ask.go           Ask 循环：LLM → 工具调用 → 执行 → 循环，直到最终回答
  tool.go          工具接口与注册表
  session.go       会话消息历史
  hooks.go         生命周期钩子（日志/遥测/权限拦截）
  store.go         会话存储抽象（内存 / JSON 文件实现）
├──────────────────────── 内置扩展（可选项，按需注册）────────────────────────
tools/mcp/         MCP 客户端：接入任意语言编写的 MCP server 工具
tools/knowledge/   本地文档知识库：目录扫描 + 全文关键词检索工具
tools/exec/        shell 命令执行工具（超时封顶、输出安全截断）
tools/filesystem/  文件读写工具组（路径限制在 root 内，防越权）
```

### Agent 循环（agent/ask.go）

```
用户提问 → 携带历史+工具声明调用 LLM
  → 模型返回工具调用？ → 执行工具（钩子可拦截）→ 结果回传 → 继续
  → 模型给出最终回答 → 返回
```

循环上限 10 轮（`Options.MaxIterations` 可配），避免死循环。

## 框架能力

| 能力 | 说明 |
|------|------|
| `llm.Provider` | 统一 LLM 接口，内置 openai（兼容）/ anthropic / gemini 三个适配器，换厂商只改一行配置 |
| `agent.New` + `Ask` | 核心循环：多轮工具调度、失败自动回滚历史、空回答防护 |
| `agent.Tool` + `Registry` | 结构化接口，实现 4 个方法即成为工具；重名保护、确定性工具声明排序 |
| `agent.Hooks` | 生命周期钩子：`OnMessage` / `OnLLMCall`（含 token 消耗与耗时）/ `OnBeforeToolCall`（可拦截）/ `OnAfterToolCall` / `OnError` / `OnFinish` |
| `agent.NewSubagentTool` | 子 agent 原语：把派生好的 Agent 包装成工具，父 agent 委派任务、只回收最终答复，多 agent 编排的基础 |
| `agent.TaskHub` | 异步任务编排：`task_start` 分发后台任务（goroutine + channel 并发）、主 agent 继续自己的工作，`task_status` / `task_wait` / `task_cancel` 管理任务 |
| `agent.WithJSONMode` | 结构化输出：要求本次回答只输出合法 JSON（OpenAI/Gemini 原生映射，Anthropic 系统提示约束） |
| Skill 系统 | `SKILL.md` 发现与加载；`allowed-tools`（工具白名单，双重生效）、`model`、`temperature` 元数据均生效 |
| `agent.SessionStore` | 会话持久化抽象，内置内存与 JSON 文件实现；接入 SQLite/Redis 只需实现 3 个方法 |
| `tools/mcp` | MCP 客户端：启动子进程 server、自动发现并注册工具 |
| `tools/knowledge` | 可选扩展：文档检索工具（`search_knowledge` / `read_document`，含路径越权防护） |

## 配置文件（vortex.json，仅参考 CLI 使用）

| 字段 | 说明 |
|------|------|
| `provider.name` | `openai` / `anthropic` / `gemini` |
| `provider.apiKeyEnv` | API 密钥环境变量名，空则按默认查找 |
| `provider.baseURL` | 自定义服务地址（OpenAI 兼容厂商填这里） |
| `provider.model` | 模型名称 |
| `provider.thinking` | 是否启用思考模式（如 DeepSeek R1 / Claude） |
| `knowledge` | 知识库根目录列表（可选，注册 search/read 工具） |
| `tools` | 内置工具开关（可选）：`tools.exec` 启用 shell 执行、`tools.filesystem` 启用文件读写（路径限制在 root 内），默认全关 |
| `mcpServers` | MCP server 列表，启动时自动连接并注册全部工具（可选） |
| `systemPrompt` | 自定义系统提示词 |
| `sessionFile` | 会话持久化文件（可选），非空时重启后自动继续上次对话 |

## 用框架构建你自己的 agent

1. **写一个工具**：实现 `agent.Tool` 的 `Name / Description / Schema / Call` 四个方法，注册进 `Registry`。
   工具是 agent 能力的唯一来源——框架不内置任何业务工具。
2. **接入自定义工具（MCP）**：任何语言实现一个 MCP server（见 `examples/mcp-server` 模板），
   或在 CLI 的 `mcpServers` 中声明，agent 自动发现并调用。Python/Node 生态的现成
   MCP server（filesystem、fetch 等）同样适用。
3. **加知识库（可选）**：注册 `knowledge.NewKBTools(kb)` 即可获得文档检索能力，
   不注册则 agent 与文档毫无关系。
4. **观察与拦截**：通过 `agent.Hooks` 记录每条消息、拦截危险工具调用（权限控制）、
   收集遥测、上报错误。
5. **持久化会话**：设置 `Options.Store`，每次 Ask 自动保存；用 `JSONSessionStore` 可
   实现"重启后继续上次对话"。
6. **换模型厂商**：`llm.NewProvider` 改 `Name` 即可，循环与工具完全不用改。
7. **多 agent 编排（子 agent）**：为不同专员场景各建一个 Agent（独立系统提示词与工具箱），
   用 `agent.NewSubagentTool`（同步委派，当轮拿结果）或 `agent.TaskHub`（异步分发，
   主 agent 继续自己的工作、稍后收结果）注册进总机 agent 的 Registry——智能客服、
   编码 agent 的"总-分"结构由此衍生（见 `examples/customer-service-agent` 与
   `examples/async-agent`）。

## 开发

```bash
go build ./...            # 编译
go test ./...             # 全部测试（含端到端集成测试，不依赖真实 API）
go run ./cmd/vortex       # 本地运行参考 CLI
```

### 测试策略

- `llm/*_test.go`：httptest mock 各厂商 API，验证请求体序列化与响应解析
- `agent/loop_test.go`：fake provider 验证工具循环、回滚、空回答等逻辑
- `agent/hooks_test.go`：钩子触发顺序与拦截行为
- `agent/store_test.go`：会话存储存取、快照语义、JSON 落盘重载
- `agent/e2e_test.go`：全链路端到端（mock OpenAI + 知识库扩展 + 工具循环）
- `tools/mcp/client_test.go`：真实拉起子进程 MCP server 验证协议握手

## 路线图

- [ ] 上下文压缩 compaction（历史超长时摘要，对应 pi 的 agent-harness）
- [ ] 向量检索扩展（替换 knowledge 内部实现，或作为新扩展）
- [ ] 多会话管理与分支
- [ ] RPC / Server 模式（进程集成）
- [ ] 更多内置扩展（shell、http、filesystem……）

## License

MIT
