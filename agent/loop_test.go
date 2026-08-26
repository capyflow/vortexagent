package agent

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/capyflow/vortexagent/agent/sessionstore"
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

func (f *fakeProvider) Name() string              { return f.name }
func (f *fakeProvider) ContextWindow() int         { return 128000 }

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
	session := sessionstore.NewSession("m1")

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
	session := sessionstore.NewSession("m1")

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
	session := sessionstore.NewSession("m1")

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
	session := sessionstore.NewSession("m1")

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

type errProvider struct{ name string }

func (f *errProvider) Name() string              { return f.name }
func (f *errProvider) ContextWindow() int         { return 128000 }
func (f *errProvider) Chat(context.Context, *llm.ChatRequest, func(llm.Delta) error) (*llm.ChatResponse, error) {
	return nil, errors.New("模拟 API 故障")
}

type emptyProvider struct{ name string }

func (f *emptyProvider) Name() string              { return f.name }
func (f *emptyProvider) ContextWindow() int         { return 128000 }
func (f *emptyProvider) Chat(context.Context, *llm.ChatRequest, func(llm.Delta) error) (*llm.ChatResponse, error) {
	return &llm.ChatResponse{
		Message: llm.Message{
			Role:    llm.RoleAssistant,
			Content: []llm.Content{{Type: llm.ContentThinking, Thinking: "思考了很久"}},
		},
		FinishReason: "length",
	}, nil
}

// TestAsk_EmptyAnswer 校验模型未返回文本内容时报错并回滚历史。
func TestAsk_EmptyAnswer(t *testing.T) {
	ag := New(Options{Provider: &emptyProvider{name: "empty"}, Model: "m1"})
	session := sessionstore.NewSession("m1")

	_, err := ag.Ask(context.Background(), session, "问题")
	if err == nil {
		t.Fatal("期望模型未返回文本内容时报错")
	}
	if len(session.Messages()) != 0 {
		t.Errorf("空回答后历史应回滚为空，实际 %d 条: %+v", len(session.Messages()), session.Messages())
	}
}

// TestAsk_RollsBackHistoryOnError 校验 Ask 失败时历史回滚到提问前，
// 未回答的问题与半截工具轮次不会残留在会话中。
func TestAsk_RollsBackHistoryOnError(t *testing.T) {
	ag := New(Options{Provider: &errProvider{name: "err"}, Model: "m1"})
	session := sessionstore.NewSession("m1")
	session.Add(llm.NewTextMessage(llm.RoleUser, "之前的问题"))

	_, err := ag.Ask(context.Background(), session, "新问题")
	if err == nil {
		t.Fatal("期望 Ask 报错")
	}
	if len(session.Messages()) != 1 {
		t.Errorf("失败后历史应回滚到 1 条（之前的问题），实际 %d 条: %+v",
			len(session.Messages()), session.Messages())
	}
	if session.Messages()[0].Content[0].Text != "之前的问题" {
		t.Errorf("回滚后应保留提问前的历史，实际 %+v", session.Messages())
	}
}

// namedTool 是带名称的最小工具实现。
type namedTool struct{ name string }

func (t *namedTool) Name() string        { return t.name }
func (t *namedTool) Description() string { return "测试工具" }
func (t *namedTool) Schema() map[string]any {
	return map[string]any{"type": "object"}
}
func (t *namedTool) Call(context.Context, map[string]any) (string, error) {
	return "", nil
}

// TestRegistry_ParamsSorted 校验工具声明按名称排序输出
// （map 迭代顺序随机会导致每次请求的工具声明顺序不确定）。
func TestRegistry_ParamsSorted(t *testing.T) {
	reg := NewRegistry()
	for _, name := range []string{"zeta", "alpha", "mid"} { // 逆序注册
		if err := reg.Add(&namedTool{name: name}); err != nil {
			t.Fatal(err)
		}
	}
	params := reg.Params()
	if len(params) != 3 {
		t.Fatalf("参数数量 = %d", len(params))
	}
	want := []string{"alpha", "mid", "zeta"}
	for i, p := range params {
		if p.Name != want[i] {
			t.Errorf("Params[%d].Name = %q, 期望 %q", i, p.Name, want[i])
		}
	}
}
