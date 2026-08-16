// Package vllm 提供统一的 LLM 服务接口，屏蔽不同厂商（OpenAI 兼容 / Anthropic / Gemini）的协议差异。
//
// 设计继承自旧版 llm_protocol.go，并演进为支持多内容类型（文本/图片/思考）与工具调用（Tool Calling）。
// 对应 pi 项目中 packages/ai 的定位。
package llm

import (
	"context"
	"fmt"
	"net/http"
)

// Role 表示消息的角色。
type Role string

const (
	RoleSystem    Role = "system"    // 系统提示词
	RoleUser      Role = "user"      // 用户消息
	RoleAssistant Role = "assistant" // 模型回复（可能包含工具调用）
	RoleTool      Role = "tool"      // 工具执行结果
)

// ContentType 表示单条内容的类型。
type ContentType string

const (
	ContentText     ContentType = "text"     // 文本
	ContentImage    ContentType = "image"    // 图片（URL 或 base64 数据）
	ContentThinking ContentType = "thinking" // 思考过程（reasoning），部分模型如 DeepSeek R1 / Claude 会输出
)

// Content 是一条消息中的单块内容，与 pi 的 TextContent/ImageContent/ThinkingContent 对应。
type Content struct {
	Type ContentType `json:"type"`

	// 文本内容（Type == ContentText 时有效）
	Text string `json:"text,omitempty"`

	// 图片内容（Type == ContentImage 时有效）：data 与 url 二选一
	Data []byte `json:"-"`
	URL  string `json:"url,omitempty"`
	MIME string `json:"mime,omitempty"` // 图片 MIME 类型，如 image/png

	// 思考内容（Type == ContentThinking 时有效）
	Thinking string `json:"thinking,omitempty"`
}

// ToolCall 是模型发起的工具调用请求。
type ToolCall struct {
	ID        string         `json:"id"`                  // 工具调用 ID，执行结果需要回传
	Name      string         `json:"name"`                // 工具名称
	Arguments map[string]any `json:"arguments,omitempty"` // 工具参数（JSON 对象）
}

// Message 是一轮对话消息。
//
// 各角色下的字段约定：
//   - system / user / assistant：Content 承载内容；assistant 可能同时携带 ToolCalls
//   - tool：Content 承载工具执行结果文本，ToolCallID 指明对应哪次工具调用
type Message struct {
	Role       Role       `json:"role"`
	Content    []Content  `json:"content,omitempty"`
	ToolCalls  []ToolCall `json:"toolCalls,omitempty"`
	ToolCallID string     `json:"toolCallId,omitempty"`
}

// NewTextMessage 构造一条纯文本消息。
func NewTextMessage(role Role, text string) Message {
	return Message{Role: role, Content: []Content{{Type: ContentText, Text: text}}}
}

// NewToolResultMessage 构造一条工具执行结果消息。
func NewToolResultMessage(toolCallID, result string) Message {
	return Message{Role: RoleTool, Content: []Content{{Type: ContentText, Text: result}}, ToolCallID: toolCallID}
}

// ToolParam 是暴露给模型的工具声明（OpenAI function calling / Anthropic tools 的统一形态）。
type ToolParam struct {
	Name        string         `json:"name"`        // 工具名称
	Description string         `json:"description"` // 工具说明，模型据此决定是否调用
	Schema      map[string]any `json:"schema"`      // 参数 JSON Schema（object 类型）
}

// ChatRequest 是一次完整的对话请求。
type ChatRequest struct {
	Model       string      `json:"model"`                 // 模型名称
	Messages    []Message   `json:"messages"`              // 历史消息（含最新一条用户消息）
	Tools       []ToolParam `json:"tools,omitempty"`       // 可用工具列表，可为空
	Temperature *float64    `json:"temperature,omitempty"` // 采样温度，nil 使用模型默认
	MaxTokens   int         `json:"maxTokens,omitempty"`   // 最大输出 token，0 使用模型默认
	Thinking    bool        `json:"thinking,omitempty"`    // 是否启用思考模式（旧版 Think 字段的演进）
}

// Usage 统计一次请求的 token 消耗。
type Usage struct {
	InputTokens      int // 输入 token 数
	OutputTokens     int // 输出 token 数
	CacheReadTokens  int // 命中缓存的输入 token 数
	CacheWriteTokens int // 写入缓存的输入 token 数
}

// ChatResponse 是模型的完整回复（流式结束后由 provider 汇总）。
//
// 设计说明：流式回调（Delta）只用于实时展示；工具调用等结构化信息在流结束后
// 统一通过本结构体返回，避免流式半成品解析的复杂度。这与 pi 的
// assistant 消息组装方式一致。
type ChatResponse struct {
	Message      Message // 完整回复（文本 + 可选 ToolCalls）
	Usage        Usage
	FinishReason string // "stop" / "tool_calls" / "length" / "content_filter"
}

// Delta 是流式输出过程中的增量事件。
type Delta struct {
	Text     string // 增量文本（用于实时展示）
	Thinking string // 增量思考内容
	Done     bool   // 流结束标记
}

// Provider 是 LLM 服务提供方的统一接口（旧版 LLMCommunicationService 的演进）。
//
// onDelta 非 nil 时按流式处理，收到的每个增量都会回调；无论是否流式，
// 返回的 ChatResponse 都包含完整消息。onDelta 返回 error 时中止流。
type Provider interface {
	// Name 返回 provider 名称，如 "openai" / "anthropic" / "gemini"
	Name() string

	// Chat 发送一次对话请求并返回完整回复。
	Chat(ctx context.Context, req *ChatRequest, onDelta func(Delta) error) (*ChatResponse, error)
}

// ProviderOptions 是各 provider 构造函数共有的配置项。
//
// 所有 provider 都放在 vllm 这一个包里，为避免每个文件重复定义 Option 类型
// 造成命名冲突，统一使用本类型。
type ProviderOptions struct {
	BaseURL        string       // 服务地址，空则使用默认
	HTTPClient     *http.Client // HTTP 客户端，nil 使用 http.DefaultClient
	Model          string       // 模型名称，空则使用 provider 默认
	MaxTokens      int          // 最大输出 token，0 使用默认（Anthropic 必填，默认 4096）
	Temperature    *float64     // 采样温度，nil 使用模型默认
	ThinkingBudget int          // 思考预算 token（Anthropic），>0 时启用思考模式
}

// ProviderOption 修改 ProviderOptions。
type ProviderOption func(*ProviderOptions)

// WithBaseURL 设置服务地址。
func WithBaseURL(url string) ProviderOption {
	return func(o *ProviderOptions) { o.BaseURL = url }
}

// WithHTTPClient 设置自定义 HTTP 客户端。
func WithHTTPClient(c *http.Client) ProviderOption {
	return func(o *ProviderOptions) { o.HTTPClient = c }
}

// WithModel 设置模型名称。
func WithModel(model string) ProviderOption {
	return func(o *ProviderOptions) { o.Model = model }
}

// WithMaxTokens 设置最大输出 token 数。
func WithMaxTokens(n int) ProviderOption {
	return func(o *ProviderOptions) { o.MaxTokens = n }
}

// WithTemperature 设置采样温度。
func WithTemperature(t float64) ProviderOption {
	return func(o *ProviderOptions) { o.Temperature = &t }
}

// WithThinkingBudget 设置思考预算 token，>0 时启用思考模式（仅 Anthropic 生效）。
func WithThinkingBudget(n int) ProviderOption {
	return func(o *ProviderOptions) { o.ThinkingBudget = n }
}

// apply 将选项应用到配置。
func (o *ProviderOptions) apply(opts []ProviderOption) {
	for _, opt := range opts {
		if opt != nil {
			opt(o)
		}
	}
}

// NewProvider 按名称创建 provider（工厂方法）。
//
// 支持的 name："openai"（OpenAI 兼容，含 DeepSeek/Qwen/智谱等）、"anthropic"、"gemini"。
func NewProvider(cfg ProviderConfig) (Provider, error) {
	opts := []ProviderOption{WithModel(cfg.Model)}
	if cfg.BaseURL != "" {
		opts = append(opts, WithBaseURL(cfg.BaseURL))
	}
	if cfg.MaxTokens > 0 {
		opts = append(opts, WithMaxTokens(cfg.MaxTokens))
	}
	if cfg.Temperature != nil {
		opts = append(opts, WithTemperature(*cfg.Temperature))
	}
	switch cfg.Name {
	case "openai":
		return NewOpenAIProvider(cfg.APIKey, opts...), nil
	case "anthropic":
		return NewAnthropicProvider(cfg.APIKey, opts...), nil
	case "gemini":
		return NewGeminiProvider(cfg.APIKey, opts...), nil
	default:
		return nil, fmt.Errorf("vllm: 未知的 provider %q（支持 openai / anthropic / gemini）", cfg.Name)
	}
}

// ProviderConfig 描述创建 provider 所需的参数，通常从配置文件加载。
type ProviderConfig struct {
	Name        string   // "openai" | "anthropic" | "gemini"
	APIKey      string   // API 密钥
	BaseURL     string   // 自定义服务地址（可选）
	Model       string   // 模型名称（可选，使用 provider 默认）
	MaxTokens   int      // 最大输出 token（可选）
	Temperature *float64 // 采样温度（可选）
}
