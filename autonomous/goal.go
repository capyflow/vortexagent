// Package autonomous 提供 Vortex 的自治 agent 能力：
// 目标管理、调度、自主唤醒与执行。
//
// 本文件定义 Goal（目标）结构体、GoalStore（目标存储）接口，
// 以及 Goal 的并发安全访问器。
package autonomous

import (
	"sync"
	"time"
)

// GoalStatus 是目标的生命周期状态。
type GoalStatus string

const (
	GoalStatusPending  GoalStatus = "pending"  // 等待执行
	GoalStatusActive   GoalStatus = "active"   // 活跃（定期执行）
	GoalStatusDone     GoalStatus = "done"     // 已完成（一次性任务）
	GoalStatusFailed   GoalStatus = "failed"   // 失败超限
	GoalStatusCanceled GoalStatus = "canceled" // 被用户取消
)

// ScheduleType 是调度类型。
type ScheduleType string

const (
	ScheduleOneShot  ScheduleType = "oneshot"  // 一次性：延迟执行
	ScheduleInterval ScheduleType = "interval" // 周期性：间隔执行
	ScheduleCron     ScheduleType = "cron"     // 定时：cron 表达式
	ScheduleEvent    ScheduleType = "event"    // 事件驱动
)

// Schedule 描述目标的调度规则。
type Schedule struct {
	Type     ScheduleType  // 调度类型
	Delay    time.Duration // 一次性延迟（30 分钟后）
	Interval time.Duration // 周期间隔（每 30 分钟）
	Cron     string        // cron 表达式（"0 8 * * *"）
	Event    string        // 事件类型（"github.push"）
	MaxRuns  int           // 最大执行次数（0=无限）
}

// Goal 是一个自治 agent 的长期目标。
//
// Goal 的并发安全由内嵌的 *sync.RWMutex 保证：
// Scheduler 通过 RLock 读取 NextRunAt/Status，
// Run goroutine 通过 Lock 写入执行结果。
type Goal struct {
	mu          *sync.RWMutex `json:"-"` // 保护可变字段，指针避免 JSON 序列化问题
	ID          string        // 唯一 ID
	Title       string        // 标题（"每天早上查天气"）
	Description string        // 详细描述
	Status      GoalStatus    // 当前状态
	Priority    int           // 优先级（越高越先执行）
	Schedule    Schedule      // 调度规则
	CreatedAt   time.Time
	UpdatedAt   time.Time
	NextRunAt   time.Time // 下次执行时间（调度器计算）
	LastRunAt   time.Time // 上次执行时间
	RunCount    int       // 已执行次数
	FailCount   int       // 连续失败次数
	LastResult  string    // 上次执行结果摘要
	LastReflect string    // 上次反思结论
}

// NewGoal 创建 Goal 并初始化 mutex。
func NewGoal() *Goal {
	return &Goal{
		mu: &sync.RWMutex{},
	}
}

// GetNextRunAt 并发安全地读取下次执行时间。
func (g *Goal) GetNextRunAt() time.Time {
	g.mu.RLock()
	defer g.mu.RUnlock()
	return g.NextRunAt
}

// SetNextRunAt 并发安全地设置下次执行时间。
func (g *Goal) SetNextRunAt(t time.Time) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.NextRunAt = t
}

// GetStatus 并发安全地读取状态。
func (g *Goal) GetStatus() GoalStatus {
	g.mu.RLock()
	defer g.mu.RUnlock()
	return g.Status
}

// SetStatus 并发安全地设置状态。
func (g *Goal) SetStatus(s GoalStatus) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.Status = s
}

// Snapshot 返回目标的值拷贝；调用方可安全读取任意字段而无需再加锁。
// 拷贝中的 mu 指针与原目标共享，仅用于包内序列化，快照使用方不应触碰。
func (g *Goal) Snapshot() Goal {
	g.mu.RLock()
	defer g.mu.RUnlock()
	return *g
}

// GoalStore 是目标存储的接口。
type GoalStore interface {
	// LoadAll 加载所有目标。
	LoadAll() ([]*Goal, error)
	// Save 保存或更新单个目标。
	Save(goal *Goal) error
	// Delete 删除目标。
	Delete(id string) error
	// Get 按 ID 获取目标。
	Get(id string) (*Goal, error)
}
