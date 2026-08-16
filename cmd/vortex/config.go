package main

import (
	"encoding/json"
	"fmt"
	"os"
)

// Config 是 vortex 的顶层配置，从 JSON 文件加载（默认 ./vortex.json，可用 -config 覆盖）。
type Config struct {
	// Provider 配置（name 支持 openai / anthropic / gemini）
	Provider struct {
		Name        string   `json:"name"`
		APIKeyEnv   string   `json:"apiKeyEnv"`   // 存放 API 密钥的环境变量名，空则按 provider 默认查找
		BaseURL     string   `json:"baseURL"`     // 自定义服务地址（可选）
		Model       string   `json:"model"`       // 模型名称（可选）
		MaxTokens   int      `json:"maxTokens"`   // 最大输出 token（可选）
		Temperature *float64 `json:"temperature"` // 采样温度（可选）
		Thinking    bool     `json:"thinking"`    // 是否启用思考模式（可选）
	} `json:"provider"`

	// Knowledge 是知识库根目录列表（可选）
	Knowledge []string `json:"knowledge"`

	// MCPServers 是 MCP server 列表，每个 server 的工具会自动注册给 agent（可选）
	MCPServers []MCPServerConfig `json:"mcpServers"`

	// SystemPrompt 自定义系统提示词（可选）
	SystemPrompt string `json:"systemPrompt"`
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
