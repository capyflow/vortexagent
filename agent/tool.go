// 本文件实现工具系统：Tool 接口（agent 能力的来源）与 Registry 注册表
// （名字 → 工具的映射，重名保护、确定性排序）。领域能力全部来自工具，
// 框架本身不内置任何业务工具。
//
// 渐进式披露（Progressive Disclosure）：
//   - 初始发送极简描述（Overview），减少 80-90% token 占用
//   - 模型确定调用某个工具后，通过 get_tool_schema 获取完整定义
//   - 兼容传统全量发送模式（未实现 OverviewProvider 接口的工具）
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

// OverviewProvider 是可选接口，实现渐进式披露的工具需额外实现此接口。
//
// 未实现此接口的工具将使用 Description 作为 Overview（向后兼容）。
type OverviewProvider interface {
	// Overview 返回极简描述（<30字），用于初始工具列表。
	// 模型看到所有工具的 Overview 后，通过 get_tool_schema 获取完整定义。
	Overview() string
}

// ToolOverview 是工具的极简描述，用于渐进式披露的初始工具列表。
type ToolOverview struct {
	Name        string `json:"name"`
	Overview    string `json:"overview"`
	Description string `json:"description,omitempty"`
}

// Registry 是工具注册表，维护名称到工具的映射。
type Registry struct {
	tools           map[string]Tool
	progressiveMode bool // 是否启用渐进式披露模式
}

// NewRegistry 创建一个空注册表。
func NewRegistry() *Registry {
	return &Registry{tools: make(map[string]Tool)}
}

// NewRegistryWithProgressive 创建启用渐进式披露模式的注册表。
func NewRegistryWithProgressive() *Registry {
	return &Registry{
		tools:           make(map[string]Tool),
		progressiveMode: true,
	}
}

// SetProgressiveMode 设置是否启用渐进式披露模式。
func (r *Registry) SetProgressiveMode(enabled bool) {
	r.progressiveMode = enabled
}

// IsProgressiveMode 返回是否启用渐进式披露模式。
func (r *Registry) IsProgressiveMode() bool {
	return r.progressiveMode
}

// Add 注册工具，重名时返回错误。
func (r *Registry) Add(t Tool) error {
	if t == nil {
		return fmt.Errorf("agent: 不能注册空工具")
	}
	name := t.Name()
	if name == "" {
		return fmt.Errorf("agent: 工具名不能为空")
	}
	if _, dup := r.tools[name]; dup {
		return fmt.Errorf("agent: 工具 %q 已存在", name)
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

// Call 调用工具，未知工具返回错误（标记为不可重试：换参数才有意义，原样重试无意义）。
func (r *Registry) Call(ctx context.Context, name string, args map[string]any) (string, error) {
	t, ok := r.tools[name]
	if !ok {
		return "", &nonRetryableError{fmt.Errorf("agent: 未知工具 %q", name)}
	}
	return t.Call(ctx, args)
}

// Overviews 返回所有工具的极简描述，用于渐进式披露的初始工具列表。
func (r *Registry) Overviews() []ToolOverview {
	names := r.Names()
	overviews := make([]ToolOverview, 0, len(names))
	for _, name := range names {
		t := r.tools[name]
		overview := t.Description() // 默认使用完整描述
		if op, ok := t.(OverviewProvider); ok {
			overview = op.Overview()
		}
		overviews = append(overviews, ToolOverview{
			Name:     name,
			Overview: overview,
		})
	}
	return overviews
}

// GetFullSchema 返回指定工具的完整定义（schema + description）。
func (r *Registry) GetFullSchema(name string) (map[string]any, error) {
	t, ok := r.tools[name]
	if !ok {
		return nil, &nonRetryableError{fmt.Errorf("agent: 未知工具 %q", name)}
	}
	return map[string]any{
		"name":        t.Name(),
		"description": t.Description(),
		"schema":      t.Schema(),
	}, nil
}

// GetToolSchemaTool 是 get_tool_schema 工具的实现，用于渐进式披露。
type GetToolSchemaTool struct {
	registry *Registry
}

// NewGetToolSchemaTool 创建 get_tool_schema 工具。
func NewGetToolSchemaTool(registry *Registry) *GetToolSchemaTool {
	return &GetToolSchemaTool{registry: registry}
}

func (t *GetToolSchemaTool) Name() string { return "get_tool_schema" }

func (t *GetToolSchemaTool) Description() string {
	return "获取指定工具的完整定义，包括参数 schema 和详细说明"
}

func (t *GetToolSchemaTool) Schema() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"tool_name": map[string]any{
				"type":        "string",
				"description": "要获取定义的工具名称",
			},
		},
		"required": []string{"tool_name"},
	}
}

func (t *GetToolSchemaTool) Call(ctx context.Context, args map[string]any) (string, error) {
	toolName, _ := args["tool_name"].(string)
	if toolName == "" {
		return "", fmt.Errorf("tool_name 不能为空")
	}

	schema, err := t.registry.GetFullSchema(toolName)
	if err != nil {
		return "", err
	}

	// 格式化输出
	result := fmt.Sprintf("工具: %s\n说明: %s\n参数 Schema: %v",
		schema["name"], schema["description"], schema["schema"])
	return result, nil
}
