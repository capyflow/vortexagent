package ctxengine

import (
	"testing"

	"github.com/capyflow/vortexagent/llm"
)

// mkToolTurn 构造一轮带工具调用的消息：assistant(tool_calls) + 若干 tool 结果。
func mkToolTurn(callIDs []string, results map[string]string) []llm.Message {
	msgs := []llm.Message{}
	calls := make([]llm.ToolCall, 0, len(callIDs))
	for _, id := range callIDs {
		calls = append(calls, llm.ToolCall{ID: id, Name: "tool_" + id})
	}
	msgs = append(msgs, llm.Message{Role: llm.RoleAssistant, Content: []llm.Content{{Type: llm.ContentText, Text: "调用工具"}}, ToolCalls: calls})
	for _, id := range callIDs {
		msgs = append(msgs, llm.NewToolResultMessage(id, results[id]))
	}
	return msgs
}

func TestSplitBlocks_KeepsToolPairingAtomic(t *testing.T) {
	history := []llm.Message{
		llm.NewTextMessage(llm.RoleUser, "问题一"),
		llm.NewTextMessage(llm.RoleAssistant, "回答一"),
		llm.NewTextMessage(llm.RoleUser, "问题二"),
	}
	history = append(history, mkToolTurn([]string{"c1", "c2"}, map[string]string{"c1": "结果1", "c2": "结果2"})...)
	history = append(history, llm.NewTextMessage(llm.RoleAssistant, "回答二"))
	history = append(history, llm.NewTextMessage(llm.RoleUser, "问题三"))
	history = append(history, llm.NewTextMessage(llm.RoleAssistant, "回答三"))

	blocks := SplitBlocks(history)
	if len(blocks) != 3 {
		t.Fatalf("SplitBlocks = %d 块，期望 3", len(blocks))
	}
	// 块 2 必须完整包含 assistant(tool_calls) + 两个 tool 结果 + 后续助手回复。
	b2 := blocks[1]
	if b2.Messages[0].Role != llm.RoleUser {
		t.Fatalf("块 2 首条消息应为用户消息，得到 %s", b2.Messages[0].Role)
	}
	toolMsgs := 0
	hasToolCalls := false
	for _, m := range b2.Messages {
		if m.Role == llm.RoleTool {
			toolMsgs++
		}
		if len(m.ToolCalls) == 2 {
			hasToolCalls = true
		}
	}
	if !hasToolCalls || toolMsgs != 2 {
		t.Fatalf("块 2 未完整包含工具调用配对：hasToolCalls=%v, tool结果=%d", hasToolCalls, toolMsgs)
	}
}

func TestSplitBlocks_Empty(t *testing.T) {
	if got := SplitBlocks(nil); got != nil {
		t.Fatalf("空历史应返回 nil，得到 %v", got)
	}
}

func TestSplitBlocks_FlattenRoundTrip(t *testing.T) {
	history := []llm.Message{
		llm.NewTextMessage(llm.RoleUser, "a"),
		llm.NewTextMessage(llm.RoleAssistant, "b"),
	}
	history = append(history, mkToolTurn([]string{"c1"}, map[string]string{"c1": "r"})...)
	blocks := SplitBlocks(history)
	if got := FlattenBlocks(blocks); len(got) != len(history) {
		t.Fatalf("FlattenBlocks 消息数不一致：%d vs %d", len(got), len(history))
	}
}

func TestBlock_FirstUserText(t *testing.T) {
	b := Block{Messages: []llm.Message{
		llm.NewTextMessage(llm.RoleUser, "帮我查 postgres 报错"),
		llm.NewTextMessage(llm.RoleAssistant, "好的"),
	}}
	if got := b.FirstUserText(); got != "帮我查 postgres 报错" {
		t.Fatalf("FirstUserText = %q", got)
	}
}
