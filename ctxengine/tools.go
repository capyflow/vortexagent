package ctxengine

import (
	"context"
	"fmt"
	"strings"
)

// Tool 是 ctxengine 对外工具的结构化实现。方法签名与 agent 包约定的
// Tool 接口一致，经本地投影断言（duck typing），不 import agent——
// agent 反向依赖本包（Options.ContextEngine），导入会成环。
type Tool struct {
	name        string
	description string
	schema      map[string]any
	call        func(ctx context.Context, args map[string]any) (string, error)
}

func (t *Tool) Name() string           { return t.name }
func (t *Tool) Description() string    { return t.description }
func (t *Tool) Schema() map[string]any { return t.schema }
func (t *Tool) Call(ctx context.Context, args map[string]any) (string, error) {
	return t.call(ctx, args)
}

// toolLike 是 agent 侧 Tool 接口的本地投影，用于编译期签名断言。
type toolLike interface {
	Name() string
	Description() string
	Schema() map[string]any
	Call(ctx context.Context, args map[string]any) (string, error)
}

var _ toolLike = (*Tool)(nil)

// NewTools 返回归档检索工具对：search_history / expand_chunk。
//
// 会话归属经 context 解析（agent.Ask 入口统一注入 WithSession），
// 也可由调用方显式传 session_id 参数覆盖（默认不暴露给模型 schema，
// 供多会话宿主内部使用）。
func NewTools(e *Engine) []*Tool {
	return []*Tool{newSearchHistoryTool(e), newExpandChunkTool(e)}
}

func newSearchHistoryTool(e *Engine) *Tool {
	return &Tool{
		name: "search_history",
		description: "按关键词检索本会话已归档的早期对话（摘要与片段主题），返回带出处（片段 ID、时间、消息数）的命中列表。" +
			"当需要回忆早期对话的具体内容（此前讨论过的问题、结论、错误与修复）时先调用本工具定位，再用 expand_chunk 展开原文；不要凭记忆或摘要猜测细节。",
		schema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"query": map[string]any{
					"type":        "string",
					"description": "检索关键词（可多个词，如 \"postgres 连接 报错\"）",
				},
				"limit": map[string]any{
					"type":        "number",
					"description": "返回条数上限，默认 5，最大 20",
				},
			},
			"required": []string{"query"},
		},
		call: func(ctx context.Context, args map[string]any) (string, error) {
			query, _ := args["query"].(string)
			query = strings.TrimSpace(query)
			if query == "" {
				return "", fmt.Errorf("query 不能为空")
			}
			limit := e.cfg.RetrievalLimit
			if v, ok := args["limit"].(float64); ok && v > 0 {
				limit = int(v)
			}
			sessionID := resolveSessionID(ctx, args)
			if sessionID == "" {
				return "", fmt.Errorf("无法确定当前会话（缺少会话上下文）")
			}
			hits, err := e.SearchHistory(ctx, sessionID, query, limit)
			if err != nil {
				return "", err
			}
			if len(hits) == 0 {
				return "未在归档中找到匹配内容。可尝试换更具体的关键词（如涉及的工具名、文件名、错误信息）重试。", nil
			}
			var sb strings.Builder
			fmt.Fprintf(&sb, "在会话归档中找到 %d 条相关内容（按相关度排序）：\n", len(hits))
			for _, h := range hits {
				kind := "对话片段"
				if h.Kind == "summary" {
					kind = h.Level + " 摘要"
				}
				fmt.Fprintf(&sb, "\n- ID: %s\n  类型: %s  时间: %s  消息数: %d\n  内容: %s\n  （用 expand_chunk 展开原文）\n", h.ID, kind, h.Time, h.MsgCount, h.Summary)
			}
			return sb.String(), nil
		},
	}
}

func newExpandChunkTool(e *Engine) *Tool {
	return &Tool{
		name: "expand_chunk",
		description: "展开一段已归档对话的原文（按 search_history 返回的片段 ID）。返回「[时间] 角色: 内容」的文本记录。" +
			"展开内容只在当前轮有效，不会常驻上下文。",
		schema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"chunk_id": map[string]any{
					"type":        "string",
					"description": "search_history 返回的片段 ID",
				},
				"max_tokens": map[string]any{
					"type":        "number",
					"description": "返回内容 token 上限，默认 2000",
				},
			},
			"required": []string{"chunk_id"},
		},
		call: func(ctx context.Context, args map[string]any) (string, error) {
			id, _ := args["chunk_id"].(string)
			maxTokens := 2000
			if v, ok := args["max_tokens"].(float64); ok && v > 0 {
				maxTokens = int(v)
			}
			sessionID := resolveSessionID(ctx, args)
			if sessionID == "" {
				return "", fmt.Errorf("无法确定当前会话（缺少会话上下文）")
			}
			out, err := e.ExpandChunk(ctx, sessionID, id, maxTokens)
			if err != nil {
				return "", err
			}
			return out, nil
		},
	}
}

// resolveSessionID 会话解析：显式参数 > context 注入。
func resolveSessionID(ctx context.Context, args map[string]any) string {
	if id, ok := args["session_id"].(string); ok && id != "" {
		return id
	}
	return sessionFromCtx(ctx)
}
