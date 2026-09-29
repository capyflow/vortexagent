package ctxengine

import (
	"context"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/capyflow/vortexagent/agent/sessionstore"
	"github.com/capyflow/vortexagent/llm"
)

// fakeSummarizer 可编程的摘要器：按预设脚本返回，或记录调用次数。
type fakeSummarizer struct {
	handoff  string
	facts    []string
	failLeft int // 前 N 次返回错误（测试降级链）
	calls    atomic.Int32
}

func (f *fakeSummarizer) Summarize(ctx context.Context, msgs []llm.Message) (string, []string, error) {
	f.calls.Add(1)
	if f.failLeft > 0 {
		f.failLeft--
		return "", nil, context.DeadlineExceeded
	}
	return f.handoff, f.facts, nil
}

func newTestEngine(t *testing.T, sum Summarizer, mutate func(*Config)) (*Engine, *FileSystemArchive) {
	t.Helper()
	archive, err := NewFileSystemArchive(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	cfg := Config{Model: "test-model", KeepRecentBlocks: 2, MaxBlocks: 4}
	if mutate != nil {
		mutate(&cfg)
	}
	return New(archive, sum, cfg), archive
}

// buildSession 构造 n 轮「用户+助手」的会话。
func buildSession(t *testing.T, n int) *sessionstore.Session {
	t.Helper()
	s := sessionstore.NewSession("test-model")
	for i := 0; i < n; i++ {
		s.Add(userMsg(itoa(i) + "：帮我解决第" + itoa(i) + "号 postgres 报错问题"))
		s.Add(asstMsg(itoa(i) + "：已解决，根因是连接池耗尽"))
	}
	return s
}

func itoa(i int) string {
	if i == 0 {
		return "零"
	}
	d := "零一二三四五六七八九"
	if i < 10 {
		return string([]rune(d)[i])
	}
	return itoa(i/10) + string([]rune(d)[i%10])
}

func TestEngine_ArchiveSession_ReplacesHistoryAndKeepsOriginal(t *testing.T) {
	sum := &fakeSummarizer{handoff: "【任务意图】排查 postgres 报错\n【当前状态】前几轮问题均已解决"}
	engine, archive := newTestEngine(t, sum, nil)
	session := buildSession(t, 10)
	ctx := context.Background()

	if err := engine.Compact(ctx, session.ID, session); err != nil {
		t.Fatal(err)
	}

	// 活跃区只剩 KeepRecentBlocks=2 块（4 条消息）。
	if got := session.MessageCount(); got != 4 {
		t.Fatalf("归档后活跃消息数 = %d，期望 4", got)
	}
	// 归档保留 8 块原文。
	blocks, _ := archive.ListBlocks(ctx, session.ID)
	if len(blocks) != 8 {
		t.Fatalf("归档块数 = %d，期望 8", len(blocks))
	}
	sums, _ := archive.ListSummaries(ctx, session.ID)
	if len(sums) != 1 || !strings.Contains(sums[0].Content, "postgres") {
		t.Fatalf("交接摘要缺失或内容不符：%+v", sums)
	}

	// expand 回读原文。
	out, err := engine.ExpandChunk(ctx, session.ID, sums[0].ID, 20000)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "帮我解决第一号 postgres 报错问题") {
		t.Fatalf("展开内容缺失原始消息：%s", out)
	}

	// 检索命中。
	hits, err := engine.SearchHistory(ctx, session.ID, "postgres 报错", 5)
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) == 0 {
		t.Fatal("search_history 未命中")
	}

	// 下一次归档：指纹去重，不产生重复块。
	if err := engine.Compact(ctx, session.ID, session); err != nil {
		t.Fatal(err)
	}
	blocks2, _ := archive.ListBlocks(ctx, session.ID)
	if len(blocks2) != 8 {
		t.Fatalf("重复归档产生重复块：%d", len(blocks2))
	}
}

func TestEngine_SummaryFailure_KeepsHistory(t *testing.T) {
	sum := &fakeSummarizer{failLeft: 100}
	engine, archive := newTestEngine(t, sum, nil)
	session := buildSession(t, 10)

	err := engine.Compact(context.Background(), session.ID, session)
	if err == nil {
		t.Fatal("摘要失败时应返回错误")
	}
	// 原文不动：活跃区仍是完整历史。
	if got := session.MessageCount(); got != 20 {
		t.Fatalf("摘要失败后活跃消息数 = %d，期望 20（不卸载）", got)
	}
	// 但原文可能已落库（取决于失败时机）；重试按指纹去重，不产生重复。
	sum.failLeft = 0
	if err := engine.Compact(context.Background(), session.ID, session); err != nil {
		t.Fatal(err)
	}
	blocks, _ := archive.ListBlocks(context.Background(), session.ID)
	if len(blocks) != 8 {
		t.Fatalf("重试后归档块数 = %d，期望 8（指纹去重）", len(blocks))
	}
}

func TestEngine_OnTurnDone_TriggersAndRuns(t *testing.T) {
	sum := &fakeSummarizer{handoff: "【当前状态】测试"}
	engine, archive := newTestEngine(t, sum, nil)
	session := buildSession(t, 10)

	engine.OnTurnDone(session.ID, session)
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if blocks, _ := archive.ListBlocks(context.Background(), session.ID); len(blocks) > 0 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	blocks, _ := archive.ListBlocks(context.Background(), session.ID)
	if len(blocks) == 0 {
		t.Fatal("OnTurnDone 未触发归档")
	}
}

func TestEngine_Relieve_TrimsOldestThird(t *testing.T) {
	sum := &fakeSummarizer{handoff: "【当前状态】紧急裁剪"}
	engine, archive := newTestEngine(t, sum, nil)
	session := buildSession(t, 9) // 9 块

	if !engine.Relieve(context.Background(), session.ID, session) {
		t.Fatal("Relieve 应发生裁剪")
	}
	// 裁 1/3 = 3 块，剩 6 块 = 12 条消息。
	if got := session.MessageCount(); got != 12 {
		t.Fatalf("Relieve 后活跃消息数 = %d，期望 12", got)
	}
	blocks, _ := archive.ListBlocks(context.Background(), session.ID)
	if len(blocks) != 3 {
		t.Fatalf("Relieve 归档块数 = %d，期望 3", len(blocks))
	}
}

func TestEngine_RecordUsage_CalibrationConverges(t *testing.T) {
	engine, _ := newTestEngine(t, &fakeSummarizer{}, nil)
	msgs := []llm.Message{userMsg(strings.Repeat("a", 400))} // 估算 100 token
	for i := 0; i < 20; i++ {
		engine.RecordUsage("s", msgs, llm.Usage{InputTokens: 200}) // 真实恒为 2 倍
	}
	st := engine.state("s")
	st.mu.Lock()
	calib := st.calibration
	st.mu.Unlock()
	if calib < 1.9 || calib > 2.1 {
		t.Fatalf("校准系数未收敛到 2 附近：%f", calib)
	}
}

func TestEngine_IsContextWindowError(t *testing.T) {
	cases := map[string]bool{
		"request failed: This model's maximum context length is 8192 tokens": true,
		"prompt is too long: 200000 tokens > 128000 maximum":                 true,
		"connection refused": false,
	}
	for msg, want := range cases {
		if got := IsContextWindowError(errString(msg)); got != want {
			t.Fatalf("IsContextWindowError(%q) = %v", msg, got)
		}
	}
}

type errString string

func (e errString) Error() string { return string(e) }

func TestEngine_TTLCleanup(t *testing.T) {
	archive, err := NewFileSystemArchive(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	b := mkBlock(1)
	if err := archive.AppendBlocks(ctx, "old-session", []ArchivedBlock{{
		ID: NewBlockID(b), FirstUserText: b.FirstUserText(), Messages: b.Messages,
		ArchivedAt: time.Now(),
	}}); err != nil {
		t.Fatal(err)
	}
	// 把会话目录 mtime 拨到 10 天前。
	old := time.Now().AddDate(0, 0, -10)
	if err := setDirModTime(archive.sessionDir("old-session"), old); err != nil {
		t.Skipf("无法调整目录 mtime（平台不支持）: %v", err)
	}

	engine := New(archive, &fakeSummarizer{}, Config{Model: "m", RetentionDays: 7})
	engine.cleanupOnce(ctx)

	metas, _ := archive.ListSessions(ctx)
	if len(metas) != 0 {
		t.Fatalf("过期会话未被清理：%v", metas)
	}
}

func TestMergeSummaries_ProducesGlobalAndMarksMerged(t *testing.T) {
	sum := &fakeSummarizer{handoff: "【已确认事实】合并后的全局摘要"}
	engine, archive := newTestEngine(t, sum, nil)
	ctx := context.Background()
	sessionID := "s"

	// 直接预置 3 个 chunk 摘要。
	now := time.Now()
	for i, content := range []string{"postgres 排查", "部署讨论", "监控告警"} {
		s := &Summary{ID: itoa(i) + "-chunk", Level: LevelChunk, Content: content, CreatedAt: now.Add(time.Duration(i) * time.Second)}
		if err := archive.SaveSummary(ctx, sessionID, s); err != nil {
			t.Fatal(err)
		}
	}
	engine.setCachedSummaries(sessionID, mustList(t, archive, sessionID))

	if err := engine.mergeSummaries(ctx, sessionID); err != nil {
		t.Fatal(err)
	}
	sums, _ := archive.ListSummaries(ctx, sessionID)
	var global *Summary
	merged := 0
	for _, s := range sums {
		if s.Level == LevelGlobal {
			global = s
		}
		if s.Merged {
			merged++
		}
	}
	if global == nil || len(global.MergedIDs) != 3 {
		t.Fatalf("global 摘要缺失或溯源错误：%+v", global)
	}
	if merged != 3 {
		t.Fatalf("被合并 chunk 未全部标记 merged：%d", merged)
	}
	// 摘要区组装：merged 的不再出现。
	msg := engine.summaryZoneMessage(ctx, sessionID)
	if msg == nil || !strings.Contains(msg.Content[0].Text, "合并后的全局摘要") {
		t.Fatalf("摘要区应包含 global 摘要：%v", msg)
	}
	for _, s := range []string{"postgres 排查"} {
		if strings.Contains(msg.Content[0].Text, s) {
			t.Fatalf("已合并的 chunk 不应再进摘要区：%s", msg.Content[0].Text)
		}
	}
}

func mustList(t *testing.T, a *FileSystemArchive, sessionID string) []*Summary {
	t.Helper()
	sums, err := a.ListSummaries(context.Background(), sessionID)
	if err != nil {
		t.Fatal(err)
	}
	return sums
}
