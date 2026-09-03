// Command vortex-serve 是基于本框架的参考服务端：把 agent 挂到 HTTP 上（SSE 流式接口），
// 可选启用自治 agent（目标调度 + webhook 触发）。配置加载与 vortex CLI 共用 config 包。
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/capyflow/vortexagent/agent"
	"github.com/capyflow/vortexagent/agent/sessionstore"
	"github.com/capyflow/vortexagent/autonomous"
	"github.com/capyflow/vortexagent/config"
	"github.com/capyflow/vortexagent/llm"
	"github.com/capyflow/vortexagent/server"
	"github.com/capyflow/vortexagent/tools/knowledge"
)

func main() {
	configPath := flag.String("config", config.DefaultPath, "配置文件路径（必填，如 ./vortex/deploy_agent/my-agent.json；可构建时烧录，运行时可覆盖）")
	addr := flag.String("addr", ":8080", "监听地址")
	flag.Parse()

	if *configPath == "" {
		fmt.Fprintln(os.Stderr, "错误: 未指定配置文件路径：构建时 -ldflags \"-X github.com/capyflow/vortexagent/config.DefaultPath=路径\" 烧录，或运行时 -config 传入")
		flag.Usage()
		os.Exit(1)
	}

	if _, err := os.Stat(*configPath); os.IsNotExist(err) {
		fmt.Fprintln(os.Stderr, "错误: 配置文件不存在:", *configPath)
		fmt.Fprintln(os.Stderr, "请先运行 vortex 创建配置")
		os.Exit(1)
	}

	cfg, err := config.LoadConfig(*configPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, "错误:", err)
		os.Exit(1)
	}

	if err := config.LoadDotEnv(".env"); err != nil {
		fmt.Fprintf(os.Stderr, "警告: 加载 .env 失败: %v\n", err)
	}

	var apiKey string
	if cfg.Provider.APIKey != "" {
		apiKey = cfg.Provider.APIKey
	} else if cfg.Provider.APIKeyEnv != "" {
		apiKey = os.Getenv(cfg.Provider.APIKeyEnv)
	} else {
		apiKey = os.Getenv(config.DefaultAPIKeyEnv(cfg.Provider.Name))
	}
	if apiKey == "" {
		fmt.Fprintln(os.Stderr, "错误: 未找到 API 密钥")
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

	registry := agent.NewRegistry()
	if len(cfg.Knowledge) > 0 {
		kb := knowledge.NewKB(cfg.Knowledge)
		for _, t := range knowledge.NewKBTools(kb) {
			registry.Add(t)
		}
		fmt.Printf("已加载知识库: %s\n", strings.Join(cfg.Knowledge, ", "))
	}

	// 会话持久化（可选）：配置 session.type=json 后，会话跨重启保留
	var store sessionstore.Store
	if cfg.Session.Type == "json" {
		if cfg.Session.File == "" {
			fmt.Fprintln(os.Stderr, "错误: session.type=json 但未配置 session.file")
			os.Exit(1)
		}
		jsonStore, jerr := sessionstore.NewJSON(config.ExpandPath(cfg.Session.File))
		if jerr != nil {
			fmt.Fprintln(os.Stderr, "错误:", jerr)
			os.Exit(1)
		}
		store = jsonStore
		fmt.Printf("会话持久化: %s\n", cfg.Session.File)
	}

	ag := agent.New(agent.Options{
		Provider:     provider,
		Registry:     registry,
		Model:        cfg.Provider.Model,
		Thinking:     cfg.Provider.Thinking,
		MaxTokens:    cfg.Provider.MaxTokens,
		SystemPrompt: cfg.SystemPrompt,
		MaxRetries:   3,
	})

	var autoAgent *autonomous.AutonomousAgent
	var webhookSecret string
	if cfg.Autonomous != nil && cfg.Autonomous.Enabled {
		goalStore, err := autonomous.NewJSONGoalStore(config.ExpandPath(cfg.Autonomous.GoalStore.File))
		if err != nil {
			fmt.Fprintf(os.Stderr, "错误: 初始化目标存储失败: %v\n", err)
			os.Exit(1)
		}

		autoAgent = autonomous.New(autonomous.Config{
			Agent:     ag,
			Model:     cfg.Provider.Model,
			Registry:  registry,
			GoalStore: goalStore,
			MaxSleep:  time.Duration(cfg.Autonomous.MaxSleepMin) * time.Minute,
		})

		for _, g := range cfg.Autonomous.Goals {
			goal, err := config.GoalFromConfig(g)
			if err != nil {
				fmt.Fprintf(os.Stderr, "警告: 跳过无效的自治目标配置 %q: %v\n", g.Title, err)
				continue
			}
			if err := autoAgent.AddGoal(goal); err != nil {
				fmt.Fprintf(os.Stderr, "警告: 添加目标 %q 失败: %v\n", goal.Title, err)
			}
		}

		webhookSecret = cfg.Autonomous.WebhookSecret
		if webhookSecret == "" {
			webhookSecret = os.Getenv("VORTEX_WEBHOOK_SECRET")
		}
		if webhookSecret == "" {
			fmt.Println("警告: 未设置 VORTEX_WEBHOOK_SECRET，/webhook/ 仅允许本机来源访问")
		}

		fmt.Printf("自治 agent 已启用（目标存储: %s）\n", cfg.Autonomous.GoalStore.File)
	}

	srv := server.New(server.Config{
		Addr:          *addr,
		Agent:         ag,
		Autonomous:    autoAgent,
		Store:         store,
		WebhookSecret: webhookSecret,
	})

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	if autoAgent != nil {
		go func() {
			if err := autoAgent.Run(ctx); err != nil {
				fmt.Fprintf(os.Stderr, "自治 agent 错误: %v\n", err)
			}
		}()
	}

	go func() {
		fmt.Printf("vortex HTTP server 启动在 %s\n", *addr)
		if err := srv.Start(); err != nil && err != context.Canceled {
			fmt.Fprintln(os.Stderr, "服务器错误:", err)
		}
	}()

	<-ctx.Done()
	fmt.Println("\n正在关闭服务器...")
	srv.Shutdown(context.Background())
	fmt.Println("服务器已关闭")
}
