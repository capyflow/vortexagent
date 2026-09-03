package main

import (
	"encoding/json"
	"fmt"
	"os"
)

// Config 是 vortex 的顶层配置，从 -config 指定的 JSON 文件加载（必填，无默认值）。
// 一个机器跑多个 agent 时，每个 agent 各指向独立文件，如 ./vortex/deploy_agent/my-agent.json。
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
	Tools        *ToolsConfig      `json:"tools,omitempty"`
}

// ToolsConfig 内置工具开关（默认全关：内置工具涉及本机执行，按需启用）。
type ToolsConfig struct {
	// Exec 启用 exec_command 工具（执行 shell 命令）。风险较高，
	// 建议配合 Hooks.OnBeforeToolCall 做命令白名单。
	Exec *ExecToolConfig `json:"exec,omitempty"`

	// Filesystem 启用 read_file / write_file / edit_file / list_files 工具，
	// 所有路径被限制在 root 目录内（防 prompt injection 越权读写）。
	Filesystem *FilesystemToolConfig `json:"filesystem,omitempty"`

	// Memory 启用长期记忆工具组（memory_save / memory_update / memory_delete /
	// memory_get / memory_search / memory_list）。记忆是 agent 运行中自己写入、
	// 跨会话持久化的事实条目，与单会话的历史（session）相互独立。
	Memory *MemoryToolConfig `json:"memory,omitempty"`
}

// ExecToolConfig exec 工具配置。
type ExecToolConfig struct {
	Enabled bool   `json:"enabled"`
	Workdir string `json:"workdir,omitempty"` // 工作目录，默认当前目录
}

// FilesystemToolConfig filesystem 工具组配置。
type FilesystemToolConfig struct {
	Enabled bool   `json:"enabled"`
	Root    string `json:"root,omitempty"` // 允许访问的根目录，默认当前目录
}

// MemoryToolConfig 长期记忆工具组配置。
type MemoryToolConfig struct {
	Enabled bool   `json:"enabled"`
	Dir     string `json:"dir,omitempty"` // 记忆存储目录，默认 ~/.vortex/memory
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
