package agent

import (
	"context"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"github.com/capyflow/vortexagent/agent/sessionstore"
	"github.com/capyflow/vortexagent/llm"
)

type slowTool struct {
	delay time.Duration
	calls atomic.Int32
}

func (t *slowTool) Name() string        { return "slow_tool" }
func (t *slowTool) Description() string { return "一个执行较慢的工具" }
func (t *slowTool) Schema() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"id": map[string]any{"type": "string"},
		},
	}
}
func (t *slowTool) Call(ctx context.Context, args map[string]any) (string, error) {
	t.calls.Add(1)
	time.Sleep(t.delay)
	id, _ := args["id"].(string)
	return fmt.Sprintf("完成: %s", id), nil
}

func TestParallelTools(t *testing.T) {
	tool := &slowTool{delay: 100 * time.Millisecond}

	registry := NewRegistry()
	registry.Add(tool)

	var callCount atomic.Int32
	mockProvider := &mockProviderForParallel{
		responses: []*llm.ChatResponse{
			{
				Message: llm.Message{
					Role: llm.RoleAssistant,
					ToolCalls: []llm.ToolCall{
						{ID: "call_1", Name: "slow_tool", Arguments: map[string]any{"id": "task1"}},
						{ID: "call_2", Name: "slow_tool", Arguments: map[string]any{"id": "task2"}},
						{ID: "call_3", Name: "slow_tool", Arguments: map[string]any{"id": "task3"}},
					},
				},
			},
			{
				Message: llm.Message{
					Role:    llm.RoleAssistant,
					Content: []llm.Content{{Type: llm.ContentText, Text: "所有任务完成"}},
				},
			},
		},
		callIndex: &callCount,
	}

	t.Run("串行模式", func(t *testing.T) {
		callCount.Store(0)
		tool.calls.Store(0)

		ag := New(Options{
			Provider:      mockProvider,
			Registry:      registry,
			ParallelTools: false,
		})

		session := sessionstore.NewSession("test")
		start := time.Now()

		answer, err := ag.Ask(context.Background(), session, "并行测试")
		if err != nil {
			t.Fatalf("Ask 失败: %v", err)
		}

		elapsed := time.Since(start)
		totalCalls := tool.calls.Load()

		if totalCalls != 3 {
			t.Errorf("期望 3 次调用，实际 %d", totalCalls)
		}
		if elapsed < 300*time.Millisecond {
			t.Errorf("串行模式应该耗时 >= 300ms，实际 %v", elapsed)
		}
		if answer != "所有任务完成" {
			t.Errorf("期望回答 '所有任务完成'，实际 '%s'", answer)
		}
		t.Logf("串行模式: %v, 调用次数: %d", elapsed, totalCalls)
	})

	t.Run("并行模式", func(t *testing.T) {
		callCount.Store(0)
		tool.calls.Store(0)

		ag := New(Options{
			Provider:      mockProvider,
			Registry:      registry,
			ParallelTools: true,
		})

		session := sessionstore.NewSession("test")
		start := time.Now()

		answer, err := ag.Ask(context.Background(), session, "并行测试")
		if err != nil {
			t.Fatalf("Ask 失败: %v", err)
		}

		elapsed := time.Since(start)
		totalCalls := tool.calls.Load()

		if totalCalls != 3 {
			t.Errorf("期望 3 次调用，实际 %d", totalCalls)
		}
		if elapsed >= 300*time.Millisecond {
			t.Errorf("并行模式应该耗时 < 300ms，实际 %v", elapsed)
		}
		if answer != "所有任务完成" {
			t.Errorf("期望回答 '所有任务完成'，实际 '%s'", answer)
		}
		t.Logf("并行模式: %v, 调用次数: %d", elapsed, totalCalls)
	})
}

type mockProviderForParallel struct {
	responses []*llm.ChatResponse
	callIndex *atomic.Int32
}

func (m *mockProviderForParallel) Name() string      { return "mock" }
func (m *mockProviderForParallel) ContextWindow() int { return 4096 }

func (m *mockProviderForParallel) Chat(ctx context.Context, req *llm.ChatRequest, onDelta func(llm.Delta) error) (*llm.ChatResponse, error) {
	idx := int(m.callIndex.Add(1) - 1)
	if idx >= len(m.responses) {
		idx = len(m.responses) - 1
	}
	return m.responses[idx], nil
}
