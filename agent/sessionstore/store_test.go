package sessionstore

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/capyflow/vortexagent/llm"
)

type fakeProvider struct {
	name      string
	maxRounds int
}

func (f *fakeProvider) Name() string { return f.name }
func (f *fakeProvider) Chat(ctx context.Context, req *llm.ChatRequest, onDelta func(llm.Delta) error) (*llm.ChatResponse, error) {
	return &llm.ChatResponse{
		Message: llm.NewTextMessage(llm.RoleAssistant, "你好！"),
	}, nil
}

type errProvider struct {
	name string
}

func (e *errProvider) Name() string { return e.name }
func (e *errProvider) Chat(ctx context.Context, req *llm.ChatRequest, onDelta func(llm.Delta) error) (*llm.ChatResponse, error) {
	return nil, context.DeadlineExceeded
}

func TestMemoryStore_RoundTrip(t *testing.T) {
	s := NewMemory()
	sess := NewSession("m1")
	sess.Add(llm.NewTextMessage(llm.RoleUser, "你好"))

	if err := s.Save(context.Background(), sess); err != nil {
		t.Fatalf("Save 失败: %v", err)
	}
	got, err := s.Load(context.Background(), sess.ID)
	if err != nil {
		t.Fatalf("Load 失败: %v", err)
	}
	if got == nil {
		t.Fatal("Load 不应返回 nil")
	}
	if len(got.Messages()) != 1 {
		t.Errorf("历史条数 = %d, 期望 1", len(got.Messages()))
	}

	missing, err := s.Load(context.Background(), "no-such-id")
	if err != nil {
		t.Fatalf("Load 失败: %v", err)
	}
	if missing != nil {
		t.Error("不存在的会话应返回 nil")
	}

	if err := s.Delete(context.Background(), sess.ID); err != nil {
		t.Fatalf("Delete 失败: %v", err)
	}
	if got, _ := s.Load(context.Background(), sess.ID); got != nil {
		t.Error("删除后不应再加载到会话")
	}
}

func TestJSONStore_RoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sessions.json")
	s, err := NewJSON(path)
	if err != nil {
		t.Fatalf("NewJSON 失败: %v", err)
	}
	sess := NewSession("m1")
	sess.Add(llm.NewTextMessage(llm.RoleUser, "你好"))
	sess.Add(llm.NewTextMessage(llm.RoleAssistant, "你好！有什么可以帮你？"))
	if err := s.Save(context.Background(), sess); err != nil {
		t.Fatalf("Save 失败: %v", err)
	}

	s2, err := NewJSON(path)
	if err != nil {
		t.Fatalf("重新打开失败: %v", err)
	}
	got, err := s2.Load(context.Background(), sess.ID)
	if err != nil {
		t.Fatalf("Load 失败: %v", err)
	}
	if got == nil {
		t.Fatal("重启后应能加载会话")
	}
	if len(got.Messages()) != 2 {
		t.Errorf("历史条数 = %d, 期望 2", len(got.Messages()))
	}
	if got.Messages()[0].Content[0].Text != "你好" {
		t.Errorf("消息内容 = %q, 期望 %q", got.Messages()[0].Content[0].Text, "你好")
	}
}

func TestJSONStore_LoadLatest(t *testing.T) {
	s, err := NewJSON(filepath.Join(t.TempDir(), "sessions.json"))
	if err != nil {
		t.Fatalf("NewJSON 失败: %v", err)
	}
	older := NewSession("m1")
	older.Add(llm.NewTextMessage(llm.RoleUser, "旧会话"))
	if err := s.Save(context.Background(), older); err != nil {
		t.Fatal(err)
	}
	time.Sleep(10 * time.Millisecond)
	newer := NewSession("m1")
	newer.Add(llm.NewTextMessage(llm.RoleUser, "新会话"))
	if err := s.Save(context.Background(), newer); err != nil {
		t.Fatal(err)
	}

	latest, err := s.LoadLatest(context.Background())
	if err != nil {
		t.Fatalf("LoadLatest 失败: %v", err)
	}
	if latest == nil {
		t.Fatal("LoadLatest 不应返回 nil")
	}
	if latest.ID != newer.ID {
		t.Errorf("Latest.ID = %s, 期望 %s", latest.ID, newer.ID)
	}

	empty, err := NewJSON(filepath.Join(t.TempDir(), "empty.json"))
	if err != nil {
		t.Fatal(err)
	}
	if got, err := empty.LoadLatest(context.Background()); err != nil || got != nil {
		t.Errorf("空存储 LoadLatest = (%v, %v), 期望 (nil, nil)", got, err)
	}
}

func TestJSONStore_SaveIsSnapshot(t *testing.T) {
	s, err := NewJSON(filepath.Join(t.TempDir(), "sessions.json"))
	if err != nil {
		t.Fatalf("NewJSON 失败: %v", err)
	}
	sess := NewSession("m1")
	sess.Add(llm.NewTextMessage(llm.RoleUser, "第一条"))
	if err := s.Save(context.Background(), sess); err != nil {
		t.Fatal(err)
	}
	sess.Add(llm.NewTextMessage(llm.RoleUser, "第二条"))

	got, err := s.Load(context.Background(), sess.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Messages()) != 1 {
		t.Errorf("存储应保存快照，历史条数 = %d, 期望 1", len(got.Messages()))
	}
}

func TestJSONStore_MissingFile(t *testing.T) {
	s, err := NewJSON(filepath.Join(t.TempDir(), "no-such.json"))
	if err != nil {
		t.Fatalf("文件不存在不应报错: %v", err)
	}
	got, err := s.Load(context.Background(), "any")
	if err != nil {
		t.Fatalf("Load 失败: %v", err)
	}
	if got != nil {
		t.Error("空存储 Load 应返回 nil")
	}
}
