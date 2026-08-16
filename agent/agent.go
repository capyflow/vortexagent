// Package agent 提供 Agent 运行时：LLM 与工具的循环调度、会话管理，
// 以及生命周期钩子（Hooks）与会话存储（SessionStore）扩展点。
// 对应 pi 项目中 packages/agent 的定位。
//
// 包内文件职责：
//
//	agent.go   Agent 本体与配置（Options / New / 默认提示词）
//	ask.go     Ask 循环：LLM ↔ 工具 的调度主循环
//	tool.go    工具接口（Tool）与注册表（Registry）
//	session.go 会话（Session）：对话历史与回滚
//	hooks.go   生命周期钩子（Hooks）：观察与拦截扩展点
//	store.go   会话存储（SessionStore）：持久化抽象与内置实现
package agent

import (
	"github.com/capyflow/vortexagent/llm"
)

// Options 是创建 Agent 的配置。
type Options struct {
	Provider llm.Provider // LLM 服务（必填）
	Registry *Registry    // 工具注册表（可空，空表示不支持工具）
	Model    string       // 模型名称，空使用 provider 默认
	Thinking bool         // 是否启用思考模式
	// MaxTokens 最大输出 token，0 使用默认。
	MaxTokens int

	// MaxIterations 限制单次 Ask 中工具调用循环的最大轮数（默认 10）。
	MaxIterations int

	// SystemPrompt 系统提示词，空使用 DefaultSystemPrompt。
	SystemPrompt string

	// OnDelta 流式增量回调（用于实时展示），可空。
	OnDelta func(llm.Delta)

	// Hooks 生命周期钩子（日志、遥测、权限拦截等），可空。
	Hooks *Hooks

	// Store 会话存储，非 nil 时每次 Ask 结束（成功或失败）都会自动保存会话。
	Store SessionStore
}

// DefaultSystemPrompt 默认系统提示词。
const DefaultSystemPrompt = "你是一个通用 AI 助手，可以使用提供的工具完成任务。请根据用户的指令选择合适的工具，必要时调用工具获取信息，最后给出清晰准确的回答。"

// Agent 是框架核心：循环调度 LLM 与工具，直到模型给出最终回答。
// 它与任何具体领域无关——领域能力全部来自注入的工具与提示词。
type Agent struct {
	provider     llm.Provider
	registry     *Registry
	model        string
	thinking     bool
	maxTokens    int
	maxIter      int
	systemPrompt string
	onDelta      func(llm.Delta)
	hooks        *Hooks
	store        SessionStore
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
		hooks:        opts.Hooks,
		store:        opts.Store,
	}
}
