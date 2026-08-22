// Command knowledge-agent 演示如何基于框架构建一个"文档问答 agent"：
// 注册 knowledge 扩展工具后，agent 就能检索本地文档目录并基于检索结果回答。
//
// 运行方式（在仓库根目录）：
//
//	export OPENAI_API_KEY=sk-...
//	go run ./examples/knowledge-agent "框架支持哪些厂商？"
//
// 与 examples/minimal-agent 的唯一区别是多了两行：用 tools/knowledge
// 扩展构造知识库工具并注册。这说明知识库只是框架的一个可选扩展——
// 框架本身与任何领域无关。
package main

import (
	"context"
	"fmt"
	"os"
	"strings"

	"github.com/capyflow/vortexagent/agent"
	"github.com/capyflow/vortexagent/agent/sessionstore"
	"github.com/capyflow/vortexagent/llm"
	"github.com/capyflow/vortexagent/tools/knowledge"
)

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, "用法: go run ./examples/knowledge-agent <问题>")
		os.Exit(1)
	}
	apiKey := os.Getenv("OPENAI_API_KEY")
	if apiKey == "" {
		fmt.Fprintln(os.Stderr, "请设置环境变量 OPENAI_API_KEY")
		os.Exit(1)
	}

	provider, err := llm.NewProvider(llm.ProviderConfig{
		Name:   "openai",
		APIKey: apiKey,
	})
	if err != nil {
		fmt.Fprintln(os.Stderr, "创建 provider 失败:", err)
		os.Exit(1)
	}

	// 知识库根目录：默认仓库根目录，可用第二个参数覆盖
	kbDir := "."
	if len(os.Args) > 2 {
		kbDir = os.Args[2]
	}

	registry := agent.NewRegistry()
	kb := knowledge.NewKB([]string{kbDir})
	for _, tool := range knowledge.NewKBTools(kb) {
		if err := registry.Add(tool); err != nil {
			fmt.Fprintln(os.Stderr, "注册知识库工具失败:", err)
			os.Exit(1)
		}
	}

	// 系统提示词告诉模型知识库工具的存在，引导它先检索再回答
	ag := agent.New(agent.Options{
		Provider: provider,
		Registry: registry,
		SystemPrompt: "你是一个文档问答助手。回答前先用 search_knowledge 检索本地文档，" +
			"需要细节时再用 read_document 读取全文，最后基于检索内容回答。",
		OnDelta: func(d llm.Delta) { fmt.Print(d.Text) },
	})

	session := sessionstore.NewSession("default")
	answer, err := ag.Ask(context.Background(), session, os.Args[1])
	fmt.Println()
	if err != nil {
		fmt.Fprintln(os.Stderr, "错误:", err)
		os.Exit(1)
	}
	if strings.TrimSpace(answer) != "" {
		fmt.Println(answer)
	}
}
