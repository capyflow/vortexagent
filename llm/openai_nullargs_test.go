// TestOpenAI_NullArgumentsNormalization 回归：模型无参调用工具时，
// arguments 可能是字面量 "null"。解析侧要归一化为空 map（而非 nil，
// 否则重放时序列化回 "null"）；发送侧要把 nil map 序列化为 "{}"。
// StepFun 等 provider 会拒绝 "arguments":"null"（400 input_invalid）。
package llm

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestParseToolArguments_Null(t *testing.T) {
	args := parseToolArguments("null")
	if args == nil {
		t.Fatal("null 应归一化为空 map，得到 nil")
	}
	if len(args) != 0 {
		t.Fatalf("期望空 map，得到 %v", args)
	}
	if got := parseToolArguments(""); got == nil || len(got) != 0 {
		t.Fatalf("空字符串应归一化为空 map，得到 %v", got)
	}
}

func TestOpenAI_ReplayNilArguments(t *testing.T) {
	var captured string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		buf := make([]byte, 1<<16)
		n, _ := r.Body.Read(buf)
		captured = string(buf[:n])
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"ok"}}],"usage":{}}`))
	}))
	defer srv.Close()

	p := newTestProvider(srv)
	// 模拟历史回放：assistant 消息带 nil Arguments（"记待办"类无参调用落库后的形态）
	msg := Message{
		Role: RoleAssistant,
		ToolCalls: []ToolCall{{
			ID:        "call_1",
			Name:      "list_todos",
			Arguments: nil, // nil map → 序列化必须是 "{}" 而非 "null"
		}},
	}
	if _, err := p.Chat(t.Context(), &ChatRequest{
		Model:    "test",
		Messages: []Message{NewTextMessage(RoleUser, "hi"), msg},
	}, nil); err != nil {
		t.Fatalf("chat: %v", err)
	}
	if !strings.Contains(captured, `"arguments":"{}"`) {
		t.Fatalf("wire 中 arguments 应为 {}，实际: %s", captured)
	}
	if strings.Contains(captured, `"arguments":"null"`) {
		t.Fatal("wire 中不允许出现 arguments:null")
	}
}
