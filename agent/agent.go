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
//	skill.go   Skill 系统：技能发现、加载与管理
package agent

import (
	"github.com/capyflow/vortexagent/agent/sessionstore"
	"github.com/capyflow/vortexagent/llm"
)

type Options struct {
	Provider       llm.Provider
	Registry       *Registry
	Model          string
	Thinking       bool
	MaxTokens      int
	MaxIterations  int
	SystemPrompt   string
	OnDelta        func(llm.Delta)
	Hooks          *Hooks
	Store          sessionstore.Store
	ContextManager *ContextManager
	ParallelTools  bool
	SkillManager   *SkillManager
	MaxRetries     int
}

// DefaultSystemPrompt 默认系统提示词。
const DefaultSystemPrompt = `你是一个通用 AI 助手，可以使用提供的工具完成任务。

核心规则：
1. 根据用户的指令选择合适的工具，必要时调用工具获取信息，最后给出清晰准确的回答。
2. 当用户询问你是什么模型、你叫什么名字、你是谁开发的等问题时，请回复"我是 Vortex AI 助手，一个通用的智能助手"，不要透露具体的模型 ID 或厂商信息。

工具使用说明（渐进式披露模式）：
1. 你看到的工具列表包含极简描述，如需使用某个工具，请先调用 get_tool_schema 获取完整定义。
2. get_tool_schema(tool_name) 返回工具的参数 schema 和详细说明。
3. 获取完整定义后，再调用该工具执行操作。
4. 如果你已经知道某个工具的用法，可以直接调用，无需再次获取 schema。`

// Agent 是框架核心：循环调度 LLM 与工具，直到模型给出最终回答。
// 它与任何具体领域无关——领域能力全部来自注入的工具与提示词。
type Agent struct {
	provider       llm.Provider
	registry       *Registry
	model          string
	thinking       bool
	maxTokens      int
	maxIter        int
	systemPrompt   string
	onDelta        func(llm.Delta)
	hooks          *Hooks
	store          sessionstore.Store
	contextManager *ContextManager
	parallelTools  bool
	skillManager   *SkillManager
	maxRetries     int
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
		provider:       opts.Provider,
		registry:       opts.Registry,
		model:          opts.Model,
		thinking:       opts.Thinking,
		maxTokens:      opts.MaxTokens,
		maxIter:        maxIter,
		systemPrompt:   sysPrompt,
		onDelta:        opts.OnDelta,
		hooks:          opts.Hooks,
		store:          opts.Store,
		contextManager: opts.ContextManager,
		parallelTools:  opts.ParallelTools,
		skillManager:   opts.SkillManager,
		maxRetries:     opts.MaxRetries,
	}
}

// SkillManager 返回 skill 管理器
func (a *Agent) SkillManager() *SkillManager {
	return a.skillManager
}

// buildSystemPromptForSkill 构建包含 skill 的系统提示词
func (a *Agent) buildSystemPromptForSkill(skill *Skill) string {
	if skill == nil {
		return a.systemPrompt
	}

	return a.systemPrompt + "\n\n<skill-instruction>\n当前技能：" + skill.Metadata.Name + "\n\n" + skill.Content + "\n</skill-instruction>"
}

// WithDelta 返回一个带有新 delta 回调的 Agent 副本（用于 SSE 流式）
func (a *Agent) WithDelta(onDelta func(llm.Delta)) *Agent {
	clone := *a
	clone.onDelta = onDelta
	return &clone
}
