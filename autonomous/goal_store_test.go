package autonomous

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestJSONGoalStore(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "goals.json")

	store, err := NewJSONGoalStore(file)
	if err != nil {
		t.Fatalf("NewJSONGoalStore 不应报错: %v", err)
	}

	// 添加目标
	g1 := NewGoal()
	g1.ID = "goal-1"
	g1.Title = "每日天气"
	g1.Status = GoalStatusActive
	g1.NextRunAt = time.Now().Add(time.Hour)
	g1.Schedule = Schedule{Type: ScheduleCron, Cron: "0 8 * * *"}

	if err := store.Save(g1); err != nil {
		t.Fatalf("Save 不应报错: %v", err)
	}

	// 加载
	goals, err := store.LoadAll()
	if err != nil {
		t.Fatalf("LoadAll 不应报错: %v", err)
	}
	if len(goals) != 1 {
		t.Errorf("应有 1 个目标, got %d", len(goals))
	}
	if goals[0].ID != "goal-1" {
		t.Errorf("目标 ID 应为 goal-1, got %s", goals[0].ID)
	}

	// 获取单个
	got, err := store.Get("goal-1")
	if err != nil {
		t.Fatalf("Get 不应报错: %v", err)
	}
	if got.Title != "每日天气" {
		t.Errorf("标题应为 每日天气, got %s", got.Title)
	}

	// 删除
	if err := store.Delete("goal-1"); err != nil {
		t.Fatalf("Delete 不应报错: %v", err)
	}
	goals, _ = store.LoadAll()
	if len(goals) != 0 {
		t.Errorf("删除后应为 0 个目标, got %d", len(goals))
	}
}

func TestJSONGoalStorePersistence(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "goals.json")

	// 第一次创建并保存
	store1, err := NewJSONGoalStore(file)
	if err != nil {
		t.Fatalf("NewJSONGoalStore 不应报错: %v", err)
	}

	g := NewGoal()
	g.ID = "persist-1"
	g.Title = "持久化测试"
	g.Status = GoalStatusActive
	g.NextRunAt = time.Now().Add(time.Hour)
	store1.Save(g)

	// 第二次创建，应加载已有数据
	store2, err := NewJSONGoalStore(file)
	if err != nil {
		t.Fatalf("第二次 NewJSONGoalStore 不应报错: %v", err)
	}

	goals, err := store2.LoadAll()
	if err != nil {
		t.Fatalf("LoadAll 不应报错: %v", err)
	}
	if len(goals) != 1 {
		t.Errorf("应有 1 个目标, got %d", len(goals))
	}
	if goals[0].ID != "persist-1" {
		t.Errorf("目标 ID 应为 persist-1, got %s", goals[0].ID)
	}
	if goals[0].Title != "持久化测试" {
		t.Errorf("标题应为 持久化测试, got %s", goals[0].Title)
	}
}

func TestJSONGoalStoreRuntimeState(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "goals.json")

	store, err := NewJSONGoalStore(file)
	if err != nil {
		t.Fatalf("NewJSONGoalStore 不应报错: %v", err)
	}

	g := NewGoal()
	g.ID = "runtime-1"
	g.Title = "运行时状态"
	g.Status = GoalStatusActive
	g.RunCount = 5
	g.FailCount = 2
	g.LastResult = "上次结果"
	g.LastReflect = "上次反思"
	g.NextRunAt = time.Now().Add(time.Hour)

	if err := store.Save(g); err != nil {
		t.Fatalf("Save 不应报错: %v", err)
	}

	// 重新加载
	store2, _ := NewJSONGoalStore(file)
	goals, _ := store2.LoadAll()

	if len(goals) != 1 {
		t.Fatalf("应有 1 个目标, got %d", len(goals))
	}

	loaded := goals[0]
	if loaded.RunCount != 5 {
		t.Errorf("RunCount 应为 5, got %d", loaded.RunCount)
	}
	if loaded.FailCount != 2 {
		t.Errorf("FailCount 应为 2, got %d", loaded.FailCount)
	}
	if loaded.LastResult != "上次结果" {
		t.Errorf("LastResult 应为 上次结果, got %s", loaded.LastResult)
	}
}

func TestJSONGoalStoreAutoCreateDir(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "sub", "dir", "goals.json")

	store, err := NewJSONGoalStore(file)
	if err != nil {
		t.Fatalf("NewJSONGoalStore 不应报错: %v", err)
	}

	g := NewGoal()
	g.ID = "auto-dir"
	store.Save(g)

	// 验证文件已创建
	if _, err := os.Stat(file); os.IsNotExist(err) {
		t.Error("文件应已创建")
	}
}
