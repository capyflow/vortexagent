package textscore

import (
	"testing"
	"time"
)

func TestScore_LexicalWeights(t *testing.T) {
	// 完整词命中 > 0；无命中 = 0。
	if s := Score("postgres 连接", "项目使用 postgresql 数据库", Options{}); s <= 0 {
		t.Fatalf("子串命中应得分：%f", s)
	}
	if s := Score("kubernetes", "完全无关的中文内容", Options{}); s != 0 {
		t.Fatalf("无命中应为 0：%f", s)
	}
	// 中文二元组命中。
	if s := Score("报错", "启动时连接报错了", Options{}); s <= 0 {
		t.Fatalf("CJK 二元组应命中：%f", s)
	}
	// 标签信号最强：带标签的应高于纯内容同词命中。
	withTag := Score("数据库", "数据库相关", Options{Tags: []string{"数据库"}})
	plain := Score("数据库", "数据库相关", Options{})
	if withTag <= plain {
		t.Fatalf("标签命中应加权：%f vs %f", withTag, plain)
	}
}

func TestScore_LongQueryNormalized(t *testing.T) {
	// 长查询不应因词元多而线性膨胀：单相关词元在长查询中得分应更低。
	short := Score("postgres", "postgres 部署手册", Options{})
	long := Score("postgres kubernetes redis nginx mysql 部署监控", "postgres 部署手册", Options{})
	if long >= short {
		t.Fatalf("长查询应被 sqrt 归一：%f vs %f", long, short)
	}
}

func TestScore_RecencyDecay(t *testing.T) {
	now := time.Now()
	fresh := Score("报错", "连接报错", Options{At: now, Now: now})
	old := Score("报错", "连接报错", Options{At: now.AddDate(0, -6, 0), Now: now})
	if fresh <= old {
		t.Fatalf("越新得分应越高：%f vs %f", fresh, old)
	}
}

func TestTokenize(t *testing.T) {
	got := Tokenize("PostgreSQL 连接报错!")
	want := map[string]bool{"postgresql": true, "连接": true, "接报": true, "报错": true}
	if len(got) == 0 {
		t.Fatal("不应为空")
	}
	for w := range want {
		found := false
		for _, g := range got {
			if g == w {
				found = true
			}
		}
		if !found {
			t.Fatalf("缺少词元 %q：%v", w, got)
		}
	}
}

func TestSimilarity(t *testing.T) {
	if Similarity("相同文本", "相同文本") != 1 {
		t.Fatal("相同文本相似度应为 1")
	}
	if Similarity("完全不同", "毫无交集xyz") != 0 {
		t.Fatal("无交集相似度应为 0")
	}
}
