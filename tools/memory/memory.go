// Package memory 是框架内置的"长期记忆"扩展工具：跨会话持久化的事实记忆库，
// 支持增删改查（memory_save / memory_update / memory_delete / memory_get /
// memory_search / memory_list）。
//
// 与框架里其他三类"记忆"机制的边界：
//   - Session.History：单次会话内的短期记忆，会话结束即封存；
//   - ContextManager 卸载（OffloadedChunk）：会话内历史压缩归档，按 sessionID 隔离；
//   - tools/knowledge：人工维护的静态文档语料，agent 只读。
//
// 本包是 agent 在运行中自己写入、跨会话跨进程存活的记忆。粒度为"一条记忆一个
// 自包含的事实"（偏好、决策、教训等），不保存对话原文——对话历史的沉淀是
// ContextManager 卸载的职责。
//
// 第一版采用 JSON 文件存储 + 关键词评分检索（CJK 二元组分词），仅用标准库。
// 两个抽象为后续演进预留：
//   - Store：存储接口，文件实现可替换为 Postgres 等；
//   - Searcher：检索接口，关键词评分可替换为向量检索（RAG 化），工具层零改动。
package memory

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"sync"
	"time"
)

// 记忆类型。kind 是开放检索的过滤维度，取值收紧为四个，防止模型自由发挥
// 导致过滤维度失效。
const (
	KindFact       = "fact"       // 事实：项目结构、环境配置、外部系统行为等客观信息
	KindPreference = "preference" // 偏好：用户的工作习惯与喜好
	KindDecision   = "decision"   // 决策：已确定的技术选型、方案约定
	KindLesson     = "lesson"     // 教训：踩过的坑、验证有效的做法
)

// ImportanceDefault 新记忆的默认重要级。
const ImportanceDefault = 3

// Memory 是一条长期记忆：一个自包含的事实陈述，而非对话原文。
type Memory struct {
	ID         string    `json:"id"`
	Content    string    `json:"content"`
	Kind       string    `json:"kind,omitempty"`
	Tags       []string  `json:"tags,omitempty"`
	Importance int       `json:"importance,omitempty"` // 1-5，5 最重要
	CreatedAt  time.Time `json:"created_at"`
	UpdatedAt  time.Time `json:"updated_at"`
}

// Store 是记忆的持久化抽象，实现方必须并发安全。
type Store interface {
	// Put 新增或按 ID 覆盖更新一条记忆。ID 为空时自动生成；
	// CreatedAt 为空时置为当前时间；UpdatedAt 总是刷新为当前时间。
	Put(ctx context.Context, m *Memory) error
	// Get 按 ID 取记忆，不存在返回 (nil, nil)。
	Get(ctx context.Context, id string) (*Memory, error)
	// Delete 按 ID 删除记忆，不存在时静默成功（由工具层负责先查出
	// 记忆并向模型报告"未找到"）。
	Delete(ctx context.Context, id string) error
	// List 返回全部记忆（顺序不限）。
	List(ctx context.Context) ([]*Memory, error)
}

// Searcher 决定"查"的检索方式：从候选记忆中选出与 query 最相关的至多 limit 条。
//
// 默认实现 KeywordSearcher 是零依赖的关键词评分检索；当记忆量大或需要语义
// 泛化（如"讨厌冗余代码"能召回"偏好简洁实现"）时，可替换为向量检索实现
// （RAG），工具层与存储层均无需改动。
type Searcher interface {
	Search(ctx context.Context, memories []*Memory, query string, limit int) ([]*Memory, error)
}

// JSONStore 是 Store 的单文件 JSON 实现：整个记忆库一个文件，每次变更全量落盘。
//
// 长期记忆的特点是条目少而精（单条是提炼后的事实，不是对话原文），
// 千级条目以内全量读写毫无压力；更大规模或多进程共享时替换为
// Postgres 等实现即可（Store 接口不变）。
type JSONStore struct {
	mu    sync.RWMutex
	file  string
	mems  map[string]*Memory
	nowFn func() time.Time // 可注入时钟（测试用）
}

// NewJSONStore 创建基于 JSON 文件的记忆存储，文件不存在时自动创建空库，
// 存在时加载已有记忆。
func NewJSONStore(file string) (*JSONStore, error) {
	if file == "" {
		return nil, errors.New("记忆文件路径不能为空")
	}
	if dir := filepath.Dir(file); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nil, fmt.Errorf("创建记忆目录失败: %w", err)
		}
	}
	s := &JSONStore{
		file:  file,
		mems:  make(map[string]*Memory),
		nowFn: time.Now,
	}
	data, err := os.ReadFile(file)
	if err != nil {
		if !os.IsNotExist(err) {
			return nil, fmt.Errorf("读取记忆文件 %s 失败: %w", file, err)
		}
		return s, nil // 首次使用，空库起步
	}
	var list []*Memory
	if err := json.Unmarshal(data, &list); err != nil {
		return nil, fmt.Errorf("解析记忆文件 %s 失败: %w", file, err)
	}
	for _, m := range list {
		if m == nil || m.ID == "" {
			continue // 跳过损坏条目，不让一条脏数据拖垮整个记忆库
		}
		s.mems[m.ID] = m
	}
	return s, nil
}

// Put 实现 Store 接口。
func (s *JSONStore) Put(ctx context.Context, m *Memory) error {
	if m == nil {
		return errors.New("memory 不能为 nil")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if m.ID == "" {
		m.ID = newID()
	}
	now := s.nowFn().UTC().Truncate(time.Second)
	if m.CreatedAt.IsZero() {
		m.CreatedAt = now
	}
	m.UpdatedAt = now
	// 存副本而非调用方指针：外部随后改动字段不应穿透到存储内部。
	cp := *m
	cp.Tags = append([]string(nil), m.Tags...)
	s.mems[m.ID] = &cp
	return s.saveLocked()
}

// Get 实现 Store 接口。
func (s *JSONStore) Get(ctx context.Context, id string) (*Memory, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	m, ok := s.mems[id]
	if !ok {
		return nil, nil
	}
	cp := *m
	cp.Tags = append([]string(nil), m.Tags...)
	return &cp, nil
}

// Delete 实现 Store 接口。
func (s *JSONStore) Delete(ctx context.Context, id string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.mems[id]; !ok {
		return nil
	}
	delete(s.mems, id)
	return s.saveLocked()
}

// List 实现 Store 接口。
func (s *JSONStore) List(ctx context.Context) ([]*Memory, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]*Memory, 0, len(s.mems))
	for _, m := range s.mems {
		cp := *m
		cp.Tags = append([]string(nil), m.Tags...)
		out = append(out, &cp)
	}
	return out, nil
}

// saveLocked 全量落盘（调用方须持有写锁）。先写临时文件再 rename 原子替换，
// 避免写一半崩溃留下损坏的记忆文件。条目按创建时间排序，保证文件内容稳定可 diff。
func (s *JSONStore) saveLocked() error {
	list := make([]*Memory, 0, len(s.mems))
	for _, m := range s.mems {
		list = append(list, m)
	}
	sort.Slice(list, func(i, j int) bool {
		if !list[i].CreatedAt.Equal(list[j].CreatedAt) {
			return list[i].CreatedAt.Before(list[j].CreatedAt)
		}
		return list[i].ID < list[j].ID
	})
	data, err := json.MarshalIndent(list, "", "  ")
	if err != nil {
		return fmt.Errorf("序列化记忆失败: %w", err)
	}
	tmp := s.file + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return fmt.Errorf("写入记忆文件失败: %w", err)
	}
	if err := os.Rename(tmp, s.file); err != nil {
		return fmt.Errorf("替换记忆文件失败: %w", err)
	}
	return nil
}

// newID 生成 "mem-" 前缀的短随机 ID。随机源失败时退化为纳秒时间戳，
// 保证 ID 唯一性不依赖运气。
func newID() string {
	var b [4]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "mem-" + strconv.FormatInt(time.Now().UnixNano(), 16)
	}
	return "mem-" + hex.EncodeToString(b[:])
}
