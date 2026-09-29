package ctxengine

import (
	"encoding/json"

	"github.com/capyflow/vortexagent/llm"
)

// 默认参数（config.Context 未配置对应项时生效）。
const (
	DefaultSoftRatio        = 0.7  // 使用率软阈值：超过即触发归档
	DefaultHardRatio        = 0.95 // 硬阈值：估算+maxTokens 超过则发送前强制裁剪
	DefaultKeepRecentBlocks = 12   // 归档时保留的最近块数
	DefaultMaxBlocks        = 24   // 活跃块数上限（超过即触发归档）
	DefaultSummaryBudget    = 0.10 // 摘要区预算（占窗口比例）
	DefaultRetrievalLimit   = 5    // search_history 默认返回条数
	DefaultRetentionDays    = 0    // 归档保留天数，0 = 永久
	DefaultMaxOutputChars   = 8000 // 工具输出进历史前的默认截断上限（字符）
	// MinKeepBlocks 组装/裁剪时始终保留的最少块数（含当前轮的块，
	// 防止预算极端紧张时把正在处理的对话裁没）。
	MinKeepBlocks = 1
)

// EstimateMessagesTokens 估算一组消息的 token 数（未含校准系数）：
// 文本内容走 CJK 感知的 llm.EstimateTokenCount；工具调用参数按 JSON
// 序列化后估算。图片内容不估算（其 token 计费方式与文本差异过大，
// 且归档场景罕见）。
func EstimateMessagesTokens(msgs []llm.Message) int {
	total := 0
	for _, m := range msgs {
		for _, c := range m.Content {
			switch c.Type {
			case llm.ContentText:
				total += llm.EstimateTokenCount(c.Text)
			case llm.ContentThinking:
				total += llm.EstimateTokenCount(c.Thinking)
			}
		}
		for _, tc := range m.ToolCalls {
			b, err := json.Marshal(tc.Arguments)
			if err != nil {
				continue
			}
			total += llm.EstimateTokenCount(string(b))
		}
	}
	return total
}

// Budget 是一轮组装的 token 预算。窗口取 min(配置窗口, provider 报告窗口)。
type Budget struct {
	WindowTokens     int
	SoftRatio        float64
	HardRatio        float64
	SummaryBudget    float64
	KeepRecentBlocks int
	MaxBlocks        int
}

// NewBudget 填充零值字段为默认值；window<=0 时按 128k 处理。
func NewBudget(window int, soft, hard, summaryBudget float64, keep, maxBlocks int) Budget {
	if window <= 0 {
		window = 128000
	}
	if soft <= 0 || soft >= 1 {
		soft = DefaultSoftRatio
	}
	if hard <= soft || hard > 1 {
		hard = DefaultHardRatio
	}
	if summaryBudget <= 0 || summaryBudget >= 1 {
		summaryBudget = DefaultSummaryBudget
	}
	if keep <= 0 {
		keep = DefaultKeepRecentBlocks
	}
	if maxBlocks <= keep {
		maxBlocks = keep * 2
	}
	return Budget{
		WindowTokens:     window,
		SoftRatio:        soft,
		HardRatio:        hard,
		SummaryBudget:    summaryBudget,
		KeepRecentBlocks: keep,
		MaxBlocks:        maxBlocks,
	}
}

// SummaryTokens 摘要区的 token 预算。
func (b Budget) SummaryTokens() int { return int(float64(b.WindowTokens) * b.SummaryBudget) }

// SoftLimitTokens 归档触发阈值对应的 token 数。
func (b Budget) SoftLimitTokens() int { return int(float64(b.WindowTokens) * b.SoftRatio) }

// HardLimitTokens 发送前硬校验的 token 数上限。
func (b Budget) HardLimitTokens() int { return int(float64(b.WindowTokens) * b.HardRatio) }
