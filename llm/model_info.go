package llm

var modelContextWindows = map[string]int{
	"gpt-4":             8192,
	"gpt-4-turbo":       128000,
	"gpt-4o":            128000,
	"gpt-4o-mini":       128000,
	"gpt-3.5-turbo":     16385,
	"claude-3-opus":     200000,
	"claude-3-sonnet":   200000,
	"claude-3-haiku":    200000,
	"claude-3.5-sonnet": 200000,
	"gemini-1.5-pro":    1048576,
	"gemini-1.5-flash":  1048576,
	"gemini-2.0-flash":  1048576,
	"deepseek-chat":     65536,
	"deepseek-coder":    65536,
	"qwen-max":          32768,
	"qwen-plus":         131072,
	"qwen-turbo":        131072,
	"step-3.7-flash":    128000,
}

var defaultContextWindow = 128000

func GetContextWindow(model string) int {
	if model == "" {
		return defaultContextWindow
	}
	if ctx, ok := modelContextWindows[model]; ok {
		return ctx
	}
	return defaultContextWindow
}

// EstimateTokenCount 估算文本的 token 数（无法精确分词时的启发式）：
//   - CJK 字符（汉字/假名/谚文/全角标点）在主流分词器下约 1 字 1 token；
//   - 其他文本（拉丁字母等）按约 4 字符 1 token 估算。
//
// 之前统一按 rune/3 估算，对中文低估 2-3 倍，导致依赖它的上下文卸载
// （ContextManager.ShouldOffload）触发过晚、真实请求超窗。
func EstimateTokenCount(text string) int {
	cjk, other := 0, 0
	for _, r := range text {
		if isCJK(r) {
			cjk++
		} else {
			other++
		}
	}
	return cjk + other/4
}

func isCJK(r rune) bool {
	switch {
	case r >= 0x4E00 && r <= 0x9FFF, // CJK 统一表意文字
		r >= 0x3400 && r <= 0x4DBF, // 扩展 A
		r >= 0x3000 && r <= 0x303F, // CJK 标点（，。「」等）
		r >= 0x3040 && r <= 0x30FF, // 日文假名
		r >= 0xAC00 && r <= 0xD7AF, // 韩文音节
		r >= 0xFF00 && r <= 0xFFEF: // 全角符号（！？ＡＢ等）
		return true
	}
	return false
}
