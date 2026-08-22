package main

import (
	"encoding/json"
	"fmt"
	"os"
)

// Config 是 vortex 的顶层配置，从 JSON 文件加载（默认 ~/.vortex/agent.json，可用 -config 覆盖）。
type Config struct {
	Provider struct {
		Name        string   `json:"name"`
		APIKey      string   `json:"apiKey,omitempty"`
		APIKeyEnv   string   `json:"apiKeyEnv,omitempty"`
		BaseURL     string   `json:"baseURL"`
		Model       string   `json:"model"`
		MaxTokens   int      `json:"maxTokens,omitempty"`
		Temperature *float64 `json:"temperature,omitempty"`
		Thinking    bool     `json:"thinking,omitempty"`
	} `json:"provider"`

	Knowledge    []string          `json:"knowledge,omitempty"`
	MCPServers   []MCPServerConfig `json:"mcpServers,omitempty"`
	SystemPrompt string            `json:"systemPrompt,omitempty"`
	Session      SessionConfig     `json:"session"`
}

// SessionConfig 会话存储配置。
type SessionConfig struct {
	// Type 存储类型：memory / json / postgres（默认 memory）
	Type string `json:"type"`

	// JSON 配置（Type=json 时使用）
	File string `json:"file"` // 会话文件路径

	// Postgres 配置（Type=postgres 时使用）
	Postgres *PostgresConfig `json:"postgres,omitempty"`
}

// PostgresConfig PostgreSQL 连接配置。
type PostgresConfig struct {
	DSN         string `json:"dsn"`         // 连接字符串，如 "postgres://user:pass@localhost:5432/vortex?sslmode=disable"
	LockTTL     int    `json:"lockTtl"`     // 锁过期时间（秒），默认 30
	LockRenewal int    `json:"lockRenewal"` // 续期间隔（秒），默认 10
}

// MCPServerConfig 描述一个外部 MCP server 的启动方式。
type MCPServerConfig struct {
	Name    string            `json:"name"`    // 唯一名称
	Command string            `json:"command"` // 可执行文件，如 python3 / npx / go run
	Args    []string          `json:"args"`    // 启动参数
	Env     map[string]string `json:"env"`     // 附加环境变量（可选）
}

// DefaultConfig 返回空配置（provider 名默认 openai）。
func DefaultConfig() *Config {
	c := &Config{}
	c.Provider.Name = "openai"
	return c
}

// LoadConfig 从文件加载配置，文件不存在时返回默认配置（不报错）。
func LoadConfig(path string) (*Config, error) {
	cfg := DefaultConfig()
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return cfg, nil
		}
		return nil, fmt.Errorf("读取配置 %s 失败: %w", path, err)
	}
	if err := json.Unmarshal(data, cfg); err != nil {
		return nil, fmt.Errorf("解析配置 %s 失败: %w", path, err)
	}
	return cfg, nil
}

// defaultAPIKeyEnv 返回各 provider 默认的 API 密钥环境变量名。
func defaultAPIKeyEnv(name string) string {
	switch name {
	case "anthropic":
		return "ANTHROPIC_API_KEY"
	case "gemini":
		return "GEMINI_API_KEY"
	default:
		return "OPENAI_API_KEY"
	}
}
