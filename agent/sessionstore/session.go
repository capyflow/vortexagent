package sessionstore

import (
	"crypto/rand"
	"encoding/hex"
	"strconv"
	"sync"
	"time"

	"github.com/capyflow/vortexagent/llm"
)

type OffloadedChunk struct {
	ID        string         `json:"id"`
	Summary   string         `json:"summary"`
	MsgCount  int            `json:"msg_count"`
	StartIdx  int            `json:"start_idx"` // 原始完整历史中的起始位置
	EndIdx    int            `json:"end_idx"`   // 原始完整历史中的结束位置
	CreatedAt time.Time      `json:"created_at"`
	Metadata  map[string]any `json:"metadata,omitempty"`
}

type Session struct {
	mu        sync.RWMutex
	ID        string
	Model     string
	History   []llm.Message    // 活跃区
	Offloaded []OffloadedChunk // 卸载区（历史摘要）
	CreatedAt time.Time
	UpdatedAt time.Time
}

func NewSession(model string) *Session {
	now := time.Now()
	return &Session{
		ID:        now.Format("20060102-150405") + "-" + shortID(),
		Model:     model,
		CreatedAt: now,
		UpdatedAt: now,
	}
}

func shortID() string {
	var b [4]byte
	if _, err := rand.Read(b[:]); err != nil {
		return strconv.FormatInt(time.Now().UnixNano(), 16)
	}
	return hex.EncodeToString(b[:])
}

func (s *Session) Add(m llm.Message) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.History = append(s.History, m)
	s.UpdatedAt = time.Now()
}

func (s *Session) Messages() []llm.Message {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.History
}

func (s *Session) Clear() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.History = nil
	s.UpdatedAt = time.Now()
}

func (s *Session) Trim(n int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if n <= 0 || n >= len(s.History) {
		if n <= 0 {
			s.History = nil
		}
		return
	}
	s.History = s.History[len(s.History)-n:]
	s.UpdatedAt = time.Now()
}

func (s *Session) Rollback(n int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if n <= 0 {
		s.History = nil
	} else if n < len(s.History) {
		s.History = s.History[:n]
	}
	s.UpdatedAt = time.Now()
}

// Reset 用 other 的内容替换当前会话的内容（切换 / 新建会话时使用）。
// 只拷贝业务字段，不拷贝内部锁——直接对 Session 取值赋值会连 sync.RWMutex 一起拷贝。
func (s *Session) Reset(other *Session) {
	if other == nil {
		return
	}
	other.mu.RLock()
	defer other.mu.RUnlock()
	s.mu.Lock()
	defer s.mu.Unlock()
	s.ID = other.ID
	s.Model = other.Model
	s.History = other.History
	s.Offloaded = other.Offloaded
	s.CreatedAt = other.CreatedAt
	s.UpdatedAt = other.UpdatedAt
}

func (s *Session) AddOffloaded(chunk OffloadedChunk) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.Offloaded = append(s.Offloaded, chunk)
	s.UpdatedAt = time.Time{}
}

func (s *Session) GetOffloaded() []OffloadedChunk {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.Offloaded
}

func (s *Session) ClearOffloaded() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.Offloaded = nil
	s.UpdatedAt = time.Time{}
}

func (s *Session) OffloadedCount() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return len(s.Offloaded)
}
