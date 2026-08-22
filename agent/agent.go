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
	"github.com/capyflow/vortexagent/agent/sessionstore"
	"github.com/capyflow/vortexagent/llm"
)

type Options struct {
	Provider      llm.Provider
	Registry      *Registry
	Model         string
	Thinking      bool
	MaxTokens     int
	MaxIterations int
	SystemPrompt  string
	OnDelta       func(llm.Delta)
	Hooks         *Hooks
	Store         sessionstore.Store
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
	store        sessionstore.Store
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
