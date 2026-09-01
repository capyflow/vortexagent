package memory

import (
	"context"
	"fmt"
	"math"
	"sort"
	"strings"

	"github.com/capyflow/vortexagent/agent"
)

// 工具层参数与输出约束。
const (
	// MaxContentLength 单条记忆内容上限（字符）。长期记忆应是一句话能说清的
	// 事实，大段内容属于文档（tools/knowledge），不该进记忆库。
	MaxContentLength = 2000
	// MaxTags 单条记忆标签数上限。
	MaxTags = 8
	// MaxTagLength 单个标签长度上限（字符）。
	MaxTagLength = 32
	// DefaultSearchLimit / MaxSearchLimit 检索返回条数的默认值与硬上限。
	DefaultSearchLimit = 5
	MaxSearchLimit     = 20
	// DefaultListLimit / MaxListLimit 列举返回条数的默认值与硬上限。
	DefaultListLimit = 20
	MaxListLimit     = 100
	// SimilarityHint save 时近似重复提示阈值：新内容与已有记忆的词元
	// Jaccard 相似度超过该值时，在结果中提示模型注意去重。
	SimilarityHint = 0.6
)

// validKinds 合法的记忆类型集合。
var validKinds = map[string]bool{
	KindFact:       true,
	KindPreference: true,
	KindDecision:   true,
	KindLesson:     true,
}

// NewTools 构造长期记忆对外暴露的工具列表（增删改查六件套）。
//
// searcher 为 nil 时使用默认的 KeywordSearcher；替换为向量检索等实现
// 只需传入自定义 Searcher，工具层行为不变。
func NewTools(store Store, searcher Searcher) []*Tool {
	if searcher == nil {
		searcher = NewKeywordSearcher()
	}
	box := &memBox{store: store, searcher: searcher}
	return []*Tool{
		newSaveTool(box),
		newUpdateTool(box),
		newDeleteTool(box),
		newGetTool(box),
		newSearchTool(box),
		newListTool(box),
	}
}

// memBox 聚合存储与检索器，供各工具闭包共享。
type memBox struct {
	store    Store
	searcher Searcher
}

// Tool 是长期记忆工具的结构化实现，方法签名与 agent 包约定的 Tool 接口一致。
type Tool struct {
	name        string
	description string
	schema      map[string]any
	call        func(ctx context.Context, args map[string]any) (string, error)
}

// 编译期断言：*Tool 满足 agent.Tool 接口。
var _ agent.Tool = (*Tool)(nil)

func (t *Tool) Name() string                     { return t.name }
func (t *Tool) Description() string              { return t.description }
func (t *Tool) Schema() map[string]any           { return t.schema }
func (t *Tool) Call(ctx context.Context, args map[string]any) (string, error) {
	return t.call(ctx, args)
}

// newSaveTool 构造 memory_save：新增一条记忆，近似重复时提示但不阻止。
func newSaveTool(box *memBox) *Tool {
	return &Tool{
		name: "memory_save",
		description: "保存一条长期记忆（跨会话持久）。用于记住值得长期记住的事实：用户的偏好、" +
			"项目约定、重要决策、踩过的坑等。内容必须是一条自包含的事实陈述（一句话说清，" +
			"不依赖上下文也能读懂），不要保存对话原文、临时状态或大段资料。",
		schema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"content": map[string]any{
					"type":        "string",
					"description": "记忆内容（一条自包含的事实陈述）",
				},
				"kind": map[string]any{
					"type":        "string",
					"enum":        []string{KindFact, KindPreference, KindDecision, KindLesson},
					"description": "记忆类型：fact=事实 / preference=偏好 / decision=决策 / lesson=教训，默认 fact",
				},
				"tags": map[string]any{
					"type":        "array",
					"items":       map[string]any{"type": "string"},
					"description": "检索用标签（如 [\"go\", \"数据库\"]），可选",
				},
				"importance": map[string]any{
					"type":        "number",
					"description": "重要级 1-5（默认 3，5 最重要），影响检索排序",
				},
			},
			"required": []string{"content"},
		},
		call: func(ctx context.Context, args map[string]any) (string, error) {
			content, err := requireString(args, "content")
			if err != nil {
				return "", err
			}
			if runeLen(content) > MaxContentLength {
				return "", fmt.Errorf("记忆内容过长（%d 字符，上限 %d）。长期记忆应精炼成一句话事实，大段内容请放入文档", runeLen(content), MaxContentLength)
			}
			kind, err := optionalKind(args)
			if err != nil {
				return "", err
			}
			tags, err := optionalTags(args)
			if err != nil {
				return "", err
			}
			importance, err := optionalImportance(args)
			if err != nil {
				return "", err
			}

			m := &Memory{Content: content, Kind: kind, Tags: tags, Importance: importance}
			if err := box.store.Put(ctx, m); err != nil {
				return "", fmt.Errorf("保存记忆失败: %w", err)
			}

			var b strings.Builder
			fmt.Fprintf(&b, "已保存记忆 %s（类型 %s，重要级 %d）：%s", m.ID, m.Kind, m.Importance, m.Content)

			// 近似重复提示：只提醒不阻止——保存是显式意图，是否清理由模型决定。
			all, err := box.store.List(ctx)
			if err != nil {
				return b.String(), nil
			}
			best, bestScore := (*Memory)(nil), 0.0
			for _, old := range all {
				if old.ID == m.ID {
					continue
				}
				if s := Similarity(content, old.Content); s > bestScore {
					best, bestScore = old, s
				}
			}
			if best != nil && bestScore >= SimilarityHint {
				fmt.Fprintf(&b, "\n注意：已有高度相似的记忆 %s：%s。若为重复信息，建议调用 memory_update 合并或 memory_delete 清理，避免记忆库出现重复条目。", best.ID, best.Content)
			}
			return b.String(), nil
		},
	}
}

// newUpdateTool 构造 memory_update：按 ID 局部更新，至少给出一个要改的字段。
func newUpdateTool(box *memBox) *Tool {
	return &Tool{
		name:        "memory_update",
		description: "更新一条已有的长期记忆（按 ID）。可修改内容、类型、标签或重要级，至少提供一个要修改的字段。",
		schema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"id":         map[string]any{"type": "string", "description": "要更新的记忆 ID"},
				"content":    map[string]any{"type": "string", "description": "新的记忆内容（整条替换，不是追加）"},
				"kind":       map[string]any{"type": "string", "enum": []string{KindFact, KindPreference, KindDecision, KindLesson}, "description": "新的记忆类型"},
				"tags":       map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "description": "新的标签（整体替换）"},
				"importance": map[string]any{"type": "number", "description": "新的重要级 1-5"},
			},
			"required": []string{"id"},
		},
		call: func(ctx context.Context, args map[string]any) (string, error) {
			id, err := requireString(args, "id")
			if err != nil {
				return "", err
			}
			m, err := box.store.Get(ctx, id)
			if err != nil {
				return "", fmt.Errorf("查询记忆失败: %w", err)
			}
			if m == nil {
				return fmt.Sprintf("未找到记忆 %s。可先用 memory_search 或 memory_list 找到正确的 ID。", id), nil
			}

			_, hasContent := args["content"]
			_, hasKind := args["kind"]
			_, hasTags := args["tags"]
			_, hasImportance := args["importance"]
			if !hasContent && !hasKind && !hasTags && !hasImportance {
				return "", fmt.Errorf("至少提供一个要修改的字段（content / kind / tags / importance）")
			}
			var changes []string
			if hasContent {
				content, err := requireString(args, "content")
				if err != nil {
					return "", err
				}
				if runeLen(content) > MaxContentLength {
					return "", fmt.Errorf("记忆内容过长（%d 字符，上限 %d）", runeLen(content), MaxContentLength)
				}
				m.Content = content
				changes = append(changes, "内容")
			}
			if hasKind {
				kind, err := optionalKind(args)
				if err != nil {
					return "", err
				}
				m.Kind = kind
				changes = append(changes, "类型")
			}
			if hasTags {
				tags, err := optionalTags(args)
				if err != nil {
					return "", err
				}
				m.Tags = tags
				changes = append(changes, "标签")
			}
			if hasImportance {
				importance, err := optionalImportance(args)
				if err != nil {
					return "", err
				}
				m.Importance = importance
				changes = append(changes, "重要级")
			}

			if err := box.store.Put(ctx, m); err != nil {
				return "", fmt.Errorf("更新记忆失败: %w", err)
			}
			return fmt.Sprintf("已更新记忆 %s（修改了%s）：%s", m.ID, strings.Join(changes, "、"), m.Content), nil
		},
	}
}

// newDeleteTool 构造 memory_delete：按 ID 删除一条记忆。
func newDeleteTool(box *memBox) *Tool {
	return &Tool{
		name:        "memory_delete",
		description: "删除一条长期记忆（按 ID）。用于清理错误或过时的记忆——错误记忆会持续污染后续会话，发现后应及时删除。",
		schema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"id": map[string]any{"type": "string", "description": "要删除的记忆 ID"},
			},
			"required": []string{"id"},
		},
		call: func(ctx context.Context, args map[string]any) (string, error) {
			id, err := requireString(args, "id")
			if err != nil {
				return "", err
			}
			m, err := box.store.Get(ctx, id)
			if err != nil {
				return "", fmt.Errorf("查询记忆失败: %w", err)
			}
			if m == nil {
				return fmt.Sprintf("未找到记忆 %s，无需删除。", id), nil
			}
			if err := box.store.Delete(ctx, id); err != nil {
				return "", fmt.Errorf("删除记忆失败: %w", err)
			}
			return fmt.Sprintf("已删除记忆 %s：%s", id, m.Content), nil
		},
	}
}

// newGetTool 构造 memory_get：按 ID 读取一条记忆的完整内容。
func newGetTool(box *memBox) *Tool {
	return &Tool{
		name:        "memory_get",
		description: "按 ID 读取一条长期记忆的完整内容。",
		schema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"id": map[string]any{"type": "string", "description": "记忆 ID"},
			},
			"required": []string{"id"},
		},
		call: func(ctx context.Context, args map[string]any) (string, error) {
			id, err := requireString(args, "id")
			if err != nil {
				return "", err
			}
			m, err := box.store.Get(ctx, id)
			if err != nil {
				return "", fmt.Errorf("查询记忆失败: %w", err)
			}
			if m == nil {
				return fmt.Sprintf("未找到记忆 %s。", id), nil
			}
			return formatMemory(m), nil
		},
	}
}

// newSearchTool 构造 memory_search：按关键词检索记忆，可按类型/标签过滤。
func newSearchTool(box *memBox) *Tool {
	return &Tool{
		name: "memory_search",
		description: "在长期记忆中检索与关键词相关的记忆。用于回想之前记住的信息：" +
			"用户偏好、项目约定、历史决策、踩坑经验等。支持按类型或标签过滤。",
		schema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"query": map[string]any{
					"type":        "string",
					"description": "检索关键词（中英文均可，无需分词）",
				},
				"kind": map[string]any{
					"type":        "string",
					"enum":        []string{KindFact, KindPreference, KindDecision, KindLesson},
					"description": "只返回该类型的记忆，可选",
				},
				"tag": map[string]any{
					"type":        "string",
					"description": "只返回带该标签的记忆，可选",
				},
				"limit": map[string]any{
					"type":        "number",
					"description": fmt.Sprintf("返回条数上限，默认 %d，最大 %d", DefaultSearchLimit, MaxSearchLimit),
				},
			},
			"required": []string{"query"},
		},
		call: func(ctx context.Context, args map[string]any) (string, error) {
			query, err := requireString(args, "query")
			if err != nil {
				return "", err
			}
			// kind/tag 过滤缺省时表示不过滤，不能用 optionalKind（会把缺省回落成 fact）。
			kindFilter, err := optionalString(args, "kind")
			if err != nil {
				return "", err
			}
			if kindFilter != "" && !validKinds[kindFilter] {
				return "", fmt.Errorf("参数 kind 不合法: %q（合法值：fact / preference / decision / lesson）", kindFilter)
			}
			tagFilter, err := optionalString(args, "tag")
			if err != nil {
				return "", err
			}
			limit := DefaultSearchLimit
			if v, ok := args["limit"]; ok {
				n, err := coerceInt(v)
				if err != nil {
					return "", fmt.Errorf("参数 limit 不合法: %w", err)
				}
				if n > 0 {
					limit = n
				}
			}
			if limit > MaxSearchLimit {
				limit = MaxSearchLimit
			}

			all, err := box.store.List(ctx)
			if err != nil {
				return "", fmt.Errorf("读取记忆失败: %w", err)
			}
			candidates := filterMemories(all, kindFilter, tagFilter)
			hits, err := box.searcher.Search(ctx, candidates, query, limit)
			if err != nil {
				return "", fmt.Errorf("检索记忆失败: %w", err)
			}
			if len(hits) == 0 {
				return fmt.Sprintf("未找到与 %q 相关的记忆。可换用其他关键词，或用 memory_list 查看现有记忆。", query), nil
			}
			var b strings.Builder
			fmt.Fprintf(&b, "找到 %d 条相关记忆：\n", len(hits))
			for _, m := range hits {
				b.WriteString("\n" + formatMemory(m))
			}
			return b.String(), nil
		},
	}
}

// newListTool 构造 memory_list：按更新时间倒序列出现有记忆。
func newListTool(box *memBox) *Tool {
	return &Tool{
		name:        "memory_list",
		description: "列出长期记忆（按最近更新排序）。用于概览当前记住了哪些信息。",
		schema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"limit": map[string]any{
					"type":        "number",
					"description": fmt.Sprintf("返回条数上限，默认 %d，最大 %d", DefaultListLimit, MaxListLimit),
				},
			},
		},
		call: func(ctx context.Context, args map[string]any) (string, error) {
			limit := DefaultListLimit
			if v, ok := args["limit"]; ok {
				n, err := coerceInt(v)
				if err != nil {
					return "", fmt.Errorf("参数 limit 不合法: %w", err)
				}
				if n > 0 {
					limit = n
				}
			}
			if limit > MaxListLimit {
				limit = MaxListLimit
			}
			all, err := box.store.List(ctx)
			if err != nil {
				return "", fmt.Errorf("读取记忆失败: %w", err)
			}
			sort.SliceStable(all, func(i, j int) bool {
				return all[i].UpdatedAt.After(all[j].UpdatedAt)
			})
			if len(all) > limit {
				all = all[:limit]
			}
			if len(all) == 0 {
				return "记忆库为空。", nil
			}
			var b strings.Builder
			fmt.Fprintf(&b, "共 %d 条记忆（最多显示 %d 条，按最近更新排序）：\n", len(all), limit)
			for _, m := range all {
				b.WriteString("\n" + formatMemory(m))
			}
			return b.String(), nil
		},
	}
}

// filterMemories 按类型与标签过滤（大小写不敏感，均为精确匹配）。
func filterMemories(all []*Memory, kind, tag string) []*Memory {
	if kind == "" && tag == "" {
		return all
	}
	out := make([]*Memory, 0, len(all))
	for _, m := range all {
		if kind != "" && !strings.EqualFold(m.Kind, kind) {
			continue
		}
		if tag != "" {
			matched := false
			for _, t := range m.Tags {
				if strings.EqualFold(t, tag) {
					matched = true
					break
				}
			}
			if !matched {
				continue
			}
		}
		out = append(out, m)
	}
	return out
}

// formatMemory 将一条记忆格式化为模型可读的多行文本，ID 置顶方便后续引用。
func formatMemory(m *Memory) string {
	var b strings.Builder
	fmt.Fprintf(&b, "[%s] %s", m.ID, m.Content)
	var meta []string
	if m.Kind != "" {
		meta = append(meta, "类型: "+m.Kind)
	}
	if len(m.Tags) > 0 {
		meta = append(meta, "标签: "+strings.Join(m.Tags, ", "))
	}
	if m.Importance > 0 {
		meta = append(meta, fmt.Sprintf("重要级: %d", m.Importance))
	}
	if !m.UpdatedAt.IsZero() {
		meta = append(meta, "更新: "+m.UpdatedAt.Format("2006-01-02"))
	}
	if len(meta) > 0 {
		b.WriteString("\n    " + strings.Join(meta, " | "))
	}
	return b.String()
}

// ---- 参数解析辅助（语义对齐 tools/knowledge 的同名辅助）----

// requireString 取必填字符串参数，拒绝空串。
func requireString(args map[string]any, key string) (string, error) {
	v, ok := args[key]
	if !ok {
		return "", fmt.Errorf("缺少必需参数 %q", key)
	}
	s, ok := v.(string)
	if !ok {
		return "", fmt.Errorf("参数 %q 必须为字符串，实际类型为 %T", key, v)
	}
	s = strings.TrimSpace(s)
	if s == "" {
		return "", fmt.Errorf("参数 %q 不能为空", key)
	}
	return s, nil
}

// optionalString 取可选字符串参数。
func optionalString(args map[string]any, key string) (string, error) {
	v, ok := args[key]
	if !ok {
		return "", nil
	}
	s, ok := v.(string)
	if !ok {
		return "", fmt.Errorf("参数 %q 必须为字符串，实际类型为 %T", key, v)
	}
	return strings.TrimSpace(s), nil
}

// optionalKind 取可选的 kind 参数，空值回落为 fact，非法值报错。
func optionalKind(args map[string]any) (string, error) {
	kind, err := optionalString(args, "kind")
	if err != nil {
		return "", err
	}
	if kind == "" {
		return KindFact, nil
	}
	if !validKinds[kind] {
		return "", fmt.Errorf("参数 kind 不合法: %q（合法值：fact / preference / decision / lesson）", kind)
	}
	return kind, nil
}

// optionalTags 取可选的 tags 参数（JSON 数组），归一化：去空白、去空、去重、
// 限长限量。
func optionalTags(args map[string]any) ([]string, error) {
	v, ok := args["tags"]
	if !ok {
		return nil, nil
	}
	arr, ok := v.([]any)
	if !ok {
		return nil, fmt.Errorf("参数 tags 必须为字符串数组，实际类型为 %T", v)
	}
	seen := make(map[string]bool, len(arr))
	tags := make([]string, 0, len(arr))
	for _, item := range arr {
		s, ok := item.(string)
		if !ok {
			return nil, fmt.Errorf("参数 tags 的元素必须为字符串，实际类型为 %T", item)
		}
		s = strings.TrimSpace(s)
		if s == "" || seen[s] {
			continue
		}
		if runeLen(s) > MaxTagLength {
			return nil, fmt.Errorf("标签 %q 超过长度上限 %d", s, MaxTagLength)
		}
		seen[s] = true
		tags = append(tags, s)
		if len(tags) > MaxTags {
			return nil, fmt.Errorf("标签数量超过上限 %d", MaxTags)
		}
	}
	if len(tags) == 0 {
		return nil, nil
	}
	return tags, nil
}

// optionalImportance 取可选的 importance 参数，缺省 3，超出 1-5 报错。
func optionalImportance(args map[string]any) (int, error) {
	v, ok := args["importance"]
	if !ok {
		return ImportanceDefault, nil
	}
	n, err := coerceInt(v)
	if err != nil {
		return 0, fmt.Errorf("参数 importance 不合法: %w", err)
	}
	if n < 1 || n > 5 {
		return 0, fmt.Errorf("参数 importance 取值范围为 1-5，实际为 %d", n)
	}
	return n, nil
}

// coerceInt 兼容 JSON 反序列化后的多种数字类型（float64 / int / int64 / json.Number），
// 统一提取为整数；非整数值报错。
func coerceInt(v any) (int, error) {
	switch n := v.(type) {
	case int:
		return n, nil
	case int64:
		return int(n), nil
	case float64:
		if math.Trunc(n) != n {
			return 0, fmt.Errorf("期望整数，实际为 %v", n)
		}
		return int(n), nil
	default:
		return 0, fmt.Errorf("期望整数，实际类型为 %T", v)
	}
}
