// 本文件测试子 agent 原语（SubagentTool）：委派、上下文隔离、
// 父级钩子拦截、嵌套深度上限，以及永久性错误不重试的语义。
package agent

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/capyflow/vortexagent/agent/sessionstore"
	"github.com/capyflow/vortexagent/llm"
)

// scriptProvider 按脚本依次返回响应（区别于 fakeProvider 的固定轮数模式），
// 用于精确编排"父 agent 委派 → 子 agent 完成 → 父 agent 收尾"的多步流程。
// 并发安全：TaskHub 会用多个 goroutine 同时驱动同一个 provider。
type scriptProvider struct {
	name   string
	script []llm.ChatResponse
	errAt  map[int]error // 第 N 次调用时返回的错误

	mu      sync.Mutex
	calls   int
	lastReq *llm.ChatRequest
}

func (p *scriptProvider) Name() string       { return p.name }
func (p *scriptProvider) ContextWindow() int { return 128000 }

func (p *scriptProvider) Chat(_ context.Context, req *llm.ChatRequest, _ func(llm.Delta) error) (*llm.ChatResponse, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if err, ok := p.errAt[p.calls]; ok {
		p.calls++
		return nil, err
	}
	p.lastReq = req
	if p.calls >= len(p.script) {
		p.calls++
		resp := textResp("脚本已耗尽")
		return &resp, nil
	}
	resp := p.script[p.calls]
	p.calls++
	return &resp, nil
}

// callCount 返回已被调用的次数。
func (p *scriptProvider) callCount() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.calls
}

func textResp(text string) llm.ChatResponse {
	return llm.ChatResponse{
		Message:      llm.NewTextMessage(llm.RoleAssistant, text),
		FinishReason: "stop",
	}
}

func toolCallResp(name string, args map[string]any) llm.ChatResponse {
	return llm.ChatResponse{
		Message: llm.Message{
			Role: llm.RoleAssistant,
			ToolCalls: []llm.ToolCall{{
				ID:        "call-sub",
				Name:      name,
				Arguments: args,
			}},
		},
		FinishReason: "tool_calls",
	}
}

// TestSubagentTool_Delegation 校验完整委派链路：父 agent 调用子 agent 工具，
// 子 agent 独立完成任务，最终回答作为工具结果回传父 agent。
func TestSubagentTool_Delegation(t *testing.T) {
	// 子 agent：一次回答（无工具）
	subProv := &scriptProvider{name: "sub", script: []llm.ChatResponse{
		textResp("订单 A123 已发货"),
	}}
	sub := New(Options{Provider: subProv, Model: "sub-model", SystemPrompt: "你是售后专员"})

	// 父 agent：先委派，再收尾
	parentProv := &scriptProvider{name: "parent", script: []llm.ChatResponse{
		toolCallResp("order_support", map[string]any{"task": "查询订单 A123 的物流状态"}),
		textResp("您的订单已发货"),
	}}
	reg := NewRegistry()
	if err := reg.Add(NewSubagentTool("order_support", "处理订单与物流问题", sub)); err != nil {
		t.Fatal(err)
	}
	parent := New(Options{Provider: parentProv, Registry: reg, Model: "parent-model"})
	session := sessionstore.NewSession("parent-model")

	ans, err := parent.Ask(context.Background(), session, "我的订单到哪了？")
	if err != nil {
		t.Fatalf("Ask 失败: %v", err)
	}
	if ans != "您的订单已发货" {
		t.Errorf("最终回答 = %q, 期望 %q", ans, "您的订单已发货")
	}
	if subProv.callCount() != 1 {
		t.Errorf("子 agent 调用次数 = %d, 期望 1", subProv.callCount())
	}

	// 子 agent 收到的请求：system + task，两条，不含父 agent 的任何历史
	subReq := subProv.lastReq
	if len(subReq.Messages) != 2 {
		t.Fatalf("子 agent 请求消息数 = %d, 期望 2（system+task）", len(subReq.Messages))
	}
	if !strings.Contains(subReq.Messages[1].Content[0].Text, "订单 A123") {
		t.Errorf("子 agent 收到的 task = %q, 应包含委派内容", subReq.Messages[1].Content[0].Text)
	}
	for _, m := range subReq.Messages {
		if strings.Contains(m.Content[0].Text, "我的订单到哪了") {
			t.Error("父 agent 的原始用户输入不应泄漏进子 agent 上下文（除非写进 task）")
		}
	}

	// 子 agent 的最终回答作为 tool 消息进入父历史；父历史共 4 条
	// （user / assistant(委派) / tool(子回答) / assistant(最终)）
	msgs := session.Messages()
	if len(msgs) != 4 {
		t.Fatalf("父 agent 历史条数 = %d, 期望 4", len(msgs))
	}
	found := false
	for _, m := range msgs {
		if m.Role == llm.RoleTool && strings.Contains(m.Content[0].Text, "订单 A123 已发货") {
			found = true
		}
	}
	if !found {
		t.Error("父历史中应包含子 agent 最终回答的 tool 消息")
	}
}

// TestSubagentTool_SubHasOwnTools 校验子 agent 使用自己的工具箱：
// 委派给子 agent 的任务内部发生的工具循环对父 agent 不可见。
func TestSubagentTool_SubHasOwnTools(t *testing.T) {
	echo := &echoTool{}
	subReg := NewRegistry()
	if err := subReg.Add(echo); err != nil {
		t.Fatal(err)
	}
	subProv := &scriptProvider{name: "sub", script: []llm.ChatResponse{
		toolCallResp("echo", map[string]any{"query": "内部检索"}),
		textResp("内部检索完成，结论是 42"),
	}}
	sub := New(Options{Provider: subProv, Registry: subReg, Model: "m"})

	parentProv := &scriptProvider{name: "parent", script: []llm.ChatResponse{
		toolCallResp("worker", map[string]any{"task": "做一次内部检索"}),
		textResp("任务完成"),
	}}
	reg := NewRegistry()
	if err := reg.Add(NewSubagentTool("worker", "执行检索类任务", sub)); err != nil {
		t.Fatal(err)
	}
	parent := New(Options{Provider: parentProv, Registry: reg, Model: "m"})
	session := sessionstore.NewSession("m")

	if _, err := parent.Ask(context.Background(), session, "帮我做检索"); err != nil {
		t.Fatalf("Ask 失败: %v", err)
	}
	if echo.calls != 1 {
		t.Errorf("子 agent 的工具调用次数 = %d, 期望 1", echo.calls)
	}
	// 子 agent 的中间工具轮次不进父历史：父历史仍是 4 条
	if msgs := session.Messages(); len(msgs) != 4 {
		t.Errorf("父 agent 历史条数 = %d, 期望 4（子 agent 中间轮次不应泄漏）", len(msgs))
	}
}

// TestSubagentTool_InterceptedByParentHook 校验父 agent 的权限钩子对子 agent 同样生效：
// 拦截后子 agent 不应被启动。
func TestSubagentTool_InterceptedByParentHook(t *testing.T) {
	subProv := &scriptProvider{name: "sub", script: []llm.ChatResponse{textResp("不应被执行")}}
	sub := New(Options{Provider: subProv, Model: "m"})

	parentProv := &scriptProvider{name: "parent", script: []llm.ChatResponse{
		toolCallResp("worker", map[string]any{"task": "任务"}),
		textResp("好的，不委派了"),
	}}
	reg := NewRegistry()
	if err := reg.Add(NewSubagentTool("worker", "委派", sub)); err != nil {
		t.Fatal(err)
	}
	parent := New(Options{
		Provider: parentProv,
		Registry: reg,
		Model:    "m",
		Hooks: &Hooks{
			OnBeforeToolCall: func(_ context.Context, _ string, _ map[string]any) error {
				return errors.New("权限不足")
			},
		},
	})

	ans, err := parent.Ask(context.Background(), sessionstore.NewSession("m"), "委派任务")
	if err != nil {
		t.Fatalf("Ask 失败: %v", err)
	}
	if subProv.callCount() != 0 {
		t.Errorf("被拦截后子 agent 不应被调用，实际 %d 次", subProv.callCount())
	}
	if ans != "好的，不委派了" {
		t.Errorf("最终回答 = %q", ans)
	}
}

// TestSubagentTool_EmptyTask 校验空 task 报不可重试错误。
func TestSubagentTool_EmptyTask(t *testing.T) {
	sub := New(Options{Provider: &scriptProvider{name: "sub"}, Model: "m"})
	tool := NewSubagentTool("worker", "委派", sub)

	_, err := tool.Call(context.Background(), map[string]any{})
	var nr *nonRetryableError
	if err == nil || !errors.As(err, &nr) {
		t.Fatalf("空 task 应返回不可重试错误，实际: %v", err)
	}
}

// TestSubagentTool_DepthLimit 校验嵌套深度上限：达到上限时拒绝继续委派。
func TestSubagentTool_DepthLimit(t *testing.T) {
	subProv := &scriptProvider{name: "sub"}
	sub := New(Options{Provider: subProv, Model: "m"})
	tool := NewSubagentTool("worker", "委派", sub)

	ctx := context.WithValue(context.Background(), subagentDepthKey{}, MaxSubagentDepth)
	_, err := tool.Call(ctx, map[string]any{"task": "继续委派"})
	if err == nil {
		t.Fatal("达到嵌套深度上限时应报错")
	}
	if !strings.Contains(err.Error(), "嵌套深度") {
		t.Errorf("错误信息 = %v, 应包含嵌套深度说明", err)
	}
	if subProv.callCount() != 0 {
		t.Errorf("超限后子 agent 不应被调用，实际 %d 次", subProv.callCount())
	}
}

// TestSubagentTool_SubFailureNonRetryable 校验子任务整体失败时错误标记为不可重试
// （避免重跑整个子任务重复消耗与副作用）。
func TestSubagentTool_SubFailureNonRetryable(t *testing.T) {
	sub := New(Options{Provider: &errProvider{name: "err"}, Model: "m"})
	tool := NewSubagentTool("worker", "委派", sub)

	_, err := tool.Call(context.Background(), map[string]any{"task": "任务"})
	var nr *nonRetryableError
	if err == nil || !errors.As(err, &nr) {
		t.Fatalf("子任务失败应返回不可重试错误，实际: %v", err)
	}
	if !strings.Contains(err.Error(), "执行失败") {
		t.Errorf("错误信息 = %v, 应包含执行失败说明", err)
	}
}

// TestAsk_NonRetryableNoBackoff 校验永久性错误（未知工具、钩子拦截）跳过退避重试：
// 修复前此类调用会白白等待 1+2+4 秒。
func TestAsk_NonRetryableNoBackoff(t *testing.T) {
	fp := &fakeProvider{name: "fake", maxRounds: 0}
	reg := NewRegistry()
	if err := reg.Add(&echoTool{}); err != nil {
		t.Fatal(err)
	}
	ag := New(Options{
		Provider: fp,
		Registry: reg,
		Model:    "m1",
		Hooks: &Hooks{
			OnBeforeToolCall: func(_ context.Context, name string, _ map[string]any) error {
				if name == "blocked" {
					return errors.New("权限不足")
				}
				return nil
			},
		},
	})

	for _, name := range []string{"no-such-tool", "blocked"} {
		call := llm.ToolCall{ID: "c1", Name: name}
		start := time.Now()
		_, err := ag.execToolWithRetry(context.Background(), call, nil)
		if err == nil {
			t.Fatalf("%s 应返回错误", name)
		}
		if elapsed := time.Since(start); elapsed > time.Second {
			t.Errorf("%s 的不可重试错误耗时 %v, 应立即返回", name, elapsed)
		}
	}
}
