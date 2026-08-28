package llm

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
)

const (
	// anthropicDefaultBaseURL 是默认的 API 基础地址。
	anthropicDefaultBaseURL = "https://api.anthropic.com"
	// anthropicVersion 是 API 版本头。
	anthropicVersion = "2023-06-01"
	// anthropicDefaultMaxTokens 是默认最大输出 token（Anthropic 必填）。
	anthropicDefaultMaxTokens = 4096
)

// AnthropicProvider 调用 Anthropic Messages API。
type AnthropicProvider struct {
	apiKey         string
	baseURL        string
	client         *http.Client
	model          string
	maxTokens      int
	temperature    *float64
	thinkingBudget int
	contextWindow  int
}

// NewAnthropicProvider 构造 Anthropic provider。
func NewAnthropicProvider(apiKey string, opts ...ProviderOption) *AnthropicProvider {
	p := &AnthropicProvider{
		apiKey:    apiKey,
		baseURL:   anthropicDefaultBaseURL,
		client:    http.DefaultClient,
		maxTokens: anthropicDefaultMaxTokens,
	}
	po := &ProviderOptions{}
	po.apply(opts)
	if po.BaseURL != "" {
		p.baseURL = po.BaseURL
	}
	if po.HTTPClient != nil {
		p.client = po.HTTPClient
	}
	p.model = po.Model
	if po.MaxTokens > 0 {
		p.maxTokens = po.MaxTokens
	}
	p.temperature = po.Temperature
	p.thinkingBudget = po.ThinkingBudget
	p.contextWindow = po.ContextWindow
	if p.contextWindow == 0 {
		p.contextWindow = GetContextWindow(p.model)
	}
	return p
}

// Name 返回 provider 名称。
func (p *AnthropicProvider) Name() string           { return "anthropic" }
func (p *AnthropicProvider) ContextWindow() int      { return p.contextWindow }

// 编译期断言：满足 Provider 接口。
var _ Provider = (*AnthropicProvider)(nil)

// anthropicMessage 是请求体中的一条消息。
type anthropicMessage struct {
	Role    string             `json:"role"`
	Content []anthropicContent `json:"content"`
}

// anthropicContent 是消息内容块（text / thinking / image / tool_use / tool_result）。
type anthropicContent struct {
	Type string `json:"type"`

	Text      string           `json:"text,omitempty"` // text / thinking 块的文本
	Thinking  string           `json:"thinking,omitempty"`
	Source    *anthropicSource `json:"source,omitempty"`      // image 块
	ID        string           `json:"id,omitempty"`          // tool_use
	Name      string           `json:"name,omitempty"`        // tool_use
	Input     map[string]any   `json:"input,omitempty"`       // tool_use（发送时）
	ToolUseID string           `json:"tool_use_id,omitempty"` // tool_result
	Result    any              `json:"content,omitempty"`     // tool_result 的结果文本

	// rawInput 累积流式 input_json_delta 分片（不参与序列化）。
	rawInput string
}

// anthropicSource 是 image 块的数据来源。
type anthropicSource struct {
	Type      string `json:"type"`
	MediaType string `json:"media_type,omitempty"`
	Data      string `json:"data,omitempty"`
	URL       string `json:"url,omitempty"`
}

// anthropicTool 是工具声明。
type anthropicTool struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	InputSchema map[string]any `json:"input_schema"`
}

// anthropicThinking 是思考模式配置。
type anthropicThinking struct {
	Type         string `json:"type"`
	BudgetTokens int    `json:"budget_tokens"`
}

// anthropicRequest 是 Messages API 请求体。
type anthropicRequest struct {
	Model       string             `json:"model"`
	MaxTokens   int                `json:"max_tokens"`
	Temperature *float64           `json:"temperature,omitempty"`
	System      any                `json:"system,omitempty"`
	Messages    []anthropicMessage `json:"messages"`
	Tools       []anthropicTool    `json:"tools,omitempty"`
	Stream      bool               `json:"stream,omitempty"`
	Thinking    *anthropicThinking `json:"thinking,omitempty"`
}

// Chat 发送对话请求。onDelta 非 nil 时以流式方式请求。
func (p *AnthropicProvider) Chat(ctx context.Context, req *ChatRequest, onDelta func(Delta) error) (*ChatResponse, error) {
	if req == nil {
		return nil, fmt.Errorf("anthropic: ChatRequest 不能为 nil")
	}
	if p.apiKey == "" {
		return nil, fmt.Errorf("anthropic: apiKey 为空，无法发起请求")
	}
	model := req.Model
	if model == "" {
		model = p.model
	}
	if model == "" {
		return nil, fmt.Errorf("anthropic: 未指定模型名称")
	}
	maxTokens := req.MaxTokens
	if maxTokens <= 0 {
		maxTokens = p.maxTokens
	}

	body := anthropicRequest{
		Model:       model,
		MaxTokens:   maxTokens,
		Messages:    buildAnthropicMessages(req.Messages),
		Stream:      onDelta != nil,
		Temperature: req.Temperature,
	}
	if body.Temperature == nil {
		body.Temperature = p.temperature
	}
	if sys, ok := buildAnthropicSystem(req.Messages); ok {
		body.System = sys
	}
	if req.JSONMode {
		// Anthropic 协议没有统一的 response_format 字段，JSON 模式用
		// 系统提示约束输出（对所有模型生效的通用兜底）。
		body.System = withJSONInstruction(body.System)
	}
	if len(req.Tools) > 0 {
		body.Tools = make([]anthropicTool, 0, len(req.Tools))
		for _, t := range req.Tools {
			body.Tools = append(body.Tools, anthropicTool{
				Name:        t.Name,
				Description: t.Description,
				InputSchema: t.Schema,
			})
		}
	}

	thinkingBudget := p.thinkingBudget
	if req.Thinking && thinkingBudget == 0 {
		thinkingBudget = maxTokens / 2
		if thinkingBudget < 1024 {
			thinkingBudget = 1024
		}
	}
	if thinkingBudget > 0 {
		body.Thinking = &anthropicThinking{Type: "enabled", BudgetTokens: thinkingBudget}
		// Anthropic API 禁止 thinking 与 temperature 同时设置（会返回 400），
		// 启用思考模式时丢弃温度参数。
		body.Temperature = nil
	}

	payload, err := json.Marshal(body)
	if err != nil {
		return nil, fmt.Errorf("anthropic: 序列化请求体失败: %w", err)
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, p.baseURL+"/v1/messages", bytes.NewReader(payload))
	if err != nil {
		return nil, fmt.Errorf("anthropic: 构造请求失败: %w", err)
	}
	httpReq.Header.Set("content-type", "application/json")
	httpReq.Header.Set("x-api-key", p.apiKey)
	httpReq.Header.Set("anthropic-version", anthropicVersion)

	resp, err := p.client.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("anthropic: 请求失败: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		errBody, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return nil, fmt.Errorf("anthropic: HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(errBody)))
	}
	if onDelta != nil {
		return p.chatStream(resp.Body, onDelta)
	}
	return p.chatUnary(resp.Body)
}

// buildAnthropicSystem 提取所有 system 消息为顶层 system 字段。
// 单条文本返回字符串，多条或含图片返回内容块数组。
func buildAnthropicSystem(messages []Message) (any, bool) {
	var blocks []anthropicContent
	for _, m := range messages {
		if m.Role != RoleSystem {
			continue
		}
		for _, c := range m.Content {
			switch c.Type {
			case ContentText:
				blocks = append(blocks, anthropicContent{Type: "text", Text: c.Text})
			case ContentImage:
				blocks = append(blocks, anthropicContent{Type: "image", Source: anthropicImageSource(c)})
			}
		}
	}
	if len(blocks) == 0 {
		return nil, false
	}
	if len(blocks) == 1 && blocks[0].Type == "text" {
		return blocks[0].Text, true
	}
	return blocks, true
}

// jsonOutputInstruction 是 JSON 模式下追加到 system 的输出约束。
const jsonOutputInstruction = "无论被问什么，你的输出必须是一个合法的 JSON 值，" +
	"不要输出任何解释、注释或 Markdown 代码块围栏。"

// withJSONInstruction 在 system 内容前追加 JSON 输出约束，
// 兼容 system 的三种形态：nil / 字符串 / 内容块数组。
func withJSONInstruction(system any) any {
	switch v := system.(type) {
	case nil:
		return jsonOutputInstruction
	case string:
		return jsonOutputInstruction + "\n\n" + v
	case []anthropicContent:
		return append([]anthropicContent{{Type: "text", Text: jsonOutputInstruction}}, v...)
	default:
		return system
	}
}

// buildAnthropicMessages 将统一消息映射为 Anthropic 消息数组。
// 关键差异：tool 结果放在 role=user 的消息里，连续的 tool 结果合并为一条 user 消息。
func buildAnthropicMessages(messages []Message) []anthropicMessage {
	var out []anthropicMessage
	for i := 0; i < len(messages); i++ {
		m := messages[i]
		switch m.Role {
		case RoleSystem:
			// system 已在顶层字段处理
		case RoleUser:
			out = append(out, anthropicMessage{Role: "user", Content: buildUserContent(m.Content)})
		case RoleAssistant:
			out = append(out, anthropicMessage{Role: "assistant", Content: buildAssistantContent(m)})
		case RoleTool:
			var content []anthropicContent
			for i < len(messages) && messages[i].Role == RoleTool {
				tm := messages[i]
				content = append(content, anthropicContent{
					Type:      "tool_result",
					ToolUseID: tm.ToolCallID,
					Result:    joinTextContents(tm.Content),
				})
				i++
			}
			out = append(out, anthropicMessage{Role: "user", Content: content})
			i-- // 抵消外层循环自增
		}
	}
	return out
}

// buildUserContent 构造 user 消息内容块（text / image）。
func buildUserContent(content []Content) []anthropicContent {
	var blocks []anthropicContent
	for _, c := range content {
		switch c.Type {
		case ContentText:
			blocks = append(blocks, anthropicContent{Type: "text", Text: c.Text})
		case ContentImage:
			blocks = append(blocks, anthropicContent{Type: "image", Source: anthropicImageSource(c)})
		}
	}
	return blocks
}

// buildAssistantContent 构造 assistant 消息内容块。
// 顺序：文本块、思考块（前置在 tool_use 前）、tool_use 块。
func buildAssistantContent(m Message) []anthropicContent {
	var blocks []anthropicContent
	for _, c := range m.Content {
		switch c.Type {
		case ContentText:
			blocks = append(blocks, anthropicContent{Type: "text", Text: c.Text})
		case ContentThinking:
			blocks = append(blocks, anthropicContent{Type: "thinking", Thinking: c.Thinking})
		}
	}
	for _, tc := range m.ToolCalls {
		blocks = append(blocks, anthropicContent{Type: "tool_use", ID: tc.ID, Name: tc.Name, Input: tc.Arguments})
	}
	return blocks
}

// anthropicImageSource 将统一图片内容转换为 Anthropic source。
func anthropicImageSource(c Content) *anthropicSource {
	if len(c.Data) > 0 {
		mime := c.MIME
		if mime == "" {
			mime = "image/png"
		}
		return &anthropicSource{Type: "base64", MediaType: mime, Data: base64.StdEncoding.EncodeToString(c.Data)}
	}
	return &anthropicSource{Type: "url", URL: c.URL}
}

// joinTextContents 拼接消息中所有文本块。
func joinTextContents(content []Content) string {
	var sb strings.Builder
	for _, c := range content {
		if c.Type == ContentText {
			if sb.Len() > 0 {
				sb.WriteByte('\n')
			}
			sb.WriteString(c.Text)
		}
	}
	return sb.String()
}

// anthropicResponse 是非流式响应。
type anthropicResponse struct {
	Content    []anthropicContent `json:"content"`
	StopReason string             `json:"stop_reason"`
	Usage      anthropicUsage     `json:"usage"`
}

// anthropicUsage 是 token 用量。
type anthropicUsage struct {
	InputTokens           int `json:"input_tokens"`
	OutputTokens          int `json:"output_tokens"`
	CacheReadInputTokens  int `json:"cache_read_input_tokens"`
	CacheWriteInputTokens int `json:"cache_write_input_tokens"`
}

// chatUnary 解析非流式响应。
func (p *AnthropicProvider) chatUnary(body io.Reader) (*ChatResponse, error) {
	var wire anthropicResponse
	if err := json.NewDecoder(body).Decode(&wire); err != nil {
		return nil, fmt.Errorf("anthropic: 解析响应失败: %w", err)
	}
	return wire.toChatResponse()
}

// toChatResponse 将 wire 响应转换为统一 ChatResponse。
func (r anthropicResponse) toChatResponse() (*ChatResponse, error) {
	msg := Message{Role: RoleAssistant}
	for _, b := range r.Content {
		switch b.Type {
		case "text":
			msg.Content = append(msg.Content, Content{Type: ContentText, Text: b.Text})
		case "thinking":
			msg.Content = append(msg.Content, Content{Type: ContentThinking, Thinking: b.Thinking})
		case "tool_use":
			msg.ToolCalls = append(msg.ToolCalls, ToolCall{ID: b.ID, Name: b.Name, Arguments: b.Input})
		}
	}
	return &ChatResponse{
		Message:      msg,
		Usage:        r.Usage.toUsage(),
		FinishReason: mapAnthropicFinishReason(r.StopReason),
	}, nil
}

func (u anthropicUsage) toUsage() Usage {
	return Usage{
		InputTokens:      u.InputTokens,
		OutputTokens:     u.OutputTokens,
		CacheReadTokens:  u.CacheReadInputTokens,
		CacheWriteTokens: u.CacheWriteInputTokens,
	}
}

// mapAnthropicFinishReason 将 Anthropic stop_reason 映射为统一取值。
func mapAnthropicFinishReason(reason string) string {
	switch reason {
	case "end_turn":
		return "stop"
	case "tool_use":
		return "tool_calls"
	case "max_tokens":
		return "length"
	default:
		return reason
	}
}

// anthropicStreamEvent 是 SSE 流事件。
type anthropicStreamEvent struct {
	Type         string                 `json:"type"`
	Index        int                    `json:"index"`
	ContentBlock *anthropicContent      `json:"content_block"`
	Delta        *anthropicDelta        `json:"delta"`
	Message      *anthropicMessageStart `json:"message"`
	Usage        *anthropicUsage        `json:"usage"`
	Error        *anthropicStreamError  `json:"error"`
}

// anthropicMessageStart 是 message_start 事件中的 message 对象。
type anthropicMessageStart struct {
	Usage anthropicUsage `json:"usage"`
}

// anthropicDelta 是 content_block_delta / message_delta 的增量。
type anthropicDelta struct {
	Type        string `json:"type"`
	Text        string `json:"text"`
	Thinking    string `json:"thinking"`
	PartialJSON string `json:"partial_json"`
	StopReason  string `json:"stop_reason"`
}

// anthropicStreamError 是 error 事件。
type anthropicStreamError struct {
	Type    string `json:"type"`
	Message string `json:"message"`
}

// streamState 累积流式事件的状态。
type streamState struct {
	blocks     []anthropicContent
	usage      anthropicUsage
	stopReason string
}

// chatStream 解析 SSE 流并回调增量。
func (p *AnthropicProvider) chatStream(body io.Reader, onDelta func(Delta) error) (*ChatResponse, error) {
	state := &streamState{}
	reader := bufio.NewReader(body)
	var eventName string
	var dataBuf strings.Builder

	flush := func() (bool, error) {
		if eventName == "" || dataBuf.Len() == 0 {
			return false, nil
		}
		done, err := p.handleStreamEvent(eventName, dataBuf.String(), state, onDelta)
		eventName = ""
		dataBuf.Reset()
		return done, err
	}

	for {
		line, err := reader.ReadString('\n')
		if len(line) > 0 {
			line = strings.TrimRight(line, "\r\n")
			switch {
			case strings.HasPrefix(line, "event:"):
				eventName = strings.TrimSpace(strings.TrimPrefix(line, "event:"))
			case strings.HasPrefix(line, "data:"):
				data := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
				if dataBuf.Len() > 0 {
					dataBuf.WriteByte('\n')
				}
				dataBuf.WriteString(data)
			case line == "":
				if done, serr := flush(); serr != nil {
					return nil, serr
				} else if done {
					return p.finishStream(state)
				}
			}
		}
		if err != nil {
			if err == io.EOF {
				if done, serr := flush(); serr != nil {
					return nil, serr
				} else if done {
					return p.finishStream(state)
				}
				break
			}
			return nil, fmt.Errorf("anthropic: 读取流失败: %w", err)
		}
	}
	return p.finishStream(state)
}

// handleStreamEvent 处理单个 SSE 事件，返回是否到达 message_stop。
func (p *AnthropicProvider) handleStreamEvent(eventName, data string, state *streamState, onDelta func(Delta) error) (bool, error) {
	if eventName == "ping" {
		return false, nil
	}
	var ev anthropicStreamEvent
	if err := json.Unmarshal([]byte(data), &ev); err != nil {
		return false, fmt.Errorf("anthropic: 解析流事件 %s 失败: %w", eventName, err)
	}
	switch ev.Type {
	case "message_start":
		if ev.Message != nil {
			state.usage.InputTokens = ev.Message.Usage.InputTokens
			state.usage.CacheReadInputTokens = ev.Message.Usage.CacheReadInputTokens
			state.usage.CacheWriteInputTokens = ev.Message.Usage.CacheWriteInputTokens
		}
	case "content_block_start":
		if ev.ContentBlock != nil {
			state.blocks = append(state.blocks, *ev.ContentBlock)
		}
	case "content_block_delta":
		if ev.Delta == nil || ev.Index >= len(state.blocks) {
			return false, nil
		}
		block := &state.blocks[ev.Index]
		switch ev.Delta.Type {
		case "text_delta":
			block.Text += ev.Delta.Text
			if onDelta != nil {
				if err := onDelta(Delta{Text: ev.Delta.Text}); err != nil {
					return false, err
				}
			}
		case "thinking_delta":
			block.Thinking += ev.Delta.Thinking
			if onDelta != nil {
				if err := onDelta(Delta{Thinking: ev.Delta.Thinking}); err != nil {
					return false, err
				}
			}
		case "input_json_delta":
			block.rawInput += ev.Delta.PartialJSON
		}
	case "content_block_stop":
		if ev.Index < len(state.blocks) {
			finalizeAnthropicInput(&state.blocks[ev.Index])
		}
	case "message_delta":
		if ev.Delta != nil && ev.Delta.StopReason != "" {
			state.stopReason = ev.Delta.StopReason
		}
		// message_delta 事件中的 usage 是累计值（文档明确），直接赋值而非累加，
		// 避免重复计费统计。
		if ev.Usage != nil {
			state.usage.OutputTokens = ev.Usage.OutputTokens
		}
	case "message_stop":
		if onDelta != nil {
			if err := onDelta(Delta{Done: true}); err != nil {
				return false, err
			}
		}
		return true, nil
	case "error":
		if ev.Error != nil {
			return false, fmt.Errorf("anthropic: 流错误: %s (%s)", ev.Error.Message, ev.Error.Type)
		}
		return false, fmt.Errorf("anthropic: 未知流错误")
	}
	return false, nil
}

// finalizeAnthropicInput 将累积的 input_json_delta 反序列化为 tool_use 的 Input。
func finalizeAnthropicInput(block *anthropicContent) {
	if block.Type != "tool_use" || strings.TrimSpace(block.rawInput) == "" {
		return
	}
	args := map[string]any{}
	if err := json.Unmarshal([]byte(block.rawInput), &args); err != nil {
		// 流式参数拼接失败时保留空参数，不中断会话
		args = map[string]any{}
	}
	block.Input = args
	block.rawInput = ""
}

// finishStream 将累积的流状态组装为完整响应。
func (p *AnthropicProvider) finishStream(state *streamState) (*ChatResponse, error) {
	wire := anthropicResponse{
		Content:    state.blocks,
		StopReason: state.stopReason,
		Usage:      state.usage,
	}
	// 流未正常收到 content_block_stop 时兜底解析
	for i := range wire.Content {
		if wire.Content[i].rawInput != "" {
			finalizeAnthropicInput(&wire.Content[i])
		}
	}
	return wire.toChatResponse()
}
