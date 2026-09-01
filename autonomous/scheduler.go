package autonomous

import (
	"sort"
	"sync"
	"time"
)

// Scheduler 管理目标的调度：计算下次唤醒时间、返回到期目标。
//
// 并发安全：goals 切片受 mu 保护，Goal 的字段受各自的 mu 保护。
type Scheduler struct {
	mu       sync.RWMutex
	goals    []*Goal
	maxSleep time.Duration // 最大睡眠间隔（默认 1 小时）
}

// NewScheduler 创建调度器。
func NewScheduler(maxSleep time.Duration) *Scheduler {
	if maxSleep <= 0 {
		maxSleep = time.Hour
	}
	return &Scheduler{
		goals:    make([]*Goal, 0),
		maxSleep: maxSleep,
	}
}

// NextWakeTime 计算下次应该醒来的时间。
// 取所有活跃目标中最近的 NextRunAt；如果没有活跃目标，返回 now + maxSleep。
func (s *Scheduler) NextWakeTime() time.Time {
	s.mu.RLock()
	defer s.mu.RUnlock()

	var earliest time.Time
	for _, g := range s.goals {
		g.mu.RLock()
		status := g.Status
		nextRun := g.NextRunAt
		g.mu.RUnlock()

		if status != GoalStatusActive && status != GoalStatusPending {
			continue
		}
		if nextRun.IsZero() {
			continue
		}
		if earliest.IsZero() || nextRun.Before(earliest) {
			earliest = nextRun
		}
	}

	if earliest.IsZero() {
		return time.Now().Add(s.maxSleep)
	}
	return earliest
}

// SleepDuration 计算当前应该睡眠多久（受最大间隔限制）。
func (s *Scheduler) SleepDuration() time.Duration {
	nextWake := s.NextWakeTime()
	wait := time.Until(nextWake)
	if wait > s.maxSleep {
		return s.maxSleep
	}
	if wait < 0 {
		return 0
	}
	return wait
}

// DueGoals 返回所有到期的目标，按优先级降序排列。
func (s *Scheduler) DueGoals() []*Goal {
	s.mu.RLock()
	defer s.mu.RUnlock()

	var due []*Goal
	now := time.Now()
	for _, g := range s.goals {
		g.mu.RLock()
		status := g.Status
		nextRun := g.NextRunAt
		g.mu.RUnlock()

		if status != GoalStatusActive && status != GoalStatusPending {
			continue
		}
		if !nextRun.IsZero() && !nextRun.After(now) {
			due = append(due, g)
		}
	}

	sort.Slice(due, func(i, j int) bool {
		return due[i].Priority > due[j].Priority
	})
	return due
}

// Goals 返回所有目标的副本（供外部读取）。
func (s *Scheduler) Goals() []*Goal {
	s.mu.RLock()
	defer s.mu.RUnlock()

	out := make([]*Goal, len(s.goals))
	copy(out, s.goals)
	return out
}
