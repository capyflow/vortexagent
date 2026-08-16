package agent

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/capyflow/vortexagent/llm"
)

// TestMemoryStore_RoundTrip 校验内存存储的存取与"不存在返回 nil"语义。
func TestMemoryStore_RoundTrip(t *testing.T) {
	s := NewMemorySessionStore()
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

// TestJSONStore_RoundTrip 校验 JSON 存储"进程重启"后仍能恢复会话。
func TestJSONStore_RoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sessions.json")
	s, err := NewJSONSessionStore(path)
	if err != nil {
		t.Fatalf("NewJSONSessionStore 失败: %v", err)
	}
	sess := NewSession("m1")
	sess.Add(llm.NewTextMessage(llm.RoleUser, "你好"))
	sess.Add(llm.NewTextMessage(llm.RoleAssistant, "你好！有什么可以帮你？"))
	if err := s.Save(context.Background(), sess); err != nil {
		t.Fatalf("Save 失败: %v", err)
	}

	// 重新打开文件，模拟进程重启
	s2, err := NewJSONSessionStore(path)
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

// TestJSONStore_LoadLatest 校验返回最近更新的会话（用于"继续上次对话"）。
func TestJSONStore_LoadLatest(t *testing.T) {
	s, err := NewJSONSessionStore(filepath.Join(t.TempDir(), "sessions.json"))
	if err != nil {
		t.Fatalf("NewJSONSessionStore 失败: %v", err)
	}
	older := NewSession("m1")
	older.Add(llm.NewTextMessage(llm.RoleUser, "旧会话"))
	if err := s.Save(context.Background(), older); err != nil {
		t.Fatal(err)
	}
	time.Sleep(10 * time.Millisecond) // 确保 UpdatedAt 严格递增
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
		t.Errorf("Latest.ID = %s, 期望 %s（最近更新的会话）", latest.ID, newer.ID)
	}

	// 空存储时返回 nil
	empty, err := NewJSONSessionStore(filepath.Join(t.TempDir(), "empty.json"))
	if err != nil {
		t.Fatal(err)
	}
	if got, err := empty.LoadLatest(context.Background()); err != nil || got != nil {
		t.Errorf("空存储 LoadLatest = (%v, %v), 期望 (nil, nil)", got, err)
	}
}

// TestJSONStore_SaveIsSnapshot 校验 JSON 存储保存的是快照：
// 保存后对原会话的修改不影响已保存的数据。
func TestJSONStore_SaveIsSnapshot(t *testing.T) {
	s, err := NewJSONSessionStore(filepath.Join(t.TempDir(), "sessions.json"))
	if err != nil {
		t.Fatalf("NewJSONSessionStore 失败: %v", err)
	}
	sess := NewSession("m1")
	sess.Add(llm.NewTextMessage(llm.RoleUser, "第一条"))
	if err := s.Save(context.Background(), sess); err != nil {
		t.Fatal(err)
	}
	sess.Add(llm.NewTextMessage(llm.RoleUser, "第二条")) // 保存后继续修改

	got, err := s.Load(context.Background(), sess.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Messages()) != 1 {
		t.Errorf("存储应保存快照，历史条数 = %d, 期望 1", len(got.Messages()))
	}
}

// TestJSONStore_MissingFile 校验文件不存在时从空存储开始（不报错）。
func TestJSONStore_MissingFile(t *testing.T) {
	s, err := NewJSONSessionStore(filepath.Join(t.TempDir(), "no-such.json"))
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

// TestAsk_AutoSavesSession 校验配置 Store 后，Ask 成功会自动保存会话。
func TestAsk_AutoSavesSession(t *testing.T) {
	fp := &fakeProvider{name: "fake", maxRounds: 0}
	store := NewMemorySessionStore()
	ag := New(Options{Provider: fp, Model: "m1", Store: store})
	session := NewSession("m1")

	if _, err := ag.Ask(context.Background(), session, "你好"); err != nil {
		t.Fatalf("Ask 失败: %v", err)
	}
	got, err := store.Load(context.Background(), session.ID)
	if err != nil {
		t.Fatalf("Load 失败: %v", err)
	}
	if got == nil {
		t.Fatal("Ask 后应自动保存会话")
	}
	if len(got.Messages()) != 2 {
		t.Errorf("历史条数 = %d, 期望 2（user+assistant）", len(got.Messages()))
	}
}

// TestAsk_AutoSavesOnError 校验 Ask 失败时也会保存（回滚后的）会话。
func TestAsk_AutoSavesOnError(t *testing.T) {
	store := NewMemorySessionStore()
	ag := New(Options{Provider: &errProvider{name: "err"}, Model: "m1", Store: store})
	session := NewSession("m1")

	if _, err := ag.Ask(context.Background(), session, "问题"); err == nil {
		t.Fatal("期望 Ask 报错")
	}
	got, err := store.Load(context.Background(), session.ID)
	if err != nil {
		t.Fatalf("Load 失败: %v", err)
	}
	if got == nil {
		t.Fatal("失败路径也应自动保存会话")
	}
	if len(got.Messages()) != 0 {
		t.Errorf("失败后保存的历史应为空（已回滚），实际 %d 条", len(got.Messages()))
	}
}
