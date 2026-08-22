package sessionstore

import (
	"crypto/rand"
	"encoding/hex"
	"strconv"
	"time"

	"github.com/capyflow/vortexagent/llm"
)

type Session struct {
	ID        string
	Model     string
	History   []llm.Message
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
	s.History = append(s.History, m)
	s.UpdatedAt = time.Now()
}

func (s *Session) Messages() []llm.Message {
	return s.History
}

func (s *Session) Clear() {
	s.History = nil
	s.UpdatedAt = time.Now()
}

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

func (s *Session) Rollback(n int) {
	if n <= 0 {
		s.History = nil
	} else if n < len(s.History) {
		s.History = s.History[:n]
	}
	s.UpdatedAt = time.Now()
}
