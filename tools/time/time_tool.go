package timetool

import (
	"context"
	"time"
)

type TimeTool struct{}

func (t *TimeTool) Name() string        { return "current_time" }
func (t *TimeTool) Description() string { return "获取当前时间" }
func (t *TimeTool) Schema() map[string]any {
	return map[string]any{"type": "object"}
}
func (t *TimeTool) Call(_ context.Context, _ map[string]any) (string, error) {
	return time.Now().Format(time.RFC3339), nil
}
