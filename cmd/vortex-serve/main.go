package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/capyflow/vortexagent/agent"
	"github.com/capyflow/vortexagent/agent/sessionstore"
	"github.com/capyflow/vortexagent/llm"
	"github.com/capyflow/vortexagent/server"
	"github.com/capyflow/vortexagent/tools/knowledge"
)

func main() {
	configPath := flag.String("config", "", "配置文件路径")
	addr := flag.String("addr", ":8080", "监听地址")
	flag.Parse()

	if *configPath == "" {
		home, _ := os.UserHomeDir()
		*configPath = filepath.Join(home, ".vortex", "agent.json")
	}

	if _, err := os.Stat(*configPath); os.IsNotExist(err) {
		fmt.Fprintln(os.Stderr, "错误: 配置文件不存在:", *configPath)
		fmt.Fprintln(os.Stderr, "请先运行 vortex 创建配置")
		os.Exit(1)
	}

	cfg, err := LoadConfig(*configPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, "错误:", err)
		os.Exit(1)
	}

	if err := loadDotEnv(".env"); err != nil {
		fmt.Fprintf(os.Stderr, "警告: 加载 .env 失败: %v\n", err)
	}

	var apiKey string
	if cfg.Provider.APIKey != "" {
		apiKey = cfg.Provider.APIKey
	} else if cfg.Provider.APIKeyEnv != "" {
		apiKey = os.Getenv(cfg.Provider.APIKeyEnv)
	} else {
		apiKey = os.Getenv(defaultAPIKeyEnv(cfg.Provider.Name))
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
		jsonStore, jerr := sessionstore.NewJSON(expandPath(cfg.Session.File))
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

	srv := server.New(server.Config{
		Addr:  *addr,
		Agent: ag,
		Store: store,
	})

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

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

type Config struct {
	Provider struct {
		Name        string   `json:"name"`
		APIKey      string   `json:"apiKey,omitempty"`
		APIKeyEnv   string   `json:"apiKeyEnv,omitempty"`
		BaseURL     string   `json:"baseURL,omitempty"`
		Model       string   `json:"model,omitempty"`
		MaxTokens   int      `json:"maxTokens,omitempty"`
		Temperature *float64 `json:"temperature,omitempty"`
		Thinking    bool     `json:"thinking,omitempty"`
	} `json:"provider"`
	Knowledge    []string      `json:"knowledge,omitempty"`
	SystemPrompt string        `json:"systemPrompt,omitempty"`
	Session      SessionConfig `json:"session,omitempty"`
}

// SessionConfig 会话持久化配置（与 vortex CLI 的同名配置一致）。
type SessionConfig struct {
	// Type 存储类型：memory（默认，不持久化）/ json
	Type string `json:"type"`
	// File JSON 存储文件路径（Type=json 时使用）
	File string `json:"file"`
}

func LoadConfig(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var cfg Config
	if err := json.Unmarshal(data, &cfg); err != nil {
		return nil, err
	}
	return &cfg, nil
}

func defaultAPIKeyEnv(name string) string {
	switch name {
	case "openai":
		return "OPENAI_API_KEY"
	case "anthropic":
		return "ANTHROPIC_API_KEY"
	case "gemini":
		return "GEMINI_API_KEY"
	default:
		return "OPENAI_API_KEY"
	}
}

// expandPath 展开 ~ 前缀的路径。
func expandPath(path string) string {
	if strings.HasPrefix(path, "~") {
		home, _ := os.UserHomeDir()
		return filepath.Join(home, path[1:])
	}
	return path
}

func loadDotEnv(path string) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	lines := strings.Split(string(data), "\n")
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		parts := strings.SplitN(line, "=", 2)
		if len(parts) != 2 {
			continue
		}
		key := strings.TrimSpace(parts[0])
		value := strings.TrimSpace(parts[1])
		if os.Getenv(key) == "" {
			os.Setenv(key, value)
		}
	}
	return nil
}
