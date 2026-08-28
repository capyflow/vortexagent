// 本文件实现生命周期钩子（Hooks）：Agent 运行过程中的观察与拦截点，
// 用于日志、遥测、权限控制等场景。钩子都是可选的，不配置即为空操作。
package agent

import (
	"context"
	"time"

	"github.com/capyflow/vortexagent/llm"
)

// Hooks 是 Agent 生命周期钩子集合，是框架对外的主要扩展点之一。
//
// 通过 Options.Hooks 注入，用于日志、遥测、审计、权限控制、限流等场景。
// 所有字段均可空；空的钩子自动跳过。钩子与 Agent 运行在同一 goroutine，
// 实现方应避免在钩子内做阻塞操作（如需异步处理请自行起 goroutine）。
type Hooks struct {
	// OnMessage 在每条 LLM 回复（含工具调用轮次与最终回答）加入会话后调用。
	// msg 是模型本轮生成的完整消息，可能携带 ToolCalls。
	OnMessage func(ctx context.Context, msg llm.Message)

	// OnLLMCall 在每次 LLM 调用返回后调用（成功失败都触发），用于 token 消耗
	// 统计、延迟监控与调用审计。范围是本 Agent 的 Ask 循环调用——工具执行与
	// 子 agent 的内部调用不触发（子 agent 如需观测，配置它自己的 Hooks）。
	OnLLMCall func(ctx context.Context, info LLMCallInfo)

	// OnBeforeToolCall 在工具执行前调用，可用于权限拦截 / 熔断：
	// 返回非 nil 错误时工具不会执行，错误文本作为工具结果回传给模型，
	// 循环继续（模型可改用其他工具或直接回答）。
	OnBeforeToolCall func(ctx context.Context, name string, args map[string]any) error

	// OnAfterToolCall 在工具执行完成后调用（无论成功失败）。
	// err 非 nil 表示工具调用出错，此时 result 为空。
	OnAfterToolCall func(ctx context.Context, name string, args map[string]any, result string, err error)

	// OnError 在 Ask 流程任一处出错时调用（LLM 调用失败、上下文取消、
	// 模型空回答、超过最大循环轮数、会话保存失败等）。
	OnError func(ctx context.Context, err error)

	// OnFinish 在 Ask 成功返回最终回答后调用（工具循环已结束）。
	OnFinish func(ctx context.Context, answer string)
}

// LLMCallInfo 是一次 LLM 调用的观测信息（OnLLMCall 钩子使用）。
type LLMCallInfo struct {
	Model    string        // 实际请求的模型名
	Round    int           // Ask 循环中的轮次（从 1 开始）
	Duration time.Duration // 本次调用耗时
	Usage    llm.Usage     // token 消耗（调用失败时为零值）
	Err      error         // 非 nil 表示本次调用失败
}

// fireLLMCall 触发 OnLLMCall 钩子。
func (a *Agent) fireLLMCall(ctx context.Context, info LLMCallInfo) {
	if a.hooks != nil && a.hooks.OnLLMCall != nil {
		a.hooks.OnLLMCall(ctx, info)
	}
}

// fireMessage 触发 OnMessage 钩子。
func (a *Agent) fireMessage(ctx context.Context, m llm.Message) {
	if a.hooks != nil && a.hooks.OnMessage != nil {
		a.hooks.OnMessage(ctx, m)
	}
}

// fireError 触发 OnError 钩子。
func (a *Agent) fireError(ctx context.Context, err error) {
	if a.hooks != nil && a.hooks.OnError != nil {
		a.hooks.OnError(ctx, err)
	}
}
