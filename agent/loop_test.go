package agent

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/capyflow/vortexagent/llm"
)

// fakeProvider 是内存中的假 provider：按脚本响应工具调用或最终回答。
type fakeProvider struct {
	name      string
	toolName  string // 前 maxToolRounds 轮返回该工具调用
	rounds    int    // 已响应轮数
	maxRounds int    // 工具调用轮数上限，超过后返回最终回答
	lastReq   *llm.ChatRequest
}

func (f *fakeProvider) Name() string { return f.name }

func (f *fakeProvider) Chat(_ context.Context, req *llm.ChatRequest, onDelta func(llm.Delta) error) (*llm.ChatResponse, error) {
	f.lastReq = req
	f.rounds++
	if onDelta != nil {
		_ = onDelta(llm.Delta{Text: "partial"})
		_ = onDelta(llm.Delta{Done: true})
	}
	if f.rounds <= f.maxRounds {
		return &llm.ChatResponse{
			Message: llm.Message{
				Role: llm.RoleAssistant,
				ToolCalls: []llm.ToolCall{{
					ID:        "call-1",
					Name:      f.toolName,
					Arguments: map[string]any{"query": "测试"},
				}},
			},
			FinishReason: "tool_calls",
		}, nil
	}
	return &llm.ChatResponse{
		Message:      llm.NewTextMessage(llm.RoleAssistant, "最终回答"),
		FinishReason: "stop",
	}, nil
}

// echoTool 是测试工具：把 query 参数原样返回。
type echoTool struct{ calls int }

func (t *echoTool) Name() string        { return "echo" }
func (t *echoTool) Description() string { return "回显测试工具" }
func (t *echoTool) Schema() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"query": map[string]any{"type": "string"},
		},
		"required": []string{"query"},
	}
}
func (t *echoTool) Call(_ context.Context, args map[string]any) (string, error) {
	t.calls++
	b, _ := json.Marshal(args)
	return string(b), nil
}

func TestAsk_SimpleAnswer(t *testing.T) {
	fp := &fakeProvider{name: "fake", maxRounds: 0}
	ag := New(Options{Provider: fp, Model: "m1"})
	session := NewSession("m1")

	ans, err := ag.Ask(context.Background(), session, "你好")
	if err != nil {
		t.Fatalf("Ask 失败: %v", err)
	}
	if ans != "最终回答" {
		t.Errorf("回答 = %q, 期望 %q", ans, "最终回答")
	}
	if len(session.Messages()) != 2 {
		t.Errorf("历史条数 = %d, 期望 2（user+assistant）", len(session.Messages()))
	}
	if fp.lastReq.Messages[0].Role != llm.RoleSystem {
		t.Error("第一条消息应为 system 提示词")
	}
}

func TestAsk_ToolLoop(t *testing.T) {
	fp := &fakeProvider{name: "fake", toolName: "echo", maxRounds: 2}
	echo := &echoTool{}
	reg := NewRegistry()
	if err := reg.Add(echo); err != nil {
		t.Fatal(err)
	}
	ag := New(Options{Provider: fp, Registry: reg, Model: "m1"})
	session := NewSession("m1")

	ans, err := ag.Ask(context.Background(), session, "帮我查一下")
	if err != nil {
		t.Fatalf("Ask 失败: %v", err)
	}
	if ans != "最终回答" {
		t.Errorf("回答 = %q, 期望 %q", ans, "最终回答")
	}
	if echo.calls != 2 {
		t.Errorf("工具调用次数 = %d, 期望 2", echo.calls)
	}
	// 历史应为: user/assistant(工具调用)/tool 两轮 + 最终 assistant = 6 条（system 不入会话）
	if len(session.Messages()) != 6 {
		t.Errorf("历史条数 = %d, 期望 6", len(session.Messages()))
	}
	// 工具结果消息应为 tool 角色且带 ToolCallID
	foundTool := false
	for _, m := range session.Messages() {
		if m.Role == llm.RoleTool {
			foundTool = true
			if m.ToolCallID != "call-1" {
				t.Errorf("tool 消息 ToolCallID = %q, 期望 call-1", m.ToolCallID)
			}
			if !strings.Contains(m.Content[0].Text, "query") {
				t.Errorf("tool 消息内容 = %q, 应包含参数", m.Content[0].Text)
			}
		}
	}
	if !foundTool {
		t.Error("历史中缺少 tool 角色消息")
	}
	// 请求应携带工具声明
	if len(fp.lastReq.Tools) != 1 || fp.lastReq.Tools[0].Name != "echo" {
		t.Errorf("请求工具声明 = %+v, 期望包含 echo", fp.lastReq.Tools)
	}
}

func TestAsk_ToolLoopExceeded(t *testing.T) {
	fp := &fakeProvider{name: "fake", toolName: "echo", maxRounds: 99}
	reg := NewRegistry()
	if err := reg.Add(&echoTool{}); err != nil {
		t.Fatal(err)
	}
	ag := New(Options{Provider: fp, Registry: reg, Model: "m1", MaxIterations: 3})
	session := NewSession("m1")

	_, err := ag.Ask(context.Background(), session, "无限循环测试")
	if err == nil {
		t.Fatal("期望超过最大轮数时报错")
	}
	if !strings.Contains(err.Error(), "3 轮") {
		t.Errorf("错误信息 = %v, 应包含轮数", err)
	}
}

func TestAsk_UnknownTool(t *testing.T) {
	fp := &fakeProvider{name: "fake", toolName: "no-such-tool", maxRounds: 1}
	ag := New(Options{Provider: fp, Registry: NewRegistry(), Model: "m1", MaxIterations: 3})
	session := NewSession("m1")

	// 未知工具应返回错误文本而非 panic，且循环继续
	ans, err := ag.Ask(context.Background(), session, "测试未知工具")
	if err != nil {
		t.Fatalf("Ask 失败: %v", err)
	}
	_ = ans
}

func TestRegistry_Duplicate(t *testing.T) {
	reg := NewRegistry()
	if err := reg.Add(&echoTool{}); err != nil {
		t.Fatal(err)
	}
	if err := reg.Add(&echoTool{}); err == nil {
		t.Error("重复注册应报错")
	}
	if _, err := reg.Call(context.Background(), "echo", map[string]any{"query": "x"}); err != nil {
		t.Errorf("Call echo 失败: %v", err)
	}
	if _, err := reg.Call(context.Background(), "missing", nil); err == nil {
		t.Error("调用未知工具应报错")
	}
}
