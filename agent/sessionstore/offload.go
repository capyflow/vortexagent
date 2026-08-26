package sessionstore

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"

	"github.com/capyflow/vortexagent/llm"
)

type OffloadStore interface {
	SaveChunk(ctx context.Context, sessionID string, chunk *OffloadedChunk, messages []llm.Message) error
	LoadChunk(ctx context.Context, sessionID string, chunkID string) ([]llm.Message, error)
	ListChunks(ctx context.Context, sessionID string) ([]*OffloadedChunk, error)
	DeleteChunk(ctx context.Context, sessionID string, chunkID string) error
}

type FileSystemOffload struct {
	mu      sync.Mutex
	baseDir string
}

func NewFileSystemOffload(baseDir string) (*FileSystemOffload, error) {
	if err := os.MkdirAll(baseDir, 0o755); err != nil {
		return nil, fmt.Errorf("创建归档目录失败: %w", err)
	}
	return &FileSystemOffload{baseDir: baseDir}, nil
}

func (f *FileSystemOffload) sessionDir(sessionID string) string {
	return filepath.Join(f.baseDir, sessionID)
}

func (f *FileSystemOffload) chunkPath(sessionID, chunkID string) string {
	return filepath.Join(f.sessionDir(sessionID), chunkID+".json")
}

func (f *FileSystemOffload) messagesPath(sessionID, chunkID string) string {
	return filepath.Join(f.sessionDir(sessionID), chunkID+"_messages.json")
}

func (f *FileSystemOffload) SaveChunk(ctx context.Context, sessionID string, chunk *OffloadedChunk, messages []llm.Message) error {
	f.mu.Lock()
	defer f.mu.Unlock()

	dir := f.sessionDir(sessionID)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("创建会话归档目录失败: %w", err)
	}

	chunkData, err := json.MarshalIndent(chunk, "", "  ")
	if err != nil {
		return fmt.Errorf("序列化卸载块失败: %w", err)
	}
	if err := os.WriteFile(f.chunkPath(sessionID, chunk.ID), chunkData, 0o600); err != nil {
		return fmt.Errorf("写入卸载块失败: %w", err)
	}

	msgData, err := json.MarshalIndent(messages, "", "  ")
	if err != nil {
		return fmt.Errorf("序列化消息失败: %w", err)
	}
	if err := os.WriteFile(f.messagesPath(sessionID, chunk.ID), msgData, 0o600); err != nil {
		return fmt.Errorf("写入消息失败: %w", err)
	}

	return nil
}

func (f *FileSystemOffload) LoadChunk(ctx context.Context, sessionID string, chunkID string) ([]llm.Message, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	msgPath := f.messagesPath(sessionID, chunkID)
	data, err := os.ReadFile(msgPath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("读取消息失败: %w", err)
	}

	var messages []llm.Message
	if err := json.Unmarshal(data, &messages); err != nil {
		return nil, fmt.Errorf("反序列化消息失败: %w", err)
	}
	return messages, nil
}

func (f *FileSystemOffload) ListChunks(ctx context.Context, sessionID string) ([]*OffloadedChunk, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	dir := f.sessionDir(sessionID)
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("读取归档目录失败: %w", err)
	}

	var chunks []*OffloadedChunk
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		name := entry.Name()
		if len(name) < 6 || name[len(name)-5:] != ".json" {
			continue
		}
		if len(name) >= 16 && name[len(name)-16:] == "_messages.json" {
			continue
		}

		data, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			continue
		}

		var chunk OffloadedChunk
		if err := json.Unmarshal(data, &chunk); err != nil {
			continue
		}
		chunks = append(chunks, &chunk)
	}
	return chunks, nil
}

func (f *FileSystemOffload) DeleteChunk(ctx context.Context, sessionID string, chunkID string) error {
	f.mu.Lock()
	defer f.mu.Unlock()

	os.Remove(f.chunkPath(sessionID, chunkID))
	os.Remove(f.messagesPath(sessionID, chunkID))
	return nil
}

var _ OffloadStore = (*FileSystemOffload)(nil)
