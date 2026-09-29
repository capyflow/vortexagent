package ctxengine

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/capyflow/vortexagent/llm"
)

// CandidateFact 是归档时抽取的候选长期记忆：低置信（Importance≤2、带来源），
// 模型确认后经 memory_update 转正，或 memory_delete 删除。
type CandidateFact struct {
	Content       string `json:"content"`
	SourceChunkID string `json:"source_chunk_id"`
}

// Summarizer 负责把一批待归档消息压缩为状态交接摘要，并抽取候选事实。
// 返回错误时调用方放弃本轮归档（原文不动，下轮重试）。
type Summarizer interface {
	Summarize(ctx context.Context, msgs []llm.Message) (handoff string, facts []string, err error)
}

// llmSummarizer 是 Summarizer 的 LLM 实现，内置三级降级链：
//  1. 双输出 JSON（handoff + candidate_facts），解析失败重试一次；
//  2. 降级为自由文本摘要（一次调用）；
//  3. 仍失败 → 返回错误，调用方放弃归档。
type llmSummarizer struct {
	provider llm.Provider
	model    string
	// nowFn 可注入时钟（测试用）。
	nowFn func() time.Time
}

// NewLLMSummarizer 创建基于 provider 的摘要器。
func NewLLMSummarizer(provider llm.Provider, model string) *llmSummarizer {
	return &llmSummarizer{provider: provider, model: model, nowFn: time.Now}
}

// handoffDoc 是交接摘要的结构化形态。压缩保留的是"接着干活所需的状态"，
// 不是对话流水账（ZCode 式 compaction 的核心）。
type handoffDoc struct {
	Task      string `json:"task"`
	Facts     string `json:"facts"`
	Decisions string `json:"decisions"`
	Errors    string `json:"errors"`
	Pending   string `json:"pending"`
	Status    string `json:"status"`
}

const handoffSystemPrompt = `你是对话状态压缩助手。把给定的对话历史压缩为「状态交接文档」，供后续对话接续工作使用。压缩保留的是工作状态，不是对话流水账：寒暄、过程性内容可舍弃；事实、决策、结论必须保留。

只输出合法 JSON，格式：
{"task":"用户的任务/意图","facts":"已确认的关键事实","decisions":"已做的决策与约定","errors":"遇到的错误与解决方式","pending":"未完成的事项","status":"当前进行到哪一步"}

各字段用简洁中文填写，没有内容的字段留空字符串。另外在 JSON 之外不要输出任何文字。`

const handoffUserPrefix = "请压缩以下对话历史：\n\n"

// handoffRetryPrompt 后缀：第一次解析失败后附加，强调输出约束。
const handoffRetryPrompt = "\n\n（上一次输出不是合法 JSON。请严格只输出符合格式要求的单个 JSON 对象，不要有其他文字或代码块标记。）"

const fallbackSystemPrompt = `你是对话摘要助手。把对话历史压缩成不超过300字的状态交接摘要，保留关键事实、决策、结论与未完成事项，供后续对话接续使用。只输出摘要文本。`

func (s *llmSummarizer) chat(ctx context.Context, system, user string, maxTokens int) (string, error) {
	req := &llm.ChatRequest{
		Model: s.model,
		Messages: []llm.Message{
			llm.NewTextMessage(llm.RoleSystem, system),
			llm.NewTextMessage(llm.RoleUser, user),
		},
		MaxTokens: maxTokens,
	}
	resp, err := s.provider.Chat(ctx, req, nil)
	if err != nil {
		return "", err
	}
	var sb strings.Builder
	for _, c := range resp.Message.Content {
		if c.Type == llm.ContentText {
			sb.WriteString(c.Text)
		}
	}
	return sb.String(), nil
}

func (s *llmSummarizer) Summarize(ctx context.Context, msgs []llm.Message) (string, []string, error) {
	if s.provider == nil {
		return "", nil, fmt.Errorf("ctxengine: 未配置摘要 provider，无法归档")
	}
	body := renderMessagesForSummary(msgs)

	// 第一级：双输出 JSON，解析失败重试一次。
	user := handoffUserPrefix + body
	for attempt := 0; attempt < 2; attempt++ {
		out, err := s.chat(ctx, handoffSystemPrompt, user+handoffRetrySuffix(attempt), 800)
		if err != nil {
			break // provider 层失败直接进入降级，不再原地重试
		}
		if doc, facts, ok := parseHandoff(out); ok {
			return doc, facts, nil
		}
	}

	// 第二级：降级为自由文本摘要（一次调用）。
	out, err := s.chat(ctx, fallbackSystemPrompt, body, 500)
	if err != nil {
		return "", nil, fmt.Errorf("ctxengine: 生成摘要失败: %w", err)
	}
	out = strings.TrimSpace(out)
	if out == "" {
		return "", nil, fmt.Errorf("ctxengine: 摘要为空")
	}
	return out, nil, nil
}

func handoffRetrySuffix(attempt int) string {
	if attempt == 0 {
		return ""
	}
	return handoffRetryPrompt
}

// parseHandoff 解析双输出 JSON；容忍模型包一层 ```json 代码块。
func parseHandoff(out string) (string, []string, bool) {
	raw := strings.TrimSpace(out)
	raw = strings.TrimPrefix(raw, "```json")
	raw = strings.TrimPrefix(raw, "```")
	raw = strings.TrimSuffix(raw, "```")
	raw = strings.TrimSpace(raw)
	// 截取首个 { 到末个 } 之间的内容，容忍前后杂散文字。
	if i := strings.Index(raw, "{"); i >= 0 {
		if j := strings.LastIndex(raw, "}"); j > i {
			raw = raw[i : j+1]
		}
	}
	var payload struct {
		Handoff  handoffDoc `json:"handoff"`
		Facts    []string   `json:"candidate_facts"`
		FactsAlt []string   `json:"facts"`
	}
	if err := json.Unmarshal([]byte(raw), &payload); err != nil {
		return "", nil, false
	}
	doc := payload.Handoff
	if doc.Task == "" && doc.Facts == "" && doc.Decisions == "" && doc.Status == "" {
		return "", nil, false
	}
	facts := payload.Facts
	if len(facts) == 0 {
		facts = payload.FactsAlt
	}
	return doc.Render(), facts, true
}

// Render 把交接文档渲染为进上下文的紧凑文本。
func (h handoffDoc) Render() string {
	var sb strings.Builder
	writeSection := func(title, body string) {
		body = strings.TrimSpace(body)
		if body == "" {
			return
		}
		fmt.Fprintf(&sb, "【%s】%s\n", title, body)
	}
	writeSection("任务意图", h.Task)
	writeSection("已确认事实", h.Facts)
	writeSection("决策记录", h.Decisions)
	writeSection("错误与修复", h.Errors)
	writeSection("未完成事项", h.Pending)
	writeSection("当前状态", h.Status)
	out := strings.TrimRight(sb.String(), "\n")
	if out == "" {
		return ""
	}
	return out
}

// renderMessagesForSummary 把消息渲染为摘要输入（限制单条长度，防超长工具输出挤爆摘要请求）。
func renderMessagesForSummary(msgs []llm.Message) string {
	const maxPerMsg = 2000
	var sb strings.Builder
	for _, m := range msgs {
		role := "用户"
		switch m.Role {
		case llm.RoleAssistant:
			role = "助手"
		case llm.RoleTool:
			role = "工具"
		}
		text := textOfMessage(m)
		if text == "" {
			continue
		}
		if len([]rune(text)) > maxPerMsg {
			text = string([]rune(text)[:maxPerMsg]) + "…(截断)"
		}
		fmt.Fprintf(&sb, "[%s] %s\n", role, text)
	}
	return sb.String()
}

func textOfMessage(m llm.Message) string {
	var sb strings.Builder
	for _, c := range m.Content {
		if c.Type == llm.ContentText {
			sb.WriteString(c.Text)
		}
	}
	return sb.String()
}
