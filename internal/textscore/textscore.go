// Package textscore 是框架内共享的文本词法评分检索内核：CJK 二元组分词 +
// 加权命中评分 + 可选的新鲜度衰减与重要级加分，仅依赖标准库。
//
// 它是三处检索实现共用的底层——长期记忆（tools/memory）、文档知识库
// （tools/knowledge）、单会话历史归档（ctxengine）——三者数据、工具、目录
// 完全独立，仅共享这套评分实现（"本质隔离、实现复用"）。
//
// 检索语义是"部分匹配按相关度排序"而非"全部词元必须命中"：典型场景是拿
// 片段找内容（"上次那个 Postgres 的问题"），要求全部词元命中会因单个词
// 不匹配而全军覆没。词法得分为 0（无任何命中）的内容不返回。
//
// 词法得分 = 查询的每个词元按命中的最高权重计一次（长查询除以 sqrt(词元数)
// 归一，不因词元多而线性膨胀）+ 可选的新鲜度加分（30 天半衰期衰减）+ 可选的
// 重要级加分。
package textscore

import (
	"math"
	"strings"
	"time"
	"unicode"
)

// 命中权重：完整词 > 子串 > 单字；标签与类型是人工归类/结构化维度，信号最强。
const (
	WeightWord    = 1.0 // 完整词 / CJK 二元组在内容中命中
	WeightSubstr  = 0.7 // 长词（≥3 字符）子串命中，如 postgres 命中 postgresql
	WeightUnigram = 0.3 // 单个非 ASCII 字符命中（区分度低，权重压低）
	WeightTag     = 1.5 // 标签精确命中（标签是人工归类，信号最强）
	WeightKind    = 0.5 // 类型命中

	RecencyBoost       = 0.2  // 新鲜度加分上限
	RecencyHalflife    = 30.0 // 新鲜度半衰期（天）
	ImportanceBoostMax = 0.15 // 重要级加分上限（5 级封顶）
	importBoostPerLvl  = 0.03 // 重要级每级加分
)

// Options 是一次评分的附加维度，均可选（零值 = 只做纯词法评分）。
type Options struct {
	// Tags 参与分词的标签词元，命中权重最高（信号最强）。
	Tags []string
	// Kind 结构化类型（如记忆的 kind），查询词元与之精确相等时加分。
	Kind string
	// At 内容的时间戳：非零时按 30 天半衰期给新鲜度加分（越新越好）。
	At time.Time
	// Importance 重要级（0-5），每级 +importBoostPerLvl。
	Importance int
	// Now 当前时间（测试可注入）；零值取 time.Now。
	Now time.Time
}

// Score 返回 query 相对 text 的相关度得分，词法上无任何命中时返回 0。
func Score(query, text string, o Options) float64 {
	q := strings.TrimSpace(query)
	if q == "" {
		return 0
	}
	tokens := Tokenize(q)
	lower := strings.ToLower(text)
	contentTokens := TokenSet(lower)

	var tagTokens map[string]bool
	if len(o.Tags) > 0 {
		tagTokens = make(map[string]bool)
		for _, t := range o.Tags {
			for tok := range TokenSet(t) {
				tagTokens[tok] = true
			}
		}
	}
	kind := strings.ToLower(o.Kind)

	var lexical float64
	for _, tok := range tokens {
		switch {
		case tagTokens[tok]:
			lexical += WeightTag
		case kind != "" && tok == kind:
			lexical += WeightKind
		case contentTokens[tok]:
			lexical += tokenWeight(tok)
		case RuneLen(tok) >= 3 && strings.Contains(lower, tok):
			lexical += WeightSubstr
		}
	}
	if lexical <= 0 {
		return 0
	}
	lexical /= math.Sqrt(float64(len(tokens)))

	score := lexical
	if !o.At.IsZero() {
		now := o.Now
		if now.IsZero() {
			now = time.Now()
		}
		age := now.Sub(o.At)
		if age < 0 {
			age = 0
		}
		score += RecencyBoost * math.Exp(-age.Hours()/(24*RecencyHalflife))
	}
	if o.Importance > 0 {
		score += importBoostPerLvl * float64(o.Importance)
	}
	return score
}

// tokenWeight 单个词元的命中权重：非 ASCII 单字（区分度低）压低，其余全额。
func tokenWeight(tok string) float64 {
	if RuneLen(tok) == 1 && tok[0] >= 0x80 {
		return WeightUnigram
	}
	return WeightWord
}

// Similarity 返回两段文本的词元 Jaccard 相似度（0-1），用于近似重复提示。
func Similarity(a, b string) float64 {
	sa, sb := TokenSet(a), TokenSet(b)
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

// Tokenize 将文本切分为检索词元（内部统一小写化，调用方无需预处理）：
//   - ASCII 字母 / 数字 / 下划线的连续串为一个词元（如 postgresql、user_id）；
//   - 其他 Unicode 字母（中日韩等）连续串切成二元组（bigram）——中文没有
//     分词器可用，二元组是零依赖方案里召回/精度平衡最好的粒度；
//     串长为 1 时保留单字，避免单字查询无法命中；
//   - 标点、空白等分隔符被丢弃，且会切断 CJK 连续串（不产生跨标点的词元）。
func Tokenize(text string) []string {
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

// TokenSet 切词并去重。
func TokenSet(text string) map[string]bool {
	set := make(map[string]bool)
	for _, tok := range Tokenize(text) {
		set[tok] = true
	}
	return set
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

// RuneLen 返回字符串的 rune 数。
func RuneLen(s string) int { return len([]rune(s)) }
