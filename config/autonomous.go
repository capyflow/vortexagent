package config

import (
	"fmt"
	"time"

	"github.com/capyflow/vortexagent/autonomous"
)

// AutonomousConfig 自治 agent 配置（vortex-serve 使用）。
type AutonomousConfig struct {
	Enabled       bool            `json:"enabled"`
	MaxSleepMin   int             `json:"max_sleep_minutes"`
	GoalStore     GoalStoreConfig `json:"goal_store"`
	Goals         []GoalConfig    `json:"goals"`
	WebhookSecret string          `json:"webhook_secret,omitempty"`
}

// GoalStoreConfig 自治目标存储配置。
type GoalStoreConfig struct {
	Type string `json:"type"`
	File string `json:"file"`
}

// GoalConfig 自治目标的静态配置项。
type GoalConfig struct {
	Title        string  `json:"title"`
	Description  string  `json:"description"`
	ScheduleType string  `json:"schedule_type"`
	Cron         string  `json:"cron,omitempty"`
	IntervalMin  float64 `json:"interval_minutes,omitempty"`
	DelayMin     float64 `json:"delay_minutes,omitempty"`
	Priority     int     `json:"priority"`
}

// GoalFromConfig 把配置文件中的目标描述转换成运行时 Goal。
func GoalFromConfig(c GoalConfig) (*autonomous.Goal, error) {
	if c.Title == "" {
		return nil, fmt.Errorf("缺少 title")
	}
	if c.ScheduleType == "" {
		return nil, fmt.Errorf("缺少 schedule_type")
	}

	g := autonomous.NewGoal()
	g.Title = c.Title
	g.Description = c.Description
	g.Status = autonomous.GoalStatusActive
	g.Priority = c.Priority
	if g.Priority <= 0 {
		g.Priority = 5
	}

	switch c.ScheduleType {
	case "oneshot":
		g.Schedule.Type = autonomous.ScheduleOneShot
		g.Schedule.Delay = time.Duration(c.DelayMin) * time.Minute
		if g.Schedule.Delay <= 0 {
			g.Schedule.Delay = time.Hour
		}
		g.NextRunAt = time.Now().Add(g.Schedule.Delay)
	case "interval":
		g.Schedule.Type = autonomous.ScheduleInterval
		g.Schedule.Interval = time.Duration(c.IntervalMin) * time.Minute
		if g.Schedule.Interval <= 0 {
			g.Schedule.Interval = time.Hour
		}
		g.NextRunAt = time.Now().Add(g.Schedule.Interval)
	case "cron":
		g.Schedule.Type = autonomous.ScheduleCron
		g.Schedule.Cron = c.Cron
		if g.Schedule.Cron == "" {
			g.Schedule.Cron = "0 8 * * *"
		}
		trigger := &autonomous.CronTrigger{Expr: g.Schedule.Cron}
		next, err := trigger.NextRun(time.Now())
		if err != nil {
			return nil, fmt.Errorf("无效的 cron 表达式 %q: %w", g.Schedule.Cron, err)
		}
		g.NextRunAt = next
	default:
		return nil, fmt.Errorf("未知的 schedule_type %q", c.ScheduleType)
	}

	return g, nil
}
