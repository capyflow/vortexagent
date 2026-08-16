// Package vagent 提供 Agent 运行时：LLM 与工具的循环调度、会话管理。
// 对应 pi 项目中 packages/agent 的定位。
package agent

import (
	"context"
	"fmt"
	"sort"

	"github.com/capyflow/vortexagent/llm"
)

// Tool 是 agent 可调用的工具接口。
//
// 实现方可以是内置工具（如知识库检索）或 MCP 工具包装器；
// Go 接口是结构化的，实现包无需 import 本包。
type Tool interface {
	// Name 返回工具名称（模型在 tool call 中使用的名字）。
	Name() string
	// Description 返回工具说明，模型据此决定是否调用。
	Description() string
	// Schema 返回参数 JSON Schema（object 类型）。
	Schema() map[string]any
	// Call 执行工具并返回结果文本。
	Call(ctx context.Context, args map[string]any) (string, error)
}

// Registry 是工具注册表，维护名称到工具的映射。
type Registry struct {
	tools map[string]Tool
}

// NewRegistry 创建一个空注册表。
func NewRegistry() *Registry {
	return &Registry{tools: make(map[string]Tool)}
}

// Add 注册工具，重名时返回错误。
func (r *Registry) Add(t Tool) error {
	if t == nil {
		return fmt.Errorf("vagent: 不能注册空工具")
	}
	name := t.Name()
	if name == "" {
		return fmt.Errorf("vagent: 工具名不能为空")
	}
	if _, dup := r.tools[name]; dup {
		return fmt.Errorf("vagent: 工具 %q 已存在", name)
	}
	r.tools[name] = t
	return nil
}

// Get 按名称查找工具。
func (r *Registry) Get(name string) (Tool, bool) {
	t, ok := r.tools[name]
	return t, ok
}

// Names 返回按字典序排序的工具名列表。
func (r *Registry) Names() []string {
	names := make([]string, 0, len(r.tools))
	for name := range r.tools {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// Params 将所有工具转换为发给模型的工具声明。
//
// 按工具名排序输出，保证每次请求的工具声明顺序确定
// （map 迭代顺序随机会影响模型对工具的选择）。
func (r *Registry) Params() []llm.ToolParam {
	names := r.Names()
	params := make([]llm.ToolParam, 0, len(names))
	for _, name := range names {
		t := r.tools[name]
		params = append(params, llm.ToolParam{
			Name:        t.Name(),
			Description: t.Description(),
			Schema:      t.Schema(),
		})
	}
	return params
}

// Call 调用工具，未知工具返回错误。
func (r *Registry) Call(ctx context.Context, name string, args map[string]any) (string, error) {
	t, ok := r.tools[name]
	if !ok {
		return "", fmt.Errorf("vagent: 未知工具 %q", name)
	}
	return t.Call(ctx, args)
}
