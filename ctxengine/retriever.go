package ctxengine

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/capyflow/vortexagent/internal/textscore"
	"github.com/capyflow/vortexagent/llm"
)

// Hit 是一条检索命中：可以是 chunk/global 摘要，也可以是单个归档块。
// ID 交给 expand_chunk 展开；出处字段（时间/消息数）用于抑制幻觉。
type Hit struct {
	ID       string  `json:"id"`
	Kind     string  `json:"kind"` // "summary" | "block"
	Level    string  `json:"level,omitempty"`
	Time     string  `json:"time"`
	MsgCount int     `json:"msg_count"`
	Summary  string  `json:"summary"`
	Score    float64 `json:"score"`
}

// Retriever 基于共享评分内核 textscore 实现归档检索。
type Retriever struct {
	engine *Engine
}

// Search 对 [chunk/global 摘要] + [归档块首条用户消息] 评分，返回 top-N。
// 词法无命中的条目不返回；命中 0 条由工具层提示换关键词。
func (r *Retriever) Search(ctx context.Context, sessionID, query string, limit int) ([]Hit, error) {
	if strings.TrimSpace(query) == "" {
		return nil, fmt.Errorf("检索关键词不能为空")
	}
	if limit <= 0 {
		limit = r.engine.cfg.RetrievalLimit
	}
	if limit > 20 {
		limit = 20
	}

	var hits []Hit

	summaries, err := r.engine.archive.ListSummaries(ctx, sessionID)
	if err != nil {
		return nil, err
	}
	for _, s := range summaries {
		score := textscore.Score(query, s.Content, textscore.Options{
			At:   s.CreatedAt,
			Tags: []string{s.Level},
		})
		if score <= 0 {
			continue
		}
		hits = append(hits, Hit{
			ID:       s.ID,
			Kind:     "summary",
			Level:    s.Level,
			Time:     s.CreatedAt.Format("2006-01-02 15:04"),
			MsgCount: s.MsgCount,
			Summary:  truncateRunes(s.Content, 300),
			Score:    score,
		})
	}

	blocks, err := r.engine.archive.ListBlocks(ctx, sessionID)
	if err != nil {
		return nil, err
	}
	for _, b := range blocks {
		if b.FirstUserText == "" {
			continue
		}
		score := textscore.Score(query, b.FirstUserText, textscore.Options{
			At: b.ArchivedAt,
		})
		if score <= 0 {
			continue
		}
		hits = append(hits, Hit{
			ID:       b.ID,
			Kind:     "block",
			Time:     b.ArchivedAt.Format("2006-01-02 15:04"),
			MsgCount: b.MsgCount,
			Summary:  truncateRunes(b.FirstUserText, 300),
			Score:    score,
		})
	}

	sort.SliceStable(hits, func(i, j int) bool {
		if hits[i].Score != hits[j].Score {
			return hits[i].Score > hits[j].Score
		}
		return hits[i].ID < hits[j].ID
	})
	if len(hits) > limit {
		hits = hits[:limit]
	}
	return hits, nil
}

// Expand 展开一个归档片段为纯文本记录。id 可以是：
//   - 块 ID（block-xxxxxx）→ 该块的消息原文；
//   - 摘要 ID（chunk-xxx / global-xxx）→ 其覆盖的全部块的消息原文。
//
// 输出是「[时间] 角色: 内容」的文本记录，不是消息数组——绝不以裸
// tool_result 注入，避免破坏 tool 配对。超 maxTokens 从头截断并注明。
func (r *Retriever) Expand(ctx context.Context, sessionID, id string, maxTokens int) (string, error) {
	id = strings.TrimSpace(id)
	if id == "" {
		return "", fmt.Errorf("chunk_id 不能为空")
	}
	if maxTokens <= 0 {
		maxTokens = 2000
	}

	var blocks []ArchivedBlock
	if strings.HasPrefix(id, "block-") {
		b, err := r.engine.archive.GetBlock(ctx, sessionID, id)
		if err != nil {
			return "", err
		}
		if b == nil {
			return "", fmt.Errorf("未找到归档片段 %s", id)
		}
		blocks = append(blocks, *b)
	} else {
		sums, err := r.engine.archive.ListSummaries(ctx, sessionID)
		if err != nil {
			return "", err
		}
		var target *Summary
		for _, s := range sums {
			if s.ID == id {
				target = s
				break
			}
		}
		if target == nil {
			return "", fmt.Errorf("未找到归档片段 %s", id)
		}
		for _, bid := range target.BlockIDs {
			b, err := r.engine.archive.GetBlock(ctx, sessionID, bid)
			if err != nil {
				return "", err
			}
			if b != nil {
				blocks = append(blocks, *b)
			}
		}
	}
	if len(blocks) == 0 {
		return "", fmt.Errorf("片段 %s 没有可展开的归档消息", id)
	}

	var sb strings.Builder
	used := 0
	truncated := false
	for _, b := range blocks {
		for _, m := range b.Messages {
			text := textOfMessage(m)
			if text == "" {
				continue
			}
			role := "用户"
			switch m.Role {
			case llm.RoleAssistant:
				role = "助手"
			case llm.RoleTool:
				role = "工具"
			}
			line := fmt.Sprintf("[%s] %s: %s\n", b.ArchivedAt.Format("01-02 15:04"), role, text)
			tc := llm.EstimateTokenCount(line)
			if used+tc > maxTokens {
				truncated = true
				break
			}
			sb.WriteString(line)
			used += tc
		}
		if truncated {
			break
		}
	}
	out := strings.TrimRight(sb.String(), "\n")
	if truncated {
		out += fmt.Sprintf("\n[…超出 %d token 上限，已截断。可用更具体的关键词 search_history 缩小范围后再次展开]", maxTokens)
	}
	return out, nil
}

func truncateRunes(s string, n int) string {
	rs := []rune(s)
	if len(rs) <= n {
		return s
	}
	return string(rs[:n]) + "…"
}
