// 本文件实现 Agent 的核心循环（Ask）：把"用户输入 → LLM → 工具 → LLM → 回答"
// 串成一条有限轮数的调度链。它是框架的心脏，也是初学者读懂 agent 的第一站。
package agent

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

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
// AskOption 是 Ask 方法的可选参数
type AskOption interface {
	apply(*askConfig)
}

type askConfig struct {
	skill    *Skill
	jsonMode bool
}

type withSkill struct {
	skill *Skill
}

func (w *withSkill) apply(cfg *askConfig) {
	cfg.skill = w.skill
}

// WithSkill 指定本次提问使用的 skill（单次有效）
func WithSkill(skill *Skill) AskOption {
	return &withSkill{skill: skill}
}

type withJSONMode struct{}

func (w *withJSONMode) apply(cfg *askConfig) {
	cfg.jsonMode = true
}

// WithJSONMode 要求本次回答只输出合法 JSON（结构化输出：意图分类、字段抽取
// 等场景）。映射到各厂商的 JSON 模式（Anthropic 为系统提示约束）。
// 解析响应仍是使用方的责任——建议对解析失败做重试或降级。
func WithJSONMode() AskOption {
	return &withJSONMode{}
}

func (a *Agent) Ask(ctx context.Context, session *sessionstore.Session, userInput string, opts ...AskOption) (answer string, err error) {
	cfg := &askConfig{}
	for _, opt := range opts {
		opt.apply(cfg)
	}
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

	if a.contextManager != nil && a.contextManager.ShouldOffload() {
		_ = a.contextManager.Offload(ctx)
	}

	for iter := 1; iter <= a.maxIter; iter++ {
		if err := ctx.Err(); err != nil {
			session.Rollback(baseLen)
			return "", err
		}

		start := time.Now()
		resp, err := a.chat(ctx, session, cfg)
		a.fireLLMCall(ctx, LLMCallInfo{
			Model:    a.effectiveModel(cfg.skill),
			Round:    iter,
			Duration: time.Since(start),
			Usage:    usageOf(resp),
			Err:      err,
		})
		if err != nil {
			session.Rollback(baseLen)
			return "", fmt.Errorf("agent: 第 %d 轮调用失败: %w", iter, err)
		}
		session.Add(resp.Message)
		a.fireMessage(ctx, resp.Message)

		if len(resp.Message.ToolCalls) == 0 {
			answer := textOf(resp.Message)
			if strings.TrimSpace(answer) == "" {
				session.Rollback(baseLen)
				return "", fmt.Errorf("agent: 模型未返回任何文本内容")
			}
			if a.hooks != nil && a.hooks.OnFinish != nil {
				a.hooks.OnFinish(ctx, answer)
			}
			return answer, nil
		}

		if a.parallelTools && len(resp.Message.ToolCalls) > 1 {
			a.execToolsParallel(ctx, session, resp.Message.ToolCalls, cfg.skill)
		} else {
			for _, call := range resp.Message.ToolCalls {
				if err := ctx.Err(); err != nil {
					session.Rollback(baseLen)
					return "", err
				}
				result, callErr := a.execToolWithRetry(ctx, call, cfg.skill)
				if callErr != nil {
					result = fmt.Sprintf("工具执行出错: %v", callErr)
				}
				session.Add(llm.NewToolResultMessage(call.ID, result))
				if a.hooks != nil && a.hooks.OnAfterToolCall != nil {
					a.hooks.OnAfterToolCall(ctx, call.Name, call.Arguments, result, callErr)
				}
			}
		}
	}
	session.Rollback(baseLen)
	return "", fmt.Errorf("agent: 工具调用超过 %d 轮仍未结束", a.maxIter)
}

func (a *Agent) chat(ctx context.Context, session *sessionstore.Session, cfg *askConfig) (*llm.ChatResponse, error) {
	var history []llm.Message
	if a.contextManager != nil {
		history = a.contextManager.BuildMessages()
	} else {
		history = session.Messages()
	}

	systemPrompt := a.buildSystemPromptForSkill(cfg.skill)
	req := &llm.ChatRequest{
		Model:    a.effectiveModel(cfg.skill),
		Messages: a.withSystemPrompt(history, systemPrompt),
		Thinking: a.thinking,
		JSONMode: cfg.jsonMode,
	}
	if a.maxTokens > 0 {
		req.MaxTokens = a.maxTokens
	}
	// Skill 元数据生效：allowed-tools 过滤工具声明，temperature 覆盖采样温度。
	if cfg.skill != nil && cfg.skill.Metadata.Temperature != nil {
		req.Temperature = cfg.skill.Metadata.Temperature
	}
	if a.registry != nil {
		if a.registry.IsProgressiveMode() {
			// 渐进式披露模式：发送极简描述 + get_tool_schema 工具
			req.Tools = a.buildProgressiveTools()
		} else {
			// 传统模式：发送完整工具定义
			req.Tools = a.registry.Params()
		}
		req.Tools = filterToolsByAllowed(req.Tools, cfg.skill)
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

// nonRetryableError 标记重试也不会成功的工具错误（权限拦截、未知工具、参数错误、
// 子任务失败等永久性错误）。execToolWithRetry 见到它立即返回，不再做退避重试。
type nonRetryableError struct{ err error }

func (e *nonRetryableError) Error() string { return e.err.Error() }
func (e *nonRetryableError) Unwrap() error { return e.err }

// execTool 执行单个工具调用并返回结果文本。
func (a *Agent) execTool(ctx context.Context, call llm.ToolCall, skill *Skill) (string, error) {
	if a.registry == nil {
		return "", &nonRetryableError{fmt.Errorf("未注册任何工具")}
	}
	if !toolAllowedBySkill(call.Name, skill) {
		return "", &nonRetryableError{fmt.Errorf(
			"工具 %q 不在当前技能 %q 的允许列表内（allowed-tools: %s）",
			call.Name, skill.Metadata.Name, strings.Join(skill.Metadata.AllowedTools, ", "))}
	}
	if a.hooks != nil && a.hooks.OnBeforeToolCall != nil {
		if err := a.hooks.OnBeforeToolCall(ctx, call.Name, call.Arguments); err != nil {
			return "", &nonRetryableError{fmt.Errorf("工具调用被拦截: %w", err)}
		}
	}
	return a.callWithRecover(ctx, call)
}

// toolAllowedBySkill 判断工具是否被当前技能允许：
// 未指定技能或 allowed-tools 为空时不限制；get_tool_schema 是渐进式披露的
// 基础设施工具，始终放行。
func toolAllowedBySkill(name string, skill *Skill) bool {
	if skill == nil || len(skill.Metadata.AllowedTools) == 0 {
		return true
	}
	if name == "get_tool_schema" {
		return true
	}
	for _, allowed := range skill.Metadata.AllowedTools {
		if allowed == name {
			return true
		}
	}
	return false
}

// filterToolsByAllowed 按技能的 allowed-tools 过滤发给模型的工具声明
// （未指定技能或列表为空时不限制）。
func filterToolsByAllowed(tools []llm.ToolParam, skill *Skill) []llm.ToolParam {
	if skill == nil || len(skill.Metadata.AllowedTools) == 0 {
		return tools
	}
	out := make([]llm.ToolParam, 0, len(tools))
	for _, t := range tools {
		if toolAllowedBySkill(t.Name, skill) {
			out = append(out, t)
		}
	}
	return out
}

// callWithRecover 把工具实现的 panic 转为错误文本：工具可能来自任意 MCP server
// 或使用方代码，一个 panic 不应打穿 Ask 循环（CLI 崩进程、server 留下半截会话）。
// panic 标记为不可重试：同样的参数大概率再次 panic，重试只是浪费退避时间。
func (a *Agent) callWithRecover(ctx context.Context, call llm.ToolCall) (result string, err error) {
	defer func() {
		if r := recover(); r != nil {
			result = ""
			err = &nonRetryableError{fmt.Errorf("工具 %s panic: %v", call.Name, r)}
		}
	}()
	return a.registry.Call(ctx, call.Name, call.Arguments)
}

// execToolWithRetry 带重试的工具执行
func (a *Agent) execToolWithRetry(ctx context.Context, call llm.ToolCall, skill *Skill) (string, error) {
	maxRetries := a.maxRetries
	if maxRetries <= 0 {
		maxRetries = 3
	}

	var lastErr error
	for i := 0; i <= maxRetries; i++ {
		if err := ctx.Err(); err != nil {
			return "", err
		}

		result, err := a.execTool(ctx, call, skill)
		if err == nil {
			return result, nil
		}
		var nr *nonRetryableError
		if errors.As(err, &nr) {
			return "", err
		}

		lastErr = err

		if i < maxRetries {
			backoff := time.Duration(1<<uint(i)) * time.Second
			select {
			case <-ctx.Done():
				return "", ctx.Err()
			case <-time.After(backoff):
			}
		}
	}

	return "", fmt.Errorf("重试 %d 次后仍失败: %w", maxRetries, lastErr)
}

// execToolsParallel 并行执行多个工具调用，结果按原始顺序追加到会话。
func (a *Agent) execToolsParallel(ctx context.Context, session *sessionstore.Session, calls []llm.ToolCall, skill *Skill) {
	type toolResult struct {
		index  int
		call   llm.ToolCall
		result string
		err    error
	}

	results := make([]toolResult, len(calls))
	var wg sync.WaitGroup

	for i, call := range calls {
		wg.Add(1)
		go func(idx int, tc llm.ToolCall) {
			defer wg.Done()
			if err := ctx.Err(); err != nil {
				results[idx] = toolResult{index: idx, call: tc, err: err}
				return
			}
			result, callErr := a.execToolWithRetry(ctx, tc, skill)
			if callErr != nil {
				result = fmt.Sprintf("工具执行出错: %v", callErr)
			}
			results[idx] = toolResult{index: idx, call: tc, result: result, err: callErr}
		}(i, call)
	}

	wg.Wait()

	// 按原始顺序追加到会话
	for _, r := range results {
		session.Add(llm.NewToolResultMessage(r.call.ID, r.result))
		if a.hooks != nil && a.hooks.OnAfterToolCall != nil {
			a.hooks.OnAfterToolCall(ctx, r.call.Name, r.call.Arguments, r.result, r.err)
		}
	}
}

// withSystemPrompt 在历史前插入指定的系统提示词（不修改原历史）。
func (a *Agent) withSystemPrompt(history []llm.Message, systemPrompt string) []llm.Message {
	msgs := make([]llm.Message, 0, len(history)+1)
	msgs = append(msgs, llm.NewTextMessage(llm.RoleSystem, systemPrompt))
	return append(msgs, history...)
}

// buildProgressiveTools 构建渐进式披露模式的工具列表。
func (a *Agent) buildProgressiveTools() []llm.ToolParam {
	var tools []llm.ToolParam

	// 添加 get_tool_schema 工具（常驻）
	schemaTool := NewGetToolSchemaTool(a.registry)
	tools = append(tools, llm.ToolParam{
		Name:        schemaTool.Name(),
		Description: schemaTool.Description(),
		Schema:      schemaTool.Schema(),
	})

	// 添加所有工具的极简描述
	overviews := a.registry.Overviews()
	for _, ov := range overviews {
		tools = append(tools, llm.ToolParam{
			Name:        ov.Name,
			Description: ov.Overview,
			Schema:      map[string]any{"type": "object"}, // 极简模式不发送完整 schema
		})
	}

	return tools
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

// usageOf 提取响应中的 token 消耗（nil 响应返回零值）。
func usageOf(resp *llm.ChatResponse) llm.Usage {
	if resp == nil {
		return llm.Usage{}
	}
	return resp.Usage
}

// effectiveModel 返回本次 Ask 实际使用的模型名（skill 可覆盖 Agent 默认模型）。
func (a *Agent) effectiveModel(skill *Skill) string {
	if skill != nil && skill.Metadata.Model != "" {
		return skill.Metadata.Model
	}
	return a.model
}
