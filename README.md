# Vortex - 智能文档 Agent

基于 Go 的智能文档助手，可以自动查询本地知识库回答问题，并支持通过 **MCP（Model Context Protocol）** 接入用户自定义工具。

架构设计借鉴了 [earendil-works/pi](https://github.com/earendil-works/pi)（91k stars 的 TS 编码 agent 工具包）的分层思想：统一 LLM API 层、Agent 运行时层、可插拔工具层。

> **想学 agent 开发？** 阅读 [docs/architecture.md](docs/architecture.md) —— 面向初学者的架构指南，从"什么是 agent"讲到每一层代码怎么工作，附带读代码顺序和扩展练习。

## 快速开始

```bash
# 1. 配置 API 密钥（三选一，对应 vortex.json 中的 provider.name）
export OPENAI_API_KEY=sk-...        # OpenAI 兼容（DeepSeek/Qwen/智谱等）
# export ANTHROPIC_API_KEY=sk-ant-...
# export GEMINI_API_KEY=...

# 2. 复制配置模板并修改
cp vortex.json.example vortex.json

# 3. 准备知识库目录（vortex.json 的 knowledge 字段指向文档目录）
mkdir -p docs && echo "Vortex 是一个智能文档助手" > docs/intro.md

# 4. 运行
go run ./cmd/vortex
```

交互界面：直接输入问题回车，`/tools` 查看可用工具，`/clear` 清空历史，`/exit` 退出。

## 架构

```
cmd/vortex/        CLI 入口（REPL，简单输入输出）
llm/               统一 LLM API（对应 pi 的 packages/ai）
  protocol.go      统一消息/工具/流式协议
  openai.go        OpenAI 兼容层（DeepSeek/Qwen/智谱等）
  anthropic.go     Anthropic Messages API（含思考模式）
  gemini.go        Google Gemini API
agent/             Agent 运行时（对应 pi 的 packages/agent）
  loop.go          Agent 循环：LLM → 工具调用 → 执行 → 循环，直到最终回答
  session.go       会话消息历史
  tool.go          工具接口与注册表
knowledge/         知识库：目录扫描 + 大小写不敏感全文检索
tools/mcp/         MCP 客户端（对应 pi 的 extensions，但用 MCP 标准）
examples/          示例 MCP server（自定义工具模板）
```

### Agent 循环（agent/loop.go）

```
用户提问 → 携带历史+工具声明调用 LLM
  → 模型返回工具调用？ → 执行工具（知识库检索 / MCP 工具）→ 结果回传 → 继续
  → 模型给出最终回答 → 返回
```

循环上限 10 轮（`Options.MaxIterations` 可配），避免死循环。

## 配置文件（vortex.json）

| 字段 | 说明 |
|------|------|
| `provider.name` | `openai` / `anthropic` / `gemini` |
| `provider.apiKeyEnv` | API 密钥环境变量名，空则按默认查找 |
| `provider.baseURL` | 自定义服务地址（OpenAI 兼容厂商填这里） |
| `provider.model` | 模型名称 |
| `provider.thinking` | 是否启用思考模式（如 DeepSeek R1 / Claude） |
| `knowledge` | 知识库根目录列表 |
| `mcpServers` | MCP server 列表，启动时自动连接并注册全部工具 |
| `systemPrompt` | 自定义系统提示词 |

## 添加自定义工具（MCP）

**任何语言**实现一个 MCP server，在 `mcpServers` 中声明即可，agent 会自动发现并调用。

以 Go 为例，`examples/mcp-server/main.go` 是一个最小模板：

```go
srv := server.NewMCPServer("my-tools", "0.1.0")

srv.AddTool(
    mcp.NewTool("get_time",
        mcp.WithDescription("获取当前时间"),
        mcp.WithString("timezone", mcp.Description("IANA 时区名")),
    ),
    func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
        return mcp.NewToolResultText(time.Now().Format(time.RFC3339)), nil
    },
)

server.ServeStdio(srv)
```

配置：

```json
{
  "mcpServers": [
    {"name": "my-tools", "command": "go", "args": ["run", "./examples/mcp-server"]}
  ]
}
```

Python / Node 生态的现成 MCP server（如官方 filesystem、fetch 等）同样适用，只要 `command` 换成对应的启动命令。

## 内置工具

| 工具 | 说明 |
|------|------|
| `search_knowledge` | 在知识库目录中按关键词检索，返回文件路径、行号、匹配行与上下文 |
| `read_document` | 读取知识库内指定文档全文（限制在知识库目录内，防越权读取） |

## 开发

```bash
go build ./...            # 编译
go test ./...             # 全部测试（含端到端集成测试，不依赖真实 API）
go run ./cmd/vortex       # 本地运行
```

### 测试策略

- `llm/*_test.go`：httptest mock 各厂商 API，验证请求体序列化与响应解析
- `agent/loop_test.go`：fake provider 验证工具循环逻辑
- `agent/e2e_test.go`：全链路端到端（mock OpenAI + 知识库 + 工具循环）
- `tools/mcp/client_test.go`：真实拉起子进程 MCP server 验证协议握手

## 路线图（借鉴 pi 的演进方向）

- [ ] 会话持久化（SQLite / JSON 文件，对应 pi 的 session-backends）
- [ ] 上下文压缩 compaction（对应 pi 的 agent-harness）
- [ ] 知识库升级为索引化检索（bleve）或向量检索
- [ ] 多会话与分支
- [ ] RPC 模式（进程集成）

## License

MIT
