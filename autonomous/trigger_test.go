package autonomous

import (
	"testing"
	"time"
)

func TestOneShotTrigger(t *testing.T) {
	now := time.Now()
	trigger := &OneShotTrigger{Delay: 30 * time.Minute}

	next, err := trigger.NextRun(now)
	if err != nil {
		t.Fatalf("OneShotTrigger.NextRun 不应报错: %v", err)
	}

	expected := now.Add(30 * time.Minute)
	if !next.Equal(expected) {
		t.Errorf("下次执行时间应为 %v, got %v", expected, next)
	}

	if trigger.Type() != ScheduleOneShot {
		t.Errorf("类型应为 oneshot, got %q", trigger.Type())
	}
}

func TestIntervalTrigger(t *testing.T) {
	lastRun := time.Now().Add(-1 * time.Hour)
	trigger := &IntervalTrigger{Interval: 30 * time.Minute}

	next, err := trigger.NextRun(lastRun)
	if err != nil {
		t.Fatalf("IntervalTrigger.NextRun 不应报错: %v", err)
	}

	expected := lastRun.Add(30 * time.Minute)
	if !next.Equal(expected) {
		t.Errorf("下次执行时间应为 %v, got %v", expected, next)
	}

	if trigger.Type() != ScheduleInterval {
		t.Errorf("类型应为 interval, got %q", trigger.Type())
	}
}

func TestCronTrigger(t *testing.T) {
	trigger := &CronTrigger{Expr: "0 8 * * *"} // 每天早上 8 点

	lastRun := time.Date(2026, 9, 1, 7, 0, 0, 0, time.UTC)
	next, err := trigger.NextRun(lastRun)
	if err != nil {
		t.Fatalf("CronTrigger.NextRun 不应报错: %v", err)
	}

	// 应在 2026-09-01 08:00 UTC
	expected := time.Date(2026, 9, 1, 8, 0, 0, 0, time.UTC)
	if !next.Equal(expected) {
		t.Errorf("下次执行时间应为 %v, got %v", expected, next)
	}

	if trigger.Type() != ScheduleCron {
		t.Errorf("类型应为 cron, got %q", trigger.Type())
	}
}

func TestCronTriggerInvalid(t *testing.T) {
	trigger := &CronTrigger{Expr: "invalid cron"}
	_, err := trigger.NextRun(time.Now())
	if err == nil {
		t.Fatal("无效 cron 表达式应返回错误")
	}
}

func TestEventTrigger(t *testing.T) {
	trigger := &EventTrigger{EventType: "github.push"}

	next, err := trigger.NextRun(time.Now())
	if err != nil {
		t.Fatalf("EventTrigger.NextRun 不应报错: %v", err)
	}

	if !next.IsZero() {
		t.Errorf("EventTrigger 下次执行时间应为零值, got %v", next)
	}

	if trigger.Type() != ScheduleEvent {
		t.Errorf("类型应为 event, got %q", trigger.Type())
	}
}

func TestNewTrigger(t *testing.T) {
	tests := []struct {
		name     string
		schedule Schedule
		expected ScheduleType
	}{
		{
			name:     "oneshot",
			schedule: Schedule{Type: ScheduleOneShot, Delay: time.Hour},
			expected: ScheduleOneShot,
		},
		{
			name:     "interval",
			schedule: Schedule{Type: ScheduleInterval, Interval: time.Hour},
			expected: ScheduleInterval,
		},
		{
			name:     "cron",
			schedule: Schedule{Type: ScheduleCron, Cron: "0 8 * * *"},
			expected: ScheduleCron,
		},
		{
			name:     "event",
			schedule: Schedule{Type: ScheduleEvent, Event: "push"},
			expected: ScheduleEvent,
		},
		{
			name:     "unknown defaults to oneshot",
			schedule: Schedule{Type: ScheduleType("unknown")},
			expected: ScheduleOneShot,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			trigger := newTrigger(tt.schedule)
			if trigger.Type() != tt.expected {
				t.Errorf("应为 %q, got %q", tt.expected, trigger.Type())
			}
		})
	}
}
