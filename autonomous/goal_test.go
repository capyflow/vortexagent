package autonomous

import (
	"sync"
	"testing"
	"time"
)

func TestGoalNewGoal(t *testing.T) {
	g := NewGoal()
	if g.mu == nil {
		t.Fatal("NewGoal 应初始化 mutex")
	}
	if g.Status != "" {
		t.Errorf("新 Goal 状态应为空，got %q", g.Status)
	}
}

func TestGoalConcurrentAccess(t *testing.T) {
	g := NewGoal()
	g.ID = "test-1"
	g.Title = "测试目标"

	var wg sync.WaitGroup
	for i := 0; i < 100; i++ {
		wg.Add(2)
		go func() {
			defer wg.Done()
			g.SetNextRunAt(time.Now().Add(time.Hour))
		}()
		go func() {
			defer wg.Done()
			_ = g.GetNextRunAt()
		}()
	}
	wg.Wait()
}

func TestGoalStatusTransition(t *testing.T) {
	g := NewGoal()
	g.Status = GoalStatusActive

	if g.GetStatus() != GoalStatusActive {
		t.Errorf("状态应为 Active, got %q", g.GetStatus())
	}

	g.SetStatus(GoalStatusDone)
	if g.GetStatus() != GoalStatusDone {
		t.Errorf("状态应为 Done, got %q", g.GetStatus())
	}
}
