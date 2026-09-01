package autonomous

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/capyflow/vortexagent/agent"
	"github.com/google/uuid"
)

// ─── 自治工具 ───

// newGoalID 生成唯一目标 ID。
// 使用随机 UUID 而非进程内序列号：序列号在重启后从 1 重计，
// 会与存储中的旧目标撞 ID，导致 Save 静默覆盖旧目标。
func newGoalID() string {
	return "goal-" + uuid.NewString()[:8]
}

// asString 安全地将 any 转为 string。
func asString(v any) string {
	if s, ok := v.(string); ok {
		return s
	}
	return ""
}

// asFloat 安全地将 any 转为 float64。
func asFloat(v any) float64 {
	if f, ok := v.(float64); ok {
		return f
	}
	return 0
}

// ─── add_goal 工具 ───

// AddGoalTool 允许用户通过对话添加长期目标。
type AddGoalTool struct {
	store    GoalStore
	registry *agent.Registry
	onAdd    func(*Goal) // 添加成功后通知调度器
}

// NewAddGoalTool 创建 add_goal 工具。
func NewAddGoalTool(store GoalStore, registry *agent.Registry, onAdd func(*Goal)) *AddGoalTool {
	return &AddGoalTool{store: store, registry: registry, onAdd: onAdd}
}

func (t *AddGoalTool) Name() string { return "add_goal" }

func (t *AddGoalTool) Description() string {
	return "添加一个长期目标，agent 会自动调度执行。支持一次性延迟、周期性间隔、cron 定时、事件驱动四种调度方式。"
}

func (t *AddGoalTool) Schema() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"title": map[string]any{
				"type":        "string",
				"description": "目标标题，如\"每天早上查天气\"",
			},
			"description": map[string]any{
				"type":        "string",
				"description": "目标详细描述，说明具体要做什么",
			},
			"schedule_type": map[string]any{
				"type":        "string",
				"enum":        []string{"oneshot", "interval", "cron", "event"},
				"description": "调度类型：oneshot=一次性延迟, interval=周期性间隔, cron=cron定时, event=事件驱动",
			},
			"delay_minutes": map[string]any{
				"type":        "number",
				"description": "延迟分钟数（oneshot 类型必填）",
			},
			"interval_minutes": map[string]any{
				"type":        "number",
				"description": "间隔分钟数（interval 类型必填）",
			},
			"cron": map[string]any{
				"type":        "string",
				"description": "cron 表达式（cron 类型必填），如 \"0 8 * * *\" 表示每天 8 点",
			},
			"event_type": map[string]any{
				"type":        "string",
				"description": "事件类型（event 类型必填），如 \"github.push\"",
			},
			"priority": map[string]any{
				"type":        "number",
				"description": "优先级（1-10，默认 5），越高越先执行",
			},
		},
		"required": []string{"title", "description", "schedule_type"},
	}
}

func (t *AddGoalTool) Call(ctx context.Context, args map[string]any) (string, error) {
	goal := NewGoal()
	goal.ID = newGoalID()
	goal.Title = strings.TrimSpace(asString(args["title"]))
	goal.Description = strings.TrimSpace(asString(args["description"]))

	if goal.Title == "" {
		return "", fmt.Errorf("title 不能为空")
	}

	goal.Status = GoalStatusActive
	goal.CreatedAt = time.Now()
	goal.Priority = int(asFloat(args["priority"]))
	if goal.Priority <= 0 {
		goal.Priority = 5
	}

	scheduleType := asString(args["schedule_type"])
	switch scheduleType {
	case "oneshot":
		goal.Schedule.Type = ScheduleOneShot
		minutes := asFloat(args["delay_minutes"])
		if minutes <= 0 {
			return "", fmt.Errorf("oneshot 类型必须指定 delay_minutes > 0")
		}
		goal.Schedule.Delay = time.Duration(minutes) * time.Minute
		goal.NextRunAt = time.Now().Add(goal.Schedule.Delay)
	case "interval":
		goal.Schedule.Type = ScheduleInterval
		minutes := asFloat(args["interval_minutes"])
		if minutes <= 0 {
			return "", fmt.Errorf("interval 类型必须指定 interval_minutes > 0")
		}
		goal.Schedule.Interval = time.Duration(minutes) * time.Minute
		goal.NextRunAt = time.Now().Add(goal.Schedule.Interval)
	case "cron":
		goal.Schedule.Type = ScheduleCron
		goal.Schedule.Cron = asString(args["cron"])
		if goal.Schedule.Cron == "" {
			return "", fmt.Errorf("cron 类型必须指定 cron 表达式")
		}
		// 计算下次执行时间
		trigger := &CronTrigger{Expr: goal.Schedule.Cron}
		next, err := trigger.NextRun(time.Now())
		if err != nil {
			return "", fmt.Errorf("cron 表达式无效: %w", err)
		}
		goal.NextRunAt = next
	case "event":
		goal.Schedule.Type = ScheduleEvent
		goal.Schedule.Event = asString(args["event_type"])
		if goal.Schedule.Event == "" {
			return "", fmt.Errorf("event 类型必须指定 event_type")
		}
		// 事件驱动不计算下次时间
		goal.NextRunAt = time.Time{}
	default:
		return "", fmt.Errorf("未知的 schedule_type: %q（支持 oneshot / interval / cron / event）", scheduleType)
	}

	if err := t.store.Save(goal); err != nil {
		return "", fmt.Errorf("保存目标失败: %w", err)
	}

	// 通知调度器
	if t.onAdd != nil {
		t.onAdd(goal)
	}

	return fmt.Sprintf("目标已添加（ID: %s），下次执行: %s",
		goal.ID, formatNextRunAt(goal.NextRunAt)), nil
}

// ─── list_goals 工具 ───

// ListGoalsTool 允许用户查看所有长期目标。
type ListGoalsTool struct {
	store GoalStore
}

// NewListGoalsTool 创建 list_goals 工具。
func NewListGoalsTool(store GoalStore) *ListGoalsTool {
	return &ListGoalsTool{store: store}
}

func (t *ListGoalsTool) Name() string { return "list_goals" }

func (t *ListGoalsTool) Description() string {
	return "列出所有长期目标，显示状态、下次执行时间等信息"
}

func (t *ListGoalsTool) Schema() map[string]any {
	return map[string]any{
		"type":       "object",
		"properties": map[string]any{},
	}
}

func (t *ListGoalsTool) Call(ctx context.Context, args map[string]any) (string, error) {
	goals, err := t.store.LoadAll()
	if err != nil {
		return "", fmt.Errorf("加载目标失败: %w", err)
	}

	if len(goals) == 0 {
		return "当前没有长期目标。使用 add_goal 工具添加一个吧。", nil
	}

	var sb strings.Builder
	fmt.Fprintf(&sb, "共 %d 个长期目标:\n\n", len(goals))
	for _, g := range goals {
		g.mu.RLock()
		fmt.Fprintf(&sb, "• [%s] %s\n", g.Status, g.Title)
		fmt.Fprintf(&sb, "  ID: %s\n", g.ID)
		fmt.Fprintf(&sb, "  下次执行: %s\n", formatNextRunAt(g.NextRunAt))
		fmt.Fprintf(&sb, "  执行次数: %d (失败 %d 次)\n", g.RunCount, g.FailCount)
		if g.LastResult != "" {
			// 截断过长的结果：按字符截断，避免切断多字节 UTF-8 字符产生乱码
			runes := []rune(g.LastResult)
			result := string(runes)
			if len(runes) > 100 {
				result = string(runes[:100]) + "..."
			}
			fmt.Fprintf(&sb, "  上次结果: %s\n", result)
		}
		g.mu.RUnlock()
		sb.WriteString("\n")
	}
	return sb.String(), nil
}

// ─── remove_goal 工具 ───

// RemoveGoalTool 允许用户移除长期目标。
type RemoveGoalTool struct {
	store    GoalStore
	registry *agent.Registry
	onRemove func(string) // 移除成功后通知调度器
}

// NewRemoveGoalTool 创建 remove_goal 工具。
func NewRemoveGoalTool(store GoalStore, registry *agent.Registry, onRemove func(string)) *RemoveGoalTool {
	return &RemoveGoalTool{store: store, registry: registry, onRemove: onRemove}
}

func (t *RemoveGoalTool) Name() string { return "remove_goal" }

func (t *RemoveGoalTool) Description() string {
	return "移除一个长期目标，移除后不再自动执行"
}

func (t *RemoveGoalTool) Schema() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"goal_id": map[string]any{
				"type":        "string",
				"description": "要移除的目标 ID（通过 list_goals 获取）",
			},
		},
		"required": []string{"goal_id"},
	}
}

func (t *RemoveGoalTool) Call(ctx context.Context, args map[string]any) (string, error) {
	id := strings.TrimSpace(asString(args["goal_id"]))
	if id == "" {
		return "", fmt.Errorf("goal_id 不能为空")
	}

	// 先检查是否存在
	if _, err := t.store.Get(id); err != nil {
		return "", fmt.Errorf("目标 %q 不存在", id)
	}

	if err := t.store.Delete(id); err != nil {
		return "", fmt.Errorf("删除目标失败: %w", err)
	}

	// 通知调度器
	if t.onRemove != nil {
		t.onRemove(id)
	}

	return fmt.Sprintf("目标 %q 已移除", id), nil
}

// ─── 辅助函数 ───

// formatNextRunAt 格式化下次执行时间的显示。
func formatNextRunAt(t time.Time) string {
	if t.IsZero() {
		return "等待事件触发"
	}
	now := time.Now()
	if t.Before(now) {
		return "已到期"
	}
	d := time.Until(t)
	if d < time.Minute {
		return "即将执行"
	}
	if d < time.Hour {
		return fmt.Sprintf("%d 分钟后", int(d.Minutes()))
	}
	if d < 24*time.Hour {
		return fmt.Sprintf("%d 小时 %d 分钟后", int(d.Hours()), int(d.Minutes())%60)
	}
	return t.Format("2006-01-02 15:04")
}
