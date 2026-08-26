package sessionstore

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/capyflow/vortexagent/llm"
)

func TestFileSystemOffload_RetrieveAfterOffload(t *testing.T) {
	dir := t.TempDir()
	store, _ := NewFileSystemOffload(dir)
	ctx := context.Background()
	sessionID := "test-session-retrieve"

	originalMessages := []llm.Message{
		llm.NewTextMessage(llm.RoleUser, "北京天气怎么样？"),
		llm.NewToolResultMessage("tool-1", "北京：晴天，25°C"),
		llm.NewTextMessage(llm.RoleAssistant, "今天北京天气晴朗"),
		llm.NewTextMessage(llm.RoleUser, "那上海呢？"),
		llm.NewToolResultMessage("tool-2", "上海：多云，22°C"),
		llm.NewTextMessage(llm.RoleAssistant, "上海今天多云"),
	}

	chunk := &OffloadedChunk{
		ID:       "chunk-retrieve-001",
		Summary:  "用户询问了北京和上海天气，助手分别回复了晴天25°C和多云22°C",
		MsgCount: 6,
		StartIdx: 0,
		EndIdx:   5,
		CreatedAt: time.Now(),
	}

	if err := store.SaveChunk(ctx, sessionID, chunk, originalMessages); err != nil {
		t.Fatalf("SaveChunk failed: %v", err)
	}

	loaded, err := store.LoadChunk(ctx, sessionID, "chunk-retrieve-001")
	if err != nil {
		t.Fatalf("LoadChunk failed: %v", err)
	}

	if len(loaded) != 6 {
		t.Fatalf("expected 6 messages, got %d", len(loaded))
	}

	if loaded[0].Content[0].Text != "北京天气怎么样？" {
		t.Errorf("first message mismatch: got %q", loaded[0].Content[0].Text)
	}

	if loaded[3].Content[0].Text != "那上海呢？" {
		t.Errorf("fourth message mismatch: got %q", loaded[3].Content[0].Text)
	}

	t.Logf("成功检索到 %d 条原始消息", len(loaded))
	for i, msg := range loaded {
		text := ""
		for _, c := range msg.Content {
			if c.Type == llm.ContentText {
				text = c.Text
			}
		}
		t.Logf("  [%d] %s: %s", i, msg.Role, text)
	}
}

func TestFileSystemOffload_FileExistOnDisk(t *testing.T) {
	dir := t.TempDir()
	store, _ := NewFileSystemOffload(dir)
	ctx := context.Background()
	sessionID := "test-session-files"

	chunk := &OffloadedChunk{
		ID:       "chunk-001",
		Summary:  "测试摘要",
		MsgCount: 2,
		CreatedAt: time.Now(),
	}
	messages := []llm.Message{
		llm.NewTextMessage(llm.RoleUser, "hello"),
		llm.NewTextMessage(llm.RoleAssistant, "hi"),
	}

	store.SaveChunk(ctx, sessionID, chunk, messages)

	chunkPath := filepath.Join(dir, sessionID, "chunk-001.json")
	msgPath := filepath.Join(dir, sessionID, "chunk-001_messages.json")

	if _, err := os.Stat(chunkPath); os.IsNotExist(err) {
		t.Error("chunk file should exist on disk")
	}
	if _, err := os.Stat(msgPath); os.IsNotExist(err) {
		t.Error("messages file should exist on disk")
	}

	t.Logf("文件已保存到磁盘:")
	t.Logf("  %s", chunkPath)
	t.Logf("  %s", msgPath)
}
