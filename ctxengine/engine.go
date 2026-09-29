package ctxengine

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/capyflow/vortexagent/agent/sessionstore"
	"github.com/capyflow/vortexagent/llm"
)

// contextKey 在 Ask 入口注入当前会话 ID，search_history / expand_chunk 工具
// 经它取到"当前在哪个会话里"。CLI 与 serve 的 Ask 都会调用 WithSession，
// 工具无需各自接线。
type contextKey struct{}

// WithSession 返回携带会话 ID 的 context。
func WithSession(ctx context.Context, sessionID string) context.Context {
	return context.WithValue(ctx, contextKey{}, sessionID)
}

func sessionFromCtx(ctx context.Context) string {
	if ctx == nil {
		return ""
	}
	id, _ := ctx.Value(contextKey{}).(string)
	return id
}

// Config 是引擎配置（config.ContextConfig 的运行时形态，零值字段取默认）。
type Config struct {
	Model        string
	WindowTokens int // 0 → llm.GetContextWindow(Model)

	SoftRatio     float64 // 归档触发：使用率软阈值
	HardRatio     float64 // 发送前硬校验阈值
	SummaryBudget float64 // 摘要区预算（占窗口比例）

	KeepRecentBlocks int // 归档保留的最近块数
	MaxBlocks        int // 活跃块数上限（超过即触发）

	RetrievalLimit int // search_history 默认条数
	RetentionDays  int // 归档保留天数，0 = 永久

	// OnCandidateFacts 归档抽取的候选长期记忆回调（M2 起接入 memory Store；
	// nil 时仅丢弃）。在后台归档 goroutine 中调用。
	OnCandidateFacts func(ctx context.Context, sessionID string, facts []CandidateFact)
}

func (c Config) withDefaults() Config {
	if c.WindowTokens <= 0 {
		c.WindowTokens = llm.GetContextWindow(c.Model)
	}
	if c.SoftRatio <= 0 || c.SoftRatio >= 1 {
		c.SoftRatio = DefaultSoftRatio
	}
	if c.HardRatio <= c.SoftRatio || c.HardRatio > 1 {
		c.HardRatio = DefaultHardRatio
	}
	if c.SummaryBudget <= 0 || c.SummaryBudget >= 1 {
		c.SummaryBudget = DefaultSummaryBudget
	}
	if c.KeepRecentBlocks <= 0 {
		c.KeepRecentBlocks = DefaultKeepRecentBlocks
	}
	if c.MaxBlocks <= c.KeepRecentBlocks {
		c.MaxBlocks = c.KeepRecentBlocks * 2
	}
	if c.RetrievalLimit <= 0 {
		c.RetrievalLimit = DefaultRetrievalLimit
	}
	return c
}

// engineState 是单个会话的引擎运行态。
type engineState struct {
	mu sync.Mutex
	// calibration 估算校准系数：真实 prompt_tokens / 估算值的滑动平均。
	calibration float64
	// summaries 摘要缓存（归档/合并完成时刷新；首次 Build 惰性加载）。
	summaries []*Summary
	loaded    bool
	// archiving 归档 singleflight 标记。
	archiving bool
	// merging 摘要合并 singleflight 标记。
	merging bool
}

// Engine 是单会话上下文引擎。全局单例，无会话指针，并发安全。
type Engine struct {
	cfg     Config
	budget  Budget
	archive ArchiveStore
	sum     Summarizer

	mu     sync.Mutex
	states map[string]*engineState
}

// New 创建引擎。summarizer 为 nil 时归档禁用（无法生成交接摘要就不卸载），
// Build 退化为纯窗口裁剪，检索仍可用（对已有归档）。
func New(archive ArchiveStore, sum Summarizer, cfg Config) *Engine {
	cfg = cfg.withDefaults()
	return &Engine{
		cfg:     cfg,
		budget:  NewBudget(cfg.WindowTokens, cfg.SoftRatio, cfg.HardRatio, cfg.SummaryBudget, cfg.KeepRecentBlocks, cfg.MaxBlocks),
		archive: archive,
		sum:     sum,
		states:  make(map[string]*engineState),
	}
}

// Config 返回生效配置（含默认值填充）。
func (e *Engine) Config() Config { return e.cfg }

func (e *Engine) state(sessionID string) *engineState {
	e.mu.Lock()
	defer e.mu.Unlock()
	st, ok := e.states[sessionID]
	if !ok {
		st = &engineState{calibration: 1.0}
		e.states[sessionID] = st
	}
	return st
}

// DropState 会话删除时清理运行态。
func (e *Engine) DropState(sessionID string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	delete(e.states, sessionID)
}

// ---------- L3：token 估算与校准 ----------

// EstimateTokens 估算消息列表 token 数（经该会话的校准系数修正）。
func (e *Engine) EstimateTokens(sessionID string, msgs []llm.Message) int {
	est := EstimateMessagesTokens(msgs)
	st := e.state(sessionID)
	st.mu.Lock()
	calib := st.calibration
	st.mu.Unlock()
	return int(float64(est) * calib)
}

// RecordUsage 用一次真实响应的 usage 校准估算系数（L3）。
// 系数为真实/估算的 EMA（α=0.3），钳制在 [0.5, 3.0] 防单次异常拉飞。
func (e *Engine) RecordUsage(sessionID string, sent []llm.Message, usage llm.Usage) {
	if usage.InputTokens <= 0 {
		return
	}
	est := EstimateMessagesTokens(sent)
	if est <= 0 {
		return
	}
	st := e.state(sessionID)
	st.mu.Lock()
	defer st.mu.Unlock()
	ratio := float64(usage.InputTokens) / float64(est)
	if ratio < 0.5 {
		ratio = 0.5
	}
	if ratio > 3.0 {
		ratio = 3.0
	}
	st.calibration = 0.7*st.calibration + 0.3*ratio
}

// ContextUsage 返回当前活跃历史（含摘要区预算内表示）的估算占用率。
func (e *Engine) ContextUsage(sessionID string, msgs []llm.Message) float64 {
	if e.budget.WindowTokens <= 0 {
		return 0
	}
	return float64(e.EstimateTokens(sessionID, msgs)) / float64(e.budget.WindowTokens)
}

// ---------- Build：每轮组装（同步路径） ----------

// Build 组装本轮发给模型的消息：摘要区 + 最近块原文。
// 不含 system prompt（由 agent 统一前置）。
//
// 摘要区超预算时本轮降级为「global 摘要 + 未合并的最近 chunk」，
// 并异步触发摘要合并（Build 内绝不做 LLM 调用）。
func (e *Engine) Build(ctx context.Context, session *sessionstore.Session) []llm.Message {
	msgs := session.Messages()
	blocks := SplitBlocks(msgs)

	summaryMsg := e.summaryZoneMessage(ctx, session.ID)
	summaryTokens := 0
	if summaryMsg != nil {
		summaryTokens = EstimateMessagesTokens([]llm.Message{*summaryMsg})
	}

	// 最近块从尾部取，受 soft 预算（扣除摘要区）约束；至少保留 1 块。
	limit := e.budget.SoftLimitTokens() - summaryTokens
	keep := make([]Block, 0, e.budget.KeepRecentBlocks)
	used := 0
	for i := len(blocks) - 1; i >= 0; i-- {
		tc := blocks[i].TokenCount()
		// 最新一块无条件保留（不能把正在处理的对话裁掉）；
		// 其余块超预算即停。
		if len(keep) > 0 && used+tc > limit {
			break
		}
		keep = append(keep, blocks[i])
		used += tc
		if len(keep) >= e.budget.KeepRecentBlocks && used > limit {
			break
		}
	}
	// 反转为时间序。
	for i, j := 0, len(keep)-1; i < j; i, j = i+1, j-1 {
		keep[i], keep[j] = keep[j], keep[i]
	}
	// 超预算的旧块顺势归档（异步；L2 的同步裁剪只在硬校验不过时发生）。
	if len(keep) < len(blocks) {
		e.dropOldBlocksAsync(ctx, session, blocks[:len(blocks)-len(keep)])
	}

	out := make([]llm.Message, 0, 4+len(keep))
	if summaryMsg != nil {
		out = append(out, *summaryMsg)
	}
	out = append(out, FlattenBlocks(keep)...)
	return out
}

// summaryZoneMessage 组装摘要区 system 消息（无摘要返回 nil）。
func (e *Engine) summaryZoneMessage(ctx context.Context, sessionID string) *llm.Message {
	summaries := e.cachedSummaries(ctx, sessionID)
	if len(summaries) == 0 {
		return nil
	}
	budget := e.budget.SummaryTokens()

	// global 摘要永远保留；chunk 按时间序从新到旧装填，超预算即停；
	// 被挤出的 chunk 异步触发合并。
	var sb strings.Builder
	sb.WriteString("【历史对话交接摘要】\n以下是更早对话的状态交接；需要具体细节时用 search_history 定位、expand_chunk 展开原文，不要凭摘要猜测。\n")
	used := EstimateMessagesTokens([]llm.Message{llm.NewTextMessage(llm.RoleSystem, sb.String())})
	included := 0
	var evicted []*Summary
	for _, s := range summaries {
		if s.Merged {
			continue // 已并入 global，不再重复出现
		}
		tc := llm.EstimateTokenCount(s.Content) + 32
		if used+tc > budget {
			evicted = append(evicted, s)
			continue
		}
		fmt.Fprintf(&sb, "\n[%s | %s]\n%s\n", s.ID, s.CreatedAt.Format("01-02 15:04"), s.Content)
		used += tc
		included++
	}
	if included == 0 && len(evicted) > 0 {
		// 全部超预算：至少给一行索引，模型仍可检索。
		sb.WriteString(fmt.Sprintf("\n更早对话已归档 %d 段（摘要暂不可见，可用 search_history 检索）。\n", len(evicted)))
	}
	if len(evicted) >= 2 {
		e.mergeSummariesAsync(ctx, sessionID)
	}
	msg := llm.NewTextMessage(llm.RoleSystem, strings.TrimRight(sb.String(), "\n"))
	return &msg
}

// cachedSummaries 取摘要缓存，未加载时从 Archive 惰性加载。
func (e *Engine) cachedSummaries(ctx context.Context, sessionID string) []*Summary {
	st := e.state(sessionID)
	st.mu.Lock()
	defer st.mu.Unlock()
	if !st.loaded {
		sums, err := e.archive.ListSummaries(ctx, sessionID)
		if err == nil {
			st.summaries = sums
		}
		st.loaded = true
	}
	return st.summaries
}

func (e *Engine) setCachedSummaries(sessionID string, sums []*Summary) {
	st := e.state(sessionID)
	st.mu.Lock()
	st.summaries = sums
	st.loaded = true
	st.mu.Unlock()
}

// ---------- 触发与归档 ----------

// ShouldArchive 判断是否应触发归档：使用率过软阈值，或活跃块数超上限。
func (e *Engine) ShouldArchive(sessionID string, msgs []llm.Message) bool {
	if e.sum == nil {
		return false
	}
	if e.ContextUsage(sessionID, msgs) >= e.budget.SoftRatio {
		return true
	}
	return len(SplitBlocks(msgs)) > e.budget.MaxBlocks
}

// OnTurnDone 在每轮 Ask 结束后调用：评估触发条件，满足则后台归档。
// 永不阻塞、永不 panic 外泄；归档用独立 context（请求 ctx 可能随即取消）。
func (e *Engine) OnTurnDone(sessionID string, session *sessionstore.Session) {
	if e.sum == nil || !e.ShouldArchive(sessionID, session.Messages()) {
		return
	}
	st := e.state(sessionID)
	st.mu.Lock()
	if st.archiving {
		st.mu.Unlock()
		return
	}
	st.archiving = true
	st.mu.Unlock()

	go func() {
		defer func() {
			st.mu.Lock()
			st.archiving = false
			st.mu.Unlock()
		}()
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		_ = e.archiveSession(ctx, sessionID, session, false)
	}()
}

// Compact 同步执行一次归档（L2 硬校验 / 测试用）。
func (e *Engine) Compact(ctx context.Context, sessionID string, session *sessionstore.Session) error {
	return e.archiveSession(ctx, sessionID, session, false)
}

// dropOldBlocksAsync Build 时发现超预算旧块的异步归档入口（尽力而为）。
func (e *Engine) dropOldBlocksAsync(ctx context.Context, session *sessionstore.Session, old []Block) {
	if e.sum == nil || len(old) == 0 {
		return
	}
	st := e.state(session.ID)
	st.mu.Lock()
	if st.archiving {
		st.mu.Unlock()
		return
	}
	st.archiving = true
	st.mu.Unlock()
	go func() {
		defer func() {
			st.mu.Lock()
			st.archiving = false
			st.mu.Unlock()
		}()
		bctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		_ = e.archiveSession(bctx, session.ID, session, false)
	}()
}

// archiveSession 归档主流水线。force=true 时（L2/L4 紧急路径）摘要失败
// 不放弃裁剪：落库后生成机械摘要（块主题列表）保住索引功能。
func (e *Engine) archiveSession(ctx context.Context, sessionID string, session *sessionstore.Session, force bool) error {
	msgs := session.Messages()
	blocks := SplitBlocks(msgs)
	if len(blocks) <= e.budget.KeepRecentBlocks {
		return nil
	}
	toArchive := blocks[:len(blocks)-e.budget.KeepRecentBlocks]
	keepMsgs := FlattenBlocks(blocks[len(blocks)-e.budget.KeepRecentBlocks:])

	// 步骤 2：原文先落库（指纹去重保证重试幂等）。
	existing, err := e.archive.ListBlocks(ctx, sessionID)
	if err != nil {
		return fmt.Errorf("ctxengine: 列举归档块失败: %w", err)
	}
	seqBase := 0
	if len(existing) > 0 {
		seqBase = existing[len(existing)-1].Seq + 1
	}
	archived := make([]ArchivedBlock, 0, len(toArchive))
	blockIDs := make([]string, 0, len(toArchive))
	for i, b := range toArchive {
		id := NewBlockID(b)
		blockIDs = append(blockIDs, id)
		archived = append(archived, ArchivedBlock{
			ID:            id,
			Seq:           seqBase + i,
			FirstUserText: b.FirstUserText(),
			Messages:      b.Messages,
			MsgCount:      len(b.Messages),
			TokenCount:    b.TokenCount(),
			ArchivedAt:    time.Now(),
		})
	}
	if err := e.archive.AppendBlocks(ctx, sessionID, archived); err != nil {
		return fmt.Errorf("ctxengine: 归档原文落库失败: %w", err)
	}

	// 步骤 3：交接摘要（三级降级链在 Summarizer 内）。
	handoff := ""
	var facts []string
	if e.sum != nil {
		h, f, err := e.sum.Summarize(ctx, FlattenBlocks(toArchive))
		if err != nil {
			if !force {
				// 原文已落库但摘要失败：放弃推进，活跃区不动，下轮重试。
				return fmt.Errorf("ctxengine: %w", err)
			}
			handoff = mechanicalSummary(toArchive)
		} else {
			handoff = h
			facts = f
		}
	} else {
		handoff = mechanicalSummary(toArchive)
	}

	// 步骤 4：写摘要。
	now := time.Now()
	summary := &Summary{
		ID:         fmt.Sprintf("chunk-%d", now.UnixMilli()),
		Level:      LevelChunk,
		Content:    handoff,
		BlockIDs:   blockIDs,
		MsgCount:   len(FlattenBlocks(toArchive)),
		TokenCount: llm.EstimateTokenCount(handoff),
		CreatedAt:  now,
	}
	if err := e.archive.SaveSummary(ctx, sessionID, summary); err != nil {
		if !force {
			return fmt.Errorf("ctxengine: 写入摘要失败: %w", err)
		}
		// 紧急路径：摘要写不进去也要裁（原文已安全），索引靠块首条消息仍可检索。
	} else {
		// 摘要缓存以 Store 回读为准，保证与持久层一致；回读失败则本地追加。
		if fresh, err := e.archive.ListSummaries(ctx, sessionID); err == nil {
			e.setCachedSummaries(sessionID, fresh)
		} else {
			e.setCachedSummaries(sessionID, e.appendSummary(e.cachedSummaries(ctx, sessionID), summary))
		}
	}

	// 步骤 5：替换活跃区（锁内原子替换）。
	session.ResetTo(keepMsgs)

	// 步骤 6：候选事实回调。
	if e.cfg.OnCandidateFacts != nil && len(facts) > 0 {
		cfs := make([]CandidateFact, 0, len(facts))
		for _, f := range facts {
			f = strings.TrimSpace(f)
			if f == "" {
				continue
			}
			cfs = append(cfs, CandidateFact{Content: f, SourceChunkID: summary.ID})
		}
		if len(cfs) > 0 {
			e.cfg.OnCandidateFacts(ctx, sessionID, cfs)
		}
	}
	return nil
}

// mechanicalSummary 紧急路径的机械摘要：块主题列表，保住检索索引功能。
func mechanicalSummary(blocks []Block) string {
	var sb strings.Builder
	sb.WriteString("【机械截断】因上下文超限裁剪，未经语义压缩。本段包含的主题：\n")
	seen := 0
	for _, b := range blocks {
		t := strings.TrimSpace(b.FirstUserText())
		if t == "" {
			continue
		}
		runes := []rune(t)
		if len(runes) > 60 {
			t = string(runes[:60]) + "…"
		}
		fmt.Fprintf(&sb, "- %s\n", t)
		seen++
		if seen >= 20 {
			break
		}
	}
	return strings.TrimRight(sb.String(), "\n")
}

func (e *Engine) appendSummary(sums []*Summary, s *Summary) []*Summary {
	out := make([]*Summary, 0, len(sums)+1)
	out = append(out, sums...)
	out = append(out, s)
	return out
}

// ---------- 摘要合并（摘要区自清理） ----------

// mergeSummariesAsync 异步把最旧的若干 chunk 摘要合并为一个 global 摘要。
func (e *Engine) mergeSummariesAsync(ctx context.Context, sessionID string) {
	if e.sum == nil {
		return
	}
	st := e.state(sessionID)
	st.mu.Lock()
	if st.merging || st.archiving {
		st.mu.Unlock()
		return
	}
	st.merging = true
	st.mu.Unlock()

	go func() {
		defer func() {
			st.mu.Lock()
			st.merging = false
			st.mu.Unlock()
		}()
		mctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		_ = e.mergeSummaries(mctx, sessionID)
	}()
}

// mergeSummaries 同步合并：取最旧的至多 8 个未合并 chunk，拼接后一次 LLM
// 调用生成 global 摘要；旧 chunk 标记 merged（不再进摘要区，仍可检索）。
func (e *Engine) mergeSummaries(ctx context.Context, sessionID string) error {
	sums := e.cachedSummaries(ctx, sessionID)
	var chunks []*Summary
	for _, s := range sums {
		if s.Level == LevelChunk && !s.Merged {
			chunks = append(chunks, s)
		}
	}
	if len(chunks) < 2 {
		return nil
	}
	if len(chunks) > 8 {
		chunks = chunks[:8]
	}

	var body strings.Builder
	for _, c := range chunks {
		fmt.Fprintf(&body, "%s\n\n", c.Content)
	}
	merged, _, err := e.sum.Summarize(ctx, []llm.Message{llm.NewTextMessage(llm.RoleUser, body.String())})
	if err != nil {
		// 合并失败不影响正确性：摘要区降级逻辑已兜底。
		return err
	}

	now := time.Now()
	global := &Summary{
		ID:         fmt.Sprintf("global-%d", now.UnixMilli()),
		Level:      LevelGlobal,
		Content:    merged,
		MergedIDs:  summaryIDs(chunks),
		TokenCount: llm.EstimateTokenCount(merged),
		CreatedAt:  now,
	}
	// global 需要覆盖块范围（供 expand 溯源）：继承被合并 chunk 的 BlockIDs。
	var blockIDs []string
	msgCount := 0
	for _, c := range chunks {
		blockIDs = append(blockIDs, c.BlockIDs...)
		msgCount += c.MsgCount
		c.Merged = true
		if err := e.archive.SaveSummary(ctx, sessionID, c); err != nil {
			return err
		}
	}
	global.BlockIDs = blockIDs
	global.MsgCount = msgCount
	if err := e.archive.SaveSummary(ctx, sessionID, global); err != nil {
		return err
	}

	fresh, err := e.archive.ListSummaries(ctx, sessionID)
	if err == nil {
		e.setCachedSummaries(sessionID, fresh)
	}
	return nil
}

func summaryIDs(summaries []*Summary) []string {
	ids := make([]string, 0, len(summaries))
	for _, s := range summaries {
		ids = append(ids, s.ID)
	}
	return ids
}

// ---------- L4：超窗兜底 ----------

// IsContextWindowError 尽力识别 provider 的上下文超窗错误
// （各厂商文案不同，按常见关键词匹配）。
func IsContextWindowError(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	for _, kw := range []string{
		"context length", "context_length", "maximum context", "context window",
		"too many tokens", "token limit", "input length exceeds", "prompt is too long",
		"reduce the length", "exceeds the maximum",
	} {
		if strings.Contains(msg, kw) {
			return true
		}
	}
	return false
}

// Relieve 超窗兜底（L4）：把最旧 1/3 块先落库再裁掉（force，机械摘要保索引）。
// 返回是否发生了裁剪。
func (e *Engine) Relieve(ctx context.Context, sessionID string, session *sessionstore.Session) bool {
	msgs := session.Messages()
	blocks := SplitBlocks(msgs)
	if len(blocks) <= MinKeepBlocks+1 {
		return false
	}
	cut := len(blocks) / 3
	if cut < 1 {
		cut = 1
	}
	if err := e.archiveSessionRange(ctx, sessionID, session, blocks[:cut], blocks[cut:]); err != nil {
		return false
	}
	return true
}

// archiveSessionRange 对指定块范围执行"落库→摘要→替换"（Relieve 专用，
// 绕过 KeepRecentBlocks 的常规选块逻辑；force 语义）。
func (e *Engine) archiveSessionRange(ctx context.Context, sessionID string, session *sessionstore.Session, toArchive, keep []Block) error {
	existing, err := e.archive.ListBlocks(ctx, sessionID)
	if err != nil {
		return err
	}
	seqBase := 0
	if len(existing) > 0 {
		seqBase = existing[len(existing)-1].Seq + 1
	}
	archived := make([]ArchivedBlock, 0, len(toArchive))
	blockIDs := make([]string, 0, len(toArchive))
	for i, b := range toArchive {
		id := NewBlockID(b)
		blockIDs = append(blockIDs, id)
		archived = append(archived, ArchivedBlock{
			ID:            id,
			Seq:           seqBase + i,
			FirstUserText: b.FirstUserText(),
			Messages:      b.Messages,
			MsgCount:      len(b.Messages),
			TokenCount:    b.TokenCount(),
			ArchivedAt:    time.Now(),
		})
	}
	if err := e.archive.AppendBlocks(ctx, sessionID, archived); err != nil {
		return err
	}

	handoff := mechanicalSummary(toArchive)
	if e.sum != nil {
		if h, _, err := e.sum.Summarize(ctx, FlattenBlocks(toArchive)); err == nil {
			handoff = h
		}
	}
	summary := &Summary{
		ID:         fmt.Sprintf("chunk-%d", time.Now().UnixMilli()),
		Level:      LevelChunk,
		Content:    handoff,
		BlockIDs:   blockIDs,
		MsgCount:   len(FlattenBlocks(toArchive)),
		TokenCount: llm.EstimateTokenCount(handoff),
		CreatedAt:  time.Now(),
	}
	if err := e.archive.SaveSummary(ctx, sessionID, summary); err != nil {
		return err
	}
	if fresh, err := e.archive.ListSummaries(ctx, sessionID); err == nil {
		e.setCachedSummaries(sessionID, fresh)
	}
	session.ResetTo(FlattenBlocks(keep))
	return nil
}

// ---------- TTL 清理 ----------

// StartRetention 启动每日惰性 TTL 清理（retentionDays=0 时不做任何事）。
// 阻塞直至 ctx 取消，调用方放入 goroutine。
func (e *Engine) StartRetention(ctx context.Context) {
	if e.cfg.RetentionDays <= 0 {
		return
	}
	go func() {
		e.cleanupOnce(ctx)
		t := time.NewTicker(24 * time.Hour)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				e.cleanupOnce(ctx)
			}
		}
	}()
}

func (e *Engine) cleanupOnce(ctx context.Context) {
	metas, err := e.archive.ListSessions(ctx)
	if err != nil {
		return
	}
	cutoff := time.Now().AddDate(0, 0, -e.cfg.RetentionDays)
	for _, m := range metas {
		if m.UpdatedAt.Before(cutoff) {
			_ = e.archive.DeleteSession(ctx, m.ID)
		}
	}
}

// ---------- 检索（供 tools.go 调用） ----------

func (e *Engine) retriever() *Retriever { return &Retriever{engine: e} }

// SearchHistory 按相关度检索归档摘要与块主题。
func (e *Engine) SearchHistory(ctx context.Context, sessionID, query string, limit int) ([]Hit, error) {
	return e.retriever().Search(ctx, sessionID, query, limit)
}

// ExpandChunk 展开一个归档片段（chunk/global 摘要 ID 或块 ID）为纯文本记录。
func (e *Engine) ExpandChunk(ctx context.Context, sessionID, chunkID string, maxTokens int) (string, error) {
	return e.retriever().Expand(ctx, sessionID, chunkID, maxTokens)
}
