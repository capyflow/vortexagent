package llm

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestGemini_NonStreaming_FunctionCall 校验非流式响应解析：functionCall、
// thinking part、usage、finishReason 映射，以及请求体（systemInstruction /
// 角色映射 / tools / thinkingConfig / maxOutputTokens）与 query key。
func TestGemini_NonStreaming_FunctionCall(t *testing.T) {
	reqCh := make(chan []byte, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.URL.Query().Get("key"); got != "test-key" {
			t.Errorf("query key 错误: %q", got)
		}
		if r.URL.Path != "/v1beta/models/gemini-2.5-flash:generateContent" {
			t.Errorf("请求路径错误: %s", r.URL.Path)
		}
		if got := r.Header.Get("Content-Type"); got != "application/json" {
			t.Errorf("Content-Type 头错误: %q", got)
		}
		body, _ := io.ReadAll(r.Body)
		reqCh <- body
		fmt.Fprint(w, `{
			"candidates": [{
				"content": {
					"role": "model",
					"parts": [
						{"thought": true, "text": "先分析天气情况"},
						{"text": "北京晴转多云。"},
						{"functionCall": {"name": "get_weather", "args": {"city": "beijing", "unit": "celsius"}}}
					]
				},
				"finishReason": "TOOL_CALLS"
			}],
			"usageMetadata": {"promptTokenCount": 12, "candidatesTokenCount": 34, "cachedContentTokenCount": 5}
		}`)
	}))
	defer srv.Close()

	p := NewGeminiProvider("test-key", WithBaseURL(srv.URL), WithMaxTokens(100))
	resp, err := p.Chat(context.Background(), &ChatRequest{
		Messages: []Message{
			NewTextMessage(RoleSystem, "你是一个助手"),
			NewTextMessage(RoleUser, "北京天气怎么样？"),
		},
		Thinking: true,
		Tools: []ToolParam{{
			Name:        "get_weather",
			Description: "查询天气",
			Schema:      map[string]any{"type": "object"},
		}},
	}, nil)
	if err != nil {
		t.Fatalf("Chat 出错: %v", err)
	}

	// 响应解析
	if len(resp.Message.Content) != 2 {
		t.Fatalf("应解析出 2 块内容，实际 %d", len(resp.Message.Content))
	}
	if resp.Message.Content[0].Type != ContentText || resp.Message.Content[0].Text != "北京晴转多云。" {
		t.Errorf("文本内容错误: %+v", resp.Message.Content)
	}
	if resp.Message.Content[1].Type != ContentThinking || resp.Message.Content[1].Thinking != "先分析天气情况" {
		t.Errorf("思考内容错误: %+v", resp.Message.Content)
	}
	if len(resp.Message.ToolCalls) != 1 {
		t.Fatalf("应解析出 1 个工具调用，实际 %d", len(resp.Message.ToolCalls))
	}
	tc := resp.Message.ToolCalls[0]
	if tc.Name != "get_weather" {
		t.Errorf("工具调用名称错误: %+v", tc)
	}
	if tc.Arguments["city"] != "beijing" || tc.Arguments["unit"] != "celsius" {
		t.Errorf("工具参数解析错误: %+v", tc.Arguments)
	}
	if resp.FinishReason != "tool_calls" {
		t.Errorf("FinishReason 应为 tool_calls，实际 %q", resp.FinishReason)
	}
	if resp.Usage.InputTokens != 12 || resp.Usage.OutputTokens != 34 || resp.Usage.CacheReadTokens != 5 {
		t.Errorf("usage 解析错误: %+v", resp.Usage)
	}

	// 请求体映射
	var got wireGeminiRequest
	if err := json.Unmarshal(<-reqCh, &got); err != nil {
		t.Fatalf("解析请求体失败: %v", err)
	}
	if got.SystemInstruction == nil || got.SystemInstruction.Parts[0].Text != "你是一个助手" {
		t.Errorf("systemInstruction 映射错误: %+v", got.SystemInstruction)
	}
	if len(got.Contents) != 1 || got.Contents[0].Role != "user" {
		t.Errorf("contents 角色映射错误: %+v", got.Contents)
	}
	if len(got.Tools) != 1 || got.Tools[0].FunctionDeclarations[0].Name != "get_weather" {
		t.Errorf("tools 映射错误: %+v", got.Tools)
	}
	if got.GenerationConfig == nil || got.GenerationConfig.ThinkingConfig == nil ||
		!got.GenerationConfig.ThinkingConfig.IncludeThoughts {
		t.Errorf("thinkingConfig 映射错误: %+v", got.GenerationConfig)
	}
	if got.GenerationConfig.MaxOutputTokens != 100 {
		t.Errorf("maxOutputTokens 映射错误: %d", got.GenerationConfig.MaxOutputTokens)
	}
}

// TestGemini_Streaming_FunctionCall 校验流式响应：thinking/text 增量回调、
// functionCall 的 args 分片 JSON 跨块累积、usage 取最后一块、finishReason 映射。
func TestGemini_Streaming_FunctionCall(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.URL.Query().Get("key"); got != "test-key" {
			t.Errorf("query key 错误: %q", got)
		}
		if got := r.URL.Query().Get("alt"); got != "sse" {
			t.Errorf("query alt 错误: %q", got)
		}
		if r.URL.Path != "/v1beta/models/test-model:streamGenerateContent" {
			t.Errorf("请求路径错误: %s", r.URL.Path)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, `data: {"candidates":[{"content":{"role":"model","parts":[{"thought":true,"text":"思考中"}]}}]}

data: {"candidates":[{"content":{"role":"model","parts":[{"functionCall":{"name":"get_weather","args":"{\"city\":\"beijing\",\"u"}}]}}]}

data: {"candidates":[{"content":{"role":"model","parts":[{"text":"北京晴。"},{"functionCall":{"args":"nits\":\"celsius\"}"}}]}}]}

data: {"candidates":[{"content":{"role":"model","parts":[]},"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":7,"candidatesTokenCount":9,"cachedContentTokenCount":2}}

`)
	}))
	defer srv.Close()

	p := NewGeminiProvider("test-key", WithBaseURL(srv.URL))
	var deltas []Delta
	resp, err := p.Chat(context.Background(), &ChatRequest{Model: "test-model"}, func(d Delta) error {
		deltas = append(deltas, d)
		return nil
	})
	if err != nil {
		t.Fatalf("Chat 出错: %v", err)
	}

	// 增量回调：思考块、文本块、结束标记
	if len(deltas) != 3 {
		t.Fatalf("应收到 3 个增量，实际 %d: %+v", len(deltas), deltas)
	}
	if deltas[0].Thinking != "思考中" {
		t.Errorf("第 1 个增量应为思考内容，实际 %+v", deltas[0])
	}
	if deltas[1].Text != "北京晴。" {
		t.Errorf("第 2 个增量应为文本，实际 %+v", deltas[1])
	}
	if !deltas[2].Done {
		t.Errorf("第 3 个增量应为流结束标记，实际 %+v", deltas[2])
	}

	// 完整消息汇总
	if len(resp.Message.Content) != 2 {
		t.Fatalf("应汇总出 2 块内容，实际 %d", len(resp.Message.Content))
	}
	if resp.Message.Content[0].Text != "北京晴。" {
		t.Errorf("汇总文本错误: %+v", resp.Message.Content)
	}
	if resp.Message.Content[1].Thinking != "思考中" {
		t.Errorf("汇总思考内容错误: %+v", resp.Message.Content)
	}

	// 分片参数累积
	if len(resp.Message.ToolCalls) != 1 {
		t.Fatalf("应累积出 1 个工具调用，实际 %d", len(resp.Message.ToolCalls))
	}
	tc := resp.Message.ToolCalls[0]
	if tc.Name != "get_weather" {
		t.Errorf("工具调用名称错误: %+v", tc)
	}
	if tc.Arguments["city"] != "beijing" || tc.Arguments["units"] != "celsius" {
		t.Errorf("分片参数累积错误: %+v", tc.Arguments)
	}

	if resp.FinishReason != "stop" {
		t.Errorf("FinishReason 应为 stop，实际 %q", resp.FinishReason)
	}
	if resp.Usage.InputTokens != 7 || resp.Usage.OutputTokens != 9 || resp.Usage.CacheReadTokens != 2 {
		t.Errorf("usage 解析错误: %+v", resp.Usage)
	}
}

// TestGemini_FunctionResponse_Mapping 校验请求体映射：assistant 工具调用转为
// model 角色的 function_call part，tool 消息转为 user 角色的 function_response
// part，图片转为 inline_data（base64）。
func TestGemini_FunctionResponse_Mapping(t *testing.T) {
	reqCh := make(chan []byte, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		reqCh <- body
		fmt.Fprint(w, `{"candidates":[{"content":{"role":"model","parts":[{"text":"已查询"}]},"finishReason":"STOP"}]}`)
	}))
	defer srv.Close()

	p := NewGeminiProvider("test-key", WithBaseURL(srv.URL))
	png := []byte{0x89, 0x50}
	_, err := p.Chat(context.Background(), &ChatRequest{
		Messages: []Message{
			{
				Role: RoleUser,
				Content: []Content{
					{Type: ContentText, Text: "查一下北京的天气"},
					{Type: ContentImage, MIME: "image/png", Data: png},
				},
			},
			{
				Role:      RoleAssistant,
				Content:   []Content{{Type: ContentText, Text: "我来查询"}},
				ToolCalls: []ToolCall{{ID: "call_1", Name: "get_weather", Arguments: map[string]any{"city": "beijing"}}},
			},
			{
				Role:       RoleTool,
				Content:    []Content{{Type: ContentText, Text: "晴"}},
				ToolCallID: "call_1",
			},
		},
	}, nil)
	if err != nil {
		t.Fatalf("Chat 出错: %v", err)
	}

	var got wireGeminiRequest
	if err := json.Unmarshal(<-reqCh, &got); err != nil {
		t.Fatalf("解析请求体失败: %v", err)
	}
	if len(got.Contents) != 3 {
		t.Fatalf("应映射出 3 条 contents，实际 %d: %+v", len(got.Contents), got.Contents)
	}

	// user 消息：文本 + 图片(inline_data base64)
	u := got.Contents[0]
	if u.Role != "user" {
		t.Errorf("第 1 条角色应为 user，实际 %q", u.Role)
	}
	if len(u.Parts) != 2 || u.Parts[0].Text != "查一下北京的天气" {
		t.Errorf("用户文本映射错误: %+v", u.Parts)
	}
	if u.Parts[1].InlineData == nil || u.Parts[1].InlineData.MimeType != "image/png" ||
		u.Parts[1].InlineData.Data != base64.StdEncoding.EncodeToString(png) {
		t.Errorf("图片 inline_data 映射错误: %+v", u.Parts[1])
	}

	// assistant 消息：model 角色 + function_call part
	a := got.Contents[1]
	if a.Role != "model" {
		t.Errorf("第 2 条角色应为 model，实际 %q", a.Role)
	}
	if len(a.Parts) != 2 {
		t.Fatalf("assistant 应映射出 2 个 part，实际 %d", len(a.Parts))
	}
	fc := a.Parts[1].FunctionCall
	if fc == nil || fc.Name != "get_weather" || fc.Args["city"] != "beijing" {
		t.Errorf("function_call part 映射错误: %+v", a.Parts[1])
	}

	// tool 消息：user 角色 + function_response part（名称取 ToolCall.Name）
	tr := got.Contents[2]
	if tr.Role != "user" {
		t.Errorf("第 3 条角色应为 user，实际 %q", tr.Role)
	}
	if len(tr.Parts) != 1 {
		t.Fatalf("tool 应映射出 1 个 part，实际 %d", len(tr.Parts))
	}
	fr := tr.Parts[0].FunctionResponse
	if fr == nil {
		t.Fatalf("function_response part 缺失: %+v", tr.Parts[0])
	}
	if fr.Name != "get_weather" {
		t.Errorf("function_response 名称应为 get_weather，实际 %q", fr.Name)
	}
	if fr.Response["result"] != "晴" {
		t.Errorf("function_response 结果应包一层 result，实际 %+v", fr.Response)
	}
}

// TestGemini_Usage 校验非流式 usage 解析（含缓存读 token）。
func TestGemini_Usage(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"candidates":[{"content":{"role":"model","parts":[{"text":"hi"}]},"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":10,"candidatesTokenCount":20,"cachedContentTokenCount":30}}`)
	}))
	defer srv.Close()

	p := NewGeminiProvider("test-key", WithBaseURL(srv.URL))
	resp, err := p.Chat(context.Background(), &ChatRequest{}, nil)
	if err != nil {
		t.Fatalf("Chat 出错: %v", err)
	}
	if resp.Usage.InputTokens != 10 || resp.Usage.OutputTokens != 20 ||
		resp.Usage.CacheReadTokens != 30 || resp.Usage.CacheWriteTokens != 0 {
		t.Errorf("usage 解析错误: %+v", resp.Usage)
	}
	if resp.FinishReason != "stop" {
		t.Errorf("FinishReason 应为 stop，实际 %q", resp.FinishReason)
	}
}
