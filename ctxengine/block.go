// Package ctxengine 是单会话上下文引擎：让活跃上下文保持有界，同时保证
// "需要时能查到"。
//
// 核心机制：活跃上下文 = 摘要区（有预算）+ 最近块原文 + 模型按需检索的片段。
// 原始历史不可变外部化归档（Archive），每轮响应结束后异步压缩（状态交接式
// 摘要），检索是模型主动的工具调用（纯被动，无自动预取）。
//
// 设计原则（详见 docs/context-design.md）：
//   - 原文先落库再动活跃区——摘要失败、进程崩溃都不会丢消息；
//   - 块（相邻用户消息之间的全部消息）是归档/检索/裁剪的原子单位，
//     assistant(tool_calls) 与其全部 tool 结果结构上永不拆散；
//   - 摘要失败不推进——宁可这轮不卸载，不生成假摘要；
//   - 检索结果必须带出处。
//
// 引擎不绑定 Session 指针：方法以会话为参，per-session 运行态按 sessionID
// 键控，同一实例可同时服务 REPL / serve 多会话 / subagent 等宿主。
package ctxengine

import (
	"strings"

	"github.com/capyflow/vortexagent/llm"
)

// Block 是一次完整的交互单元：相邻两条用户消息之间的全部消息。
//
// 该划分天然保证 assistant(tool_calls) 与其全部 tool 结果永不拆散——
// Ask 工具循环内不会出现用户消息，配对消息必然落在同一条用户消息之后。
// 归档、裁剪、检索全部以块为原子单位。
type Block struct {
	Messages []llm.Message
}

// SplitBlocks 把扁平历史划为块序列。空历史返回空切片。
// 块边界在每个 RoleUser 消息处开启；位于第一条用户消息之前的消息
// （正常不会出现）归入第一个块。
func SplitBlocks(history []llm.Message) []Block {
	if len(history) == 0 {
		return nil
	}
	blocks := make([]Block, 0, len(history)/2+1)
	cur := Block{Messages: make([]llm.Message, 0, 4)}
	started := false
	for _, m := range history {
		if m.Role == llm.RoleUser && started {
			blocks = append(blocks, cur)
			cur = Block{Messages: make([]llm.Message, 0, 4)}
		}
		cur.Messages = append(cur.Messages, m)
		started = true
	}
	if len(cur.Messages) > 0 {
		blocks = append(blocks, cur)
	}
	return blocks
}

// FlattenBlocks 把块序列拼回扁平消息列表。
func FlattenBlocks(blocks []Block) []llm.Message {
	n := 0
	for _, b := range blocks {
		n += len(b.Messages)
	}
	msgs := make([]llm.Message, 0, n)
	for _, b := range blocks {
		msgs = append(msgs, b.Messages...)
	}
	return msgs
}

// TokenCount 估算块内消息的 token 数（未含校准系数）。
func (b Block) TokenCount() int { return EstimateMessagesTokens(b.Messages) }

// FirstUserText 返回块内第一条用户消息的文本（无则空串）。
// 它是块的主题索引，参与归档检索评分。
func (b Block) FirstUserText() string {
	for _, m := range b.Messages {
		if m.Role != llm.RoleUser {
			continue
		}
		var sb strings.Builder
		for _, c := range m.Content {
			if c.Type == llm.ContentText {
				sb.WriteString(c.Text)
			}
		}
		return sb.String()
	}
	return ""
}
