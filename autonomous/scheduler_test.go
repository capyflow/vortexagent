package autonomous

import (
	"testing"
	"time"
)

func TestNewScheduler(t *testing.T) {
	s := NewScheduler(time.Hour)
	if s == nil {
		t.Fatal("NewScheduler 不应返回 nil")
	}
	if s.maxSleep != time.Hour {
		t.Errorf("maxSleep 应为 1h, got %v", s.maxSleep)
	}
	if len(s.goals) != 0 {
		t.Errorf("初始 goals 应为空, got %d", len(s.goals))
	}
}

func TestNewScheduler_DefaultMaxSleep(t *testing.T) {
	s := NewScheduler(0)
	if s.maxSleep != time.Hour {
		t.Errorf("默认 maxSleep 应为 1h, got %v", s.maxSleep)
	}
}

func TestSchedulerNextWakeTime_NoGoals(t *testing.T) {
	s := NewScheduler(time.Hour)
	next := s.NextWakeTime()

	// 没有目标时，应为 now + maxSleep
	expected := time.Now().Add(time.Hour)
	diff := next.Sub(expected)
	if diff > time.Second || diff < -time.Second {
		t.Errorf("无目标时下次唤醒应为 now+1h, got %v (diff %v)", next, diff)
	}
}

func TestSchedulerNextWakeTime_WithGoals(t *testing.T) {
	s := NewScheduler(time.Hour)

	// 添加两个目标，一个 2 小时后，一个 30 分钟后
	g1 := NewGoal()
	g1.Status = GoalStatusActive
	g1.NextRunAt = time.Now().Add(2 * time.Hour)
	s.goals = append(s.goals, g1)

	g2 := NewGoal()
	g2.Status = GoalStatusActive
	g2.NextRunAt = time.Now().Add(30 * time.Minute)
	s.goals = append(s.goals, g2)

	next := s.NextWakeTime()

	// 应返回较早的那个（30 分钟后）
	expected := time.Now().Add(30 * time.Minute)
	diff := next.Sub(expected)
	if diff > time.Second || diff < -time.Second {
		t.Errorf("下次唤醒应为 30 分钟后, got %v (diff %v)", next, diff)
	}
}

func TestSchedulerSleepDuration(t *testing.T) {
	s := NewScheduler(time.Hour)

	// 添加一个 2 小时后的目标
	g := NewGoal()
	g.Status = GoalStatusActive
	g.NextRunAt = time.Now().Add(2 * time.Hour)
	s.goals = append(s.goals, g)

	d := s.SleepDuration()

	// 超过 maxSleep，应返回 maxSleep
	if d != time.Hour {
		t.Errorf("睡眠时长应为 1h (maxSleep), got %v", d)
	}
}

func TestSchedulerDueGoals(t *testing.T) {
	s := NewScheduler(time.Hour)

	now := time.Now()

	// 已过期目标
	g1 := NewGoal()
	g1.ID = "due-1"
	g1.Status = GoalStatusActive
	g1.NextRunAt = now.Add(-1 * time.Minute)
	g1.Priority = 5
	s.goals = append(s.goals, g1)

	// 未到期目标
	g2 := NewGoal()
	g2.ID = "future-1"
	g2.Status = GoalStatusActive
	g2.NextRunAt = now.Add(1 * time.Hour)
	s.goals = append(s.goals, g2)

	// 已完成的到期目标
	g3 := NewGoal()
	g3.ID = "done-1"
	g3.Status = GoalStatusDone
	g3.NextRunAt = now.Add(-1 * time.Minute)
	s.goals = append(s.goals, g3)

	// 高优先级已过期目标
	g4 := NewGoal()
	g4.ID = "due-high"
	g4.Status = GoalStatusActive
	g4.NextRunAt = now.Add(-5 * time.Minute)
	g4.Priority = 10
	s.goals = append(s.goals, g4)

	due := s.DueGoals()

	// 应返回 2 个到期目标（排除 Done 状态）
	if len(due) != 2 {
		t.Errorf("到期目标应为 2 个, got %d", len(due))
	}

	// 按优先级排序，第一个应是 g4（priority 10）
	if len(due) > 0 && due[0].ID != "due-high" {
		t.Errorf("第一个到期目标应为 due-high, got %s", due[0].ID)
	}
}

func TestSchedulerGoalsCopy(t *testing.T) {
	s := NewScheduler(time.Hour)

	g := NewGoal()
	g.ID = "test"
	g.Status = GoalStatusActive
	s.goals = append(s.goals, g)

	copied := s.Goals()
	if len(copied) != 1 {
		t.Errorf("应返回 1 个目标, got %d", len(copied))
	}

	copied = append(copied, NewGoal())
	if len(s.goals) != 1 {
		t.Error("修改副本切片不应影响原数据")
	}
}
