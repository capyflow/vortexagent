package memory

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// newTestStore 创建测试用存储，时钟从 base 开始，可通过推进返回的指针改变当前时间。
func newTestStore(t *testing.T) (*JSONStore, *time.Time) {
	t.Helper()
	s, err := NewJSONStore(filepath.Join(t.TempDir(), "memories.json"))
	if err != nil {
		t.Fatalf("创建存储失败: %v", err)
	}
	base := time.Date(2026, 1, 1, 8, 0, 0, 0, time.UTC)
	clock := base
	s.nowFn = func() time.Time { return clock }
	return s, &clock
}

// TestJSONStore_putGetRoundtrip 验证自动生成 ID、时间戳填充与副本语义：
// 修改 Get 返回值不应穿透到存储内部。
func TestJSONStore_putGetRoundtrip(t *testing.T) {
	s, _ := newTestStore(t)
	ctx := context.Background()

	m := &Memory{Content: "项目使用 Go 1.23", Kind: KindFact, Tags: []string{"go"}, Importance: 4}
	if err := s.Put(ctx, m); err != nil {
		t.Fatalf("Put 失败: %v", err)
	}
	if m.ID == "" || !strings.HasPrefix(m.ID, "mem-") {
		t.Fatalf("期望自动生成 mem- 前缀 ID，实际 %q", m.ID)
	}
	if m.CreatedAt.IsZero() || m.UpdatedAt.IsZero() {
		t.Fatal("期望 Put 填充 CreatedAt / UpdatedAt")
	}

	got, err := s.Get(ctx, m.ID)
	if err != nil || got == nil {
		t.Fatalf("Get 失败: %v, %+v", err, got)
	}
	if got.Content != m.Content || got.Kind != KindFact || got.Importance != 4 || len(got.Tags) != 1 {
		t.Errorf("Get 内容不匹配: %+v", got)
	}

	// 副本语义：篡改返回值与 Tags 切片都不应影响存储。
	got.Content = "篡改"
	got.Tags[0] = "篡改"
	again, _ := s.Get(ctx, m.ID)
	if again.Content != m.Content || again.Tags[0] != "go" {
		t.Errorf("Get 返回的不是副本，存储被穿透: %+v", again)
	}
}

// TestJSONStore_updateKeepsCreatedAt 验证更新保留创建时间、刷新更新时间。
func TestJSONStore_updateKeepsCreatedAt(t *testing.T) {
	s, clock := newTestStore(t)
	ctx := context.Background()

	m := &Memory{Content: "原始内容"}
	if err := s.Put(ctx, m); err != nil {
		t.Fatalf("Put 失败: %v", err)
	}
	createdAt := m.CreatedAt

	*clock = (*clock).Add(time.Hour) // 推进 1 小时后再更新
	m.Content = "更新后的内容"
	if err := s.Put(ctx, m); err != nil {
		t.Fatalf("Put 失败: %v", err)
	}
	if !m.CreatedAt.Equal(createdAt) {
		t.Errorf("CreatedAt 被重置: 期望 %v，实际 %v", createdAt, m.CreatedAt)
	}
	if !m.UpdatedAt.After(createdAt) {
		t.Errorf("UpdatedAt 未被刷新: %v", m.UpdatedAt)
	}
}

// TestJSONStore_persistsAcrossReopen 验证重启（重新打开文件）后记忆完整恢复。
func TestJSONStore_persistsAcrossReopen(t *testing.T) {
	file := filepath.Join(t.TempDir(), "memories.json")
	s, err := NewJSONStore(file)
	if err != nil {
		t.Fatalf("创建存储失败: %v", err)
	}
	ctx := context.Background()
	want := []*Memory{
		{Content: "部署在 k8s 集群", Kind: KindFact, Tags: []string{"ops"}, Importance: 3},
		{Content: "用户偏好中文回复", Kind: KindPreference},
	}
	for _, m := range want {
		if err := s.Put(ctx, m); err != nil {
			t.Fatalf("Put 失败: %v", err)
		}
	}

	reopened, err := NewJSONStore(file)
	if err != nil {
		t.Fatalf("重新打开存储失败: %v", err)
	}
	list, err := reopened.List(ctx)
	if err != nil {
		t.Fatalf("List 失败: %v", err)
	}
	if len(list) != len(want) {
		t.Fatalf("期望 %d 条记忆，实际 %d 条", len(want), len(list))
	}
	byID := map[string]*Memory{}
	for _, m := range list {
		byID[m.ID] = m
	}
	for _, w := range want {
		got := byID[w.ID]
		if got == nil {
			t.Fatalf("重启后丢失记忆 %s", w.ID)
		}
		if got.Content != w.Content || got.Kind != w.Kind || got.Importance != w.Importance {
			t.Errorf("记忆 %s 内容不匹配: %+v", w.ID, got)
		}
	}
}

// TestJSONStore_delete 验证删除后不可再取，删除不存在的 ID 静默成功。
func TestJSONStore_delete(t *testing.T) {
	s, _ := newTestStore(t)
	ctx := context.Background()

	m := &Memory{Content: "临时记忆"}
	if err := s.Put(ctx, m); err != nil {
		t.Fatalf("Put 失败: %v", err)
	}
	if err := s.Delete(ctx, "mem-不存在"); err != nil {
		t.Errorf("删除不存在的记忆不应报错: %v", err)
	}
	if err := s.Delete(ctx, m.ID); err != nil {
		t.Fatalf("Delete 失败: %v", err)
	}
	got, err := s.Get(ctx, m.ID)
	if err != nil || got != nil {
		t.Errorf("删除后 Get 期望 (nil, nil)，实际 (%+v, %v)", got, err)
	}
}

// TestJSONStore_skipsCorruptEntries 验证文件中的损坏条目被跳过而非拖垮整个库。
func TestJSONStore_skipsCorruptEntries(t *testing.T) {
	file := filepath.Join(t.TempDir(), "memories.json")
	content := `[{"id":"mem-good","content":"正常记忆"}, null, {"content":"缺少ID"}, {"id":"","content":""}]`
	if err := os.WriteFile(file, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	s, err := NewJSONStore(file)
	if err != nil {
		t.Fatalf("加载含损坏条目的文件不应报错: %v", err)
	}
	list, _ := s.List(context.Background())
	if len(list) != 1 || list[0].ID != "mem-good" {
		t.Errorf("期望仅保留 1 条正常记忆，实际 %d 条: %+v", len(list), list)
	}
}

// TestJSONStore_corruptFile 报错而不是返回空库，避免静默吞掉用户全部记忆。
func TestJSONStore_corruptFile(t *testing.T) {
	file := filepath.Join(t.TempDir(), "memories.json")
	if err := os.WriteFile(file, []byte("这不是JSON"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := NewJSONStore(file); err == nil {
		t.Fatal("损坏的 JSON 文件应返回错误")
	}
}

// TestJSONStore_concurrentAccess 并发读写不 panic、无数据竞争（-race 下验证）。
func TestJSONStore_concurrentAccess(t *testing.T) {
	s, _ := newTestStore(t)
	ctx := context.Background()

	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			m := &Memory{Content: "并发记忆", Tags: []string{"t"}}
			_ = s.Put(ctx, m)
			_, _ = s.Get(ctx, m.ID)
			_, _ = s.List(ctx)
			if i%4 == 0 {
				_ = s.Delete(ctx, m.ID)
			}
		}(i)
	}
	wg.Wait()

	list, err := s.List(ctx)
	if err != nil {
		t.Fatalf("并发后 List 失败: %v", err)
	}
	// 未删除的 12 条应全部存活（16 个中 i%4==0 的 4 个被删）。
	if len(list) != 12 {
		t.Errorf("并发后期望 12 条记忆，实际 %d 条", len(list))
	}
}
