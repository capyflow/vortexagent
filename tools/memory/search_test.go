package memory

import (
	"context"
	"testing"
	"time"
)

// TestTokenize 验证分词规则：ASCII 整词、CJK 二元组、单字保留、分隔符丢弃。
func TestTokenize(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want []string
	}{
		{"英文整词", "PostgreSQL DB", []string{"postgresql", "db"}},
		{"CJK 二元组", "用户偏好", []string{"用户", "户偏", "偏好"}},
		{"单字保留", "云", []string{"云"}},
		{"混合中英", "Go语言", []string{"go", "语言"}},
		{"下划线连词", "user_id", []string{"user_id"}},
		{"标点切断CJK串", "你好，世界！", []string{"你好", "世界"}},
		{"数字与字母混合串分开", "v1.2", []string{"v1", "2"}},
		{"空串", "", nil},
		{"纯标点", "。，！", nil},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := tokenize(c.in)
			if len(got) != len(c.want) {
				t.Fatalf("tokenize(%q) = %v，期望 %v", c.in, got, c.want)
			}
			for i := range got {
				if got[i] != c.want[i] {
					t.Fatalf("tokenize(%q) = %v，期望 %v", c.in, got, c.want)
				}
			}
		})
	}
}

// newFixedSearcher 创建时钟固定的检索器。
func newFixedSearcher(now time.Time) *KeywordSearcher {
	return &KeywordSearcher{nowFn: func() time.Time { return now }}
}

// TestKeywordSearcher_ranksByLexicalRelevance 词元命中越多排名越靠前，零命中被排除。
func TestKeywordSearcher_ranksByLexicalRelevance(t *testing.T) {
	s := newFixedSearcher(time.Now())
	mems := []*Memory{
		{ID: "a", Content: "项目使用 PostgreSQL 作为主数据库"},
		{ID: "b", Content: "数据库备份任务每天凌晨执行"},
		{ID: "c", Content: "用户偏好 Go 语言"},
	}
	got, err := s.Search(context.Background(), mems, "postgres 数据库", 10)
	if err != nil {
		t.Fatalf("Search 失败: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("期望 2 条命中（零命中的 c 应被排除），实际 %d 条: %+v", len(got), got)
	}
	if got[0].ID != "a" || got[1].ID != "b" {
		t.Errorf("期望 a（postgres+数据库）排在 b（仅数据库）前，实际 [%s, %s]", got[0].ID, got[1].ID)
	}
}

// TestKeywordSearcher_tagBoost 标签命中是最强信号：仅靠标签命中的记忆可排在内容命中之前。
func TestKeywordSearcher_tagBoost(t *testing.T) {
	s := newFixedSearcher(time.Now())
	mems := []*Memory{
		{ID: "tagged", Content: "负责报表模块的日常维护", Tags: []string{"数据库"}},
		{ID: "content", Content: "数据库设计文档评审记录"},
	}
	got, err := s.Search(context.Background(), mems, "数据库", 10)
	if err != nil {
		t.Fatalf("Search 失败: %v", err)
	}
	if len(got) != 2 || got[0].ID != "tagged" {
		t.Errorf("标签命中的 tagged 应排第一，实际: %+v", got)
	}
}

// TestKeywordSearcher_recency 词法得分相同时，更新时间更近的排前面。
func TestKeywordSearcher_recency(t *testing.T) {
	now := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)
	s := newFixedSearcher(now)
	mems := []*Memory{
		{ID: "old", Content: "发布流程需要审批", UpdatedAt: now.AddDate(0, 0, -90)},
		{ID: "new", Content: "发布流程需要审批", UpdatedAt: now.Add(-time.Hour)},
	}
	got, err := s.Search(context.Background(), mems, "发布流程", 10)
	if err != nil {
		t.Fatalf("Search 失败: %v", err)
	}
	if len(got) != 2 || got[0].ID != "new" {
		t.Errorf("更新更近的 new 应排第一，实际: %+v", got)
	}
}

// TestKeywordSearcher_importance 词法与时间都相同时，重要级高的排前面。
func TestKeywordSearcher_importance(t *testing.T) {
	now := time.Now()
	s := newFixedSearcher(now)
	mems := []*Memory{
		{ID: "low", Content: "定时任务在凌晨触发", Importance: 1, UpdatedAt: now},
		{ID: "high", Content: "定时任务在凌晨触发", Importance: 5, UpdatedAt: now},
	}
	got, _ := s.Search(context.Background(), mems, "定时任务", 10)
	if len(got) != 2 || got[0].ID != "high" {
		t.Errorf("重要级高的 high 应排第一，实际: %+v", got)
	}
}

// TestKeywordSearcher_substringMatch 长词子串命中（postgres ↔ postgresql）。
func TestKeywordSearcher_substringMatch(t *testing.T) {
	s := newFixedSearcher(time.Now())
	mems := []*Memory{
		{ID: "pg", Content: "生产环境使用 postgresql 15"},
	}
	got, err := s.Search(context.Background(), mems, "postgres", 10)
	if err != nil {
		t.Fatalf("Search 失败: %v", err)
	}
	if len(got) != 1 {
		t.Errorf("postgres 应以子串权重命中 postgresql，实际 %d 条", len(got))
	}
}

// TestKeywordSearcher_noMatch 零命中返回空列表而非错误。
func TestKeywordSearcher_noMatch(t *testing.T) {
	s := newFixedSearcher(time.Now())
	mems := []*Memory{{ID: "a", Content: "用户偏好中文"}}
	got, err := s.Search(context.Background(), mems, "kubernetes 部署", 10)
	if err != nil {
		t.Fatalf("Search 不应报错: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("零命中应返回空列表，实际: %+v", got)
	}
}

// TestKeywordSearcher_emptyQuery 空查询报错。
func TestKeywordSearcher_emptyQuery(t *testing.T) {
	s := newFixedSearcher(time.Now())
	if _, err := s.Search(context.Background(), nil, "  ", 10); err == nil {
		t.Fatal("空查询应返回错误")
	}
}

// TestKeywordSearcher_limit 结果按 limit 截断。
func TestKeywordSearcher_limit(t *testing.T) {
	s := newFixedSearcher(time.Now())
	var mems []*Memory
	for i := 0; i < 10; i++ {
		mems = append(mems, &Memory{ID: string(rune('a' + i)), Content: "部署脚本说明"})
	}
	got, _ := s.Search(context.Background(), mems, "部署脚本", 3)
	if len(got) != 3 {
		t.Errorf("limit=3 期望返回 3 条，实际 %d 条", len(got))
	}
}

// TestSimilarity Jaccard 相似度边界：完全相同为 1，无交集为 0。
func TestSimilarity(t *testing.T) {
	if s := Similarity("用户偏好深色主题", "用户偏好深色主题"); s != 1 {
		t.Errorf("相同文本相似度应为 1，实际 %v", s)
	}
	if s := Similarity("用户偏好深色主题", "服务器部署在杭州"); s != 0 {
		t.Errorf("无交集文本相似度应为 0，实际 %v", s)
	}
	s := Similarity("项目使用 PostgreSQL 数据库", "项目使用 PostgreSQL 数据库存储数据")
	if s <= 0 || s >= 1 {
		t.Errorf("部分重叠文本相似度应在 (0,1) 内，实际 %v", s)
	}
}
