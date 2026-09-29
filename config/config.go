// Package config 提供参考 CLI（cmd/vortex、cmd/vortex-serve）与下游项目共用的
// 配置文件 schema 与加载逻辑。
//
// 配置文件路径必填，两种指定方式（运行时优先）：
//   - 构建时烧录：go build -ldflags "-X github.com/capyflow/vortexagent/config.DefaultPath=路径"
//   - 运行时传入：-config 路径
//
// 一个机器跑多个 agent 时，每个 agent 各指向独立文件，如 ./vortex/deploy_agent/my-agent.json。
package config

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// DefaultPath 构建时可通过 -ldflags 烧录的默认配置文件路径：
//
//	go build -ldflags "-X github.com/capyflow/vortexagent/config.DefaultPath=./vortex/deploy_agent/my-agent.json"
//
// 空值表示未烧录，由调用方要求运行时通过 -config 显式传入。运行时传入优先于烧录值。
var DefaultPath string

// Config 是 vortex 的顶层配置。
type Config struct {
	Provider     ProviderConfig     `json:"provider"`
	Knowledge    []string           `json:"knowledge,omitempty"`
	MCPServers   []MCPServerConfig  `json:"mcpServers,omitempty"`
	SystemPrompt string             `json:"systemPrompt,omitempty"`
	Session      SessionConfig      `json:"session"`
	Tools        *ToolsConfig       `json:"tools,omitempty"`
	Permissions  *PermissionsConfig `json:"permissions,omitempty"`
	Autonomous   *AutonomousConfig  `json:"autonomous,omitempty"`
	Context      *ContextConfig     `json:"context,omitempty"`
}

// ProviderConfig LLM 提供方配置。
type ProviderConfig struct {
	Name        string   `json:"name"`
	APIKey      string   `json:"apiKey,omitempty"`
	APIKeyEnv   string   `json:"apiKeyEnv,omitempty"`
	BaseURL     string   `json:"baseURL"`
	Model       string   `json:"model"`
	MaxTokens   int      `json:"maxTokens,omitempty"`
	Temperature *float64 `json:"temperature,omitempty"`
	Thinking    bool     `json:"thinking,omitempty"`
}

// ToolsConfig 内置工具开关（默认全关：内置工具涉及本机执行，按需启用）。
type ToolsConfig struct {
	// Exec 启用 exec_command 工具（执行 shell 命令）。风险较高，
	// 建议配合 permissions 的 allow/deny 规则限制可执行的命令。
	Exec *ExecToolConfig `json:"exec,omitempty"`

	// Filesystem 启用 read_file / write_file / edit_file / list_files 工具，
	// 所有路径被限制在 root 目录内（防 prompt injection 越权读写）。
	Filesystem *FilesystemToolConfig `json:"filesystem,omitempty"`

	// Memory 启用长期记忆工具组（memory_save / memory_update / memory_delete /
	// memory_get / memory_search / memory_list）。记忆是 agent 运行中自己写入、
	// 跨会话持久化的事实条目，与单会话的历史（session）相互独立。
	Memory *MemoryToolConfig `json:"memory,omitempty"`
}

// PermissionsConfig 全局工具权限配置：作用于所有工具（内置工具与 MCP 工具），
// 在每次工具执行前判定。deny 规则在任何模式（含 full_access）下都生效。
//
// 规则格式："工具名" 或 "工具名:参数模式"（按第一个冒号切分），支持尾缀 *
// 通配。MCP 工具名带命名空间：mcp__<server> 整个 server、mcp__<server>__<tool>
// 单个工具。参数模式由工具域解释，exec_command 的模式按 shell 命令理解：
//
//	allow: ["read_file", "exec_command:git status*"]
//	deny:  ["exec_command:rm -rf*", "exec_command:mkfs*"]
type PermissionsConfig struct {
	// Mode 执行模式：full_access（全放行，deny 仍拦截）/
	// confirm（默认，放行规则未命中的调用询问用户）/ whitelist（仅执行
	// allow 规则命中的调用）
	Mode string `json:"mode,omitempty"`

	// Allow 放行规则：命中的调用直接放行，不再询问
	Allow []string `json:"allow,omitempty"`

	// Deny 拒绝规则：命中的调用直接拒绝，任何模式下都生效
	Deny []string `json:"deny,omitempty"`
}

// ExecToolConfig exec 工具配置。
type ExecToolConfig struct {
	Enabled bool   `json:"enabled"`
	Workdir string `json:"workdir,omitempty"` // 工作目录，默认当前目录
	// MaxOutput 命令输出进入会话历史前的截断上限（字符）。
	// 0/未配置取默认 8000；-1 表示不限制（不建议）。
	MaxOutput int `json:"maxOutput,omitempty"`
}

// FilesystemToolConfig filesystem 工具组配置。
type FilesystemToolConfig struct {
	Enabled bool   `json:"enabled"`
	Root    string `json:"root,omitempty"` // 允许访问的根目录，默认当前目录
	// MaxContent read_file 内容进入会话历史前的截断上限（字符）。
	// 0/未配置取默认 8000；-1 表示不限制（不建议）。
	MaxContent int `json:"maxContent,omitempty"`
}

// MemoryToolConfig 长期记忆工具组配置。
type MemoryToolConfig struct {
	Enabled bool   `json:"enabled"`
	Dir     string `json:"dir,omitempty"` // 记忆存储目录，默认 ~/.vortex/memory
}

// ContextConfig 单会话上下文引擎（ctxengine）配置：活跃上下文有界 +
// 原始历史归档 + 按需检索。详见 docs/context.md。
type ContextConfig struct {
	Enabled bool `json:"enabled"`
	// SoftRatio 归档触发阈值：估算占用率超过窗口该比例时归档。默认 0.7。
	SoftRatio float64 `json:"softRatio,omitempty"`
	// HardRatio 发送前硬校验阈值：估算+maxTokens 超过窗口该比例则先裁剪。
	// 默认 0.95。
	HardRatio float64 `json:"hardRatio,omitempty"`
	// KeepRecentBlocks 归档时保留的最近块数（块 = 相邻用户消息之间的全部
	// 消息，工具调用与其结果保证同块）。默认 12。
	KeepRecentBlocks int `json:"keepRecentBlocks,omitempty"`
	// SummaryBudget 摘要区预算（占窗口比例），超预算触发旧摘要合并。默认 0.10。
	SummaryBudget float64 `json:"summaryBudget,omitempty"`
	// Archive 归档存储配置。
	Archive *ContextArchiveConfig `json:"archive,omitempty"`
	// Retrieval 检索配置。
	Retrieval *ContextRetrievalConfig `json:"retrieval,omitempty"`
	// RetentionDays 归档保留天数，超期自动清理。0 = 永久保留。
	RetentionDays int `json:"retentionDays,omitempty"`
}

// ContextArchiveConfig 归档存储配置。
type ContextArchiveConfig struct {
	// Backend 存储后端：fs（当前唯一实现）。
	Backend string `json:"backend,omitempty"`
	// Dir 归档目录，默认 ~/.vortex/<group>/archive。
	Dir string `json:"dir,omitempty"`
}

// ContextRetrievalConfig 检索配置。
type ContextRetrievalConfig struct {
	// Limit search_history 默认返回条数。默认 5，最大 20。
	Limit int `json:"limit,omitempty"`
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

// DefaultAPIKeyEnv 返回各 provider 默认的 API 密钥环境变量名。
func DefaultAPIKeyEnv(name string) string {
	switch name {
	case "anthropic":
		return "ANTHROPIC_API_KEY"
	case "gemini":
		return "GEMINI_API_KEY"
	default:
		return "OPENAI_API_KEY"
	}
}

// ExpandPath 展开 ~ 前缀的路径。
func ExpandPath(path string) string {
	if strings.HasPrefix(path, "~") {
		home, _ := os.UserHomeDir()
		return filepath.Join(home, path[1:])
	}
	return path
}
