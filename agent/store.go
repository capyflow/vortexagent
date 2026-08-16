// 本文件实现会话存储（SessionStore）：会话持久化的抽象接口与内置实现
// （内存 / JSON 文件）。把 Options.Store 设置为任意实现，Ask 结束即自动保存。
package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"sync"
)

// SessionStore 是会话持久化的抽象接口，框架用户可实现自己的存储后端
// （SQLite、Redis、云存储等），或使用内置的 MemorySessionStore /
// JSONSessionStore。把 Agent.Options.Store 设置为任意实现即可自动持久化。
type SessionStore interface {
	// Save 保存或覆盖一个会话。实现可存储引用或快照（见各实现的文档），
	// 但必须保证后续 Load 返回的会话可安全使用。
	Save(ctx context.Context, s *Session) error

	// Load 按 ID 读取会话；不存在时返回 (nil, nil)。
	Load(ctx context.Context, id string) (*Session, error)

	// Delete 删除会话；不存在时静默成功。
	Delete(ctx context.Context, id string) error
}

// MemorySessionStore 是进程内内存存储（框架默认行为）：不提供跨进程持久化，
// 但满足 SessionStore 接口，可作为默认实现与测试替身。
// 注意：本实现存储的是会话引用（非快照），调用方对会话的修改会反映到存储中。
type MemorySessionStore struct {
	mu   sync.Mutex
	data map[string]*Session
}

// NewMemorySessionStore 创建一个空的内存会话存储。
func NewMemorySessionStore() *MemorySessionStore {
	return &MemorySessionStore{data: make(map[string]*Session)}
}

// Save 保存会话到内存。
func (s *MemorySessionStore) Save(_ context.Context, sess *Session) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.data[sess.ID] = sess
	return nil
}

// Load 按 ID 读取会话，不存在时返回 (nil, nil)。
func (s *MemorySessionStore) Load(_ context.Context, id string) (*Session, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.data[id], nil
}

// Delete 删除会话，不存在时静默成功。
func (s *MemorySessionStore) Delete(_ context.Context, id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.data, id)
	return nil
}

// JSONSessionStore 把会话持久化到单个 JSON 文件（map[会话ID]Session），
// 供单进程 CLI / 示例应用在进程重启后恢复会话使用。每次 Save 全量落盘，
// 适合会话数少、频率低的场景；大规模场景请换用数据库后端。
type JSONSessionStore struct {
	mu   sync.Mutex
	path string
	data map[string]*Session
}

// NewJSONSessionStore 打开（或创建）指定路径的会话文件。
// 文件不存在时从空存储开始；文件损坏时返回错误。
func NewJSONSessionStore(path string) (*JSONSessionStore, error) {
	s := &JSONSessionStore{path: path, data: make(map[string]*Session)}
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return s, nil // 文件不存在：从空存储开始
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

// Save 保存会话并立即落盘。
func (s *JSONSessionStore) Save(ctx context.Context, sess *Session) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	// 深拷贝：落盘的必须是快照，调用方后续对会话的修改不影响已保存数据。
	cp, err := cloneSession(sess)
	if err != nil {
		return fmt.Errorf("序列化会话失败: %w", err)
	}
	s.data[cp.ID] = cp
	return s.flushLocked()
}

// Load 按 ID 读取会话，不存在时返回 (nil, nil)。返回的是副本，修改不影响存储。
func (s *JSONSessionStore) Load(ctx context.Context, id string) (*Session, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	orig, ok := s.data[id]
	if !ok {
		return nil, nil
	}
	return cloneSession(orig)
}

// Delete 删除会话，不存在时静默成功。
func (s *JSONSessionStore) Delete(ctx context.Context, id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.data, id)
	return s.flushLocked()
}

// LoadLatest 返回最近更新（UpdatedAt 最大）的会话，用于"继续上次对话"。
// 无任何会话时返回 (nil, nil)。返回的是副本，修改不影响存储。
func (s *JSONSessionStore) LoadLatest(ctx context.Context) (*Session, error) {
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

// flushLocked 将内存数据全量写入文件（调用方需持有锁）。
func (s *JSONSessionStore) flushLocked() error {
	data, err := json.MarshalIndent(s.data, "", "  ")
	if err != nil {
		return fmt.Errorf("序列化会话失败: %w", err)
	}
	if err := os.WriteFile(s.path, data, 0o600); err != nil {
		return fmt.Errorf("写入会话文件失败: %w", err)
	}
	return nil
}

// cloneSession 通过 JSON 往返深拷贝一个会话。
// 注意：Content 中的图片 Data 字段标有 json:"-"，往返后会丢失，
// 当前会话模型不依赖跨存储的图片字节，如需支持请扩展序列化方案。
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

// compile-time 断言：内置存储均满足 SessionStore 接口。
var (
	_ SessionStore = (*MemorySessionStore)(nil)
	_ SessionStore = (*JSONSessionStore)(nil)
)
