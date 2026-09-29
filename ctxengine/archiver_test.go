package ctxengine

import (
	"context"
	"strings"
	"testing"

	"github.com/capyflow/vortexagent/llm"
)

// scriptedLLMProvider 按预设文本序列响应 Chat。
type scriptedLLMProvider struct {
	responses []string
	errs      []error // 与 responses 等长时按位取；否则全部成功
	i         int
}

func (p *scriptedLLMProvider) Name() string       { return "scripted" }
func (p *scriptedLLMProvider) ContextWindow() int { return 128000 }
func (p *scriptedLLMProvider) Chat(context.Context, *llm.ChatRequest, func(llm.Delta) error) (*llm.ChatResponse, error) {
	i := p.i
	p.i++
	if i < len(p.errs) && p.errs[i] != nil {
		return nil, p.errs[i]
	}
	text := "ok"
	if i < len(p.responses) {
		text = p.responses[i]
	}
	return &llm.ChatResponse{Message: llm.NewTextMessage(llm.RoleAssistant, text)}, nil
}

const validHandoff = `{"handoff":{"task":"排查 postgres 报错","facts":"连接池耗尽","decisions":"改用 pgx","errors":"连接超时","pending":"压测验证","status":"待压测"},"candidate_facts":["生产库连接池上限 200","已从 lib/pq 迁移到 pgx"]}`

func TestLLMSummarizer_ValidJSON(t *testing.T) {
	s := NewLLMSummarizer(&scriptedLLMProvider{responses: []string{validHandoff}}, "m")
	handoff, facts, err := s.Summarize(context.Background(), []llm.Message{llm.NewTextMessage(llm.RoleUser, "x")})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"【任务意图】排查 postgres 报错", "【决策记录】改用 pgx", "【当前状态】待压测"} {
		if !strings.Contains(handoff, want) {
			t.Fatalf("交接摘要缺少 %q：%s", want, handoff)
		}
	}
	if len(facts) != 2 || facts[0] != "生产库连接池上限 200" {
		t.Fatalf("候选事实错误：%v", facts)
	}
}

func TestLLMSummarizer_RetryAfterBadJSON(t *testing.T) {
	// 第一次输出非法 JSON，第二次合法：应通过重试恢复。
	s := NewLLMSummarizer(&scriptedLLMProvider{responses: []string{"抱歉我不会", validHandoff}}, "m")
	handoff, facts, err := s.Summarize(context.Background(), []llm.Message{llm.NewTextMessage(llm.RoleUser, "x")})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(handoff, "【任务意图】") || len(facts) != 2 {
		t.Fatalf("重试后应产出结构化交接：%s %v", handoff, facts)
	}
}

func TestLLMSummarizer_FallbackToFreeText(t *testing.T) {
	// 两次都非法 JSON → 降级自由文本摘要（第三次调用）。
	s := NewLLMSummarizer(&scriptedLLMProvider{
		responses: []string{"不是 JSON", "还不是 JSON", "这是自由文本摘要：postgres 问题已解决"},
	}, "m")
	handoff, facts, err := s.Summarize(context.Background(), []llm.Message{llm.NewTextMessage(llm.RoleUser, "x")})
	if err != nil {
		t.Fatal(err)
	}
	if facts != nil {
		t.Fatalf("降级路径不应有候选事实：%v", facts)
	}
	if !strings.Contains(handoff, "自由文本摘要") {
		t.Fatalf("应返回自由文本摘要：%s", handoff)
	}
}

func TestLLMSummarizer_AllFailed_ReturnsError(t *testing.T) {
	// 全部失败：返回错误，调用方放弃归档（原文不动）。
	s := NewLLMSummarizer(&scriptedLLMProvider{
		responses: []string{"a", "b", "c", "d"},
		errs:      []error{errString("timeout"), errString("timeout"), errString("timeout"), nil},
	}, "m")
	_, _, err := s.Summarize(context.Background(), []llm.Message{llm.NewTextMessage(llm.RoleUser, "x")})
	if err == nil {
		t.Fatal("全部失败应返回错误")
	}
}

func TestParseHandoff_ToleratesCodeFence(t *testing.T) {
	out := "```json\n" + validHandoff + "\n```"
	handoff, facts, ok := parseHandoff(out)
	if !ok || !strings.Contains(handoff, "【任务意图】") || len(facts) != 2 {
		t.Fatalf("应容忍代码块包裹：%q %v", handoff, facts)
	}
}
