package llm

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// newTestProvider 构造指向 mock 服务器的 provider。
func newTestProvider(srv *httptest.Server) *OpenAIProvider {
	return NewOpenAIProvider("test-key", WithBaseURL(srv.URL))
}

// TestOpenAI_NonStreaming_ToolCalls 校验非流式工具调用解析（arguments 为 JSON 字符串需反序列化）。
func TestOpenAI_NonStreaming_ToolCalls(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer test-key" {
			t.Errorf("Authorization 头错误: %q", got)
		}
		if got := r.Header.Get("Content-Type"); got != "application/json" {
			t.Errorf("Content-Type 头错误: %q", got)
		}
		if r.URL.Path != "/chat/completions" {
			t.Errorf("请求路径错误: %s", r.URL.Path)
		}
		fmt.Fprint(w, `{
			"choices": [{
				"message": {
					"role": "assistant",
					"content": "需要查询天气",
					"tool_calls": [{
						"id": "call_1",
						"type": "function",
						"function": {"name": "get_weather", "arguments": "{\"city\":\"北京\",\"unit\":\"celsius\"}"}
					}]
				},
				"finish_reason": "tool_calls"
			}],
			"usage": {"prompt_tokens": 100, "completion_tokens": 50}
		}`)
	}))
	defer srv.Close()

	resp, err := newTestProvider(srv).Chat(context.Background(), &ChatRequest{Model: "m"}, nil)
	if err != nil {
		t.Fatalf("Chat 出错: %v", err)
	}
	if resp.FinishReason != "tool_calls" {
		t.Errorf("FinishReason 应为 tool_calls，实际 %q", resp.FinishReason)
	}
	if len(resp.Message.Content) != 1 || resp.Message.Content[0].Text != "需要查询天气" {
		t.Errorf("文本内容错误: %+v", resp.Message.Content)
	}
	if len(resp.Message.ToolCalls) != 1 {
		t.Fatalf("应解析出 1 个工具调用，实际 %d", len(resp.Message.ToolCalls))
	}
	tc := resp.Message.ToolCalls[0]
	if tc.ID != "call_1" || tc.Name != "get_weather" {
		t.Errorf("工具调用 ID/Name 错误: %+v", tc)
	}
	if tc.Arguments["city"] != "北京" || tc.Arguments["unit"] != "celsius" {
		t.Errorf("工具参数解析错误: %+v", tc.Arguments)
	}
	if resp.Usage.InputTokens != 100 || resp.Usage.OutputTokens != 50 {
		t.Errorf("usage 解析错误: %+v", resp.Usage)
	}
}

// TestOpenAI_NonStreaming_EmptyArguments 校验空字符串/无效 arguments 时置空 map 而非报错。
func TestOpenAI_NonStreaming_EmptyArguments(t *testing.T) {
	for name, args := range map[string]string{
		"空字符串":   `""`,
		"无效JSON": `"{not json"`,
	} {
		t.Run(name, func(t *testing.T) {
			body := `{"choices":[{"message":{"role":"assistant","content":"","tool_calls":[{"id":"x","type":"function","function":{"name":"f","arguments":` + args + `}}]},"finish_reason":"tool_calls"}]}`
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				fmt.Fprint(w, body)
			}))
			defer srv.Close()

			resp, err := newTestProvider(srv).Chat(context.Background(), &ChatRequest{Model: "m"}, nil)
			if err != nil {
				t.Fatalf("Chat 不应报错: %v", err)
			}
			if len(resp.Message.ToolCalls) != 1 {
				t.Fatalf("应解析出 1 个工具调用")
			}
			if len(resp.Message.ToolCalls[0].Arguments) != 0 {
				t.Errorf("arguments 应为空 map，实际 %v", resp.Message.ToolCalls[0].Arguments)
			}
		})
	}
}

// TestOpenAI_Streaming_ToolCallAssembly 校验流式 tool_calls 增量组装：
// arguments 分片直接拼接，id 只在首个分片出现，多个 index 分别累积。
func TestOpenAI_Streaming_ToolCallAssembly(t *testing.T) {
	var gotBody []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotBody, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "text/event-stream")
		for _, line := range []string{
			`data: {"choices":[{"delta":{"role":"assistant","content":"我来查","tool_calls":[{"index":0,"id":"call_a","type":"function","function":{"name":"get_weather","arguments":""}}]},"finish_reason":null}]}`,
			`data: {"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":"{\"city\":"}}]},"finish_reason":null}]}`,
			`data: {"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":"\"北京\"}"}},{"index":1,"id":"call_b","type":"function","function":{"name":"get_time","arguments":""}}]},"finish_reason":null}]}`,
			`data: {"choices":[{"delta":{"tool_calls":[{"index":1,"function":{"arguments":"{\"tz\":\"Asia/Shanghai\"}"}}]},"finish_reason":null}]}`,
			`data: {"choices":[{"delta":{},"finish_reason":"tool_calls"}]}`,
			`data: [DONE]`,
		} {
			fmt.Fprintln(w, line)
		}
	}))
	defer srv.Close()

	var gotDeltas []Delta
	resp, err := newTestProvider(srv).Chat(context.Background(), &ChatRequest{Model: "m"}, func(d Delta) error {
		gotDeltas = append(gotDeltas, d)
		return nil
	})
	if err != nil {
		t.Fatalf("Chat 出错: %v", err)
	}

	// 请求体应为流式
	var sent struct {
		Stream bool `json:"stream"`
	}
	if err := json.Unmarshal(gotBody, &sent); err != nil {
		t.Fatalf("解析请求体失败: %v", err)
	}
	if !sent.Stream {
		t.Error("请求体 stream 应为 true")
	}

	// 文本增量实时回调
	var streamedText string
	for _, d := range gotDeltas {
		streamedText += d.Text
	}
	if streamedText != "我来查" {
		t.Errorf("流式文本增量错误: %q", streamedText)
	}

	// 完整回复：文本 + 按 index 组装的工具调用
	if len(resp.Message.Content) != 1 || resp.Message.Content[0].Text != "我来查" {
		t.Errorf("文本内容错误: %+v", resp.Message.Content)
	}
	if resp.FinishReason != "tool_calls" {
		t.Errorf("FinishReason 应为 tool_calls，实际 %q", resp.FinishReason)
	}
	if len(resp.Message.ToolCalls) != 2 {
		t.Fatalf("应组装出 2 个工具调用，实际 %d", len(resp.Message.ToolCalls))
	}
	a, b := resp.Message.ToolCalls[0], resp.Message.ToolCalls[1]
	if a.ID != "call_a" || a.Name != "get_weather" {
		t.Errorf("工具调用 a 错误: %+v", a)
	}
	if a.Arguments["city"] != "北京" {
		t.Errorf("工具调用 a 参数拼接错误: %v", a.Arguments)
	}
	if b.ID != "call_b" || b.Name != "get_time" {
		t.Errorf("工具调用 b 错误: %+v", b)
	}
	if b.Arguments["tz"] != "Asia/Shanghai" {
		t.Errorf("工具调用 b 参数错误: %v", b.Arguments)
	}
}

// TestOpenAI_Streaming_ReasoningContent 校验流式 reasoning_content 透传到 Thinking，
// 以及流末分片 usage 的收集。
func TestOpenAI_Streaming_ReasoningContent(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		for _, line := range []string{
			`data: {"choices":[{"delta":{"reasoning_content":"先看条件"},"finish_reason":null}]}`,
			`data: {"choices":[{"delta":{"reasoning_content":"再计算"},"finish_reason":null}]}`,
			`data: {"choices":[{"delta":{"content":"答案是 42"},"finish_reason":null}]}`,
			`data: {"choices":[{"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":30,"completion_tokens":9,"prompt_cache_hit_tokens":20,"prompt_cache_miss_tokens":10}}`,
			`data: [DONE]`,
		} {
			fmt.Fprintln(w, line)
		}
	}))
	defer srv.Close()

	var gotThinking []string
	resp, err := newTestProvider(srv).Chat(context.Background(), &ChatRequest{Model: "m"}, func(d Delta) error {
		if d.Thinking != "" {
			gotThinking = append(gotThinking, d.Thinking)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("Chat 出错: %v", err)
	}

	if strings.Join(gotThinking, "") != "先看条件再计算" {
		t.Errorf("思考增量透传错误: %v", gotThinking)
	}
	if len(resp.Message.Content) != 2 {
		t.Fatalf("应有文本与思考两个内容块，实际 %+v", resp.Message.Content)
	}
	if resp.Message.Content[0].Type != ContentText || resp.Message.Content[0].Text != "答案是 42" {
		t.Errorf("文本内容错误: %+v", resp.Message.Content[0])
	}
	if resp.Message.Content[1].Type != ContentThinking || resp.Message.Content[1].Thinking != "先看条件再计算" {
		t.Errorf("思考内容错误: %+v", resp.Message.Content[1])
	}
	if resp.FinishReason != "stop" {
		t.Errorf("FinishReason 应为 stop，实际 %q", resp.FinishReason)
	}
	if resp.Usage.InputTokens != 30 || resp.Usage.OutputTokens != 9 ||
		resp.Usage.CacheReadTokens != 20 || resp.Usage.CacheWriteTokens != 10 {
		t.Errorf("流式 usage 解析错误: %+v", resp.Usage)
	}
}

// TestOpenAI_Streaming_AbortOnDeltaError 校验 onDelta 返回 error 时中止流。
func TestOpenAI_Streaming_AbortOnDeltaError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		for i := 0; i < 10; i++ {
			fmt.Fprintln(w, `data: {"choices":[{"delta":{"content":"x"},"finish_reason":null}]}`)
		}
		fmt.Fprintln(w, `data: [DONE]`)
	}))
	defer srv.Close()

	deltaCount := 0
	_, err := newTestProvider(srv).Chat(context.Background(), &ChatRequest{Model: "m"}, func(d Delta) error {
		deltaCount++
		if deltaCount == 2 {
			return errors.New("stop-stream")
		}
		return nil
	})
	if err == nil || !strings.Contains(err.Error(), "stop-stream") {
		t.Errorf("onDelta 报错时应返回该错误，实际 %v", err)
	}
}

// TestOpenAI_MultiTurnRequest 校验多轮对话（系统/用户/助手工具调用/tool 结果/图片消息）
// 请求体 messages 数组的序列化正确性。
func TestOpenAI_MultiTurnRequest(t *testing.T) {
	var gotBody []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotBody, _ = io.ReadAll(r.Body)
		fmt.Fprint(w, `{"choices":[{"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{}}`)
	}))
	defer srv.Close()

	temp := 0.7
	req := &ChatRequest{
		Model: "test-model",
		Messages: []Message{
			NewTextMessage(RoleSystem, "你是助手"),
			NewTextMessage(RoleUser, "今天天气？"),
			{
				Role:    RoleAssistant,
				Content: []Content{{Type: ContentText, Text: "让我查一下"}},
				ToolCalls: []ToolCall{{
					ID:        "call_1",
					Name:      "get_weather",
					Arguments: map[string]any{"city": "北京"},
				}},
			},
			NewToolResultMessage("call_1", "晴"),
			{
				Role: RoleUser,
				Content: []Content{
					{Type: ContentText, Text: "看看这张图"},
					{Type: ContentImage, Data: []byte{0x89, 0x50, 0x4e, 0x47, 0x0d, 0x0a, 0x1a, 0x0a}, MIME: "image/png"},
					{Type: ContentImage, URL: "https://example.com/a.png"},
				},
			},
		},
		Tools: []ToolParam{{
			Name:        "get_weather",
			Description: "查询天气",
			Schema:      map[string]any{"type": "object"},
		}},
		Temperature: &temp,
		MaxTokens:   256,
	}

	if _, err := newTestProvider(srv).Chat(context.Background(), req, nil); err != nil {
		t.Fatalf("Chat 出错: %v", err)
	}

	var sent struct {
		Model    string `json:"model"`
		Messages []struct {
			Role      string          `json:"role"`
			Content   json.RawMessage `json:"content"`
			ToolCalls []struct {
				ID       string `json:"id"`
				Type     string `json:"type"`
				Function struct {
					Name      string `json:"name"`
					Arguments string `json:"arguments"`
				} `json:"function"`
			} `json:"tool_calls"`
			ToolCallID string `json:"tool_call_id"`
		} `json:"messages"`
		Tools []struct {
			Type     string `json:"type"`
			Function struct {
				Name        string         `json:"name"`
				Description string         `json:"description"`
				Parameters  map[string]any `json:"parameters"`
			} `json:"function"`
		} `json:"tools"`
		Stream      bool    `json:"stream"`
		Temperature float64 `json:"temperature"`
		MaxTokens   int     `json:"max_tokens"`
	}
	if err := json.Unmarshal(gotBody, &sent); err != nil {
		t.Fatalf("解析请求体失败: %v", err)
	}

	if sent.Model != "test-model" || sent.Stream || sent.Temperature != 0.7 || sent.MaxTokens != 256 {
		t.Errorf("请求体顶层字段错误: %+v", sent)
	}
	if len(sent.Messages) != 5 {
		t.Fatalf("messages 数量应为 5，实际 %d", len(sent.Messages))
	}

	// messages[0]：system 单字符串 content
	if sent.Messages[0].Role != "system" {
		t.Errorf("messages[0] role 错误: %q", sent.Messages[0].Role)
	}
	var systemContent string
	if err := json.Unmarshal(sent.Messages[0].Content, &systemContent); err != nil || systemContent != "你是助手" {
		t.Errorf("messages[0] content 错误: %v %q", err, systemContent)
	}

	// messages[1]：user 纯文本单字符串
	if sent.Messages[1].Role != "user" {
		t.Errorf("messages[1] role 错误: %q", sent.Messages[1].Role)
	}
	var userText string
	if err := json.Unmarshal(sent.Messages[1].Content, &userText); err != nil || userText != "今天天气？" {
		t.Errorf("messages[1] content 错误: %v %q", err, userText)
	}

	// messages[2]：assistant 带 tool_calls，arguments 为 JSON 字符串
	asst := sent.Messages[2]
	if asst.Role != "assistant" {
		t.Errorf("messages[2] role 错误: %q", asst.Role)
	}
	var asstText string
	if err := json.Unmarshal(asst.Content, &asstText); err != nil || asstText != "让我查一下" {
		t.Errorf("messages[2] content 错误: %v %q", err, asstText)
	}
	if len(asst.ToolCalls) != 1 {
		t.Fatalf("messages[2] 应有 1 个 tool_calls")
	}
	tc := asst.ToolCalls[0]
	if tc.ID != "call_1" || tc.Type != "function" || tc.Function.Name != "get_weather" {
		t.Errorf("messages[2] tool_calls 错误: %+v", tc)
	}
	var tcArgs map[string]any
	if err := json.Unmarshal([]byte(tc.Function.Arguments), &tcArgs); err != nil || tcArgs["city"] != "北京" {
		t.Errorf("messages[2] arguments 应为 JSON 字符串且可解析: %v %q", err, tc.Function.Arguments)
	}

	// messages[3]：tool 结果回传 tool_call_id
	toolMsg := sent.Messages[3]
	if toolMsg.Role != "tool" || toolMsg.ToolCallID != "call_1" {
		t.Errorf("messages[3] 错误: %+v", toolMsg)
	}
	var toolText string
	if err := json.Unmarshal(toolMsg.Content, &toolText); err != nil || toolText != "晴" {
		t.Errorf("messages[3] content 错误: %v %q", err, toolText)
	}

	// messages[4]：user 含图片 → content 数组（文本 + base64 图片 + URL 图片）
	if sent.Messages[4].Role != "user" {
		t.Errorf("messages[4] role 错误: %q", sent.Messages[4].Role)
	}
	var imageParts []struct {
		Type     string `json:"type"`
		Text     string `json:"text"`
		ImageURL struct {
			URL string `json:"url"`
		} `json:"image_url"`
	}
	if err := json.Unmarshal(sent.Messages[4].Content, &imageParts); err != nil {
		t.Fatalf("messages[4] content 应为数组: %v", err)
	}
	if len(imageParts) != 3 {
		t.Fatalf("messages[4] 应有 3 个内容块，实际 %d", len(imageParts))
	}
	if imageParts[0].Type != "text" || imageParts[0].Text != "看看这张图" {
		t.Errorf("messages[4] 文本块错误: %+v", imageParts[0])
	}
	wantBase64 := "data:image/png;base64,iVBORw0KGgo="
	if imageParts[1].Type != "image_url" || imageParts[1].ImageURL.URL != wantBase64 {
		t.Errorf("messages[4] base64 图片错误: %+v", imageParts[1])
	}
	if imageParts[2].Type != "image_url" || imageParts[2].ImageURL.URL != "https://example.com/a.png" {
		t.Errorf("messages[4] URL 图片错误: %+v", imageParts[2])
	}

	// tools 声明序列化
	if len(sent.Tools) != 1 {
		t.Fatalf("tools 数量应为 1，实际 %d", len(sent.Tools))
	}
	toolDecl := sent.Tools[0]
	if toolDecl.Type != "function" || toolDecl.Function.Name != "get_weather" ||
		toolDecl.Function.Description != "查询天气" || toolDecl.Function.Parameters["type"] != "object" {
		t.Errorf("tools 声明错误: %+v", toolDecl)
	}
}

// TestOpenAI_Usage 校验 usage 解析：字段缺失置 0，缓存 token 两种命名都支持。
func TestOpenAI_Usage(t *testing.T) {
	cases := []struct {
		name     string
		usageRaw string
		want     Usage
	}{
		{
			name:     "DeepSeek缓存字段",
			usageRaw: `{"prompt_tokens":100,"completion_tokens":50,"prompt_cache_hit_tokens":60,"prompt_cache_miss_tokens":40}`,
			want:     Usage{InputTokens: 100, OutputTokens: 50, CacheReadTokens: 60, CacheWriteTokens: 40},
		},
		{
			name:     "cached_tokens别名",
			usageRaw: `{"prompt_tokens":10,"completion_tokens":2,"cached_tokens":7}`,
			want:     Usage{InputTokens: 10, OutputTokens: 2, CacheReadTokens: 7},
		},
		{
			name:     "字段缺失置0",
			usageRaw: `{"prompt_tokens":5}`,
			want:     Usage{InputTokens: 5},
		},
		{
			name:     "usage缺失",
			usageRaw: ``,
			want:     Usage{},
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			usageJSON := ""
			if c.usageRaw != "" {
				usageJSON = `,"usage":` + c.usageRaw
			}
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				fmt.Fprint(w, `{"choices":[{"message":{"role":"assistant","content":"hi"},"finish_reason":"stop"}]`+usageJSON+`}`)
			}))
			defer srv.Close()

			resp, err := newTestProvider(srv).Chat(context.Background(), &ChatRequest{Model: "m"}, nil)
			if err != nil {
				t.Fatalf("Chat 出错: %v", err)
			}
			if resp.Usage != c.want {
				t.Errorf("usage 解析错误: got %+v, want %+v", resp.Usage, c.want)
			}
		})
	}
}

// TestOpenAI_EmptyAPIKey 校验 apiKey 为空时返回错误。
func TestOpenAI_EmptyAPIKey(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"choices":[]}`)
	}))
	defer srv.Close()

	prov := NewOpenAIProvider("", WithBaseURL(srv.URL))
	if _, err := prov.Chat(context.Background(), &ChatRequest{Model: "m"}, nil); err == nil {
		t.Error("apiKey 为空时应返回错误")
	}
}
