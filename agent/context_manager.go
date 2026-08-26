package agent

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/capyflow/vortexagent/agent/sessionstore"
	"github.com/capyflow/vortexagent/llm"
)

type ContextManager struct {
	session      *sessionstore.Session
	offloadStore sessionstore.OffloadStore
	provider     llm.Provider
	model        string
	strategy     OffloadStrategy

	MaxActiveMessages int
	OffloadThreshold  int
	ContextWindow     int
}

type ContextManagerConfig struct {
	MaxActiveMessages int
	OffloadThreshold  int
	ContextWindow     int
	Model             string
	Strategy          OffloadStrategy
}

func NewContextManager(
	session *sessionstore.Session,
	offloadStore sessionstore.OffloadStore,
	provider llm.Provider,
	config *ContextManagerConfig,
) *ContextManager {
	maxActive := 20
	threshold := 30
	model := ""
	contextWindow := 0
	var strategy OffloadStrategy
	if config != nil {
		if config.MaxActiveMessages > 0 {
			maxActive = config.MaxActiveMessages
		}
		if config.OffloadThreshold > 0 {
			threshold = config.OffloadThreshold
		}
		model = config.Model
		contextWindow = config.ContextWindow
		strategy = config.Strategy
	}
	if strategy == nil {
		strategy = &SlidingWindowStrategy{}
	}
	if contextWindow == 0 {
		contextWindow = llm.GetContextWindow(model)
	}
	return &ContextManager{
		session:           session,
		offloadStore:      offloadStore,
		provider:          provider,
		model:             model,
		strategy:          strategy,
		MaxActiveMessages: maxActive,
		OffloadThreshold:  threshold,
		ContextWindow:     contextWindow,
	}
}

const OffloadThresholdRatio = 0.75

func (cm *ContextManager) ShouldOffload() bool {
	if cm.ContextWindow > 0 {
		usage := cm.ContextUsage()
		return usage >= OffloadThresholdRatio
	}
	return len(cm.session.History) > cm.OffloadThreshold
}

func (cm *ContextManager) EstimateTokens() int {
	total := 0
	for _, msg := range cm.session.History {
		for _, c := range msg.Content {
			if c.Type == llm.ContentText {
				total += llm.EstimateTokenCount(c.Text)
			}
		}
	}
	for _, chunk := range cm.session.Offloaded {
		total += llm.EstimateTokenCount(chunk.Summary)
	}
	return total
}

func (cm *ContextManager) ContextUsage() float64 {
	used := cm.EstimateTokens()
	if cm.ContextWindow == 0 {
		return 0
	}
	return float64(used) / float64(cm.ContextWindow)
}

func (cm *ContextManager) Offload(ctx context.Context) error {
	history := cm.session.History
	if len(history) <= cm.MaxActiveMessages {
		return nil
	}

	toOffload, toKeep := cm.strategy.SelectMessages(history, cm.MaxActiveMessages)
	if len(toOffload) == 0 {
		return nil
	}

	summary, err := cm.generateSummary(ctx, toOffload)
	if err != nil {
		return fmt.Errorf("生成摘要失败: %w", err)
	}

	chunk := sessionstore.OffloadedChunk{
		ID:        fmt.Sprintf("chunk-%d", time.Now().UnixMilli()),
		Summary:   summary,
		MsgCount:  len(toOffload),
		StartIdx:  0,
		EndIdx:    len(toOffload) - 1,
		CreatedAt: time.Now(),
	}

	if cm.offloadStore != nil {
		if err := cm.offloadStore.SaveChunk(ctx, cm.session.ID, &chunk, toOffload); err != nil {
			return fmt.Errorf("保存归档失败: %w", err)
		}
	}

	cm.session.History = toKeep
	cm.session.AddOffloaded(chunk)

	return nil
}

func (cm *ContextManager) generateSummary(ctx context.Context, messages []llm.Message) (string, error) {
	if cm.provider == nil {
		return cm.simpleSummary(messages), nil
	}

	var sb strings.Builder
	sb.WriteString("请将以下对话历史压缩成简洁的摘要，保留关键信息：\n\n")
	for _, msg := range messages {
		role := "用户"
		if msg.Role == llm.RoleAssistant {
			role = "助手"
		} else if msg.Role == llm.RoleTool {
			role = "工具"
		}
		for _, c := range msg.Content {
			if c.Type == llm.ContentText {
				sb.WriteString(fmt.Sprintf("[%s] %s\n", role, c.Text))
			}
		}
	}
	sb.WriteString("\n摘要：")

	req := &llm.ChatRequest{
		Model: cm.model,
		Messages: []llm.Message{
			llm.NewTextMessage(llm.RoleSystem, "你是一个对话摘要助手。请将对话历史压缩成简洁的摘要，保留关键信息、决策和结果。摘要应不超过200字。"),
			llm.NewTextMessage(llm.RoleUser, sb.String()),
		},
		MaxTokens: 300,
	}

	resp, err := cm.provider.Chat(ctx, req, nil)
	if err != nil {
		return cm.simpleSummary(messages), nil
	}

	summary := ""
	for _, c := range resp.Message.Content {
		if c.Type == llm.ContentText {
			summary += c.Text
		}
	}

	if strings.TrimSpace(summary) == "" {
		return cm.simpleSummary(messages), nil
	}
	return summary, nil
}

func (cm *ContextManager) simpleSummary(messages []llm.Message) string {
	userCount := 0
	assistantCount := 0
	toolCount := 0
	for _, msg := range messages {
		switch msg.Role {
		case llm.RoleUser:
			userCount++
		case llm.RoleAssistant:
			assistantCount++
		case llm.RoleTool:
			toolCount++
		}
	}
	return fmt.Sprintf("历史对话摘要：%d条用户消息，%d条助手回复，%d条工具调用", userCount, assistantCount, toolCount)
}

func (cm *ContextManager) BuildMessages() []llm.Message {
	msgs := make([]llm.Message, 0)

	offloaded := cm.session.GetOffloaded()
	if len(offloaded) > 0 {
		summary := cm.buildOffloadedSummary(offloaded)
		msgs = append(msgs, llm.NewTextMessage(llm.RoleSystem, summary))
	}

	msgs = append(msgs, cm.session.History...)

	return msgs
}

func (cm *ContextManager) buildOffloadedSummary(offloaded []sessionstore.OffloadedChunk) string {
	var sb strings.Builder
	sb.WriteString("【历史对话摘要】\n")
	for i, chunk := range offloaded {
		sb.WriteString(fmt.Sprintf("%d. %s\n", i+1, chunk.Summary))
	}
	return sb.String()
}

func (cm *ContextManager) Retrieve(ctx context.Context, query string) (string, error) {
	if cm.offloadStore == nil {
		return "", nil
	}

	chunks, err := cm.offloadStore.ListChunks(ctx, cm.session.ID)
	if err != nil {
		return "", err
	}
	if len(chunks) == 0 {
		return "", nil
	}

	var sb strings.Builder
	sb.WriteString("【相关历史对话】\n")
	for _, chunk := range chunks {
		if cm.isRelevant(chunk.Summary, query) {
			sb.WriteString(fmt.Sprintf("- %s\n", chunk.Summary))
		}
	}
	return sb.String(), nil
}

func (cm *ContextManager) isRelevant(summary, query string) bool {
	queryLower := strings.ToLower(query)
	summaryLower := strings.ToLower(summary)
	return strings.Contains(summaryLower, queryLower) || strings.Contains(queryLower, summaryLower)
}
