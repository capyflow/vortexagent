package ctxengine

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/capyflow/vortexagent/llm"
)

func newTestStore(t *testing.T) *FileSystemArchive {
	t.Helper()
	s, err := NewFileSystemArchive(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func userMsg(text string) llm.Message { return llm.NewTextMessage(llm.RoleUser, text) }
func asstMsg(text string) llm.Message { return llm.NewTextMessage(llm.RoleAssistant, text) }

func mkBlock(i int) Block {
	return Block{Messages: []llm.Message{
		userMsg(fmt.Sprintf("第 %d 个问题", i)),
		asstMsg(fmt.Sprintf("第 %d 个回答", i)),
	}}
}

func TestFileSystemArchive_AppendBlocks_Dedup(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	b := mkBlock(1)
	archived := ArchivedBlock{
		ID:            NewBlockID(b),
		Seq:           0,
		FirstUserText: b.FirstUserText(),
		Messages:      b.Messages,
		MsgCount:      len(b.Messages),
		ArchivedAt:    time.Now(),
	}
	if err := store.AppendBlocks(ctx, "s1", []ArchivedBlock{archived}); err != nil {
		t.Fatal(err)
	}
	// 同一批消息再次归档（重试场景）：不得产生第二条。
	if err := store.AppendBlocks(ctx, "s1", []ArchivedBlock{archived}); err != nil {
		t.Fatal(err)
	}
	blocks, err := store.ListBlocks(ctx, "s1")
	if err != nil {
		t.Fatal(err)
	}
	if len(blocks) != 1 {
		t.Fatalf("指纹去重失败：期望 1 块，得到 %d", len(blocks))
	}
}

func TestFileSystemArchive_SummaryLifecycle(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	s1 := &Summary{ID: "chunk-1", Level: LevelChunk, Content: "postgres 报错的排查记录", BlockIDs: []string{"b1"}, MsgCount: 4, CreatedAt: time.Now()}
	s2 := &Summary{ID: "chunk-2", Level: LevelChunk, Content: "部署脚本的讨论", BlockIDs: []string{"b2"}, MsgCount: 2, CreatedAt: time.Now().Add(time.Second)}
	for _, s := range []*Summary{s1, s2} {
		if err := store.SaveSummary(ctx, "s1", s); err != nil {
			t.Fatal(err)
		}
	}
	sums, err := store.ListSummaries(ctx, "s1")
	if err != nil {
		t.Fatal(err)
	}
	if len(sums) != 2 || sums[0].ID != "chunk-1" {
		t.Fatalf("ListSummaries 顺序/数量错误：%v", sums)
	}

	// 合并：标记 merged 后仍可检索（原文与摘要都在）。
	s1.Merged = true
	if err := store.SaveSummary(ctx, "s1", s1); err != nil {
		t.Fatal(err)
	}
	sums, _ = store.ListSummaries(ctx, "s1")
	if len(sums) != 2 {
		t.Fatalf("merged 摘要不应被删除，得到 %d 条", len(sums))
	}

	if err := store.DeleteSession(ctx, "s1"); err != nil {
		t.Fatal(err)
	}
	if blocks, _ := store.ListBlocks(ctx, "s1"); len(blocks) != 0 {
		t.Fatal("DeleteSession 后仍有块残留")
	}
}

func TestFileSystemArchive_BlockRoundTrip(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	b := mkBlock(7)
	orig := ArchivedBlock{
		ID:            NewBlockID(b),
		Seq:           3,
		FirstUserText: b.FirstUserText(),
		Messages:      b.Messages,
		MsgCount:      len(b.Messages),
		ArchivedAt:    time.Now(),
	}
	if err := store.AppendBlocks(ctx, "s1", []ArchivedBlock{orig}); err != nil {
		t.Fatal(err)
	}
	got, err := store.GetBlock(ctx, "s1", orig.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got == nil || got.FirstUserText != orig.FirstUserText || got.MsgCount != orig.MsgCount || got.Seq != 3 {
		t.Fatalf("块回读不一致：%+v", got)
	}
	if missing, _ := store.GetBlock(ctx, "s1", "b不存在"); missing != nil {
		t.Fatal("不存在的块应返回 nil")
	}
}
