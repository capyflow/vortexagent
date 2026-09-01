package memory

import (
	"context"
	"errors"
	"math"
	"sort"
	"strings"
	"time"
	"unicode"
)

// errEmptyQuery 查询词为空时返回。
var errEmptyQuery = errors.New("检索关键词不能为空")

// 评分权重：查询的每个词元只按命中的最高权重计一次，长查询不因词元多而线性膨胀
//（总分会除以 sqrt(词元数) 归一）。新鲜度与重要级是封顶的小额加分。
const (
	weightWord    = 1.0 // 完整词 / CJK 二元组在内容中命中
	weightSubstr  = 0.7 // 长词（≥3 字符）子串命中，如 postgres 命中 postgresql
	weightUnigram = 0.3 // 单个非 ASCII 字符命中（区分度低，权重压低）
	weightTag     = 1.5 // 标签精确命中（标签是人工归类，信号最强）
	weightKind    = 0.5 // 类型命中
	recencyBoost  = 0.2 // 新鲜度加分上限（30 天半衰期衰减）
	importBoost   = 0.03 // 重要级每级加分（5 级封顶 +0.15）
	recencyHalflifeDays = 30.0
)

// KeywordSearcher 是零依赖的关键词评分检索器，是 Searcher 的默认实现。
//
// 检索语义是"部分匹配按相关度排序"而非"全部词元必须命中"：记忆召回的
// 典型场景是拿片段找记忆（"上次那个 Postgres 的问题"），要求全部词元命中
// 会因单个词不匹配而全军覆没。得分 = 词法得分（词元命中加权，长度归一）
// + 新鲜度加分 + 重要级加分；词法得分为 0 的记忆不返回。
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
	q := strings.TrimSpace(query)
	if q == "" {
		return nil, errEmptyQuery
	}
	tokens := tokenize(q)
	lowerQ := strings.ToLower(q)

	type hit struct {
		mem   *Memory
		score float64
	}
	var hits []hit
	for _, m := range memories {
		if s := k.score(m, tokens, lowerQ); s > 0 {
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
func (k *KeywordSearcher) score(m *Memory, qTokens []string, lowerQ string) float64 {
	lower := strings.ToLower(m.Content)
	contentTokens := tokenSet(lower)
	// 标签同样参与分词：查询词元命中标签的词元即获得标签权重（信号最强）。
	var tagTokens map[string]bool
	if len(m.Tags) > 0 {
		tagTokens = make(map[string]bool)
		for _, t := range m.Tags {
			for tok := range tokenSet(t) {
				tagTokens[tok] = true
			}
		}
	}
	kind := strings.ToLower(m.Kind)

	var lexical float64
	for _, tok := range qTokens {
		switch {
		case tagTokens[tok]:
			lexical += weightTag
		case kind != "" && tok == kind:
			lexical += weightKind
		case contentTokens[tok]:
			lexical += tokenWeight(tok)
		case runeLen(tok) >= 3 && strings.Contains(lower, tok):
			lexical += weightSubstr
		}
	}
	if lexical <= 0 {
		return 0
	}
	lexical /= math.Sqrt(float64(len(qTokens)))

	// 新鲜度：越新越好，按 30 天半衰期指数衰减到 0。
	age := k.nowFn().Sub(m.UpdatedAt)
	if age < 0 {
		age = 0
	}
	fresh := recencyBoost * math.Exp(-age.Hours()/(24*recencyHalflifeDays))
	imp := importBoost * float64(m.Importance)
	return lexical + fresh + imp
}

// tokenWeight 单个词元的命中权重：非 ASCII 单字（区分度低）压低，其余全额。
func tokenWeight(tok string) float64 {
	if runeLen(tok) == 1 && tok[0] >= 0x80 {
		return weightUnigram
	}
	return weightWord
}

// Similarity 返回两段文本的词元 Jaccard 相似度（0-1），用于 save 时的
// 近似重复提示。
func Similarity(a, b string) float64 {
	sa, sb := tokenSet(a), tokenSet(b)
	if len(sa) == 0 || len(sb) == 0 {
		return 0
	}
	inter := 0
	for tok := range sa {
		if sb[tok] {
			inter++
		}
	}
	return float64(inter) / float64(len(sa)+len(sb)-inter)
}

// tokenize 将文本切分为检索词元（内部统一小写化，调用方无需预处理）：
//   - ASCII 字母 / 数字 / 下划线的连续串为一个词元（如 postgresql、user_id）；
//   - 其他 Unicode 字母（中日韩等）连续串切成二元组（bigram）——中文没有
//     分词器可用，二元组是零依赖方案里召回/精度平衡最好的粒度；
//     串长为 1 时保留单字，避免单字查询无法命中；
//   - 标点、空白等分隔符被丢弃，且会切断 CJK 连续串（不产生跨标点的词元）。
func tokenize(text string) []string {
	runes := []rune(strings.ToLower(text))
	var tokens []string
	i := 0
	for i < len(runes) {
		r := runes[i]
		switch {
		case isASCIIWord(r):
			j := i
			for j < len(runes) && isASCIIWord(runes[j]) {
				j++
			}
			tokens = append(tokens, string(runes[i:j]))
			i = j
		case r >= 0x80 && unicode.IsLetter(r):
			j := i
			for j < len(runes) && runes[j] >= 0x80 && unicode.IsLetter(runes[j]) {
				j++
			}
			tokens = append(tokens, ngramTokens(runes[i:j])...)
			i = j
		default:
			i++
		}
	}
	return tokens
}

// ngramTokens 将非 ASCII 字母串切成二元组；长度为 1 时保留单字。
func ngramTokens(rs []rune) []string {
	if len(rs) == 1 {
		return []string{string(rs)}
	}
	tokens := make([]string, 0, len(rs)-1)
	for i := 0; i+1 < len(rs); i++ {
		tokens = append(tokens, string(rs[i:i+2]))
	}
	return tokens
}

func isASCIIWord(r rune) bool {
	return r == '_' || (r < 0x80 && (unicode.IsLetter(r) || unicode.IsDigit(r)))
}

// tokenSet 切词并去重。
func tokenSet(text string) map[string]bool {
	set := make(map[string]bool)
	for _, tok := range tokenize(text) {
		set[tok] = true
	}
	return set
}

func runeLen(s string) int { return len([]rune(s)) }
