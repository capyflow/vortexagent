package sessionstore

import (
	"context"
	"sync"
)

type Memory struct {
	mu   sync.Mutex
	data map[string]*Session
}

func NewMemory() *Memory {
	return &Memory{data: make(map[string]*Session)}
}

func (s *Memory) Save(_ context.Context, sess *Session) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.data[sess.ID] = sess
	return nil
}

func (s *Memory) Load(_ context.Context, id string) (*Session, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.data[id], nil
}

func (s *Memory) Delete(_ context.Context, id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.data, id)
	return nil
}

func (s *Memory) List(_ context.Context) ([]*Session, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	sessions := make([]*Session, 0, len(s.data))
	for _, sess := range s.data {
		sessions = append(sessions, sess)
	}
	sortSessions(sessions)
	return sessions, nil
}

var _ Store = (*Memory)(nil)
