package auth

import (
	"context"
	"sync"
)

// Store 是 Session 的持久化抽象：key 由调用方定义（如 MCP server 名）。
// 文件/Keychain/数据库实现由调用方提供；包内只带内存实现供测试与示例使用。
type Store interface {
	Get(ctx context.Context, key string) (*Session, error)
	Put(ctx context.Context, key string, session *Session) error
	Delete(ctx context.Context, key string) error
}

// MemoryStore 是并发安全的内存 Store。
type MemoryStore struct {
	mu sync.RWMutex
	m  map[string]Session
}

// NewMemoryStore 创建空的内存 Store。
func NewMemoryStore() *MemoryStore {
	return &MemoryStore{m: map[string]Session{}}
}

func (s *MemoryStore) Get(ctx context.Context, key string) (*Session, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	v, ok := s.m[key]
	if !ok {
		return nil, ErrNotFound
	}
	cp := v
	return &cp, nil
}

func (s *MemoryStore) Put(ctx context.Context, key string, session *Session) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if session == nil {
		return ErrNotFound
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.m[key] = *session
	return nil
}

func (s *MemoryStore) Delete(ctx context.Context, key string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.m, key)
	return nil
}
