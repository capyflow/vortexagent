package llm

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
)

const (
	// defaultBaseURL 是默认的 API 基础地址。
	defaultBaseURL = "https://api.openai.com/v1"
)

// OpenAIProvider 是基于 OpenAI Chat Completions 协议的 Provider 实现，
// DeepSeek / Qwen / 智谱等兼容厂商可通过 WithBaseURL 接入。
//
// allow: SIZE_OK — 单个 OpenAI 协议适配器（请求体/流式/非流式/工具调用/wire 类型），
// 任务要求集中在 llm/openai.go 单个文件内交付，拆分会破坏既定文件布局。
type OpenAIProvider struct {
	apiKey      string
	baseURL     string
	client      *http.Client
	model         string
	maxTokens     int
	temperature   *float64
	contextWindow int
}

// NewOpenAIProvider 构造 OpenAI 兼容 provider。
//
// 配置项复用 protocol.go 中统一的 ProviderOption（WithBaseURL / WithHTTPClient /
// WithModel / WithMaxTokens / WithTemperature 等）。
func NewOpenAIProvider(apiKey string, opts ...ProviderOption) *OpenAIProvider {
	cfg := &ProviderOptions{BaseURL: defaultBaseURL}
	cfg.apply(opts)
	if cfg.BaseURL == "" {
		cfg.BaseURL = defaultBaseURL
	}
	client := cfg.HTTPClient
	if client == nil {
		client = http.DefaultClient
	}
	p := &OpenAIProvider{
		apiKey:        apiKey,
		baseURL:       strings.TrimSuffix(cfg.BaseURL, "/"),
		client:        client,
		model:         cfg.Model,
		maxTokens:     cfg.MaxTokens,
		temperature:   cfg.Temperature,
		contextWindow: cfg.ContextWindow,
	}
	if p.contextWindow == 0 {
		p.contextWindow = GetContextWindow(p.model)
	}
	return p
}

func (p *OpenAIProvider) Name() string           { return "openai" }
func (p *OpenAIProvider) ContextWindow() int      { return p.contextWindow }

// 编译期校验 OpenAIProvider 满足 Provider 接口。
var _ Provider = (*OpenAIProvider)(nil)

// Chat 发送一次对话请求。
//
// onDelta 非 nil 时以流式方式请求，每个增量都会回调；无论是否流式，
// 返回的 ChatResponse 都包含完整消息。onDelta 返回 error 时中止流。
func (p *OpenAIProvider) Chat(ctx context.Context, req *ChatRequest, onDelta func(Delta) error) (*ChatResponse, error) {
	if req == nil {
		return nil, errors.New("openai: req 不能为空")
	}
	if p.apiKey == "" {
		return nil, errors.New("openai: apiKey 为空，无法发起请求")
	}

	model := req.Model
	if model == "" {
		model = p.model
	}
	if model == "" {
		return nil, errors.New("openai: 未指定模型，请通过 req.Model 或 WithModel 设置")
	}

	// 组装请求体
	wire := wireChatRequest{Model: model}
	wire.Messages = make([]wireChatMessage, 0, len(req.Messages))
	for _, m := range req.Messages {
		wireMsg, err := p.buildWireMessage(m)
		if err != nil {
			return nil, err
		}
		wire.Messages = append(wire.Messages, wireMsg)
	}
	if len(req.Tools) > 0 {
		wire.Tools = make([]wireToolDecl, 0, len(req.Tools))
		for _, t := range req.Tools {
			wire.Tools = append(wire.Tools, wireToolDecl{
				Type: "function",
				Function: wireFunctionDecl{
					Name:        t.Name,
					Description: t.Description,
					Parameters:  t.Schema,
				},
			})
		}
	}
	wire.Temperature = req.Temperature
	if wire.Temperature == nil {
		wire.Temperature = p.temperature
	}
	wire.MaxTokens = req.MaxTokens
	if wire.MaxTokens == 0 {
		wire.MaxTokens = p.maxTokens
	}

	streaming := onDelta != nil
	wire.Stream = streaming

	body, err := json.Marshal(wire)
	if err != nil {
		return nil, fmt.Errorf("openai: 序列化请求体失败: %w", err)
	}

	// 发送 POST {baseURL}/chat/completions
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, p.baseURL+"/chat/completions", bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("openai: 构造请求失败: %w", err)
	}
	httpReq.Header.Set("Authorization", "Bearer "+p.apiKey)
	httpReq.Header.Set("Content-Type", "application/json")
	if streaming {
		httpReq.Header.Set("Accept", "text/event-stream")
	}

	resp, err := p.client.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("openai: 请求失败: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		errBody, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return nil, fmt.Errorf("openai: 非 200 响应 (%d): %s", resp.StatusCode, string(errBody))
	}

	if streaming {
		return p.readStream(resp.Body, onDelta)
	}
	return p.readOnce(resp.Body)
}

// buildWireMessage 将统一 Message 转换为 OpenAI 兼容的请求体消息。
func (p *OpenAIProvider) buildWireMessage(m Message) (wireChatMessage, error) {
	w := wireChatMessage{Role: string(m.Role)}

	switch m.Role {
	case RoleSystem:
		// system 消息使用单字符串 content
		w.Content = textContentOf(m)
	case RoleUser:
		// 纯文本直接输出单字符串；含图片时输出 content 数组
		if hasImage(m) {
			parts := make([]wireContentPart, 0, len(m.Content))
			for _, c := range m.Content {
				switch c.Type {
				case ContentText:
					parts = append(parts, wireContentPart{Type: "text", Text: c.Text})
				case ContentImage:
					url, err := imageDataURL(c)
					if err != nil {
						return w, err
					}
					parts = append(parts, wireContentPart{Type: "image_url", ImageURL: &wireImageURL{URL: url}})
				}
			}
			w.Content = parts
		} else {
			w.Content = textContentOf(m)
		}
	case RoleAssistant:
		// content 为拼接后的文本；带工具调用时额外输出 tool_calls 数组
		w.Content = textContentOf(m)
		if len(m.ToolCalls) > 0 {
			w.ToolCalls = make([]wireToolCall, 0, len(m.ToolCalls))
			for _, tc := range m.ToolCalls {
				args, err := json.Marshal(tc.Arguments)
				if err != nil {
					return w, fmt.Errorf("openai: 序列化工具参数失败: %w", err)
				}
				w.ToolCalls = append(w.ToolCalls, wireToolCall{
					ID:   tc.ID,
					Type: "function",
					Function: wireFunction{
						Name:      tc.Name,
						Arguments: string(args),
					},
				})
			}
		}
	case RoleTool:
		// tool 消息回传工具调用 ID 与执行结果
		w.Content = textContentOf(m)
		w.ToolCallID = m.ToolCallID
	default:
		return w, fmt.Errorf("openai: 不支持的消息角色 %q", m.Role)
	}
	return w, nil
}

// readOnce 解析非流式响应。
func (p *OpenAIProvider) readOnce(body io.Reader) (*ChatResponse, error) {
	var wire wireChatResponse
	if err := json.NewDecoder(body).Decode(&wire); err != nil {
		return nil, fmt.Errorf("openai: 解析响应失败: %w", err)
	}

	resp := &ChatResponse{Usage: parseUsage(wire.Usage)}
	if len(wire.Choices) == 0 {
		return resp, nil
	}

	msg := wire.Choices[0].Message
	resp.FinishReason = wire.Choices[0].FinishReason
	resp.Message = Message{Role: RoleAssistant}
	content := make([]Content, 0, 2)
	if msg.Content != "" {
		content = append(content, Content{Type: ContentText, Text: msg.Content})
	}
	// 思考模式：reasoning_content 透传到 Thinking 内容块
	if msg.ReasoningContent != "" {
		content = append(content, Content{Type: ContentThinking, Thinking: msg.ReasoningContent})
	}
	resp.Message.Content = content

	for _, tc := range msg.ToolCalls {
		resp.Message.ToolCalls = append(resp.Message.ToolCalls, ToolCall{
			ID:        tc.ID,
			Name:      tc.Function.Name,
			Arguments: parseToolArguments(tc.Function.Arguments),
		})
	}
	return resp, nil
}

// readStream 解析 SSE 流，累积增量并在流结束后组装完整消息。
func (p *OpenAIProvider) readStream(body io.Reader, onDelta func(Delta) error) (*ChatResponse, error) {
	text := new(strings.Builder)
	thinking := new(strings.Builder)
	accCalls := make([]streamToolCallAcc, 0)
	accUsage := &wireUsage{}
	resp := &ChatResponse{Message: Message{Role: RoleAssistant}}

	scanner := bufio.NewScanner(body)
	// 放大单行容量，避免大分片（如长图片说明文本）被截断
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)

	for scanner.Scan() {
		line := scanner.Text()
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		data := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if data == "" {
			continue
		}
		if data == "[DONE]" {
			break
		}

		var chunk wireStreamChunk
		if err := json.Unmarshal([]byte(data), &chunk); err != nil {
			return nil, fmt.Errorf("openai: 解析流式分片失败: %w", err)
		}
		// usage 可能在流末分片出现（DeepSeek 等），与 choices 无关，单独收集
		if chunk.Usage != nil {
			accUsage = chunk.Usage
		}
		if len(chunk.Choices) == 0 {
			continue
		}

		delta := chunk.Choices[0].Delta
		if delta.Content != "" {
			text.WriteString(delta.Content)
			if err := onDelta(Delta{Text: delta.Content}); err != nil {
				return nil, err // 回调报错即中止流
			}
		}
		if delta.ReasoningContent != "" {
			thinking.WriteString(delta.ReasoningContent)
			if err := onDelta(Delta{Thinking: delta.ReasoningContent}); err != nil {
				return nil, err
			}
		}
		// tool_calls 按 index 累积：id 可能只在首个分片出现，arguments 分片直接拼接
		for _, tc := range delta.ToolCalls {
			for len(accCalls) <= tc.Index {
				accCalls = append(accCalls, streamToolCallAcc{})
			}
			acc := &accCalls[tc.Index]
			if tc.ID != "" {
				acc.ID = tc.ID
			}
			if tc.Function.Name != "" {
				acc.Name = tc.Function.Name
			}
			acc.Arguments.WriteString(tc.Function.Arguments)
		}
		// finish_reason 取最后一个分片的非空值
		if fr := chunk.Choices[0].FinishReason; fr != "" {
			resp.FinishReason = fr
		}
	}

	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("openai: 读取流失败: %w", err)
	}

	// 流结束：整体反序列化拼接后的 arguments
	for _, acc := range accCalls {
		resp.Message.ToolCalls = append(resp.Message.ToolCalls, ToolCall{
			ID:        acc.ID,
			Name:      acc.Name,
			Arguments: parseToolArguments(acc.Arguments.String()),
		})
	}
	content := make([]Content, 0, 2)
	if text.Len() > 0 {
		content = append(content, Content{Type: ContentText, Text: text.String()})
	}
	if thinking.Len() > 0 {
		content = append(content, Content{Type: ContentThinking, Thinking: thinking.String()})
	}
	resp.Message.Content = content
	resp.Usage = parseUsage(accUsage)
	if err := onDelta(Delta{Done: true}); err != nil {
		return nil, err
	}
	return resp, nil
}

// parseToolArguments 解析工具参数 JSON；空字符串或无效 JSON 时返回空 map
// （部分模型会发送空 arguments，这里置空而非报错）。
func parseToolArguments(raw string) map[string]any {
	args := map[string]any{}
	if strings.TrimSpace(raw) == "" {
		return args
	}
	if err := json.Unmarshal([]byte(raw), &args); err != nil {
		return map[string]any{}
	}
	return args
}

// parseUsage 映射 token 统计；各字段可能缺失，缺失时置 0。
// prompt_cache_hit_tokens 与 cached_tokens 是不同厂商对缓存命中的两种命名。
func parseUsage(u *wireUsage) Usage {
	if u == nil {
		return Usage{}
	}
	usage := Usage{
		InputTokens:      u.PromptTokens,
		OutputTokens:     u.CompletionTokens,
		CacheWriteTokens: u.PromptCacheMissTokens,
	}
	if u.PromptCacheHitTokens > 0 {
		usage.CacheReadTokens = u.PromptCacheHitTokens
	} else {
		usage.CacheReadTokens = u.CachedTokens
	}
	return usage
}

// textContentOf 拼接消息中的所有文本块。
func textContentOf(m Message) string {
	var sb strings.Builder
	for _, c := range m.Content {
		if c.Type == ContentText {
			sb.WriteString(c.Text)
		}
	}
	return sb.String()
}

// hasImage 判断消息是否包含图片内容。
func hasImage(m Message) bool {
	for _, c := range m.Content {
		if c.Type == ContentImage {
			return true
		}
	}
	return false
}

// imageDataURL 将图片内容转为 OpenAI 兼容的 url：base64 数据或原始 URL。
func imageDataURL(c Content) (string, error) {
	if c.URL != "" {
		return c.URL, nil
	}
	if len(c.Data) == 0 {
		return "", errors.New("openai: 图片内容既无 Data 也无 URL")
	}
	mime := c.MIME
	if mime == "" {
		mime = http.DetectContentType(c.Data)
	}
	return "data:" + mime + ";base64," + base64.StdEncoding.EncodeToString(c.Data), nil
}

// —— 以下为与 OpenAI Chat Completions 协议对应的 wire 结构 ——

// wireChatRequest 是请求体的 wire 形态。
type wireChatRequest struct {
	Model       string            `json:"model"`
	Messages    []wireChatMessage `json:"messages"`
	Tools       []wireToolDecl    `json:"tools,omitempty"`
	Stream      bool              `json:"stream,omitempty"`
	Temperature *float64          `json:"temperature,omitempty"`
	MaxTokens   int               `json:"max_tokens,omitempty"`
}

// wireChatMessage 是请求体单条消息的 wire 形态，content 可能是字符串或数组。
type wireChatMessage struct {
	Role       string         `json:"role"`
	Content    any            `json:"content"`
	ToolCalls  []wireToolCall `json:"tool_calls,omitempty"`
	ToolCallID string         `json:"tool_call_id,omitempty"`
}

// wireContentPart 是多内容消息（含图片）中的单块内容。
type wireContentPart struct {
	Type     string        `json:"type"`
	Text     string        `json:"text,omitempty"`
	ImageURL *wireImageURL `json:"image_url,omitempty"`
}

// wireImageURL 是图片引用的 wire 形态（URL 或 base64 数据 URI）。
type wireImageURL struct {
	URL string `json:"url"`
}

// wireToolDecl 是工具声明的 wire 形态。
type wireToolDecl struct {
	Type     string           `json:"type"`
	Function wireFunctionDecl `json:"function"`
}

// wireFunctionDecl 描述一个可调用的函数。
type wireFunctionDecl struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	Parameters  map[string]any `json:"parameters"`
}

// wireToolCall 是助手消息中的工具调用（arguments 为 JSON 字符串）。
type wireToolCall struct {
	ID       string       `json:"id"`
	Type     string       `json:"type"`
	Function wireFunction `json:"function"`
}

// wireFunction 是工具调用的函数部分。
type wireFunction struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

// wireToolCallFragment 是流式增量/非流式响应中的工具调用分片。
type wireToolCallFragment struct {
	ID       string       `json:"id"`
	Index    int          `json:"index"`
	Function wireFunction `json:"function"`
}

// streamToolCallAcc 在流式过程中按 index 累积单个工具调用。
type streamToolCallAcc struct {
	ID        string
	Name      string
	Arguments strings.Builder
}

// wireStreamChunk 是 SSE 流中的单个 data 分片。
type wireStreamChunk struct {
	Choices []struct {
		Delta struct {
			Content          string                 `json:"content"`
			ReasoningContent string                 `json:"reasoning_content"`
			ToolCalls        []wireToolCallFragment `json:"tool_calls"`
		} `json:"delta"`
		FinishReason string `json:"finish_reason"`
	} `json:"choices"`
	Usage *wireUsage `json:"usage,omitempty"`
}

// wireChatResponse 是非流式响应的 wire 形态。
type wireChatResponse struct {
	Choices []struct {
		Message struct {
			Content          string                 `json:"content"`
			ReasoningContent string                 `json:"reasoning_content"`
			ToolCalls        []wireToolCallFragment `json:"tool_calls"`
		} `json:"message"`
		FinishReason string `json:"finish_reason"`
	} `json:"choices"`
	Usage *wireUsage `json:"usage,omitempty"`
}

// wireUsage 是 token 统计的 wire 形态。
type wireUsage struct {
	PromptTokens          int `json:"prompt_tokens"`
	CompletionTokens      int `json:"completion_tokens"`
	PromptCacheHitTokens  int `json:"prompt_cache_hit_tokens"`
	PromptCacheMissTokens int `json:"prompt_cache_miss_tokens"`
	CachedTokens          int `json:"cached_tokens"`
}
