[English](README.md) | **简体中文**

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

# 2. 运行（首次启动进入配置向导；-group 决定数据目录 ~/.vortex/my-agent/）
go run ./cmd/vortex -group my-agent
#    不想用 group 的话，也可以显式指定配置：go run ./cmd/vortex -config 路径/agent.json
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
cmd/vortex-serve/  参考应用：HTTP 服务模式（SSE 流式接口 + webhook，可挂载自治 agent）
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
autonomous/        自治 agent：目标存储与调度（cron / 间隔 / 一次性），配合 vortex-serve 运行
config/            配置文件 schema 与加载（agent group 数据隔离：~/.vortex/<group>/）
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
| `agent.Checker` | 全局工具权限：执行模式（full_access / confirm / whitelist）+ allow/deny 规则 + 交互确认 UI；所有工具（含 MCP）在 Ask 循环统一拦截，deny 在任何模式下都生效 |
| `agent.NewSubagentTool` | 子 agent 原语：把派生好的 Agent 包装成工具，父 agent 委派任务、只回收最终答复，多 agent 编排的基础 |
| `agent.TaskHub` | 异步任务编排：`task_start` 分发后台任务（goroutine + channel 并发）、主 agent 继续自己的工作，`task_status` / `task_wait` / `task_cancel` 管理任务 |
| `agent.WithJSONMode` | 结构化输出：要求本次回答只输出合法 JSON（OpenAI/Gemini 原生映射，Anthropic 系统提示约束） |
| Skill 系统 | `SKILL.md` 发现与加载；`allowed-tools`（工具白名单，双重生效）、`model`、`temperature` 元数据均生效 |
| `agent.SessionStore` | 会话持久化抽象，内置内存与 JSON 文件实现；接入 SQLite/Redis 只需实现 3 个方法 |
| `tools/mcp` | MCP 客户端：启动子进程 server、自动发现并注册工具 |
| `tools/exec` | 内置 shell 工具（exec_command）：超时与输出截断防护；自描述权限匹配器（`PermissionMatcherProvider`），`registry.Add` 后带参数模式的权限规则即按 shell 语义生效 |
| `tools/knowledge` | 可选扩展：文档检索工具（`search_knowledge` / `read_document`，含路径越权防护） |

## 配置文件（仅参考 CLI 使用）

推荐用 **agent group** 管理配置与数据隔离：group 决定一组独立的本地数据目录
`~/.vortex/<group>/`，配置、会话、长期记忆、自治目标、上下文卸载文件都默认落在其下：

```
~/.vortex/<group>/agent.json      配置文件
~/.vortex/<group>/sessions.json   会话存储（session.type=json 且 file 留空时）
~/.vortex/<group>/memory/         长期记忆（tools.memory.dir 留空时）
~/.vortex/<group>/goals.json      自治目标（autonomous.goal_store.file 留空时）
~/.vortex/<group>/offload/        上下文卸载归档（下游项目接 FileSystemOffload 时）
```

配置来源优先级：`-config` 显式路径 > group（运行时 `-group` 或构建时烧录）> 烧录 `DefaultPath`：

```bash
# 运行时指定 group
go run ./cmd/vortex -group my-agent
go run ./cmd/vortex-serve -group my-agent -addr :8080

# 构建时烧录 group 名，运行后无需任何参数
# 烧录目标是框架 config 包的 DefaultGroup——下游项目引入框架后，用自己的构建命令
# 烧录同一个变量即可；代码里也可直接读 config.DefaultGroup 作默认值
go build -ldflags "-X github.com/capyflow/vortexagent/config.DefaultGroup=my-agent" -o my-agent ./cmd/vortex
./my-agent
```

配置文件里**留空的路径字段会自动解析进 group 目录**；显式指定的路径保持原样（可用于
有意跨 agent 共享数据等场景）。group 和路径都未指定时启动报错。

| 字段 | 说明 |
|------|------|
| `provider.name` | `openai` / `anthropic` / `gemini` |
| `provider.apiKeyEnv` | API 密钥环境变量名，空则按默认查找 |
| `provider.baseURL` | 自定义服务地址（OpenAI 兼容厂商填这里） |
| `provider.model` | 模型名称 |
| `provider.thinking` | 是否启用思考模式（如 DeepSeek R1 / Claude） |
| `knowledge` | 知识库根目录列表（可选，注册 search/read 工具） |
| `tools` | 内置工具开关（可选）：`tools.exec` 启用 shell 执行、`tools.filesystem` 启用文件读写（路径限制在 root 内）、`tools.memory` 启用长期记忆，默认全关 |
| `permissions` | 全局工具权限（可选）：执行模式 + allow/deny 规则，见下文「工具权限与确认」 |
| `mcpServers` | MCP server 列表，启动时自动连接并注册全部工具（可选） |
| `systemPrompt` | 自定义系统提示词 |
| `session` | 会话持久化（可选）：`session.type` 为 `memory`（默认）/ `json` / `postgres`，`session.file` 为 JSON 存储路径，启用后重启可继续上次对话 |

### 一机多 Agent 部署

一台机器跑多个 agent 时，每个 agent 一个独立 group——配置与会话/记忆/目标/卸载文件
天然隔离在各自的 `~/.vortex/<group>/` 下，无需逐项配置路径（JSON 会话存储是整文件
覆盖写，共用路径会互相丢数据，group 模式从布局上杜绝了这一点）：

```bash
vortex -group agent-a -addr :8081
vortex -group agent-b -addr :8082
```

也可以给每个 agent 单独构建一个烧录了 group 名的二进制（见上）。仅当需要**有意共享**
某类数据（如多个 agent 共用一份记忆）时，才在配置文件里写显式路径。`vortex-serve`
多实例注意端口错开；`.env` 按进程工作目录加载，不同 agent 建议各用独立的工作目录。

## 工具权限与确认

所有工具（内置与 MCP）在每次执行前经过全局权限检查（`agent.Checker`，在 Ask 循环的
工具分发点统一拦截），按执行模式决定"没命中规则时问不问"：

| 模式 | 行为 |
|------|------|
| `full_access` | 全部放行，不询问（deny 规则仍然拦截） |
| `confirm`（默认） | 放行规则未命中的调用询问用户：y 允许 / n 拒绝 / a 本会话总是允许 |
| `whitelist` | 仅 allow 规则命中的调用可执行，其余直接拒绝 |

```json
"permissions": {
  "mode": "confirm",
  "allow": ["read_file", "list_files", "exec_command:git status*", "mcp__github"],
  "deny": ["exec_command:rm -rf*", "exec_command:mkfs*"]
}
```

- **规则格式**：`工具名` 或 `工具名:参数模式`（按第一个冒号切分），工具名支持尾缀 `*` 通配。
  MCP 工具名带命名空间：`mcp__<server>` 覆盖整个 server，`mcp__<server>__<tool>` 单个工具
- **exec_command 的参数模式按 shell 命令理解**，宽严刻意不同：allow 从严——复合命令
  （`;` `&&` `||` `|`）要求每一段都命中，`git status*` 放行不了 `git status; rm -rf /`；
  deny 从宽——对整条命令做词边界扫描，藏在命令替换里的 `echo $(rm -rf /)` 也拦得住，
  而无关词（如 `format` 之于 `rm`）不误伤
- **优先级**：本会话记住的允许 > deny > allow > 模式；deny 在任何模式（含 full_access）下生效
- REPL 里 `/mode` 查看/切换执行模式、`/permissions` 查看规则
- 无交互环境（vortex-serve）中 confirm 的问询自动降级为拒绝；需要自动执行的调用请配置
  allow 规则、whitelist 模式或 full_access
- 这是字符串级过滤，防模型误用；防不了蓄意构造的绕过，高危环境请配合操作系统级沙箱

完整的功能描述与接入指南（库方式接入、自定义匹配器/确认 UI/检查器）见 [docs/permissions.md](docs/permissions.md)。

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
8. **复用配置文件加载（可选）**：`config` 包提供与参考 CLI 同源的配置 schema，
   `config.LoadConfig(path)` 直接解析为 `config.Config`（provider / session / tools /
   autonomous 等），配套 `config.ExpandPath`、`config.LoadDotEnv`、`config.GoalFromConfig`。
   数据隔离用 group：构建时烧录 `config.DefaultGroup`，运行时经 `config.ResolveConfigPath`
   定位 `~/.vortex/<group>/agent.json`，再由 `cfg.ResolveGroupDefaults(group)` 把留空的
   会话/记忆/目标/卸载路径解析进 group 目录。

## 开发

```bash
go build ./...            # 编译
go test ./...             # 全部测试（含端到端集成测试，不依赖真实 API）
go run ./cmd/vortex -config vortex/deploy_agent/my-agent.json   # 本地运行参考 CLI
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
