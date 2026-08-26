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
	// defaultGeminiBaseURL 是 Gemini API 的基础地址。
	defaultGeminiBaseURL = "https://generativelanguage.googleapis.com"
	// defaultGeminiModel 是默认模型名称。
	defaultGeminiModel = "gemini-2.5-flash"
)

// GeminiProvider 是基于 Google Gemini 协议的 Provider 实现。
//
// Gemini 与 OpenAI 的关键差异：角色只有 user / model；函数调用通过
// function_call / function_response part 传递而非顶层字段；思考内容放在
// part.thought 中。
type GeminiProvider struct {
	apiKey        string
	baseURL       string
	client        *http.Client
	model         string
	maxTokens     int
	temperature   *float64
	contextWindow int
}

// NewGeminiProvider 构造 Gemini provider。
//
// opts 使用 protocol.go 中统一的 ProviderOption（WithBaseURL / WithHTTPClient /
// WithModel / WithMaxTokens / WithTemperature），避免各 provider 重复定义 Option。
func NewGeminiProvider(apiKey string, opts ...ProviderOption) *GeminiProvider {
	o := &ProviderOptions{}
	o.apply(opts)
	p := &GeminiProvider{
		apiKey:        apiKey,
		baseURL:       o.BaseURL,
		client:        o.HTTPClient,
		model:         o.Model,
		maxTokens:     o.MaxTokens,
		temperature:   o.Temperature,
		contextWindow: o.ContextWindow,
	}
	if p.baseURL == "" {
		p.baseURL = defaultGeminiBaseURL
	}
	if p.client == nil {
		p.client = http.DefaultClient
	}
	if p.model == "" {
		p.model = defaultGeminiModel
	}
	p.baseURL = strings.TrimSuffix(p.baseURL, "/")
	if p.contextWindow == 0 {
		p.contextWindow = GetContextWindow(p.model)
	}
	return p
}

// Name 返回 provider 名称。
func (p *GeminiProvider) Name() string           { return "gemini" }
func (p *GeminiProvider) ContextWindow() int      { return p.contextWindow }

// 编译期校验 GeminiProvider 满足 Provider 接口。
var _ Provider = (*GeminiProvider)(nil)

// Chat 发送一次对话请求。
//
// onDelta 非 nil 时使用流式接口（streamGenerateContent），每个增量都会回调；
// 无论是否流式，返回的 ChatResponse 都包含完整消息。onDelta 返回 error 时中止流。
func (p *GeminiProvider) Chat(ctx context.Context, req *ChatRequest, onDelta func(Delta) error) (*ChatResponse, error) {
	if req == nil {
		return nil, errors.New("gemini: req 不能为空")
	}
	if p.apiKey == "" {
		return nil, errors.New("gemini: apiKey 为空，无法发起请求")
	}

	model := req.Model
	if model == "" {
		model = p.model
	}
	if model == "" {
		return nil, errors.New("gemini: 未指定模型，请通过 req.Model 或 WithModel 设置")
	}

	wire, err := p.buildRequest(req, model)
	if err != nil {
		return nil, err
	}
	body, err := json.Marshal(wire)
	if err != nil {
		return nil, fmt.Errorf("gemini: 序列化请求体失败: %w", err)
	}

	streaming := onDelta != nil
	// 非流式：POST {base}/v1beta/models/{model}:generateContent?key={key}
	// 流式：   POST {base}/v1beta/models/{model}:streamGenerateContent?alt=sse&key={key}
	endpoint := p.baseURL + "/v1beta/models/" + model + ":generateContent?key=" + p.apiKey
	if streaming {
		endpoint = p.baseURL + "/v1beta/models/" + model + ":streamGenerateContent?alt=sse&key=" + p.apiKey
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("gemini: 构造请求失败: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	if streaming {
		httpReq.Header.Set("Accept", "text/event-stream")
	}

	resp, err := p.client.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("gemini: 请求失败: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		errBody, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return nil, fmt.Errorf("gemini: 非 200 响应 (%d): %s", resp.StatusCode, string(errBody))
	}

	if streaming {
		return p.readStream(resp.Body, onDelta)
	}
	return p.readOnce(resp.Body)
}

// buildRequest 将统一 ChatRequest 映射为 Gemini 请求体。
func (p *GeminiProvider) buildRequest(req *ChatRequest, model string) (*wireGeminiRequest, error) {
	wire := &wireGeminiRequest{}

	// systemInstruction：多条 system 消息的文本按换行拼接（Gemini 只接受单个系统指令）
	var sysTexts []string
	for _, m := range req.Messages {
		if m.Role != RoleSystem {
			continue
		}
		for _, c := range m.Content {
			if c.Type == ContentText && c.Text != "" {
				sysTexts = append(sysTexts, c.Text)
			}
		}
	}
	if len(sysTexts) > 0 {
		wire.SystemInstruction = &wireGeminiContent{Parts: []wireGeminiPart{{Text: strings.Join(sysTexts, "\n")}}}
	}

	// contents：记录 assistant 工具调用的 ID -> 名称，供后续 tool 消息回填 function_response 名称
	toolNames := make(map[string]string)
	for i := 0; i < len(req.Messages); i++ {
		m := req.Messages[i]
		switch m.Role {
		case RoleSystem:
			continue
		case RoleTool:
			// Gemini 无 tool 角色：工具结果以 user 角色的 function_response part 回传。
			// 同一轮 assistant 的多个并行工具调用，其结果必须合并进同一条 user 消息
			// 且顺序与 function_call 一致（Gemini API 要求，拆成多条会报错）。
			parts := make([]wireGeminiPart, 0, 2)
			for i < len(req.Messages) && req.Messages[i].Role == RoleTool {
				tm := req.Messages[i]
				name := toolNames[tm.ToolCallID]
				if name == "" {
					name = tm.ToolCallID // 找不到时回退用调用 ID 作为名称
				}
				parts = append(parts, wireGeminiPart{
					FunctionResponse: &wireGeminiFunctionResponse{
						Name:     name,
						Response: map[string]any{"result": geminiTextOf(tm)},
					},
				})
				i++
			}
			wire.Contents = append(wire.Contents, wireGeminiContent{Role: "user", Parts: parts})
			i-- // 抵消外层循环自增
		case RoleUser, RoleAssistant:
			role := "user"
			if m.Role == RoleAssistant {
				role = "model"
			}
			parts := make([]wireGeminiPart, 0, len(m.Content)+len(m.ToolCalls))
			for _, c := range m.Content {
				switch c.Type {
				case ContentText:
					if c.Text != "" {
						parts = append(parts, wireGeminiPart{Text: c.Text})
					}
				case ContentImage:
					part, err := geminiImagePart(c)
					if err != nil {
						return nil, err
					}
					if part != nil {
						parts = append(parts, *part)
					}
				case ContentThinking:
					// 思考内容仅作为模型输出展示，请求中不发送
				}
			}
			// assistant 的工具调用追加为 function_call part
			for _, tc := range m.ToolCalls {
				toolNames[tc.ID] = tc.Name
				parts = append(parts, wireGeminiPart{
					FunctionCall: &wireGeminiFunctionCall{Name: tc.Name, Args: tc.Arguments},
				})
			}
			if len(parts) > 0 {
				wire.Contents = append(wire.Contents, wireGeminiContent{Role: role, Parts: parts})
			}
		default:
			return nil, fmt.Errorf("gemini: 不支持的消息角色 %q", m.Role)
		}
	}
	// 注意：contents 可能为空（如仅用于测试或批量场景），交由 API 侧校验，这里不拦截。

	// tools：function_declarations 数组，为空则不发送
	if len(req.Tools) > 0 {
		decls := make([]wireGeminiFunctionDecl, 0, len(req.Tools))
		for _, t := range req.Tools {
			decls = append(decls, wireGeminiFunctionDecl{
				Name:        t.Name,
				Description: t.Description,
				Parameters:  t.Schema,
			})
		}
		wire.Tools = []wireGeminiTool{{FunctionDeclarations: decls}}
	}

	// generationConfig：温度 / 最大输出 token / 思考模式
	temperature := req.Temperature
	if temperature == nil {
		temperature = p.temperature
	}
	maxTokens := req.MaxTokens
	if maxTokens == 0 {
		maxTokens = p.maxTokens
	}
	if temperature != nil || maxTokens > 0 || req.Thinking {
		gc := &wireGeminiGenerationConfig{}
		if temperature != nil {
			gc.Temperature = temperature
		}
		if maxTokens > 0 {
			gc.MaxOutputTokens = maxTokens
		}
		if req.Thinking {
			gc.ThinkingConfig = &wireGeminiThinkingConfig{IncludeThoughts: true}
		}
		wire.GenerationConfig = gc
	}
	return wire, nil
}

// geminiImagePart 将图片内容转为 Gemini part：base64 数据用 inline_data，URL 用 file_data。
func geminiImagePart(c Content) (*wireGeminiPart, error) {
	if len(c.Data) > 0 {
		mime := c.MIME
		if mime == "" {
			mime = http.DetectContentType(c.Data)
		}
		return &wireGeminiPart{
			InlineData: &wireGeminiInlineData{
				MimeType: mime,
				Data:     base64.StdEncoding.EncodeToString(c.Data),
			},
		}, nil
	}
	if c.URL != "" {
		return &wireGeminiPart{
			FileData: &wireGeminiFileData{MimeType: c.MIME, FileURI: c.URL},
		}, nil
	}
	return nil, errors.New("gemini: 图片内容既无 Data 也无 URL")
}

// geminiTextOf 拼接消息中的所有文本块。
func geminiTextOf(m Message) string {
	var sb strings.Builder
	for _, c := range m.Content {
		if c.Type == ContentText {
			sb.WriteString(c.Text)
		}
	}
	return sb.String()
}

// readOnce 解析非流式响应。
func (p *GeminiProvider) readOnce(body io.Reader) (*ChatResponse, error) {
	var wire wireGeminiResponse
	if err := json.NewDecoder(body).Decode(&wire); err != nil {
		return nil, fmt.Errorf("gemini: 解析响应失败: %w", err)
	}

	resp := &ChatResponse{Usage: parseGeminiUsage(wire.UsageMetadata)}
	if len(wire.Candidates) == 0 {
		return resp, nil
	}
	cand := wire.Candidates[0]
	resp.FinishReason = mapGeminiFinishReason(cand.FinishReason)
	resp.Message = Message{Role: RoleAssistant}

	var text, thinking strings.Builder
	for _, part := range cand.Content.Parts {
		if part.Thought {
			thinking.WriteString(part.Text)
		} else if part.Text != "" {
			text.WriteString(part.Text)
		}
		if part.FunctionCall != nil {
			args, err := parseGeminiFunctionArgs(part.FunctionCall.Args)
			if err != nil {
				return nil, fmt.Errorf("gemini: 解析函数调用参数失败: %w", err)
			}
			resp.Message.ToolCalls = append(resp.Message.ToolCalls, ToolCall{
				ID:        fmt.Sprintf("call_%d", len(resp.Message.ToolCalls)+1),
				Name:      part.FunctionCall.Name,
				Arguments: args,
			})
		}
	}
	content := make([]Content, 0, 2)
	if text.Len() > 0 {
		content = append(content, Content{Type: ContentText, Text: text.String()})
	}
	if thinking.Len() > 0 {
		content = append(content, Content{Type: ContentThinking, Thinking: thinking.String()})
	}
	resp.Message.Content = content
	return resp, nil
}

// readStream 解析 SSE 流：text/thought 逐块回调，functionCall 的 args 是分片 JSON，
// 跨块累积到流结束后统一反序列化。
func (p *GeminiProvider) readStream(body io.Reader, onDelta func(Delta) error) (*ChatResponse, error) {
	text := new(strings.Builder)
	thinking := new(strings.Builder)
	var (
		calls []ToolCall
		call  *geminiStreamCall // 当前正在累积的函数调用
		seq   int               // 工具调用序号
	)
	accUsage := &wireGeminiUsageMetadata{}
	finish := ""
	resp := &ChatResponse{Message: Message{Role: RoleAssistant}}

	scanner := bufio.NewScanner(body)
	// 放大单行容量，避免大分片（如长文本）被截断
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

		var chunk wireGeminiResponse
		if err := json.Unmarshal([]byte(data), &chunk); err != nil {
			return nil, fmt.Errorf("gemini: 解析流式分片失败: %w", err)
		}
		// usageMetadata 取流中最后一块（分片可能重复携带累计值）
		if chunk.UsageMetadata != nil {
			accUsage = chunk.UsageMetadata
		}
		if len(chunk.Candidates) == 0 {
			continue
		}
		cand := chunk.Candidates[0]
		// finishReason 取最后一个分片的非空值
		if cand.FinishReason != "" {
			finish = cand.FinishReason
		}

		for _, part := range cand.Content.Parts {
			if part.Thought {
				if part.Text != "" {
					thinking.WriteString(part.Text)
					if err := onDelta(Delta{Thinking: part.Text}); err != nil {
						return nil, err // 回调报错即中止流
					}
				}
				continue
			}
			if part.Text != "" {
				text.WriteString(part.Text)
				if err := onDelta(Delta{Text: part.Text}); err != nil {
					return nil, err
				}
			}
			// functionCall 分片：首个分片带 name，后续分片只带 args 片段，直接拼接
			if part.FunctionCall != nil {
				fc := part.FunctionCall
				if call == nil {
					call = &geminiStreamCall{}
				}
				if fc.Name != "" && call.name != "" {
					// 新的函数调用开始，先收尾前一个
					if err := finalizeGeminiCall(call, &calls, &seq); err != nil {
						return nil, err
					}
					call = &geminiStreamCall{name: fc.Name}
				} else if fc.Name != "" {
					call.name = fc.Name
				}
				frag, err := geminiArgsFragment(fc.Args)
				if err != nil {
					return nil, fmt.Errorf("gemini: 解析函数调用参数分片失败: %w", err)
				}
				call.args = append(call.args, frag...)
			}
		}
	}

	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("gemini: 读取流失败: %w", err)
	}

	// 流结束：整体反序列化拼接后的参数
	if call != nil {
		if err := finalizeGeminiCall(call, &calls, &seq); err != nil {
			return nil, err
		}
	}
	resp.Message.ToolCalls = calls

	content := make([]Content, 0, 2)
	if text.Len() > 0 {
		content = append(content, Content{Type: ContentText, Text: text.String()})
	}
	if thinking.Len() > 0 {
		content = append(content, Content{Type: ContentThinking, Thinking: thinking.String()})
	}
	resp.Message.Content = content
	resp.Usage = parseGeminiUsage(accUsage)
	resp.FinishReason = mapGeminiFinishReason(finish)
	if err := onDelta(Delta{Done: true}); err != nil {
		return nil, err
	}
	return resp, nil
}

// geminiStreamCall 在流式过程中累积单个函数调用。
type geminiStreamCall struct {
	name string
	args []byte
}

// finalizeGeminiCall 将累积完成的函数调用反序列化为 ToolCall。
func finalizeGeminiCall(call *geminiStreamCall, calls *[]ToolCall, seq *int) error {
	*seq++
	args := map[string]any{}
	if len(bytes.TrimSpace(call.args)) > 0 {
		if err := json.Unmarshal(call.args, &args); err != nil {
			return fmt.Errorf("gemini: 拼接后的函数参数解析失败: %w", err)
		}
	}
	*calls = append(*calls, ToolCall{ID: fmt.Sprintf("call_%d", *seq), Name: call.name, Arguments: args})
	return nil
}

// geminiArgsFragment 提取单块函数参数片段：args 为 JSON 字符串（流式分片）时解包，
// 为对象（非流式）时原样返回。
func geminiArgsFragment(raw json.RawMessage) ([]byte, error) {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 {
		return nil, nil
	}
	if raw[0] == '"' {
		var s string
		if err := json.Unmarshal(raw, &s); err != nil {
			return nil, err
		}
		return []byte(s), nil
	}
	return raw, nil
}

// parseGeminiFunctionArgs 解析函数调用参数为对象，兼容 JSON 对象与 JSON 字符串两种形态。
func parseGeminiFunctionArgs(raw json.RawMessage) (map[string]any, error) {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 {
		return map[string]any{}, nil
	}
	if raw[0] == '"' {
		var s string
		if err := json.Unmarshal(raw, &s); err != nil {
			return nil, err
		}
		raw = []byte(s)
	}
	args := map[string]any{}
	if err := json.Unmarshal(raw, &args); err != nil {
		return nil, err
	}
	return args, nil
}

// mapGeminiFinishReason 将 Gemini 的 finishReason 映射为协议统一取值。
func mapGeminiFinishReason(reason string) string {
	switch reason {
	case "STOP":
		return "stop"
	case "MAX_TOKENS":
		return "length"
	case "SAFETY":
		return "content_filter"
	case "TOOL_CALLS":
		return "tool_calls"
	default:
		return strings.ToLower(reason)
	}
}

// parseGeminiUsage 映射 token 统计；usageMetadata 缺失时各字段置 0。
func parseGeminiUsage(u *wireGeminiUsageMetadata) Usage {
	if u == nil {
		return Usage{}
	}
	return Usage{
		InputTokens:     u.PromptTokenCount,
		OutputTokens:    u.CandidatesTokenCount,
		CacheReadTokens: u.CachedContentTokenCount,
	}
}

// —— 以下为与 Gemini API 协议对应的 wire 结构（请求）——

// wireGeminiRequest 是请求体的 wire 形态。
type wireGeminiRequest struct {
	SystemInstruction *wireGeminiContent          `json:"systemInstruction,omitempty"`
	Contents          []wireGeminiContent         `json:"contents"`
	Tools             []wireGeminiTool            `json:"tools,omitempty"`
	GenerationConfig  *wireGeminiGenerationConfig `json:"generationConfig,omitempty"`
}

// wireGeminiContent 是单条消息（contents 元素）或系统指令的 wire 形态。
type wireGeminiContent struct {
	Role  string           `json:"role,omitempty"`
	Parts []wireGeminiPart `json:"parts"`
}

// wireGeminiPart 是消息中的单块内容 part。
type wireGeminiPart struct {
	Text             string                      `json:"text,omitempty"`
	InlineData       *wireGeminiInlineData       `json:"inline_data,omitempty"`
	FileData         *wireGeminiFileData         `json:"file_data,omitempty"`
	FunctionCall     *wireGeminiFunctionCall     `json:"function_call,omitempty"`
	FunctionResponse *wireGeminiFunctionResponse `json:"function_response,omitempty"`
}

// wireGeminiInlineData 是 base64 图片数据的 wire 形态。
type wireGeminiInlineData struct {
	MimeType string `json:"mime_type"`
	Data     string `json:"data"`
}

// wireGeminiFileData 是 URL 图片引用的 wire 形态。
type wireGeminiFileData struct {
	MimeType string `json:"mime_type,omitempty"`
	FileURI  string `json:"file_uri"`
}

// wireGeminiFunctionCall 是请求中的函数调用 part。
type wireGeminiFunctionCall struct {
	Name string         `json:"name"`
	Args map[string]any `json:"args,omitempty"`
}

// wireGeminiFunctionResponse 是工具执行结果 part。
type wireGeminiFunctionResponse struct {
	Name     string         `json:"name"`
	Response map[string]any `json:"response"`
}

// wireGeminiTool 是工具声明的 wire 形态。
type wireGeminiTool struct {
	FunctionDeclarations []wireGeminiFunctionDecl `json:"function_declarations"`
}

// wireGeminiFunctionDecl 描述一个可调用的函数。
type wireGeminiFunctionDecl struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	Parameters  map[string]any `json:"parameters,omitempty"`
}

// wireGeminiGenerationConfig 是生成参数配置。
type wireGeminiGenerationConfig struct {
	Temperature     *float64                  `json:"temperature,omitempty"`
	MaxOutputTokens int                       `json:"maxOutputTokens,omitempty"`
	ThinkingConfig  *wireGeminiThinkingConfig `json:"thinkingConfig,omitempty"`
}

// wireGeminiThinkingConfig 是思考模式配置。
type wireGeminiThinkingConfig struct {
	IncludeThoughts bool `json:"includeThoughts"`
}

// —— 以下为与 Gemini API 协议对应的 wire 结构（响应，非流式与 SSE 流共用）——

// wireGeminiResponse 是响应的 wire 形态。
type wireGeminiResponse struct {
	Candidates    []wireGeminiCandidate    `json:"candidates"`
	UsageMetadata *wireGeminiUsageMetadata `json:"usageMetadata,omitempty"`
}

// wireGeminiCandidate 是单个候选结果。
type wireGeminiCandidate struct {
	Content      wireGeminiResponseContent `json:"content"`
	FinishReason string                    `json:"finishReason"`
}

// wireGeminiResponseContent 是响应消息内容。
type wireGeminiResponseContent struct {
	Parts []wireGeminiResponsePart `json:"parts"`
}

// wireGeminiResponsePart 是响应中的单块内容 part。
type wireGeminiResponsePart struct {
	Text         string                          `json:"text,omitempty"`
	Thought      bool                            `json:"thought,omitempty"`
	FunctionCall *wireGeminiResponseFunctionCall `json:"functionCall,omitempty"`
}

// wireGeminiResponseFunctionCall 是响应中的函数调用（args 可能是对象或字符串分片）。
type wireGeminiResponseFunctionCall struct {
	Name string          `json:"name,omitempty"`
	Args json.RawMessage `json:"args,omitempty"`
}

// wireGeminiUsageMetadata 是 token 统计。
type wireGeminiUsageMetadata struct {
	PromptTokenCount        int `json:"promptTokenCount"`
	CandidatesTokenCount    int `json:"candidatesTokenCount"`
	CachedContentTokenCount int `json:"cachedContentTokenCount"`
}
