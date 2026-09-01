package memory

import (
	"context"
	"strings"
	"testing"
	"time"
)

// newTestTools 创建挂在临时目录上的工具组，返回工具名到实例的映射、底层存储与时钟控制。
func newTestTools(t *testing.T) (map[string]*Tool, *JSONStore, *time.Time) {
	t.Helper()
	s, clock := newTestStore(t)
	tools := make(map[string]*Tool)
	for _, tool := range NewTools(s, nil) {
		tools[tool.name] = tool
	}
	return tools, s, clock
}

// call 是 Tool.Call 的便捷封装，失败直接 Fatal。
func call(t *testing.T, tool *Tool, args map[string]any) string {
	t.Helper()
	out, err := tool.Call(context.Background(), args)
	if err != nil {
		t.Fatalf("%s(%v) 返回错误: %v", tool.name, args, err)
	}
	return out
}

// firstID 取存储中唯一一条记忆的 ID。
func firstID(t *testing.T, s *JSONStore) string {
	t.Helper()
	list, err := s.List(context.Background())
	if err != nil || len(list) != 1 {
		t.Fatalf("期望存储中恰有 1 条记忆，实际 %d 条（err=%v）", len(list), err)
	}
	return list[0].ID
}

// TestTools_crudFlow 端到端走一遍增查改删。
func TestTools_crudFlow(t *testing.T) {
	tools, s, _ := newTestTools(t)

	// 增
	out := call(t, tools["memory_save"], map[string]any{
		"content":    "用户偏好使用 Go 编写后端服务",
		"kind":       "preference",
		"tags":       []any{"go", "后端"},
		"importance": 4,
	})
	if !strings.Contains(out, "已保存记忆") {
		t.Fatalf("save 输出不符合预期: %s", out)
	}
	id := firstID(t, s)

	// 查（get）
	out = call(t, tools["memory_get"], map[string]any{"id": id})
	if !strings.Contains(out, "用户偏好使用 Go 编写后端服务") || !strings.Contains(out, "preference") {
		t.Fatalf("get 输出不符合预期: %s", out)
	}

	// 查（search）
	out = call(t, tools["memory_search"], map[string]any{"query": "偏好 后端"})
	if !strings.Contains(out, id) {
		t.Fatalf("search 应命中刚保存的记忆 %s，输出: %s", id, out)
	}

	// 改
	out = call(t, tools["memory_update"], map[string]any{
		"id":      id,
		"content": "用户偏好使用 Go 编写后端服务，测试用标准库 testing",
	})
	if !strings.Contains(out, "已更新记忆") {
		t.Fatalf("update 输出不符合预期: %s", out)
	}
	m, _ := s.Get(context.Background(), id)
	if !strings.Contains(m.Content, "标准库 testing") || len(m.Tags) != 2 || m.Importance != 4 {
		t.Errorf("update 只应修改 content，实际: %+v", m)
	}

	// 删
	out = call(t, tools["memory_delete"], map[string]any{"id": id})
	if !strings.Contains(out, "已删除记忆") {
		t.Fatalf("delete 输出不符合预期: %s", out)
	}
	out = call(t, tools["memory_get"], map[string]any{"id": id})
	if !strings.Contains(out, "未找到") {
		t.Fatalf("删除后 get 应返回未找到: %s", out)
	}
}

// TestTools_saveDuplicateHint 保存高度相似内容时两条都保存（提示而非阻止）。
func TestTools_saveDuplicateHint(t *testing.T) {
	tools, _, _ := newTestTools(t)
	out := call(t, tools["memory_save"], map[string]any{"content": "项目使用 PostgreSQL 作为主数据库"})
	if strings.Contains(out, "高度相似") {
		t.Fatalf("首条保存不应提示相似: %s", out)
	}
	out = call(t, tools["memory_save"], map[string]any{"content": "项目使用 PostgreSQL 作为主数据库存储"})
	if !strings.Contains(out, "高度相似") {
		t.Fatalf("相似内容保存应给出提示: %s", out)
	}
}

// TestTools_searchFilters 按 kind 与 tag 过滤检索结果。
func TestTools_searchFilters(t *testing.T) {
	tools, _, _ := newTestTools(t)
	call(t, tools["memory_save"], map[string]any{"content": "主库切换需要人工审批", "kind": "lesson", "tags": []any{"db"}})
	call(t, tools["memory_save"], map[string]any{"content": "主库每日凌晨巡检", "kind": "fact", "tags": []any{"db"}})
	call(t, tools["memory_save"], map[string]any{"content": "主库监控已接入告警", "kind": "fact", "tags": []any{"ops"}})

	out := call(t, tools["memory_search"], map[string]any{"query": "主库", "kind": "fact"})
	if strings.Contains(out, "审批") || !strings.Contains(out, "巡检") || !strings.Contains(out, "告警") {
		t.Errorf("kind=fact 过滤结果不符预期: %s", out)
	}

	out = call(t, tools["memory_search"], map[string]any{"query": "主库", "tag": "ops"})
	if !strings.Contains(out, "告警") || strings.Contains(out, "审批") || strings.Contains(out, "巡检") {
		t.Errorf("tag=ops 过滤结果不符预期: %s", out)
	}
}

// TestTools_listOrderAndLimit 列表按最近更新倒序并支持 limit。
func TestTools_listOrderAndLimit(t *testing.T) {
	tools, _, clock := newTestTools(t)
	for _, content := range []string{"第一条记忆", "第二条记忆", "第三条记忆"} {
		call(t, tools["memory_save"], map[string]any{"content": content})
		*clock = (*clock).Add(time.Hour) // 每条间隔 1 小时，保证顺序可断言
	}

	out := call(t, tools["memory_list"], nil)
	first := strings.Index(out, "第一条记忆")
	third := strings.Index(out, "第三条记忆")
	if first == -1 || third == -1 || third > first {
		t.Errorf("列表应按最近更新倒序（第三条在最前）: %s", out)
	}

	out = call(t, tools["memory_list"], map[string]any{"limit": 2})
	if strings.Contains(out, "第一条记忆") {
		t.Errorf("limit=2 不应包含最早的第一条: %s", out)
	}
}

// TestTools_notFoundIsTextNotError 查询 / 更新 / 删除不存在的 ID 返回说明文本而非错误，
// 让模型能理解发生了什么并自行纠正。
func TestTools_notFoundIsTextNotError(t *testing.T) {
	tools, _, _ := newTestTools(t)
	for name, args := range map[string]map[string]any{
		"memory_get":    {"id": "mem-missing"},
		"memory_update": {"id": "mem-missing", "content": "x"},
		"memory_delete": {"id": "mem-missing"},
	} {
		out := call(t, tools[name], args)
		if !strings.Contains(out, "未找到") {
			t.Errorf("%s 对不存在的 ID 应返回\"未找到\"提示，实际: %s", name, out)
		}
	}
}

// TestTools_updateRequiresOneField 更新时必须至少提供一个要修改的字段。
func TestTools_updateRequiresOneField(t *testing.T) {
	tools, s, _ := newTestTools(t)
	call(t, tools["memory_save"], map[string]any{"content": "待更新的记忆"})
	id := firstID(t, s)

	if _, err := tools["memory_update"].Call(context.Background(), map[string]any{"id": id}); err == nil {
		t.Fatal("不提供任何修改字段应报错")
	}
}

// TestTools_validationErrors 参数校验：非法输入必须报错，不能写进记忆库。
func TestTools_validationErrors(t *testing.T) {
	tools, _, _ := newTestTools(t)
	cases := []struct {
		name string
		tool string
		args map[string]any
	}{
		{"缺少content", "memory_save", map[string]any{"kind": "fact"}},
		{"content为空", "memory_save", map[string]any{"content": "   "}},
		{"kind非法", "memory_save", map[string]any{"content": "x", "kind": "diary"}},
		{"importance超上限", "memory_save", map[string]any{"content": "x", "importance": 6}},
		{"importance非整数", "memory_save", map[string]any{"content": "x", "importance": 3.5}},
		{"tags不是数组", "memory_save", map[string]any{"content": "x", "tags": "go"}},
		{"content超长", "memory_save", map[string]any{"content": strings.Repeat("长", MaxContentLength+1)}},
		{"search缺少query", "memory_search", map[string]any{"limit": 5}},
		{"search的kind非法", "memory_search", map[string]any{"query": "x", "kind": "diary"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if _, err := tools[c.tool].Call(context.Background(), c.args); err == nil {
				t.Fatalf("%s 应返回错误", c.tool)
			}
		})
	}
}

// TestTools_defaultKindAndImportance 缺省参数的回落值：kind=fact、importance=3。
func TestTools_defaultKindAndImportance(t *testing.T) {
	tools, s, _ := newTestTools(t)
	call(t, tools["memory_save"], map[string]any{"content": "只有内容的记忆"})
	list, _ := s.List(context.Background())
	if len(list) != 1 || list[0].Kind != KindFact || list[0].Importance != ImportanceDefault {
		t.Fatalf("缺省值不符预期: %+v", list)
	}
}
