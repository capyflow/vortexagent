package sessionstore

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/capyflow/vortexagent/llm"
)

func TestFileSystemOffload_RoundTrip(t *testing.T) {
	dir := t.TempDir()
	store, err := NewFileSystemOffload(dir)
	if err != nil {
		t.Fatalf("NewFileSystemOffload failed: %v", err)
	}

	sessionID := "test-session-001"
	chunk := &OffloadedChunk{
		ID:       "chunk-001",
		Summary:  "用户询问了天气，助手回复今天晴天",
		MsgCount: 4,
		StartIdx: 0,
		EndIdx:   3,
		CreatedAt: time.Now(),
	}

	messages := []llm.Message{
		llm.NewTextMessage(llm.RoleUser, "今天天气怎么样？"),
		llm.NewTextMessage(llm.RoleAssistant, "让我查一下"),
		llm.NewToolResultMessage("tool-1", "北京：晴天，25°C"),
		llm.NewTextMessage(llm.RoleAssistant, "今天北京天气晴朗，气温25°C"),
	}

	ctx := context.Background()

	if err := store.SaveChunk(ctx, sessionID, chunk, messages); err != nil {
		t.Fatalf("SaveChunk failed: %v", err)
	}

	chunks, err := store.ListChunks(ctx, sessionID)
	if err != nil {
		t.Fatalf("ListChunks failed: %v", err)
	}
	if len(chunks) != 1 {
		t.Fatalf("expected 1 chunk, got %d", len(chunks))
	}
	if chunks[0].Summary != chunk.Summary {
		t.Errorf("summary mismatch: got %q", chunks[0].Summary)
	}

	loaded, err := store.LoadChunk(ctx, sessionID, "chunk-001")
	if err != nil {
		t.Fatalf("LoadChunk failed: %v", err)
	}
	if len(loaded) != 4 {
		t.Fatalf("expected 4 messages, got %d", len(loaded))
	}
	if loaded[0].Content[0].Text != "今天天气怎么样？" {
		t.Errorf("first message text mismatch")
	}
}

func TestFileSystemOffload_DeleteChunk(t *testing.T) {
	dir := t.TempDir()
	store, _ := NewFileSystemOffload(dir)

	sessionID := "test-session-002"
	chunk := &OffloadedChunk{
		ID:       "chunk-002",
		Summary:  "test",
		MsgCount: 1,
		CreatedAt: time.Now(),
	}
	messages := []llm.Message{llm.NewTextMessage(llm.RoleUser, "hi")}

	ctx := context.Background()
	store.SaveChunk(ctx, sessionID, chunk, messages)

	if err := store.DeleteChunk(ctx, sessionID, "chunk-002"); err != nil {
		t.Fatalf("DeleteChunk failed: %v", err)
	}

	chunks, _ := store.ListChunks(ctx, sessionID)
	if len(chunks) != 0 {
		t.Errorf("expected 0 chunks after delete, got %d", len(chunks))
	}

	loaded, _ := store.LoadChunk(ctx, sessionID, "chunk-002")
	if loaded != nil {
		t.Errorf("expected nil messages after delete")
	}
}

func TestFileSystemOffload_SessionIsolation(t *testing.T) {
	dir := t.TempDir()
	store, _ := NewFileSystemOffload(dir)
	ctx := context.Background()

	chunk1 := &OffloadedChunk{ID: "c1", Summary: "session1", MsgCount: 1, CreatedAt: time.Now()}
	chunk2 := &OffloadedChunk{ID: "c2", Summary: "session2", MsgCount: 1, CreatedAt: time.Now()}
	msg := []llm.Message{llm.NewTextMessage(llm.RoleUser, "test")}

	store.SaveChunk(ctx, "session-1", chunk1, msg)
	store.SaveChunk(ctx, "session-2", chunk2, msg)

	chunks1, _ := store.ListChunks(ctx, "session-1")
	chunks2, _ := store.ListChunks(ctx, "session-2")

	if len(chunks1) != 1 || len(chunks2) != 1 {
		t.Errorf("sessions should be isolated")
	}
	if chunks1[0].Summary != "session1" || chunks2[0].Summary != "session2" {
		t.Errorf("chunk summaries should match their sessions")
	}
}

func TestFileSystemOffload_MissingSession(t *testing.T) {
	dir := t.TempDir()
	store, _ := NewFileSystemOffload(dir)
	ctx := context.Background()

	chunks, err := store.ListChunks(ctx, "nonexistent")
	if err != nil {
		t.Fatalf("ListChunks on missing session should not error: %v", err)
	}
	if len(chunks) != 0 {
		t.Errorf("expected 0 chunks for missing session, got %d", len(chunks))
	}
}

func TestFileSystemOffload_DirectoryCreated(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "deep", "nested", "path")
	store, err := NewFileSystemOffload(dir)
	if err != nil {
		t.Fatalf("NewFileSystemOffload should create dirs: %v", err)
	}

	info, err := os.Stat(dir)
	if err != nil {
		t.Fatalf("directory should exist: %v", err)
	}
	if !info.IsDir() {
		t.Errorf("path should be a directory")
	}

	_ = store
}
