package sessionstore

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"sync"
)

type JSON struct {
	mu   sync.Mutex
	path string
	data map[string]*Session
}

func NewJSON(path string) (*JSON, error) {
	s := &JSON{path: path, data: make(map[string]*Session)}
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return s, nil
		}
		return nil, fmt.Errorf("读取会话文件失败: %w", err)
	}
	if len(data) > 0 {
		if err := json.Unmarshal(data, &s.data); err != nil {
			return nil, fmt.Errorf("解析会话文件失败: %w", err)
		}
	}
	return s, nil
}

func (s *JSON) Save(ctx context.Context, sess *Session) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	cp, err := cloneSession(sess)
	if err != nil {
		return fmt.Errorf("序列化会话失败: %w", err)
	}
	s.data[cp.ID] = cp
	return s.flushLocked()
}

func (s *JSON) Load(ctx context.Context, id string) (*Session, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	orig, ok := s.data[id]
	if !ok {
		return nil, nil
	}
	return cloneSession(orig)
}

func (s *JSON) Delete(ctx context.Context, id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.data, id)
	return s.flushLocked()
}

func (s *JSON) List(ctx context.Context) ([]*Session, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	sessions := make([]*Session, 0, len(s.data))
	for _, sess := range s.data {
		cp, err := cloneSession(sess)
		if err != nil {
			return nil, err
		}
		sessions = append(sessions, cp)
	}
	sortSessions(sessions)
	return sessions, nil
}

func (s *JSON) LoadLatest(ctx context.Context) (*Session, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var latest *Session
	for _, sess := range s.data {
		if latest == nil || sess.UpdatedAt.After(latest.UpdatedAt) {
			latest = sess
		}
	}
	if latest == nil {
		return nil, nil
	}
	return cloneSession(latest)
}

func (s *JSON) flushLocked() error {
	data, err := json.MarshalIndent(s.data, "", "  ")
	if err != nil {
		return fmt.Errorf("序列化会话失败: %w", err)
	}
	if err := os.WriteFile(s.path, data, 0o600); err != nil {
		return fmt.Errorf("写入会话文件失败: %w", err)
	}
	return nil
}

func cloneSession(s *Session) (*Session, error) {
	data, err := json.Marshal(s)
	if err != nil {
		return nil, err
	}
	var cp Session
	if err := json.Unmarshal(data, &cp); err != nil {
		return nil, err
	}
	return &cp, nil
}

var _ Store = (*JSON)(nil)
