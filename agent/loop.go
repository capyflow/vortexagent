package agent

import (
	"context"
	"fmt"
	"strings"

	"github.com/capyflow/vortexagent/llm"
)

// Options 是创建 Agent 的配置。
type Options struct {
	Provider  llm.Provider // LLM 服务（必填）
	Registry  *Registry    // 工具注册表（可空，空表示不支持工具）
	Model     string       // 模型名称，空使用 provider 默认
	Thinking  bool         // 是否启用思考模式
	MaxTokens int          // 最大输出 token，0 使用默认

	// MaxIterations 限制单次 Ask 中工具调用循环的最大轮数（默认 10）。
	MaxIterations int

	// SystemPrompt 系统提示词，空使用默认。
	SystemPrompt string

	// OnDelta 流式增量回调（用于实时展示），可空。
	OnDelta func(llm.Delta)
}

// DefaultSystemPrompt 默认系统提示词。
const DefaultSystemPrompt = "你是一个智能文档助手，可以根据知识库回答问题。如果需要查询知识库或调用工具，请使用提供的工具。"

// Agent 是文档 agent 的核心：循环调度 LLM 与工具，直到模型给出最终回答。
type Agent struct {
	provider     llm.Provider
	registry     *Registry
	model        string
	thinking     bool
	maxTokens    int
	maxIter      int
	systemPrompt string
	onDelta      func(llm.Delta)
}

// New 创建 Agent。
func New(opts Options) *Agent {
	maxIter := opts.MaxIterations
	if maxIter <= 0 {
		maxIter = 10
	}
	sysPrompt := opts.SystemPrompt
	if sysPrompt == "" {
		sysPrompt = DefaultSystemPrompt
	}
	return &Agent{
		provider:     opts.Provider,
		registry:     opts.Registry,
		model:        opts.Model,
		thinking:     opts.Thinking,
		maxTokens:    opts.MaxTokens,
		maxIter:      maxIter,
		systemPrompt: sysPrompt,
		onDelta:      opts.OnDelta,
	}
}

// Ask 处理一条用户输入，走完整个工具循环，返回最终回答文本。
//
// 流程（对应 pi 的 agent-loop）：
//  1. 追加用户消息到会话
//  2. 携带完整历史 + 工具声明调用 LLM
//  3. 若回复含工具调用 → 逐个执行并把结果追加为 tool 消息，回到第 2 步
//  4. 无工具调用 → 返回文本回答
func (a *Agent) Ask(ctx context.Context, session *Session, userInput string) (string, error) {
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
			return "", fmt.Errorf("vagent: 第 %d 轮调用失败: %w", iter, err)
		}
		session.Add(resp.Message)

		if len(resp.Message.ToolCalls) == 0 {
			answer := textOf(resp.Message)
			if strings.TrimSpace(answer) == "" {
				// 模型只输出了思考内容（如 max_tokens 耗尽）或什么都没输出：
				// 视为失败并回滚，避免用户看到空回答。
				session.Rollback(baseLen)
				return "", fmt.Errorf("vagent: 模型未返回任何文本内容")
			}
			return answer, nil
		}

		for _, call := range resp.Message.ToolCalls {
			if err := ctx.Err(); err != nil {
				session.Rollback(baseLen)
				return "", err
			}
			result, err := a.execTool(ctx, call)
			if err != nil {
				result = fmt.Sprintf("工具执行出错: %v", err)
			}
			session.Add(llm.NewToolResultMessage(call.ID, result))
		}
	}
	session.Rollback(baseLen)
	return "", fmt.Errorf("vagent: 工具调用超过 %d 轮仍未结束", a.maxIter)
}

// chat 调用 LLM，并把流式增量透传给 onDelta。
func (a *Agent) chat(ctx context.Context, session *Session) (*llm.ChatResponse, error) {
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
