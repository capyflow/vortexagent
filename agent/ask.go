// 本文件实现 Agent 的核心循环（Ask）：把"用户输入 → LLM → 工具 → LLM → 回答"
// 串成一条有限轮数的调度链。它是框架的心脏，也是初学者读懂 agent 的第一站。
package agent

import (
	"context"
	"fmt"
	"strings"

	"github.com/capyflow/vortexagent/agent/sessionstore"
	"github.com/capyflow/vortexagent/llm"
)

// Ask 处理一条用户输入，走完整个工具循环，返回最终回答文本。
//
// 流程：
//  1. 追加用户消息到会话
//  2. 携带完整历史 + 工具声明调用 LLM
//  3. 若回复含工具调用 → 逐个执行并把结果追加为 tool 消息，回到第 2 步
//  4. 无工具调用 → 返回文本回答
//
// 无论成功失败，退出前都会：触发 OnError / OnFinish 钩子（见 Hooks）、
// 在配置了 Store 时自动保存会话快照。失败时历史回滚到提问前。
func (a *Agent) Ask(ctx context.Context, session *sessionstore.Session, userInput string) (answer string, err error) {
	// 兜底：任何路径（成功或失败）退出前自动保存会话并触发错误钩子。
	defer func() {
		if a.store != nil {
			if serr := a.store.Save(ctx, session); serr != nil && err == nil {
				err = fmt.Errorf("agent: 保存会话失败: %w", serr)
			}
		}
		if err != nil {
			a.fireError(ctx, err)
		}
	}()

	// 记录追加前的历史长度：任何失败路径都回滚到这里，
	// 避免未回答的问题与半截工具轮次残留在历史中。
	baseLen := len(session.Messages())
	session.Add(llm.NewTextMessage(llm.RoleUser, userInput))

	for iter := 1; iter <= a.maxIter; iter++ {
		if err := ctx.Err(); err != nil {
			session.Rollback(baseLen)
			return "", err
		}

		resp, err := a.chat(ctx, session)
		if err != nil {
			session.Rollback(baseLen)
			return "", fmt.Errorf("agent: 第 %d 轮调用失败: %w", iter, err)
		}
		session.Add(resp.Message)
		a.fireMessage(ctx, resp.Message)

		if len(resp.Message.ToolCalls) == 0 {
			answer := textOf(resp.Message)
			if strings.TrimSpace(answer) == "" {
				// 模型只输出了思考内容（如 max_tokens 耗尽）或什么都没输出：
				// 视为失败并回滚，避免用户看到空回答。
				session.Rollback(baseLen)
				return "", fmt.Errorf("agent: 模型未返回任何文本内容")
			}
			if a.hooks != nil && a.hooks.OnFinish != nil {
				a.hooks.OnFinish(ctx, answer)
			}
			return answer, nil
		}

		for _, call := range resp.Message.ToolCalls {
			if err := ctx.Err(); err != nil {
				session.Rollback(baseLen)
				return "", err
			}
			result, callErr := a.execTool(ctx, call)
			if callErr != nil {
				// 执行失败（含钩子拦截）不中断循环：错误文本作为工具结果
				// 回传，让模型知道发生了什么，可改用其他工具或直接回答。
				result = fmt.Sprintf("工具执行出错: %v", callErr)
			}
			session.Add(llm.NewToolResultMessage(call.ID, result))
			if a.hooks != nil && a.hooks.OnAfterToolCall != nil {
				a.hooks.OnAfterToolCall(ctx, call.Name, call.Arguments, result, callErr)
			}
		}
	}
	session.Rollback(baseLen)
	return "", fmt.Errorf("agent: 工具调用超过 %d 轮仍未结束", a.maxIter)
}

// chat 调用 LLM，并把流式增量透传给 onDelta。
func (a *Agent) chat(ctx context.Context, session *sessionstore.Session) (*llm.ChatResponse, error) {
	req := &llm.ChatRequest{
		Model:    a.model,
		Messages: a.withSystem(session.Messages()),
		Thinking: a.thinking,
	}
	if a.maxTokens > 0 {
		req.MaxTokens = a.maxTokens
	}
	if a.registry != nil {
		req.Tools = a.registry.Params()
	}

	var onDelta func(llm.Delta) error
	if a.onDelta != nil {
		onDelta = func(d llm.Delta) error {
			a.onDelta(d)
			return nil
		}
	}
	return a.provider.Chat(ctx, req, onDelta)
}

// execTool 执行单个工具调用并返回结果文本。
func (a *Agent) execTool(ctx context.Context, call llm.ToolCall) (string, error) {
	if a.registry == nil {
		return "", fmt.Errorf("未注册任何工具")
	}
	// 权限拦截 / 熔断钩子：返回错误则跳过执行。
	if a.hooks != nil && a.hooks.OnBeforeToolCall != nil {
		if err := a.hooks.OnBeforeToolCall(ctx, call.Name, call.Arguments); err != nil {
			return "", fmt.Errorf("工具调用被拦截: %w", err)
		}
	}
	return a.registry.Call(ctx, call.Name, call.Arguments)
}

// withSystem 在历史前插入系统提示词（不修改原历史）。
func (a *Agent) withSystem(history []llm.Message) []llm.Message {
	msgs := make([]llm.Message, 0, len(history)+1)
	msgs = append(msgs, llm.NewTextMessage(llm.RoleSystem, a.systemPrompt))
	return append(msgs, history...)
}

// textOf 提取消息中所有文本内容并拼接。
func textOf(m llm.Message) string {
	var sb strings.Builder
	for _, c := range m.Content {
		if c.Type == llm.ContentText {
			sb.WriteString(c.Text)
		}
	}
	return sb.String()
}
