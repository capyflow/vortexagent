package autonomous

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/capyflow/vortexagent/agent"
)

func TestAddGoalTool_OneShot(t *testing.T) {
	store, _ := NewJSONGoalStore(t.TempDir() + "/goals.json")
	registry := agent.NewRegistry()
	var addedGoal *Goal

	tool := NewAddGoalTool(store, registry, func(g *Goal) {
		addedGoal = g
	})

	args := map[string]any{
		"title":         "30分钟后提醒开会",
		"description":   "30 分钟后提醒我参加团队会议",
		"schedule_type": "oneshot",
		"delay_minutes": float64(30),
		"priority":      float64(8),
	}

	result, err := tool.Call(context.Background(), args)
	if err != nil {
		t.Fatalf("Call 不应报错: %v", err)
	}

	if !strings.Contains(result, "目标已添加") {
		t.Errorf("结果应包含'目标已添加', got %q", result)
	}

	// 验证目标已保存
	goals, _ := store.LoadAll()
	if len(goals) != 1 {
		t.Fatalf("应有 1 个目标, got %d", len(goals))
	}

	g := goals[0]
	if g.Title != "30分钟后提醒开会" {
		t.Errorf("标题应为'30分钟后提醒开会', got %q", g.Title)
	}
	if g.Schedule.Type != ScheduleOneShot {
		t.Errorf("调度类型应为 oneshot, got %q", g.Schedule.Type)
	}
	if g.Schedule.Delay != 30*time.Minute {
		t.Errorf("延迟应为 30m, got %v", g.Schedule.Delay)
	}
	if g.Priority != 8 {
		t.Errorf("优先级应为 8, got %d", g.Priority)
	}

	// 验证回调被调用
	if addedGoal == nil {
		t.Error("添加回调应被调用")
	}
}

func TestAddGoalTool_Interval(t *testing.T) {
	store, _ := NewJSONGoalStore(t.TempDir() + "/goals.json")
	registry := agent.NewRegistry()

	tool := NewAddGoalTool(store, registry, nil)

	args := map[string]any{
		"title":            "检查 CPU",
		"description":      "每 30 分钟检查服务器 CPU",
		"schedule_type":    "interval",
		"interval_minutes": float64(30),
	}

	result, err := tool.Call(context.Background(), args)
	if err != nil {
		t.Fatalf("Call 不应报错: %v", err)
	}

	if !strings.Contains(result, "目标已添加") {
		t.Errorf("结果应包含'目标已添加', got %q", result)
	}

	goals, _ := store.LoadAll()
	if len(goals) != 1 {
		t.Fatalf("应有 1 个目标, got %d", len(goals))
	}

	g := goals[0]
	if g.Schedule.Type != ScheduleInterval {
		t.Errorf("调度类型应为 interval, got %q", g.Schedule.Type)
	}
	if g.Schedule.Interval != 30*time.Minute {
		t.Errorf("间隔应为 30m, got %v", g.Schedule.Interval)
	}
}

func TestAddGoalTool_Cron(t *testing.T) {
	store, _ := NewJSONGoalStore(t.TempDir() + "/goals.json")
	registry := agent.NewRegistry()

	tool := NewAddGoalTool(store, registry, nil)

	args := map[string]any{
		"title":         "每日天气",
		"description":   "每天早上 8 点查询天气",
		"schedule_type": "cron",
		"cron":          "0 8 * * *",
	}

	result, err := tool.Call(context.Background(), args)
	if err != nil {
		t.Fatalf("Call 不应报错: %v", err)
	}

	if !strings.Contains(result, "目标已添加") {
		t.Errorf("结果应包含'目标已添加', got %q", result)
	}

	goals, _ := store.LoadAll()
	if len(goals) != 1 {
		t.Fatalf("应有 1 个目标, got %d", len(goals))
	}

	g := goals[0]
	if g.Schedule.Type != ScheduleCron {
		t.Errorf("调度类型应为 cron, got %q", g.Schedule.Type)
	}
	if g.Schedule.Cron != "0 8 * * *" {
		t.Errorf("cron 应为 '0 8 * * *', got %q", g.Schedule.Cron)
	}
	if g.NextRunAt.IsZero() {
		t.Error("cron 目标应有下次执行时间")
	}
}

func TestAddGoalTool_Event(t *testing.T) {
	store, _ := NewJSONGoalStore(t.TempDir() + "/goals.json")
	registry := agent.NewRegistry()

	tool := NewAddGoalTool(store, registry, nil)

	args := map[string]any{
		"title":         "Review PR",
		"description":   "有新推送时自动 review 代码",
		"schedule_type": "event",
		"event_type":    "github.push",
	}

	_, err := tool.Call(context.Background(), args)
	if err != nil {
		t.Fatalf("Call 不应报错: %v", err)
	}

	goals, _ := store.LoadAll()
	if len(goals) != 1 {
		t.Fatalf("应有 1 个目标, got %d", len(goals))
	}

	g := goals[0]
	if g.Schedule.Type != ScheduleEvent {
		t.Errorf("调度类型应为 event, got %q", g.Schedule.Type)
	}
	if g.Schedule.Event != "github.push" {
		t.Errorf("事件类型应为 'github.push', got %q", g.Schedule.Event)
	}
}

func TestAddGoalTool_InvalidInput(t *testing.T) {
	store, _ := NewJSONGoalStore(t.TempDir() + "/goals.json")
	registry := agent.NewRegistry()
	tool := NewAddGoalTool(store, registry, nil)

	// 缺少 title
	_, err := tool.Call(context.Background(), map[string]any{
		"schedule_type": "oneshot",
		"delay_minutes": float64(30),
	})
	if err == nil {
		t.Error("缺少 title 应返回错误")
	}

	// 无效的 schedule_type
	_, err = tool.Call(context.Background(), map[string]any{
		"title":         "test",
		"description":   "test",
		"schedule_type": "invalid",
	})
	if err == nil {
		t.Error("无效 schedule_type 应返回错误")
	}

	// oneshot 缺少 delay_minutes
	_, err = tool.Call(context.Background(), map[string]any{
		"title":         "test",
		"description":   "test",
		"schedule_type": "oneshot",
	})
	if err == nil {
		t.Error("oneshot 缺少 delay_minutes 应返回错误")
	}
}

func TestListGoalsTool(t *testing.T) {
	store, _ := NewJSONGoalStore(t.TempDir() + "/goals.json")
	tool := NewListGoalsTool(store)

	// 空列表
	result, err := tool.Call(context.Background(), map[string]any{})
	if err != nil {
		t.Fatalf("Call 不应报错: %v", err)
	}
	if !strings.Contains(result, "没有长期目标") {
		t.Errorf("空列表应提示无目标, got %q", result)
	}

	// 添加目标
	g := NewGoal()
	g.ID = "test-1"
	g.Title = "测试目标"
	g.Status = GoalStatusActive
	g.NextRunAt = time.Now().Add(time.Hour)
	g.RunCount = 3
	store.Save(g)

	result, err = tool.Call(context.Background(), map[string]any{})
	if err != nil {
		t.Fatalf("Call 不应报错: %v", err)
	}
	if !strings.Contains(result, "测试目标") {
		t.Errorf("结果应包含目标标题, got %q", result)
	}
	if !strings.Contains(result, "test-1") {
		t.Errorf("结果应包含目标 ID, got %q", result)
	}
}

func TestRemoveGoalTool(t *testing.T) {
	store, _ := NewJSONGoalStore(t.TempDir() + "/goals.json")
	registry := agent.NewRegistry()
	var removedID string

	tool := NewRemoveGoalTool(store, registry, func(id string) {
		removedID = id
	})

	// 先添加目标
	g := NewGoal()
	g.ID = "to-remove"
	g.Title = "待移除"
	g.Status = GoalStatusActive
	store.Save(g)

	// 移除
	result, err := tool.Call(context.Background(), map[string]any{
		"goal_id": "to-remove",
	})
	if err != nil {
		t.Fatalf("Call 不应报错: %v", err)
	}
	if !strings.Contains(result, "已移除") {
		t.Errorf("结果应包含'已移除', got %q", result)
	}

	// 验证已删除
	goals, _ := store.LoadAll()
	if len(goals) != 0 {
		t.Errorf("移除后应为 0 个目标, got %d", len(goals))
	}

	// 验证回调
	if removedID != "to-remove" {
		t.Errorf("移除回调应传入正确 ID, got %q", removedID)
	}

	// 移除不存在的目标
	_, err = tool.Call(context.Background(), map[string]any{
		"goal_id": "nonexistent",
	})
	if err == nil {
		t.Error("移除不存在的目标应返回错误")
	}
}

func TestFormatNextRunAt(t *testing.T) {
	tests := []struct {
		name     string
		input    time.Time
		contains string
	}{
		{"zero time (event)", time.Time{}, "等待事件触发"},
		{"past time", time.Now().Add(-1 * time.Hour), "已到期"},
		{"soon (30s)", time.Now().Add(30 * time.Second), "即将执行"},
		{"minutes (30m)", time.Now().Add(30 * time.Minute), "分钟后"},
		{"hours (2h)", time.Now().Add(2 * time.Hour), "小时"},
		{"days later", time.Now().Add(48 * time.Hour), "2026-"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := formatNextRunAt(tt.input)
			if !strings.Contains(result, tt.contains) {
				t.Errorf("formatNextRunAt(%v) 应包含 %q, got %q", tt.input, tt.contains, result)
			}
		})
	}
}
