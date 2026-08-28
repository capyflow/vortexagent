// 本文件实现子 agent（Agent-as-Tool）编排原语：把一个 Agent 包装成 Tool，
// 供父 agent 像调用普通工具一样把完整任务委派出去。
//
// 这是"基于框架衍生多 agent 系统"的核心机制：
//   - 智能客服：总机 agent 按问题类型委派给售前 / 售后 / 技术支持子 agent
//   - 编码 agent：主 agent 把"只在代码库里找答案"的探索任务委派给 explore 子 agent
//
// 子 agent 拥有完全独立的会话、提示词与工具箱（自己的 Provider / Registry /
// SystemPrompt / MaxIterations / Hooks），执行完毕只把最终回答文本回传给父 agent——
// 中间过程（思考、工具轮次）不进入父 agent 的上下文。对父 agent 而言，
// 一个子 agent 与普通工具没有任何区别，因此父 agent 的 Hooks（权限拦截、审计）
// 与并行工具执行对子 agent 同样生效。
package agent

import (
	"context"
	"fmt"
	"strings"

	"github.com/capyflow/vortexagent/agent/sessionstore"
)

// MaxSubagentDepth 限制子 agent 嵌套深度，防止 A 委派 B、B 又委派 A 的递归失控。
// 达到上限时 SubagentTool 返回错误，由父 agent 的模型决定如何收尾。
const MaxSubagentDepth = 8

// subagentDepthKey 是 context 中记录子 agent 嵌套深度的键。
type subagentDepthKey struct{}

// SubagentTool 把一个 Agent 包装成 Tool，是父 agent 委派任务的入口。
//
// 用法：为每个专员场景各构建一个 Agent（独立的 SystemPrompt 与工具箱），
// 用 NewSubagentTool 包装后注册进父 agent 的 Registry：
//
//	aftersales := agent.New(agent.Options{Provider: p, Registry: 售后工具, SystemPrompt: "你是售后客服…"})
//	parentRegistry.Add(agent.NewSubagentTool("aftersales", "处理退款/换货等售后问题", aftersales))
type SubagentTool struct {
	name        string
	description string
	agent       *Agent
}

// NewSubagentTool 把 sub 包装成名为 name 的工具。
// description 写清"什么问题该委派给它"，父 agent 的模型据此决定是否委派。
func NewSubagentTool(name, description string, sub *Agent) *SubagentTool {
	return &SubagentTool{name: name, description: description, agent: sub}
}

func (t *SubagentTool) Name() string        { return t.name }
func (t *SubagentTool) Description() string { return t.description }

func (t *SubagentTool) Schema() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"task": map[string]any{
				"type":        "string",
				"description": "委派给子 agent 的完整任务描述。子 agent 看不到当前对话历史，请把背景、目标与约束写成一条自包含的指令。",
			},
		},
		"required": []string{"task"},
	}
}

// Call 执行一次委派：用全新会话让子 agent 独立完成任务，只返回其最终回答。
func (t *SubagentTool) Call(ctx context.Context, args map[string]any) (string, error) {
	if t.agent == nil {
		return "", fmt.Errorf("agent: 子 agent %q 未配置", t.name)
	}
	task := strings.TrimSpace(asText(args["task"]))
	if task == "" {
		return "", &nonRetryableError{fmt.Errorf("task 不能为空，请提供完整的任务描述")}
	}

	depth, _ := ctx.Value(subagentDepthKey{}).(int)
	if depth >= MaxSubagentDepth {
		return "", &nonRetryableError{fmt.Errorf("子 agent 嵌套深度超过上限 %d，请直接回答", MaxSubagentDepth)}
	}
	subCtx := context.WithValue(ctx, subagentDepthKey{}, depth+1)

	// 每次委派使用全新会话：子 agent 与父 agent 的上下文完全隔离。
	session := sessionstore.NewSession(t.agent.model)
	answer, err := t.agent.Ask(subCtx, session, task)
	if err != nil {
		// 整个子任务失败时不重试：重跑会重复消耗 token，且可能重复执行有副作用的
		// 操作（退款、写文件……）。错误文本回传给父 agent，由模型决定下一步。
		return "", &nonRetryableError{fmt.Errorf("子 agent %q 执行失败: %w", t.name, err)}
	}
	return answer, nil
}

// asText 提取参数中的字符串值（模型可能传非 string 类型，做个兜底转换）。
func asText(v any) string {
	switch s := v.(type) {
	case nil:
		return ""
	case string:
		return s
	case fmt.Stringer:
		return s.String()
	default:
		return fmt.Sprintf("%v", v)
	}
}
