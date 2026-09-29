package ctxengine

import (
	"context"
	"strings"
	"testing"
)

func TestTools_SearchAndExpand(t *testing.T) {
	sum := &fakeSummarizer{handoff: "【任务意图】排查 postgres 连接报错\n【当前状态】已定位根因"}
	engine, archive := newTestEngine(t, sum, nil)
	session := buildSession(t, 6)
	ctx := WithSession(context.Background(), session.ID)

	if err := engine.Compact(ctx, session.ID, session); err != nil {
		t.Fatal(err)
	}
	sums, _ := archive.ListSummaries(ctx, session.ID)
	if len(sums) == 0 {
		t.Fatal("归档摘要缺失")
	}

	tools := NewTools(engine)
	var searchTool, expandTool *Tool
	for _, tl := range tools {
		if tl.Name() == "search_history" {
			searchTool = tl
		}
		if tl.Name() == "expand_chunk" {
			expandTool = tl
		}
	}
	if searchTool == nil || expandTool == nil {
		t.Fatal("工具缺失")
	}

	// search_history：经 context 注入会话，命中摘要与块主题。
	out, err := searchTool.Call(ctx, map[string]any{"query": "postgres 报错"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "expand_chunk") || !strings.Contains(out, "postgres") {
		t.Fatalf("search_history 输出异常：%s", out)
	}

	// expand_chunk：展开摘要覆盖的原文。
	out, err = expandTool.Call(ctx, map[string]any{"chunk_id": sums[0].ID})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "帮我解决第一号 postgres 报错问题") {
		t.Fatalf("expand_chunk 未取回原文：%s", out)
	}

	// 无命中提示换关键词。
	out, err = searchTool.Call(ctx, map[string]any{"query": "完全不相关的查询词xyz"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "未在归档中找到") {
		t.Fatalf("零命中提示缺失：%s", out)
	}

	// 不存在的 chunk_id 报错。
	if _, err := expandTool.Call(ctx, map[string]any{"chunk_id": "chunk-nope"}); err == nil {
		t.Fatal("不存在的片段应报错")
	}
}
