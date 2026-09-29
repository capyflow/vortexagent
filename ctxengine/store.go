package ctxengine

import (
	"context"
	"time"

	"github.com/capyflow/vortexagent/llm"
)

// ArchivedBlock 是一条已归档的块：原始消息的不可变容器。
// ID 是内容的指纹（见 Fingerprint），归档重试时同块天然去重。
type ArchivedBlock struct {
	ID            string        `json:"id"`
	Seq           int           `json:"seq"`
	FirstUserText string        `json:"first_user_text"`
	Messages      []llm.Message `json:"messages"`
	MsgCount      int           `json:"msg_count"`
	TokenCount    int           `json:"token_count"`
	ArchivedAt    time.Time     `json:"archived_at"`
}

// 摘要级别。
const (
	LevelChunk  = "chunk"  // 单次归档的状态交接摘要
	LevelGlobal = "global" // 多个 chunk 摘要合并后的二级摘要
)

// Summary 是一段归档历史的压缩表示：活跃上下文里常驻的只有它，
// 原文在 ArchivedBlock 里按需展开。
type Summary struct {
	ID string `json:"id"`
	// Level chunk = 单次归档摘要；global = 多个 chunk 合并后的二级摘要。
	Level string `json:"level"`
	// Content 状态交接摘要文本（Render 后的【任务意图】… 格式）。
	Content string `json:"content"`
	// BlockIDs 本摘要覆盖的归档块 ID。
	BlockIDs []string `json:"block_ids,omitempty"`
	// MergedIDs Level=global 时记录被合并的 chunk 摘要 ID（溯源）。
	MergedIDs []string `json:"merged_ids,omitempty"`
	// Merged chunk 被合并进 global 后置位：不再进摘要区，仍可检索。
	Merged     bool      `json:"merged"`
	MsgCount   int       `json:"msg_count"`
	TokenCount int       `json:"token_count"`
	CreatedAt  time.Time `json:"created_at"`
}

// SessionMeta 是 TTL 清理用的会话元信息。
type SessionMeta struct {
	ID        string
	UpdatedAt time.Time
}

// ArchiveStore 是归档持久层抽象：原始块（不可变）+ 摘要（可合并标记）。
// 实现方必须并发安全。内置 FileSystemArchive；Postgres 实现为后续延伸。
type ArchiveStore interface {
	// AppendBlocks 原子落库一批块。ID 已存在的块静默跳过（归档重试去重）。
	AppendBlocks(ctx context.Context, sessionID string, blocks []ArchivedBlock) error
	// GetBlock 按 ID 取块（含消息原文），不存在返回 (nil, nil)。
	GetBlock(ctx context.Context, sessionID, blockID string) (*ArchivedBlock, error)
	// ListBlocks 返回会话全部已归档块（按 Seq 升序）。
	ListBlocks(ctx context.Context, sessionID string) ([]ArchivedBlock, error)
	// SaveSummary 写入或覆盖一条摘要（合并时用它把旧 chunk 标记为 merged）。
	SaveSummary(ctx context.Context, sessionID string, s *Summary) error
	// ListSummaries 返回会话全部摘要（含已合并的，按 CreatedAt 升序）。
	ListSummaries(ctx context.Context, sessionID string) ([]*Summary, error)
	// DeleteSession 删除会话的全部归档（TTL 清理 / 会话删除联动）。
	DeleteSession(ctx context.Context, sessionID string) error
	// ListSessions 列出有归档的会话及其最后更新时间（TTL 清理用）。
	ListSessions(ctx context.Context) ([]SessionMeta, error)
}
