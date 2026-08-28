// 本文件测试 JSON 模式（结构化输出）在各厂商适配器中的映射：
// OpenAI → response_format，Gemini → responseMimeType，Anthropic → system 约束。
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

// TestJSONMode_OpenAI 校验 OpenAI 兼容层把 JSONMode 映射为 response_format。
func TestJSONMode_OpenAI(t *testing.T) {
	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		json.Unmarshal(body, &gotBody)
		fmt.Fprint(w, `{"choices":[{"message":{"role":"assistant","content":"{\"a\":1}"},"finish_reason":"stop"}]}`)
	}))
	defer srv.Close()

	_, err := newTestProvider(srv).Chat(context.Background(), &ChatRequest{Model: "m", JSONMode: true}, nil)
	if err != nil {
		t.Fatalf("Chat 出错: %v", err)
	}
	rf, ok := gotBody["response_format"].(map[string]any)
	if !ok {
		t.Fatalf("请求体应包含 response_format: %v", gotBody)
	}
	if rf["type"] != "json_object" {
		t.Errorf("response_format.type = %v, 期望 json_object", rf["type"])
	}
}

// TestJSONMode_OpenAI_NotSetByDefault 校验不开启时不应发送 response_format。
func TestJSONMode_OpenAI_NotSetByDefault(t *testing.T) {
	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		json.Unmarshal(body, &gotBody)
		fmt.Fprint(w, `{"choices":[{"message":{"role":"assistant","content":"hi"},"finish_reason":"stop"}]}`)
	}))
	defer srv.Close()

	_, err := newTestProvider(srv).Chat(context.Background(), &ChatRequest{Model: "m"}, nil)
	if err != nil {
		t.Fatalf("Chat 出错: %v", err)
	}
	if _, exists := gotBody["response_format"]; exists {
		t.Error("未开启 JSONMode 时不应发送 response_format")
	}
}

// TestJSONMode_Gemini 校验 Gemini 把 JSONMode 映射为 responseMimeType。
func TestJSONMode_Gemini(t *testing.T) {
	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		json.Unmarshal(body, &gotBody)
		fmt.Fprint(w, `{"candidates":[{"content":{"parts":[{"text":"{}"}]}}]}`)
	}))
	defer srv.Close()

	p := NewGeminiProvider("test-key", WithBaseURL(srv.URL))
	_, err := p.Chat(context.Background(), &ChatRequest{Model: "m", JSONMode: true}, nil)
	if err != nil {
		t.Fatalf("Chat 出错: %v", err)
	}
	gc, ok := gotBody["generationConfig"].(map[string]any)
	if !ok {
		t.Fatalf("请求体应包含 generationConfig: %v", gotBody)
	}
	if gc["responseMimeType"] != "application/json" {
		t.Errorf("responseMimeType = %v, 期望 application/json", gc["responseMimeType"])
	}
}

// TestJSONMode_Anthropic 校验 Anthropic 在 system 前追加 JSON 输出约束。
func TestJSONMode_Anthropic(t *testing.T) {
	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		json.Unmarshal(body, &gotBody)
		fmt.Fprint(w, `{"content":[{"type":"text","text":"{}"}],"stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":1}}`)
	}))
	defer srv.Close()

	p := NewAnthropicProvider("test-key", WithBaseURL(srv.URL))
	req := &ChatRequest{Model: "m", JSONMode: true}
	req.Messages = []Message{NewTextMessage(RoleSystem, "你是测试助手"), NewTextMessage(RoleUser, "hi")}
	_, err := p.Chat(context.Background(), req, nil)
	if err != nil {
		t.Fatalf("Chat 出错: %v", err)
	}
	sys, ok := gotBody["system"].(string)
	if !ok {
		t.Fatalf("system 应为字符串: %v", gotBody["system"])
	}
	if !strings.Contains(sys, "JSON") {
		t.Errorf("system 应包含 JSON 输出约束: %q", sys)
	}
	if !strings.Contains(sys, "你是测试助手") {
		t.Errorf("system 应保留原有提示词: %q", sys)
	}
}
