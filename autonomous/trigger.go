package autonomous

import (
	"fmt"
	"time"

	"github.com/robfig/cron/v3"
)

// Trigger 是调度触发器接口，根据规则计算下次执行时间。
type Trigger interface {
	// NextRun 返回下次执行时间。
	NextRun(lastRun time.Time) (time.Time, error)
	// Type 返回触发器类型。
	Type() ScheduleType
}

// ─── 具体实现 ───

// OneShotTrigger：一次性延迟执行。
type OneShotTrigger struct {
	Delay time.Duration
}

func (t *OneShotTrigger) NextRun(now time.Time) (time.Time, error) {
	return now.Add(t.Delay), nil
}

func (t *OneShotTrigger) Type() ScheduleType { return ScheduleOneShot }

// IntervalTrigger：固定间隔执行。
type IntervalTrigger struct {
	Interval time.Duration
}

func (t *IntervalTrigger) NextRun(lastRun time.Time) (time.Time, error) {
	return lastRun.Add(t.Interval), nil
}

func (t *IntervalTrigger) Type() ScheduleType { return ScheduleInterval }

// CronTrigger：cron 表达式定时执行。
type CronTrigger struct {
	Expr string
}

func (t *CronTrigger) NextRun(lastRun time.Time) (time.Time, error) {
	sched, err := cron.ParseStandard(t.Expr)
	if err != nil {
		return time.Time{}, fmt.Errorf("解析 cron 表达式 %q 失败: %w", t.Expr, err)
	}
	return sched.Next(lastRun), nil
}

func (t *CronTrigger) Type() ScheduleType { return ScheduleCron }

// EventTrigger：事件驱动，不主动计算下次时间。
type EventTrigger struct {
	EventType string
}

func (t *EventTrigger) NextRun(lastRun time.Time) (time.Time, error) {
	return time.Time{}, nil
}

func (t *EventTrigger) Type() ScheduleType { return ScheduleEvent }

// newTrigger 根据 Schedule 创建对应的触发器。
func newTrigger(s Schedule) Trigger {
	switch s.Type {
	case ScheduleOneShot:
		return &OneShotTrigger{Delay: s.Delay}
	case ScheduleInterval:
		return &IntervalTrigger{Interval: s.Interval}
	case ScheduleCron:
		return &CronTrigger{Expr: s.Cron}
	case ScheduleEvent:
		return &EventTrigger{EventType: s.Event}
	default:
		return &OneShotTrigger{}
	}
}
