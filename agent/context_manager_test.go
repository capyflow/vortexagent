package agent

import (
	"context"
	"fmt"
	"testing"

	"github.com/capyflow/vortexagent/agent/sessionstore"
	"github.com/capyflow/vortexagent/llm"
)

type mockProvider struct {
	responses []string
	callCount int
}

func (m *mockProvider) Name() string              { return "mock" }
func (m *mockProvider) ContextWindow() int         { return 128000 }

func (m *mockProvider) Chat(ctx context.Context, req *llm.ChatRequest, onDelta func(llm.Delta) error) (*llm.ChatResponse, error) {
	if m.callCount < len(m.responses) {
		resp := m.responses[m.callCount]
		m.callCount++
		return &llm.ChatResponse{
			Message: llm.NewTextMessage(llm.RoleAssistant, resp),
		}, nil
	}
	return &llm.ChatResponse{
		Message: llm.NewTextMessage(llm.RoleAssistant, "ok"),
	}, nil
}

func TestContextManager_OffloadTriggered(t *testing.T) {
	session := sessionstore.NewSession("test")
	provider := &mockProvider{
		responses: []string{"摘要内容"},
	}

	cm := NewContextManager(session, nil, provider, &ContextManagerConfig{
		MaxActiveMessages: 3,
		OffloadThreshold:  5,
		ContextWindow:     10,
	})

	for i := 0; i < 8; i++ {
		session.Add(llm.NewTextMessage(llm.RoleUser, fmt.Sprintf("消息 %d", i)))
	}

	t.Logf("History length: %d", len(session.History))
	t.Logf("Estimated tokens: %d", cm.EstimateTokens())
	t.Logf("Context window: %d", cm.ContextWindow)
	t.Logf("Context usage: %.2f", cm.ContextUsage())
	t.Logf("ShouldOffload: %v", cm.ShouldOffload())

	if !cm.ShouldOffload() {
		t.Fatal("ShouldOffload should return true")
	}

	err := cm.Offload(context.Background())
	if err != nil {
		t.Fatalf("Offload failed: %v", err)
	}

	if len(session.History) != 3 {
		t.Errorf("expected 3 messages in History, got %d", len(session.History))
	}

	if len(session.Offloaded) != 1 {
		t.Fatalf("expected 1 offloaded chunk, got %d", len(session.Offloaded))
	}

	if session.Offloaded[0].Summary != "摘要内容" {
		t.Errorf("summary mismatch: got %q", session.Offloaded[0].Summary)
	}

	if session.Offloaded[0].MsgCount != 5 {
		t.Errorf("expected MsgCount=5, got %d", session.Offloaded[0].MsgCount)
	}
}

func TestContextManager_BuildMessagesWithOffloaded(t *testing.T) {
	session := sessionstore.NewSession("test")
	provider := &mockProvider{responses: []string{"摘要"}}

	cm := NewContextManager(session, nil, provider, &ContextManagerConfig{
		MaxActiveMessages: 3,
		OffloadThreshold:  5,
		ContextWindow:     10,
	})

	for i := 0; i < 8; i++ {
		session.Add(llm.NewTextMessage(llm.RoleUser, fmt.Sprintf("消息 %d", i)))
	}

	cm.Offload(context.Background())

	msgs := cm.BuildMessages()

	if len(msgs) != 4 {
		t.Fatalf("expected 4 messages (1 summary + 3 active), got %d", len(msgs))
	}

	if msgs[0].Role != llm.RoleSystem {
		t.Errorf("first message should be System, got %v", msgs[0].Role)
	}

	for _, c := range msgs[0].Content {
		if c.Type == llm.ContentText && c.Text == "" {
			t.Error("summary should not be empty")
		}
	}
}

func TestContextManager_NoOffloadWhenBelowThreshold(t *testing.T) {
	session := sessionstore.NewSession("test")
	provider := &mockProvider{}

	cm := NewContextManager(session, nil, provider, &ContextManagerConfig{
		MaxActiveMessages: 3,
		OffloadThreshold:  5,
		ContextWindow:     20,
	})

	for i := 0; i < 4; i++ {
		session.Add(llm.NewTextMessage(llm.RoleUser, fmt.Sprintf("消息 %d", i)))
	}

	if cm.ShouldOffload() {
		t.Fatal("ShouldOffload should return false when below threshold")
	}
}

func TestContextManager_MultipleOffloads(t *testing.T) {
	session := sessionstore.NewSession("test")
	provider := &mockProvider{
		responses: []string{"摘要1", "摘要2"},
	}

	cm := NewContextManager(session, nil, provider, &ContextManagerConfig{
		MaxActiveMessages: 3,
		OffloadThreshold:  5,
		ContextWindow:     10,
	})

	for i := 0; i < 5; i++ {
		session.Add(llm.NewTextMessage(llm.RoleUser, fmt.Sprintf("消息 %d", i)))
	}
	cm.Offload(context.Background())

	for i := 0; i < 5; i++ {
		session.Add(llm.NewTextMessage(llm.RoleUser, fmt.Sprintf("新消息 %d", i)))
	}
	cm.Offload(context.Background())

	if len(session.History) != 3 {
		t.Errorf("expected 3 messages, got %d", len(session.History))
	}

	if len(session.Offloaded) != 2 {
		t.Fatalf("expected 2 offloaded chunks, got %d", len(session.Offloaded))
	}
}
