package agent

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/capyflow/vortexagent/agent/sessionstore"
	"github.com/capyflow/vortexagent/llm"
)

// TestHooks_SimpleAnswer 校验无工具轮次时的钩子触发：
// OnMessage 收到 1 条最终回复、OnFinish 收到回答、OnError 不触发。
func TestHooks_SimpleAnswer(t *testing.T) {
	fp := &fakeProvider{name: "fake", maxRounds: 0}
	var gotMsg []llm.Message
	var gotFinish []string
	var gotErr []string
	ag := New(Options{
		Provider: fp,
		Model:    "m1",
		Hooks: &Hooks{
			OnMessage: func(_ context.Context, m llm.Message) { gotMsg = append(gotMsg, m) },
			OnFinish:  func(_ context.Context, a string) { gotFinish = append(gotFinish, a) },
			OnError:   func(_ context.Context, err error) { gotErr = append(gotErr, err.Error()) },
		},
	})

	ans, err := ag.Ask(context.Background(), sessionstore.NewSession("m1"), "你好")
	if err != nil {
		t.Fatalf("Ask 失败: %v", err)
	}
	if ans != "最终回答" {
		t.Errorf("回答 = %q, 期望 %q", ans, "最终回答")
	}
	if len(gotMsg) != 1 {
		t.Errorf("OnMessage 次数 = %d, 期望 1", len(gotMsg))
	}
	if len(gotFinish) != 1 || gotFinish[0] != "最终回答" {
		t.Errorf("OnFinish = %v, 期望 [最终回答]", gotFinish)
	}
	if len(gotErr) != 0 {
		t.Errorf("成功路径不应触发 OnError: %v", gotErr)
	}
}

// TestHooks_ToolRoundTrip 校验工具轮次的钩子顺序：
// OnMessage 每轮 LLM 回复各一次，OnBeforeToolCall / OnAfterToolCall 各一次。
func TestHooks_ToolRoundTrip(t *testing.T) {
	fp := &fakeProvider{name: "fake", toolName: "echo", maxRounds: 1}
	reg := NewRegistry()
	if err := reg.Add(&echoTool{}); err != nil {
		t.Fatal(err)
	}
	var messages []llm.Message
	var before, after []string
	ag := New(Options{
		Provider: fp,
		Registry: reg,
		Model:    "m1",
		Hooks: &Hooks{
			OnMessage: func(_ context.Context, m llm.Message) { messages = append(messages, m) },
			OnBeforeToolCall: func(_ context.Context, name string, _ map[string]any) error {
				before = append(before, name)
				return nil
			},
			OnAfterToolCall: func(_ context.Context, name string, _ map[string]any, _ string, err error) {
				if err != nil {
					t.Errorf("工具执行不应报错: %v", err)
				}
				after = append(after, name)
			},
		},
	})

	if _, err := ag.Ask(context.Background(), sessionstore.NewSession("m1"), "查一下"); err != nil {
		t.Fatalf("Ask 失败: %v", err)
	}
	// 2 轮 LLM 回复：工具调用轮 + 最终回答轮
	if len(messages) != 2 {
		t.Errorf("OnMessage 次数 = %d, 期望 2", len(messages))
	}
	if len(before) != 1 || before[0] != "echo" {
		t.Errorf("OnBeforeToolCall = %v, 期望 [echo]", before)
	}
	if len(after) != 1 || after[0] != "echo" {
		t.Errorf("OnAfterToolCall = %v, 期望 [echo]", after)
	}
}

// TestHooks_BeforeToolCallBlocks 校验拦截钩子：返回错误时工具不执行，
// 错误文本作为工具结果回传，循环继续（模型仍能给出最终回答）。
func TestHooks_BeforeToolCallBlocks(t *testing.T) {
	fp := &fakeProvider{name: "fake", toolName: "echo", maxRounds: 1}
	reg := NewRegistry()
	echo := &echoTool{}
	if err := reg.Add(echo); err != nil {
		t.Fatal(err)
	}
	ag := New(Options{
		Provider: fp,
		Registry: reg,
		Model:    "m1",
		Hooks: &Hooks{
			OnBeforeToolCall: func(_ context.Context, _ string, _ map[string]any) error {
				return errors.New("权限不足")
			},
		},
	})
	session := sessionstore.NewSession("m1")

	ans, err := ag.Ask(context.Background(), session, "测试拦截")
	if err != nil {
		t.Fatalf("Ask 失败: %v", err)
	}
	_ = ans
	if echo.calls != 0 {
		t.Errorf("被拦截的工具不应执行，实际执行 %d 次", echo.calls)
	}
	// 拦截原因应以 tool 消息回传，模型能看到
	foundBlocked := false
	for _, m := range session.Messages() {
		if m.Role == llm.RoleTool && strings.Contains(m.Content[0].Text, "权限不足") {
			foundBlocked = true
		}
	}
	if !foundBlocked {
		t.Error("历史中应包含拦截原因（权限不足）的 tool 消息")
	}
}

// TestHooks_OnError 校验失败路径触发 OnError 钩子。
func TestHooks_OnError(t *testing.T) {
	var gotErr []string
	ag := New(Options{
		Provider: &errProvider{name: "err"},
		Model:    "m1",
		Hooks: &Hooks{
			OnError: func(_ context.Context, err error) { gotErr = append(gotErr, err.Error()) },
		},
	})

	if _, err := ag.Ask(context.Background(), sessionstore.NewSession("m1"), "问题"); err == nil {
		t.Fatal("期望 Ask 报错")
	}
	if len(gotErr) != 1 {
		t.Errorf("OnError 次数 = %d, 期望 1: %v", len(gotErr), gotErr)
	}
}

// TestHooks_OnLLMCall 校验 LLM 调用级钩子：每轮调用各触发一次，
// 携带轮次、耗时与 token 消耗。
func TestHooks_OnLLMCall(t *testing.T) {
	fp := &fakeProvider{name: "fake", toolName: "echo", maxRounds: 2}
	reg := NewRegistry()
	if err := reg.Add(&echoTool{}); err != nil {
		t.Fatal(err)
	}
	var infos []LLMCallInfo
	ag := New(Options{
		Provider: fp,
		Registry: reg,
		Model:    "m1",
		Hooks: &Hooks{
			OnLLMCall: func(_ context.Context, info LLMCallInfo) { infos = append(infos, info) },
		},
	})

	if _, err := ag.Ask(context.Background(), sessionstore.NewSession("m1"), "查询"); err != nil {
		t.Fatalf("Ask 失败: %v", err)
	}
	if len(infos) != 3 {
		t.Fatalf("OnLLMCall 次数 = %d, 期望 3（两轮工具 + 一轮最终回答）", len(infos))
	}
	for i, info := range infos {
		if info.Round != i+1 {
			t.Errorf("infos[%d].Round = %d, 期望 %d", i, info.Round, i+1)
		}
		if info.Model != "m1" {
			t.Errorf("infos[%d].Model = %q, 期望 m1", i, info.Model)
		}
		if info.Duration <= 0 {
			t.Errorf("infos[%d].Duration 应大于 0", i)
		}
		if info.Err != nil {
			t.Errorf("infos[%d].Err 应为 nil: %v", i, info.Err)
		}
	}
}

// TestHooks_OnLLMCallError 校验 LLM 调用失败时钩子同样触发且携带错误。
func TestHooks_OnLLMCallError(t *testing.T) {
	var infos []LLMCallInfo
	ag := New(Options{
		Provider: &errProvider{name: "err"},
		Model:    "m1",
		Hooks: &Hooks{
			OnLLMCall: func(_ context.Context, info LLMCallInfo) { infos = append(infos, info) },
		},
	})

	if _, err := ag.Ask(context.Background(), sessionstore.NewSession("m1"), "问题"); err == nil {
		t.Fatal("期望 Ask 报错")
	}
	if len(infos) != 1 {
		t.Fatalf("OnLLMCall 次数 = %d, 期望 1", len(infos))
	}
	if infos[0].Err == nil {
		t.Error("失败调用的 Err 应非 nil")
	}
	if infos[0].Usage != (llm.Usage{}) {
		t.Errorf("失败调用的 Usage 应为零值: %+v", infos[0].Usage)
	}
}

// TestHooks_Nil 校验不配置钩子时一切正常（空钩子不应 panic）。
func TestHooks_Nil(t *testing.T) {
	fp := &fakeProvider{name: "fake", toolName: "echo", maxRounds: 1}
	reg := NewRegistry()
	if err := reg.Add(&echoTool{}); err != nil {
		t.Fatal(err)
	}
	ag := New(Options{Provider: fp, Registry: reg, Model: "m1"})
	if _, err := ag.Ask(context.Background(), sessionstore.NewSession("m1"), "无钩子测试"); err != nil {
		t.Fatalf("Ask 失败: %v", err)
	}
}
