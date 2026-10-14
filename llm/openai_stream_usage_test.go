// TestOpenAI_StreamUsage 回归：流式请求必须带上 stream_options.include_usage。
//
// OpenAI 兼容网关在 stream=true 时默认不在流中回传 usage，不显式索取就拿不到
// token 统计——上层用量记账会恒为 0（Friday 网页聊天总是走流式）。
package llm

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

// streamSSE 是两个文本分片 + 末片 usage + [DONE] 的标准流式响应。
const streamSSE = "data: {\"choices\":[{\"delta\":{\"content\":\"你\"}}]}\n\n" +
	"data: {\"choices\":[{\"delta\":{\"content\":\"好\"},\"finish_reason\":\"stop\"}]}\n\n" +
	"data: {\"choices\":[],\"usage\":{\"prompt_tokens\":120,\"completion_tokens\":7,\"prompt_cache_hit_tokens\":40}}\n\n" +
	"data: [DONE]\n\n"

// newStreamServer 返回一个记录请求体的 SSE 服务端。
func newStreamServer(t *testing.T, capturedBody *map[string]any) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("读取请求体失败: %v", err)
		}
		var body map[string]any
		if err := json.Unmarshal(raw, &body); err != nil {
			t.Errorf("请求体不是合法 JSON: %v (%s)", err, raw)
		}
		*capturedBody = body
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, streamSSE)
	}))
}

func streamRequest(p *OpenAIProvider) (*ChatResponse, []string, error) {
	var deltas []string
	resp, err := p.Chat(context.Background(), &ChatRequest{
		Model:    "test-model",
		Messages: []Message{NewTextMessage(RoleUser, "你好")},
	}, func(d Delta) error {
		if d.Text != "" {
			deltas = append(deltas, d.Text)
		}
		return nil
	})
	return resp, deltas, err
}

func TestOpenAI_StreamRequestsUsageByDefault(t *testing.T) {
	var body map[string]any
	srv := newStreamServer(t, &body)
	defer srv.Close()

	resp, deltas, err := streamRequest(NewOpenAIProvider("test-key", WithBaseURL(srv.URL)))
	if err != nil {
		t.Fatalf("流式请求失败: %v", err)
	}

	opts, ok := body["stream_options"]
	if !ok {
		t.Fatalf("流式请求缺少 stream_options，实际请求体: %v", body)
	}
	if include, _ := opts.(map[string]any)["include_usage"]; include != true {
		t.Errorf("stream_options.include_usage = %v，期望 true", include)
	}

	if resp.Usage.InputTokens != 120 || resp.Usage.OutputTokens != 7 {
		t.Errorf("Usage = %+v，期望 120/7", resp.Usage)
	}
	if resp.Usage.CacheReadTokens != 40 {
		t.Errorf("CacheReadTokens = %d，期望 40", resp.Usage.CacheReadTokens)
	}
	if len(deltas) != 2 || resp.Message.Content[0].Text != "你好" {
		t.Errorf("增量或汇总文本错误: deltas=%v content=%q", deltas, resp.Message.Content[0].Text)
	}
	if resp.FinishReason != "stop" {
		t.Errorf("FinishReason = %q，期望 stop", resp.FinishReason)
	}
}

// TestOpenAI_StreamUsageDisabled 覆盖严格网关因未知字段报 400 时的关断路径。
func TestOpenAI_StreamUsageDisabled(t *testing.T) {
	var body map[string]any
	srv := newStreamServer(t, &body)
	defer srv.Close()

	p := NewOpenAIProvider("test-key", WithBaseURL(srv.URL), WithStreamUsage(false))
	if _, _, err := streamRequest(p); err != nil {
		t.Fatalf("流式请求失败: %v", err)
	}
	if _, ok := body["stream_options"]; ok {
		t.Errorf("关断后仍发送 stream_options: %v", body)
	}
}

// TestProviderConfigStreamUsage 工厂路径（应用层从配置构造 provider 走这条）
// 必须把 StreamUsage 透传到 provider，否则关断逃生门形同不存在。
func TestProviderConfigStreamUsage(t *testing.T) {
	cases := []struct {
		name   string
		set    bool
		setVal bool
		want   bool
	}{
		{"未配置则默认索取", false, false, true},
		{"显式关闭", true, false, false},
		{"显式开启", true, true, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var body map[string]any
			srv := newStreamServer(t, &body)
			defer srv.Close()

			cfg := ProviderConfig{Name: "openai", APIKey: "k", BaseURL: srv.URL, Model: "test-model"}
			if tc.set {
				off := tc.setVal
				cfg.StreamUsage = &off
			}
			p, err := NewProvider(cfg)
			if err != nil {
				t.Fatalf("NewProvider: %v", err)
			}
			if _, _, err := streamRequest(p.(*OpenAIProvider)); err != nil {
				t.Fatalf("流式请求失败: %v", err)
			}
			_, got := body["stream_options"]
			if got != tc.want {
				t.Errorf("stream_options 存在 = %v，期望 %v（请求体 %v）", got, tc.want, body)
			}
		})
	}
}

// TestOpenAI_NonStreamHasNoStreamOptions 非流式请求不应带 stream_options。
func TestOpenAI_NonStreamHasNoStreamOptions(t *testing.T) {
	var body map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		var parsed map[string]any
		_ = json.Unmarshal(raw, &parsed)
		body = parsed
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"choices":[{"message":{"role":"assistant","content":"ok"}}],"usage":{"prompt_tokens":5,"completion_tokens":2}}`)
	}))
	defer srv.Close()

	p := NewOpenAIProvider("test-key", WithBaseURL(srv.URL))
	if _, err := p.Chat(context.Background(), &ChatRequest{Model: "test-model"}, nil); err != nil {
		t.Fatalf("非流式请求失败: %v", err)
	}
	if _, ok := body["stream_options"]; ok {
		t.Errorf("非流式不应带 stream_options: %v", body)
	}
}
