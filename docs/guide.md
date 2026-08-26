# VortexAgent 开发指南

## 目录

- [简介](#简介)
- [架构概览](#架构概览)
- [快速开始](#快速开始)
- [核心概念](#核心概念)
- [工具系统](#工具系统)
- [Skill 系统](#skill-系统)
- [HTTP Server](#http-server)
- [高级功能](#高级功能)
- [API 参考](#api-参考)
- [示例项目](#示例项目)

---

## 简介

VortexAgent 是一个用 Go 编写的通用 Agent 框架，提供：

- **统一 LLM 接口**：支持 OpenAI、Anthropic、Gemini
- **工具系统**：可插拔的工具注册表
- **会话管理**：多会话支持、历史持久化
- **流式输出**：实时响应
- **Skill 系统**：按需注入技能提示词
- **HTTP Server**：RESTful API 部署

## 架构概览

```
vortexagent/
├── agent/              # 核心运行时
│   ├── agent.go        # Agent 本体与配置
│   ├── ask.go          # 核心循环：LLM ↔ 工具
│   ├── tool.go         # 工具接口与注册表
│   ├── skill.go        # Skill 系统
│   ├── hooks.go        # 生命周期钩子
│   └── sessionstore/   # 会话存储
├── llm/                # LLM 统一接口
│   ├── protocol.go     # 统一协议
│   ├── openai.go       # OpenAI 适配器
│   ├── anthropic.go    # Anthropic 适配器
│   └── gemini.go       # Gemini 适配器
├── server/             # HTTP Server
├── tools/              # 内置工具
│   ├── filesystem/     # 文件系统工具
│   ├── exec/           # 命令执行工具
│   ├── knowledge/      # 知识库工具
│   └── mcp/            # MCP 客户端
├── skills/             # 内置 Skills
└── cmd/                # 命令行入口
```

## 快速开始

### 1. 安装

```bash
go get github.com/capyflow/vortexagent
```

### 2. 最小示例

```go
package main

import (
    "context"
    "fmt"
    "os"

    "github.com/capyflow/vortexagent/agent"
    "github.com/capyflow/vortexagent/llm"
)

func main() {
    // 1. 创建 Provider
    provider, _ := llm.NewProvider(llm.ProviderConfig{
        Name:   "openai",
        APIKey: os.Getenv("OPENAI_API_KEY"),
    })

    // 2. 创建 Agent
    ag := agent.New(agent.Options{
        Provider: provider,
        OnDelta: func(d llm.Delta) {
            fmt.Print(d.Text)
        },
    })

    // 3. 提问
    session := sessionstore.NewSession("default")
    answer, _ := ag.Ask(context.Background(), session, "你好")
    fmt.Println(answer)
}
```

## 核心概念

### Agent

Agent 是框架核心，负责 LLM 与工具的循环调度：

```go
ag := agent.New(agent.Options{
    Provider:       provider,        // LLM Provider
    Registry:       registry,        // 工具注册表
    Model:          "gpt-4",        // 模型名称
    SystemPrompt:   "你是一个助手",  // 系统提示词
    MaxIterations:  10,             // 最大工具循环轮数
    MaxRetries:     3,              // 工具重试次数
    ParallelTools:  true,           // 并行工具调用
})
```

### Session

Session 管理对话历史：

```go
session := sessionstore.NewSession("gpt-4")

// 添加消息
session.Add(llm.NewTextMessage(llm.RoleUser, "你好"))

// 获取历史
msgs := session.Messages()

// 回滚
session.Rollback(0) // 清空
```

### Provider

统一的 LLM 接口：

```go
// OpenAI 兼容（DeepSeek/Qwen/智谱等）
provider, _ := llm.NewProvider(llm.ProviderConfig{
    Name:    "openai",
    APIKey:  apiKey,
    BaseURL: "https://api.deepseek.com",
})

// Anthropic
provider, _ := llm.NewProvider(llm.ProviderConfig{
    Name:   "anthropic",
    APIKey: apiKey,
})

// Gemini
provider, _ := llm.NewProvider(llm.ProviderConfig{
    Name:   "gemini",
    APIKey: apiKey,
})
```

## 工具系统

### 实现工具

实现 `Tool` 接口即可创建工具：

```go
type WeatherTool struct{}

func (t *WeatherTool) Name() string { return "weather" }

func (t *WeatherTool) Description() string {
    return "查询指定城市的天气"
}

func (t *WeatherTool) Schema() map[string]any {
    return map[string]any{
        "type": "object",
        "properties": map[string]any{
            "city": map[string]any{
                "type":        "string",
                "description": "城市名",
            },
        },
        "required": []string{"city"},
    }
}

func (t *WeatherTool) Call(ctx context.Context, args map[string]any) (string, error) {
    city := args["city"].(string)
    return fmt.Sprintf("%s: 晴天 25°C", city), nil
}
```

### 注册工具

```go
registry := agent.NewRegistry()
registry.Add(&WeatherTool{})
```

### 渐进式披露

实现 `OverviewProvider` 接口，提供极简描述：

```go
func (t *WeatherTool) Overview() string {
    return "查询天气"
}
```

启用渐进式披露模式：

```go
registry := agent.NewRegistryWithProgressive()
```

### 内置工具

| 工具 | 说明 |
|------|------|
| `read_file` | 读取文件 |
| `write_file` | 写入文件 |
| `edit_file` | 编辑文件 |
| `list_files` | 列出目录 |
| `exec_command` | 执行命令 |
| `search_knowledge` | 知识库检索 |
| `read_document` | 读取文档 |
| `get_tool_schema` | 获取工具定义 |

## Skill 系统

### 创建 Skill

在 `skills/` 目录下创建 SKILL.md：

```markdown
---
name: code-review
description: "代码审查技能"
allowed-tools:
  - read_file
  - list_files
---

# Code Review

你是一个代码审查专家。

## 工作流程

1. 读取目标文件
2. 分析代码质量
3. 输出审查报告
```

### 使用 Skill

```go
// 加载 skills
skillMgr := agent.NewSkillManager(".")
skillMgr.Discover()

// 创建 agent
ag := agent.New(agent.Options{
    Provider:      provider,
    SkillManager:  skillMgr,
})

// 使用 skill（单次有效）
skill, _ := skillMgr.Get("code-review")
answer, _ := ag.Ask(ctx, session, "审查代码", agent.WithSkill(skill))

// 不用 skill
answer, _ := ag.Ask(ctx, session, "你好")
```

## HTTP Server

### 启动服务

```bash
# 使用配置文件
go run ./cmd/vortex-serve -config ~/.vortex/agent.json -addr :8080
```

### API 接口

#### POST /chat

发送消息并获取回复：

```bash
curl -X POST http://localhost:8080/chat \
  -H "Content-Type: application/json" \
  -d '{"message": "你好"}'
```

响应：

```json
{
  "session_id": "20260827-120000-abc123",
  "answer": "你好！有什么可以帮你的吗？"
}
```

#### POST /chat/stream

流式输出（SSE）：

```bash
curl -X POST http://localhost:8080/chat/stream \
  -H "Content-Type: application/json" \
  -d '{"message": "写一首诗"}'
```

#### GET /sessions

列出所有会话：

```bash
curl http://localhost:8080/sessions
```

#### POST /sessions

创建新会话：

```bash
curl -X POST http://localhost:8080/sessions \
  -H "Content-Type: application/json" \
  -d '{"model": "gpt-4"}'
```

#### DELETE /sessions/{id}

删除会话：

```bash
curl -X DELETE http://localhost:8080/sessions/20260827-120000-abc123
```

## 高级功能

### 并行工具调用

当 LLM 一次返回多个工具调用时，可并行执行：

```go
ag := agent.New(agent.Options{
    ParallelTools: true,
})
```

### 工具重试

工具调用失败时自动重试：

```go
ag := agent.New(agent.Options{
    MaxRetries: 5,  // 最多重试 5 次
})
```

重试策略：指数退避（1s → 2s → 4s → ...）

### 上下文管理

自动压缩过长的历史：

```go
ag := agent.New(agent.Options{
    ContextManager: agent.NewContextManager(session, nil, provider, nil),
})
```

### 生命周期钩子

```go
ag := agent.New(agent.Options{
    Hooks: &agent.Hooks{
        OnBeforeToolCall: func(ctx context.Context, name string, args map[string]any) error {
            // 工具调用前
            return nil // 返回 error 则拦截
        },
        OnAfterToolCall: func(ctx context.Context, name string, args map[string]any, result string, err error) {
            // 工具调用后
        },
        OnError: func(ctx context.Context, err error) {
            // 错误处理
        },
        OnFinish: func(ctx context.Context, answer string) {
            // 完成处理
        },
    },
})
```

### MCP 集成

接入外部 MCP Server：

```go
client, _ := mcp.Connect(ctx, mcp.ServerConfig{
    Name:    "my-tools",
    Command: "python",
    Args:    []string{"-m", "my_mcp_server"},
})

for _, tool := range client.Tools() {
    registry.Add(tool)
}
```

## API 参考

### Agent

```go
type Agent struct{}

func New(opts Options) *Agent
func (a *Agent) Ask(ctx context.Context, session *Session, input string, opts ...AskOption) (string, error)
func (a *Agent) WithDelta(onDelta func(llm.Delta)) *Agent
func (a *Agent) SkillManager() *SkillManager
```

### Options

```go
type Options struct {
    Provider       llm.Provider
    Registry       *Registry
    Model          string
    Thinking       bool
    MaxTokens      int
    MaxIterations  int
    SystemPrompt   string
    OnDelta        func(llm.Delta)
    Hooks          *Hooks
    Store          sessionstore.Store
    ContextManager *ContextManager
    ParallelTools  bool
    SkillManager   *SkillManager
    MaxRetries     int
}
```

### Tool

```go
type Tool interface {
    Name() string
    Description() string
    Schema() map[string]any
    Call(ctx context.Context, args map[string]any) (string, error)
}

type OverviewProvider interface {
    Overview() string
}
```

## 示例项目

### 最小 Agent

```go
// examples/minimal-agent/main.go
package main

import (
    "context"
    "fmt"
    "os"
    "time"

    "github.com/capyflow/vortexagent/agent"
    "github.com/capyflow/vortexagent/llm"
)

type timeTool struct{}

func (t *timeTool) Name() string        { return "current_time" }
func (t *timeTool) Description() string { return "获取当前时间" }
func (t *timeTool) Schema() map[string]any {
    return map[string]any{"type": "object"}
}
func (t *timeTool) Call(_ context.Context, _ map[string]any) (string, error) {
    return time.Now().Format(time.RFC3339), nil
}

func main() {
    provider, _ := llm.NewProvider(llm.ProviderConfig{
        Name:   "openai",
        APIKey: os.Getenv("OPENAI_API_KEY"),
    })

    registry := agent.NewRegistry()
    registry.Add(&timeTool{})

    ag := agent.New(agent.Options{
        Provider: provider,
        Registry: registry,
        OnDelta:  func(d llm.Delta) { fmt.Print(d.Text) },
    })

    session := sessionstore.NewSession("default")
    answer, _ := ag.Ask(context.Background(), session, os.Args[1])
    fmt.Println(answer)
}
```

### HTTP Server

```go
// 使用 vortex-serve 启动
go run ./cmd/vortex-serve -addr :8080

// 调用 API
curl -X POST http://localhost:8080/chat \
  -H "Content-Type: application/json" \
  -d '{"message": "现在几点了？"}'
```

---

## License

MIT
