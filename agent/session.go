// 本文件实现会话（Session）：一次对话的完整消息历史，即 agent 的"内存"。
// 每次 Ask 都会把完整历史发给模型，模型才能"记得"之前聊了什么。
package agent

import (
	"crypto/rand"
	"encoding/hex"
	"strconv"
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
//
// ID 由秒级时间戳 + 随机后缀组成，避免同一秒内创建多个会话时 ID 冲突。
func NewSession(model string) *Session {
	now := time.Now()
	return &Session{
		ID:        now.Format("20060102-150405") + "-" + shortID(),
		Model:     model,
		CreatedAt: now,
		UpdatedAt: now,
	}
}

// shortID 生成 4 字节十六进制随机串；crypto/rand 失败时退化为纳秒时间戳。
func shortID() string {
	var b [4]byte
	if _, err := rand.Read(b[:]); err != nil {
		return strconv.FormatInt(time.Now().UnixNano(), 16)
	}
	return hex.EncodeToString(b[:])
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

// Rollback 回滚历史到前 n 条（n <= 0 时清空）。
//
// 用于 Ask 失败时丢弃未完成轮次残留的 user/assistant/tool 消息，
// 避免下一次提问携带上一次未回答的问题。
func (s *Session) Rollback(n int) {
	if n <= 0 {
		s.History = nil
	} else if n < len(s.History) {
		s.History = s.History[:n]
	}
	s.UpdatedAt = time.Now()
}
