// Command minimal-agent 演示如何用本框架搭建一个最小通用 agent：
// 只注册一个"当前时间"内置工具，不依赖知识库、MCP 等任何扩展。
//
// 运行方式（在仓库根目录）：
//
//	export OPENAI_API_KEY=sk-...
//	go run ./examples/minimal-agent "现在几点了？"
//
// 它展示的是框架的最小编程模型，总共四步：
//
//  1. 创建 Provider（llm.NewProvider，可换 anthropic / gemini）
//  2. 创建工具注册表并注册工具（实现 agent.Tool 接口即可）
//  3. 创建 Agent（注入 provider + 注册表 + 流式回调）
//  4. 调用 Agent.Ask 提问
package main

import (
	"context"
	"fmt"
	"os"
	"time"

	"github.com/capyflow/vortexagent/agent"
	"github.com/capyflow/vortexagent/llm"
)

// timeTool 实现 agent.Tool 接口：返回当前时间。
// Go 接口是结构化的，实现方无需 import agent 包。
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
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, "用法: go run ./examples/minimal-agent <问题>")
		os.Exit(1)
	}
	apiKey := os.Getenv("OPENAI_API_KEY")
	if apiKey == "" {
		fmt.Fprintln(os.Stderr, "请设置环境变量 OPENAI_API_KEY")
		os.Exit(1)
	}

	// 1. LLM provider（换成 anthropic / gemini 只需改 Name）
	provider, err := llm.NewProvider(llm.ProviderConfig{
		Name:   "openai",
		APIKey: apiKey,
	})
	if err != nil {
		fmt.Fprintln(os.Stderr, "创建 provider 失败:", err)
		os.Exit(1)
	}

	// 2. 工具注册表
	registry := agent.NewRegistry()
	if err := registry.Add(&timeTool{}); err != nil {
		fmt.Fprintln(os.Stderr, "注册工具失败:", err)
		os.Exit(1)
	}

	// 3. Agent：注入依赖，领域能力全部来自工具与提示词
	ag := agent.New(agent.Options{
		Provider: provider,
		Registry: registry,
		OnDelta:  func(d llm.Delta) { fmt.Print(d.Text) },
	})

	// 4. 提问
	session := agent.NewSession("default")
	answer, err := ag.Ask(context.Background(), session, os.Args[1])
	fmt.Println()
	if err != nil {
		fmt.Fprintln(os.Stderr, "错误:", err)
		os.Exit(1)
	}
	fmt.Println(answer)
}
