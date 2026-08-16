package agent

import (
	"time"

	"github.com/capyflow/vortexagent/llm"
)

// Session 维护一次对话的完整消息历史。
//
// 历史包含 system 之外的 user/assistant/tool 消息，
// 每次 Ask 都会把完整历史发给模型（上下文管理后续迭代优化）。
type Session struct {
	ID        string        // 会话 ID
	Model     string        // 使用的模型名称
	History   []llm.Message // 消息历史
	CreatedAt time.Time     // 创建时间
	UpdatedAt time.Time     // 最后更新时间
}

// NewSession 创建一个新会话。
func NewSession(model string) *Session {
	now := time.Now()
	return &Session{
		ID:        now.Format("20060102-150405"),
		Model:     model,
		CreatedAt: now,
		UpdatedAt: now,
	}
}

// Add 追加一条消息并更新时间戳。
func (s *Session) Add(m llm.Message) {
	s.History = append(s.History, m)
	s.UpdatedAt = time.Now()
}

// Messages 返回完整历史（调用方不要修改返回的切片）。
func (s *Session) Messages() []llm.Message {
	return s.History
}

// Clear 清空历史，保留会话元信息。
func (s *Session) Clear() {
	s.History = nil
	s.UpdatedAt = time.Now()
}

// Trim 截断历史到最近 n 条（n <= 0 时清空）。
func (s *Session) Trim(n int) {
	if n <= 0 || n >= len(s.History) {
		if n <= 0 {
			s.History = nil
		}
		return
	}
	s.History = s.History[len(s.History)-n:]
	s.UpdatedAt = time.Now()
}
