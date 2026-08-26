package agent

import (
	"github.com/capyflow/vortexagent/llm"
)

type OffloadStrategy interface {
	SelectMessages(history []llm.Message, keepCount int) (toOffload []llm.Message, toKeep []llm.Message)
}

type SlidingWindowStrategy struct{}

func (s *SlidingWindowStrategy) SelectMessages(history []llm.Message, keepCount int) ([]llm.Message, []llm.Message) {
	if len(history) <= keepCount {
		return nil, history
	}
	offloadCount := len(history) - keepCount
	return history[:offloadCount], history[offloadCount:]
}

type ImportanceBasedStrategy struct {
	KeepUserMessages    bool
	KeepToolResults     bool
	KeepAssistantWithTC bool
}

func (s *ImportanceBasedStrategy) SelectMessages(history []llm.Message, keepCount int) ([]llm.Message, []llm.Message) {
	if len(history) <= keepCount {
		return nil, history
	}

	important := make([]llm.Message, 0)
	rest := make([]llm.Message, 0)

	for _, msg := range history {
		if s.isImportant(msg) {
			important = append(important, msg)
		} else {
			rest = append(rest, msg)
		}
	}

	totalKeep := keepCount
	if len(important) > totalKeep {
		keepImportant := important[len(important)-totalKeep:]
		offloadImportant := important[:len(important)-totalKeep]
		offloadAll := append(offloadImportant, rest...)
		return offloadAll, keepImportant
	}

	needFromRest := totalKeep - len(important)
	if needFromRest > len(rest) {
		needFromRest = len(rest)
	}
	keepFromRest := rest[len(rest)-needFromRest:]
	offloadFromRest := rest[:len(rest)-needFromRest]

	return offloadFromRest, append(important, keepFromRest...)
}

func (s *ImportanceBasedStrategy) isImportant(msg llm.Message) bool {
	if s.KeepUserMessages && msg.Role == llm.RoleUser {
		return true
	}
	if s.KeepToolResults && msg.Role == llm.RoleTool {
		return true
	}
	if s.KeepAssistantWithTC && msg.Role == llm.RoleAssistant && len(msg.ToolCalls) > 0 {
		return true
	}
	return false
}

type TimeDecayStrategy struct {
	RecentRatio float64
}

func (s *TimeDecayStrategy) SelectMessages(history []llm.Message, keepCount int) ([]llm.Message, []llm.Message) {
	if len(history) <= keepCount {
		return nil, history
	}

	recentCount := int(float64(keepCount) * s.RecentRatio)
	if recentCount < 1 {
		recentCount = 1
	}
	if recentCount > keepCount {
		recentCount = keepCount
	}

	offloadCount := len(history) - keepCount
	return history[:offloadCount], history[offloadCount:]
}
