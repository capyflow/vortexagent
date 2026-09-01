package autonomous

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
)

// JSONGoalStore 是 GoalStore 的 JSON 文件实现。
//
// 每次 Save 全量落盘。适合目标数量 < 100 的场景。
// 目标数量大时，可替换为 Postgres 实现。
type JSONGoalStore struct {
	mu    sync.RWMutex
	file  string
	goals map[string]*Goal
}

// NewJSONGoalStore 创建 JSON 文件存储。
// 文件不存在时自动创建空文件；存在时加载已有目标。
func NewJSONGoalStore(file string) (*JSONGoalStore, error) {
	store := &JSONGoalStore{
		file:  file,
		goals: make(map[string]*Goal),
	}

	// 确保目录存在
	if dir := filepath.Dir(file); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nil, fmt.Errorf("创建存储目录失败: %w", err)
		}
	}

	// 加载已有数据
	if _, err := os.Stat(file); err == nil {
		if err := store.load(); err != nil {
			return nil, fmt.Errorf("加载目标文件失败: %w", err)
		}
	}

	return store, nil
}

// LoadAll 返回所有目标。
func (s *JSONGoalStore) LoadAll() ([]*Goal, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	goals := make([]*Goal, 0, len(s.goals))
	for _, g := range s.goals {
		goals = append(goals, g)
	}
	return goals, nil
}

// Save 保存或更新单个目标。
func (s *JSONGoalStore) Save(goal *Goal) error {
	s.mu.Lock()
	s.goals[goal.ID] = goal
	s.mu.Unlock()

	return s.persist()
}

// Delete 删除目标。
func (s *JSONGoalStore) Delete(id string) error {
	s.mu.Lock()
	delete(s.goals, id)
	s.mu.Unlock()

	return s.persist()
}

// Get 按 ID 获取目标。
func (s *JSONGoalStore) Get(id string) (*Goal, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	g, ok := s.goals[id]
	if !ok {
		return nil, fmt.Errorf("目标 %q 不存在", id)
	}
	return g, nil
}

// persist 将所有目标序列化到文件。
func (s *JSONGoalStore) persist() error {
	s.mu.RLock()
	data, err := json.MarshalIndent(s.goals, "", "  ")
	s.mu.RUnlock()

	if err != nil {
		return fmt.Errorf("序列化目标失败: %w", err)
	}

	if err := os.WriteFile(s.file, data, 0o600); err != nil {
		return fmt.Errorf("写入目标文件失败: %w", err)
	}
	return nil
}

// load 从文件加载目标。
func (s *JSONGoalStore) load() error {
	data, err := os.ReadFile(s.file)
	if err != nil {
		return err
	}

	var goals map[string]*Goal
	if err := json.Unmarshal(data, &goals); err != nil {
		return err
	}

	// 确保每个 goal 都有 mutex
	for _, g := range goals {
		if g.mu == nil {
			g.mu = &sync.RWMutex{}
		}
	}

	s.goals = goals
	return nil
}
