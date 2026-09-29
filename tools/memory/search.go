package memory

import (
	"context"
	"errors"
	"sort"
	"strings"
	"time"

	"github.com/capyflow/vortexagent/internal/textscore"
)

// errEmptyQuery 查询词为空时返回。
var errEmptyQuery = errors.New("检索关键词不能为空")

// KeywordSearcher 是零依赖的关键词评分检索器，是 Searcher 的默认实现。
//
// 评分逻辑委托给 internal/textscore（与 knowledge、ctxengine 共享的检索
// 内核）：词元命中加权（完整词 > 子串 > 单字，标签/类型信号最强）、长度
// 归一、30 天半衰期新鲜度衰减、重要级加分。检索语义是"部分匹配按相关度
// 排序"而非"全部词元必须命中"——记忆召回的典型场景是拿片段找记忆
// （"上次那个 Postgres 的问题"），词法得分为 0 的记忆不返回。
type KeywordSearcher struct {
	nowFn func() time.Time // 可注入时钟（测试用）
}

// NewKeywordSearcher 创建关键词评分检索器。
func NewKeywordSearcher() *KeywordSearcher {
	return &KeywordSearcher{nowFn: time.Now}
}

// Search 实现 Searcher 接口。
func (k *KeywordSearcher) Search(ctx context.Context, memories []*Memory, query string, limit int) ([]*Memory, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if strings.TrimSpace(query) == "" {
		return nil, errEmptyQuery
	}

	type hit struct {
		mem   *Memory
		score float64
	}
	var hits []hit
	for _, m := range memories {
		if s := k.score(m, query); s > 0 {
			hits = append(hits, hit{m, s})
		}
	}
	sort.SliceStable(hits, func(i, j int) bool {
		if hits[i].score != hits[j].score {
			return hits[i].score > hits[j].score
		}
		// 同分时新记忆优先，再按 ID 保证顺序确定。
		if !hits[i].mem.UpdatedAt.Equal(hits[j].mem.UpdatedAt) {
			return hits[i].mem.UpdatedAt.After(hits[j].mem.UpdatedAt)
		}
		return hits[i].mem.ID < hits[j].mem.ID
	})
	if limit > 0 && len(hits) > limit {
		hits = hits[:limit]
	}
	out := make([]*Memory, 0, len(hits))
	for _, h := range hits {
		out = append(out, h.mem)
	}
	return out, nil
}

// score 计算单条记忆与查询的相关度得分，词法上无任何命中时返回 0。
func (k *KeywordSearcher) score(m *Memory, query string) float64 {
	return textscore.Score(query, m.Content, textscore.Options{
		Tags:       m.Tags,
		Kind:       m.Kind,
		At:         m.UpdatedAt,
		Importance: m.Importance,
		Now:        k.nowFn(),
	})
}

// Similarity 返回两段文本的词元 Jaccard 相似度（0-1），用于 save 时的
// 近似重复提示。委托共享内核 textscore。
func Similarity(a, b string) float64 { return textscore.Similarity(a, b) }

// tokenize 切分检索词元。委托共享内核 textscore（保留包内别名，兼容既有测试与调用点）。
func tokenize(text string) []string { return textscore.Tokenize(text) }

// tokenSet 切词并去重。
func tokenSet(text string) map[string]bool { return textscore.TokenSet(text) }

func runeLen(s string) int { return textscore.RuneLen(s) }
