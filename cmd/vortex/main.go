// Command vortex 是智能文档 agent 的 CLI 入口。
//
// 交互模式：输入问题回车提问，支持以下斜杠命令：
//
//	/help    显示帮助
//	/tools   列出当前可用的工具
//	/clear   清空当前会话历史
//	/exit    退出
package main

import (
	"bufio"
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"strings"

	"github.com/capyflow/vortexagent/agent"
	"github.com/capyflow/vortexagent/knowledge"
	"github.com/capyflow/vortexagent/llm"
	"github.com/capyflow/vortexagent/tools/mcp"
)

func main() {
	configPath := flag.String("config", "vortex.json", "配置文件路径")
	flag.Parse()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	cfg, err := LoadConfig(*configPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, "错误:", err)
		os.Exit(1)
	}

	// 0. 加载 .env（若存在）：填充未设置的环境变量，不覆盖已有值
	if err := loadDotEnv(".env"); err != nil {
		fmt.Fprintln(os.Stderr, "警告: 加载 .env 失败:", err)
	}

	// 1. 创建 LLM provider
	apiKey := os.Getenv(cfg.Provider.APIKeyEnv)
	if apiKey == "" {
		apiKey = os.Getenv(defaultAPIKeyEnv(cfg.Provider.Name))
	}
	if apiKey == "" {
		fmt.Fprintf(os.Stderr, "错误: 未找到 %s 的 API 密钥（设置环境变量 %s）\n",
			cfg.Provider.Name, defaultAPIKeyEnv(cfg.Provider.Name))
		os.Exit(1)
	}
	provider, err := llm.NewProvider(llm.ProviderConfig{
		Name:        cfg.Provider.Name,
		APIKey:      apiKey,
		BaseURL:     cfg.Provider.BaseURL,
		Model:       cfg.Provider.Model,
		MaxTokens:   cfg.Provider.MaxTokens,
		Temperature: cfg.Provider.Temperature,
	})
	if err != nil {
		fmt.Fprintln(os.Stderr, "错误:", err)
		os.Exit(1)
	}

	// 2. 注册工具：知识库 + MCP
	registry := agent.NewRegistry()
	if len(cfg.Knowledge) > 0 {
		kb := knowledge.NewKB(cfg.Knowledge)
		for _, t := range knowledge.NewKBTools(kb) {
			if err := registry.Add(t); err != nil {
				fmt.Fprintln(os.Stderr, "警告:", err)
			}
		}
		fmt.Printf("已加载知识库: %s\n", strings.Join(cfg.Knowledge, ", "))
	}

	var mcpClients []*mcp.Client
	for _, srv := range cfg.MCPServers {
		client, err := mcp.Connect(ctx, mcp.ServerConfig{
			Name:    srv.Name,
			Command: srv.Command,
			Args:    srv.Args,
			Env:     srv.Env,
		})
		if err != nil {
			fmt.Fprintf(os.Stderr, "警告: 连接 MCP server %s 失败: %v\n", srv.Name, err)
			continue
		}
		mcpClients = append(mcpClients, client)
		for _, t := range client.Tools() {
			if err := registry.Add(t); err != nil {
				fmt.Fprintf(os.Stderr, "警告: 注册 MCP 工具 %s 失败: %v\n", t.Name(), err)
			}
		}
		fmt.Printf("已连接 MCP server %s（%d 个工具）\n", srv.Name, len(client.Tools()))
	}
	defer func() {
		for _, c := range mcpClients {
			c.Close()
		}
	}()

	// 3. 创建 agent 并进入交互循环
	ag := agent.New(agent.Options{
		Provider:     provider,
		Registry:     registry,
		Model:        cfg.Provider.Model,
		Thinking:     cfg.Provider.Thinking,
		MaxTokens:    cfg.Provider.MaxTokens,
		SystemPrompt: cfg.SystemPrompt,
		OnDelta: func(d llm.Delta) {
			if d.Thinking != "" {
				fmt.Fprint(os.Stderr, d.Thinking)
				return
			}
			fmt.Print(d.Text)
		},
	})
	session := agent.NewSession(cfg.Provider.Model)

	fmt.Printf("vortex 文档 agent 已启动（provider=%s, model=%s, 工具: %s）\n",
		provider.Name(), modelName(cfg), strings.Join(registry.Names(), ", "))
	fmt.Println("输入问题开始对话，/help 查看命令。")

	scanner := bufio.NewScanner(os.Stdin)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	exit := false
	for {
		// Ctrl+C（SIGINT）会取消 ctx：中断当前请求后直接结束会话。
		// 注意 signal.NotifyContext 会一直拦截信号，若不在此退出，
		// 后续 Ctrl+C 无法再触发默认终止行为，用户会被困在 REPL 里。
		if ctx.Err() != nil {
			fmt.Println()
			break
		}
		fmt.Print("\n> ")
		if !scanner.Scan() {
			break
		}
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		if handleCommand(line, session, registry, &exit) {
			if exit {
				break
			}
			continue
		}

		fmt.Println()
		_, err := ag.Ask(ctx, session, line)
		fmt.Println()
		if err != nil {
			fmt.Fprintf(os.Stderr, "\n错误: %v\n", err)
		}
	}
	if err := scanner.Err(); err != nil {
		fmt.Fprintln(os.Stderr, "读取输入失败:", err)
	}
}

// handleCommand 处理斜杠命令，返回 (是否已处理, 是否退出)。
func handleCommand(line string, session *agent.Session, registry *agent.Registry, exit *bool) bool {
	switch line {
	case "/exit", "/quit":
		*exit = true
		return true
	case "/help":
		fmt.Println("命令: /help 帮助  /tools 工具列表  /clear 清空历史  /exit 退出")
		return true
	case "/tools":
		if len(registry.Names()) == 0 {
			fmt.Println("当前没有可用工具")
			return true
		}
		for _, name := range registry.Names() {
			t, _ := registry.Get(name)
			fmt.Printf("  %s - %s\n", name, t.Description())
		}
		return true
	case "/clear":
		session.Clear()
		fmt.Println("会话历史已清空")
		return true
	}
	return false
}

// modelName 返回展示用的模型名。
func modelName(cfg *Config) string {
	if cfg.Provider.Model != "" {
		return cfg.Provider.Model
	}
	return "默认"
}
