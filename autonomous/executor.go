package autonomous

import (
	"context"
	"fmt"
	"time"

	"github.com/capyflow/vortexagent/agent"
	"github.com/capyflow/vortexagent/agent/sessionstore"
)

// Executor 是目标执行器的抽象，决定"到期目标如何产出结果"。
//
// 框架负责执行编排之外的一切：唤醒调度、预检、状态推进（RunCount/LastRunAt/
// FailCount）、反思、持久化与回调；Executor 只需把目标变成一段结果。
// 默认使用 LLMExecutor（经由基础 Agent 的 LLM 会话执行）；
// 业务方可注入自定义实现，例如零 token 的 DirectExecutor：
//
//	type DirectExecutor struct{}
//
//	func (e *DirectExecutor) Execute(ctx context.Context, g *autonomous.Goal) (string, error) {
//	    return g.Description, nil // 到点直投文本，零 token
//	}
//
// Execute 由框架在主循环 goroutine 内同步调用，实现方应尊重 ctx 取消，
// 且不得长时间阻塞（会推迟后续目标与下一次唤醒）。
type Executor interface {
	Execute(ctx context.Context, g *Goal) (result string, err error)
}

// LLMExecutor 是默认执行器：构建自治 prompt 并通过 agent.Ask 调用 LLM。
//
// 由框架在 Config.Executor 为 nil 时自动创建并注入自治会话；
// 业务方也可通过 NewLLMExecutor 显式创建（例如作为自定义执行器的兜底），
// 此时同样需要交给框架使用（Config.Executor），会话由框架在 Run() 时注入。
type LLMExecutor struct {
	agent   *agent.Agent
	model   string
	session *sessionstore.Session // 自治专用 session，由框架在 Run() 时注入
}

// NewLLMExecutor 创建默认 LLM 执行器。
func NewLLMExecutor(agent *agent.Agent, model string) *LLMExecutor {
	return &LLMExecutor{agent: agent, model: model}
}

// setSession 供框架在 Run() 时注入自治会话。
func (e *LLMExecutor) setSession(s *sessionstore.Session) { e.session = s }

// Execute 实现 Executor：构建上下文与 prompt，交由 LLM 执行。
func (e *LLMExecutor) Execute(ctx context.Context, g *Goal) (string, error) {
	if e.session == nil {
		return "", fmt.Errorf("LLMExecutor 未注入自治会话：请通过 AutonomousAgent.Run() 启动，或经 Config.Executor 交给框架使用")
	}
	prompt := e.buildPrompt(g)
	return e.agent.Ask(ctx, e.session, prompt)
}

// ContextData 是构建 prompt 所需的上下文信息。
type ContextData struct {
	CurrentTime time.Time
	Goal        *Goal
}

func (e *LLMExecutor) buildContext(goal *Goal) ContextData {
	return ContextData{
		CurrentTime: time.Now(),
		Goal:        goal,
	}
}

func (e *LLMExecutor) buildPrompt(goal *Goal) string {
	ctxData := e.buildContext(goal)
	snap := goal.Snapshot()

	return fmt.Sprintf(`你是一个自治 agent。请根据以下信息判断并执行任务。

## 当前时间
%s

## 目标
- 标题: %s
- 描述: %s
- 状态: %s
- 已执行次数: %d

## 上次执行结果
%s

## 上次反思
%s

## 可用工具
你可以使用所有已注册的工具。

## 指令
1. 判断当前是否需要执行该目标描述的任务
2. 如果需要，说明具体步骤并执行（通过工具调用）
3. 如果不需要，说明原因
4. 执行完毕后，简要总结结果`,
		ctxData.CurrentTime.Format("2006-01-02 15:04:05 MST"),
		snap.Title,
		snap.Description,
		snap.Status,
		snap.RunCount,
		snap.LastResult,
		snap.LastReflect,
	)
}

// sessionSetter 是框架注入自治会话的内部约定：LLMExecutor 实现它，
// 自定义 Executor 若需要自治会话也可实现（非必需）。
type sessionSetter interface {
	setSession(s *sessionstore.Session)
}
