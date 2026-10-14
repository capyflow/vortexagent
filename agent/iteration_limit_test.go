// 轮数上限的行为回归：耗尽不回滚（ErrIterationLimit），以及按次覆盖上限。
package agent

import (
	"context"
	"errors"
	"testing"

	"github.com/capyflow/vortexagent/agent/sessionstore"
)

// TestAsk_IterationLimitKeepsHistory 轮数耗尽时保留已完成轮次：工具副作用无法撤销，
// 抹掉推理记录会让"副作用已生效但历史里查不到"的状态失真。
func TestAsk_IterationLimitKeepsHistory(t *testing.T) {
	fp := &fakeProvider{name: "fake", toolName: "echo", maxRounds: 99}
	echo := &echoTool{}
	reg := NewRegistry()
	if err := reg.Add(echo); err != nil {
		t.Fatal(err)
	}
	ag := New(Options{Provider: fp, Registry: reg, Model: "m1", MaxIterations: 3})
	session := sessionstore.NewSession("m1")

	_, err := ag.Ask(context.Background(), session, "无限循环测试")
	if !errors.Is(err, ErrIterationLimit) {
		t.Fatalf("错误 = %v，期望可用 errors.Is 识别为 ErrIterationLimit", err)
	}
	// 1 条用户消息 + 3 轮（assistant 工具调用 + tool 结果）= 7
	if got := len(session.Messages()); got != 7 {
		t.Errorf("历史长度 = %d，期望 7（耗尽后不回滚）", got)
	}
	if echo.calls != 3 {
		t.Errorf("工具执行次数 = %d，期望 3", echo.calls)
	}
}

// TestAsk_WithMaxIterationsOverridesAgentDefault 按次放宽轮数上限，
// 避免为不同预算各自再造一个 Agent 实例。
func TestAsk_WithMaxIterationsOverridesAgentDefault(t *testing.T) {
	fp := &fakeProvider{name: "fake", toolName: "echo", maxRounds: 5}
	reg := NewRegistry()
	if err := reg.Add(&echoTool{}); err != nil {
		t.Fatal(err)
	}
	ag := New(Options{Provider: fp, Registry: reg, Model: "m1", MaxIterations: 3})
	session := sessionstore.NewSession("m1")

	if _, err := ag.Ask(context.Background(), session, "默认上限"); !errors.Is(err, ErrIterationLimit) {
		t.Fatalf("Agent 级 3 轮应耗尽，得到 err=%v", err)
	}
	before := len(session.Messages())

	ans, err := ag.Ask(context.Background(), session, "按次放宽", WithMaxIterations(8))
	if err != nil {
		t.Fatalf("WithMaxIterations(8) 应跑完剩余工具轮后拿到回答: %v", err)
	}
	if ans != "最终回答" {
		t.Errorf("回答 = %q，期望 %q", ans, "最终回答")
	}
	if len(session.Messages()) <= before {
		t.Error("第二次 Ask 应继续追加历史")
	}
}

// TestAsk_WithMaxIterationsIgnoresNonPositive n<=0 时忽略，沿用 Agent 级配置。
func TestAsk_WithMaxIterationsIgnoresNonPositive(t *testing.T) {
	fp := &fakeProvider{name: "fake", toolName: "echo", maxRounds: 99}
	reg := NewRegistry()
	if err := reg.Add(&echoTool{}); err != nil {
		t.Fatal(err)
	}
	ag := New(Options{Provider: fp, Registry: reg, Model: "m1", MaxIterations: 2})
	session := sessionstore.NewSession("m1")

	if _, err := ag.Ask(context.Background(), session, "非法覆盖值", WithMaxIterations(0)); !errors.Is(err, ErrIterationLimit) {
		t.Fatalf("WithMaxIterations(0) 应被忽略并沿用上限 2，得到 err=%v", err)
	}
	if fp.rounds != 2 {
		t.Errorf("LLM 调用轮数 = %d，期望 2", fp.rounds)
	}
}
