package llm

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// anthropicTestServer 启动一个 mock Anthropic 服务器，捕获请求体与请求头。
func anthropicTestServer(t *testing.T, handler func(w http.ResponseWriter, r *http.Request)) (*httptest.Server, *AnthropicProvider) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(handler))
	prov := NewAnthropicProvider("test-key", WithBaseURL(srv.URL))
	t.Cleanup(srv.Close)
	return srv, prov
}

func TestAnthropic_UnaryToolUse(t *testing.T) {
	srv, prov := anthropicTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("x-api-key") != "test-key" {
			t.Errorf("x-api-key 头错误: %q", r.Header.Get("x-api-key"))
		}
		if r.Header.Get("anthropic-version") != "2023-06-01" {
			t.Errorf("anthropic-version 头错误: %q", r.Header.Get("anthropic-version"))
		}
		if r.URL.Path != "/v1/messages" {
			t.Errorf("请求路径错误: %s", r.URL.Path)
		}
		fmt.Fprint(w, `{
			"content": [
				{"type": "text", "text": "我来查询天气"},
				{"type": "tool_use", "id": "toolu_1", "name": "get_weather", "input": {"city": "北京"}}
			],
			"stop_reason": "tool_use",
			"usage": {"input_tokens": 120, "output_tokens": 30, "cache_read_input_tokens": 50, "cache_write_input_tokens": 20}
		}`)
	})
	_ = srv

	resp, err := prov.Chat(context.Background(), &ChatRequest{Model: "claude-3-5-sonnet"}, nil)
	if err != nil {
		t.Fatalf("Chat 失败: %v", err)
	}
	if len(resp.Message.Content) != 1 || resp.Message.Content[0].Text != "我来查询天气" {
		t.Errorf("文本内容错误: %+v", resp.Message.Content)
	}
	if len(resp.Message.ToolCalls) != 1 {
		t.Fatalf("应解析出 1 个工具调用，实际 %d", len(resp.Message.ToolCalls))
	}
	tc := resp.Message.ToolCalls[0]
	if tc.ID != "toolu_1" || tc.Name != "get_weather" {
		t.Errorf("工具调用 ID/Name 错误: %+v", tc)
	}
	if tc.Arguments["city"] != "北京" {
		t.Errorf("工具参数错误: %+v", tc.Arguments)
	}
	if resp.FinishReason != "tool_calls" {
		t.Errorf("FinishReason = %q, 期望 tool_calls", resp.FinishReason)
	}
	if resp.Usage.InputTokens != 120 || resp.Usage.OutputTokens != 30 ||
		resp.Usage.CacheReadTokens != 50 || resp.Usage.CacheWriteTokens != 20 {
		t.Errorf("usage 错误: %+v", resp.Usage)
	}
}

func TestAnthropic_UnaryThinking(t *testing.T) {
	srv, prov := anthropicTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{
			"content": [
				{"type": "thinking", "thinking": "让我想想"},
				{"type": "text", "text": "答案"}
			],
			"stop_reason": "end_turn",
			"usage": {"input_tokens": 10, "output_tokens": 5}
		}`)
	})
	_ = srv

	resp, err := prov.Chat(context.Background(), &ChatRequest{Model: "m"}, nil)
	if err != nil {
		t.Fatalf("Chat 失败: %v", err)
	}
	if len(resp.Message.Content) != 2 {
		t.Fatalf("应解析出 2 个内容块，实际 %d", len(resp.Message.Content))
	}
	if resp.Message.Content[0].Type != ContentThinking || resp.Message.Content[0].Thinking != "让我想想" {
		t.Errorf("思考块错误: %+v", resp.Message.Content[0])
	}
	if resp.Message.Content[1].Type != ContentText || resp.Message.Content[1].Text != "答案" {
		t.Errorf("文本块错误: %+v", resp.Message.Content[1])
	}
	if resp.FinishReason != "stop" {
		t.Errorf("FinishReason = %q, 期望 stop", resp.FinishReason)
	}
}

func TestAnthropic_StreamToolUse(t *testing.T) {
	sse := strings.Join([]string{
		`event: message_start`,
		`data: {"type":"message_start","message":{"usage":{"input_tokens":100,"cache_read_input_tokens":40,"cache_write_input_tokens":10}}}`,
		``,
		`event: content_block_start`,
		`data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`,
		``,
		`event: content_block_delta`,
		`data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"查"}}`,
		``,
		`event: content_block_delta`,
		`data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"询中"}}`,
		``,
		`event: content_block_stop`,
		`data: {"type":"content_block_stop","index":0}`,
		``,
		`event: content_block_start`,
		`data: {"type":"content_block_start","index":1,"content_block":{"type":"tool_use","id":"toolu_9","name":"get_weather","input":{}}}`,
		``,
		`event: content_block_delta`,
		`data: {"type":"content_block_delta","index":1,"delta":{"type":"input_json_delta","partial_json":"{\"city\":"}}`,
		``,
		`event: content_block_delta`,
		`data: {"type":"content_block_delta","index":1,"delta":{"type":"input_json_delta","partial_json":"\"北京\"}"}}`,
		``,
		`event: content_block_stop`,
		`data: {"type":"content_block_stop","index":1}`,
		``,
		`event: message_delta`,
		`data: {"type":"message_delta","delta":{"stop_reason":"tool_use"},"usage":{"output_tokens":25}}`,
		``,
		`event: message_stop`,
		`data: {"type":"message_stop"}`,
		``,
	}, "\n")

	srv, prov := anthropicTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, sse)
	})
	_ = srv

	var deltas []Delta
	resp, err := prov.Chat(context.Background(), &ChatRequest{Model: "m"}, func(d Delta) error {
		deltas = append(deltas, d)
		return nil
	})
	if err != nil {
		t.Fatalf("Chat 失败: %v", err)
	}
	// 流式增量：两次文本 + 结束标记
	if len(deltas) != 3 {
		t.Errorf("delta 数量 = %d, 期望 3（两次文本 + Done）: %+v", len(deltas), deltas)
	}
	if deltas[0].Text != "查" || deltas[1].Text != "询中" {
		t.Errorf("delta 文本错误: %+v", deltas)
	}
	if !deltas[2].Done {
		t.Error("最后一个 delta 应为 Done 标记")
	}
	// 完整消息
	if got := resp.Message.Content[0].Text; got != "查询中" {
		t.Errorf("完整文本 = %q, 期望 查询中", got)
	}
	if len(resp.Message.ToolCalls) != 1 {
		t.Fatalf("应组装出 1 个工具调用，实际 %d", len(resp.Message.ToolCalls))
	}
	tc := resp.Message.ToolCalls[0]
	if tc.ID != "toolu_9" || tc.Name != "get_weather" {
		t.Errorf("工具调用 ID/Name 错误: %+v", tc)
	}
	if tc.Arguments["city"] != "北京" {
		t.Errorf("流式参数拼接错误: %+v", tc.Arguments)
	}
	if resp.FinishReason != "tool_calls" {
		t.Errorf("FinishReason = %q, 期望 tool_calls", resp.FinishReason)
	}
	if resp.Usage.InputTokens != 100 || resp.Usage.OutputTokens != 25 ||
		resp.Usage.CacheReadTokens != 40 || resp.Usage.CacheWriteTokens != 10 {
		t.Errorf("流式 usage 错误: %+v", resp.Usage)
	}
}

// TestAnthropic_ToolResultInUserMessage 校验 tool_result 被放在 role=user 的消息中。
func TestAnthropic_ToolResultInUserMessage(t *testing.T) {
	srv, prov := anthropicTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var sent struct {
			Messages []struct {
				Role    string `json:"role"`
				Content []struct {
					Type      string `json:"type"`
					Text      string `json:"text"`
					ToolUseID string `json:"tool_use_id"`
					Content   string `json:"content"`
				} `json:"content"`
			} `json:"messages"`
			Tools []struct {
				Name        string         `json:"name"`
				Description string         `json:"description"`
				InputSchema map[string]any `json:"input_schema"`
			} `json:"tools"`
			Stream bool `json:"stream"`
		}
		if err := json.Unmarshal(body, &sent); err != nil {
			t.Fatalf("解析请求体失败: %v", err)
		}
		if len(sent.Messages) != 2 {
			t.Fatalf("消息数 = %d, 期望 2（assistant + user-tool-result 合并）", len(sent.Messages))
		}
		// 第一条是 assistant，带 tool_use
		if sent.Messages[0].Role != "assistant" {
			t.Errorf("第一条消息角色 = %q, 期望 assistant", sent.Messages[0].Role)
		}
		// 第二条是 user，承载两条 tool_result（合并）
		userMsg := sent.Messages[1]
		if userMsg.Role != "user" {
			t.Errorf("tool 结果消息角色 = %q, 期望 user", userMsg.Role)
		}
		if len(userMsg.Content) != 2 {
			t.Fatalf("tool_result 块数 = %d, 期望 2（合并）", len(userMsg.Content))
		}
		if userMsg.Content[0].Type != "tool_result" || userMsg.Content[0].ToolUseID != "call_1" || userMsg.Content[0].Content != "晴" {
			t.Errorf("第一个 tool_result 错误: %+v", userMsg.Content[0])
		}
		if userMsg.Content[1].ToolUseID != "call_2" || userMsg.Content[1].Content != "28度" {
			t.Errorf("第二个 tool_result 错误: %+v", userMsg.Content[1])
		}
		// 工具声明
		if len(sent.Tools) != 1 || sent.Tools[0].Name != "get_weather" {
			t.Errorf("工具声明错误: %+v", sent.Tools)
		}
		if sent.Tools[0].InputSchema == nil {
			t.Error("工具声明缺少 input_schema")
		}
		fmt.Fprint(w, `{"content":[{"type":"text","text":"ok"}],"stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":1}}`)
	})
	_ = srv

	req := &ChatRequest{
		Model: "m",
		Messages: []Message{
			{
				Role:    RoleAssistant,
				Content: []Content{{Type: ContentText, Text: "查一下"}},
				ToolCalls: []ToolCall{
					{ID: "call_1", Name: "get_weather", Arguments: map[string]any{"city": "北京"}},
					{ID: "call_2", Name: "get_weather", Arguments: map[string]any{"city": "上海"}},
				},
			},
			NewToolResultMessage("call_1", "晴"),
			NewToolResultMessage("call_2", "28度"),
		},
		Tools: []ToolParam{{
			Name:        "get_weather",
			Description: "查询天气",
			Schema:      map[string]any{"type": "object"},
		}},
	}
	if _, err := prov.Chat(context.Background(), req, nil); err != nil {
		t.Fatalf("Chat 失败: %v", err)
	}
}

// TestAnthropic_SystemField 校验 system 单条为字符串、多条为数组。
func TestAnthropic_SystemField(t *testing.T) {
	srv, prov := anthropicTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var sent struct {
			System json.RawMessage `json:"system"`
		}
		if err := json.Unmarshal(body, &sent); err != nil {
			t.Fatalf("解析请求体失败: %v", err)
		}
		// 单条 system → 字符串
		var single string
		if err := json.Unmarshal(sent.System, &single); err != nil {
			t.Fatalf("system 应为字符串: %s", string(sent.System))
		}
		if single != "系统提示" {
			t.Errorf("system = %q", single)
		}
		fmt.Fprint(w, `{"content":[{"type":"text","text":"ok"}],"stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":1}}`)
	})
	_ = srv

	if _, err := prov.Chat(context.Background(), &ChatRequest{
		Model:    "m",
		Messages: []Message{NewTextMessage(RoleSystem, "系统提示")},
	}, nil); err != nil {
		t.Fatalf("Chat 失败: %v", err)
	}
}

// TestAnthropic_ThinkingBudget 校验思考模式请求体。
func TestAnthropic_ThinkingBudget(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var sent struct {
			Thinking *struct {
				Type         string `json:"type"`
				BudgetTokens int    `json:"budget_tokens"`
			} `json:"thinking"`
			MaxTokens int `json:"max_tokens"`
		}
		if err := json.Unmarshal(body, &sent); err != nil {
			t.Fatalf("解析请求体失败: %v", err)
		}
		if sent.Thinking == nil || sent.Thinking.Type != "enabled" || sent.Thinking.BudgetTokens != 2048 {
			t.Errorf("thinking 配置错误: %+v", sent.Thinking)
		}
		if sent.MaxTokens != 4096 {
			t.Errorf("max_tokens = %d, 期望默认 4096", sent.MaxTokens)
		}
		fmt.Fprint(w, `{"content":[{"type":"text","text":"ok"}],"stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":1}}`)
	}))
	defer srv.Close()
	prov := NewAnthropicProvider("test-key", WithBaseURL(srv.URL), WithThinkingBudget(2048))

	if _, err := prov.Chat(context.Background(), &ChatRequest{Model: "m"}, nil); err != nil {
		t.Fatalf("Chat 失败: %v", err)
	}
}

// TestAnthropic_HTTPError 校验非 200 响应返回错误。
func TestAnthropic_HTTPError(t *testing.T) {
	srv, prov := anthropicTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		fmt.Fprint(w, `{"error":{"type":"authentication_error","message":"bad key"}}`)
	})
	_ = srv

	_, err := prov.Chat(context.Background(), &ChatRequest{Model: "m"}, nil)
	if err == nil {
		t.Fatal("期望 HTTP 401 报错")
	}
	if !strings.Contains(err.Error(), "401") {
		t.Errorf("错误信息应包含状态码: %v", err)
	}
}

// TestAnthropic_MissingModel 校验未指定模型时报错。
func TestAnthropic_MissingModel(t *testing.T) {
	prov := NewAnthropicProvider("test-key")
	_, err := prov.Chat(context.Background(), &ChatRequest{}, nil)
	if err == nil {
		t.Fatal("期望未指定模型时报错")
	}
}
