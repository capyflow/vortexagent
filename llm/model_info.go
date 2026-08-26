package llm

var modelContextWindows = map[string]int{
	"gpt-4":                8192,
	"gpt-4-turbo":          128000,
	"gpt-4o":               128000,
	"gpt-4o-mini":          128000,
	"gpt-3.5-turbo":        16385,
	"claude-3-opus":        200000,
	"claude-3-sonnet":      200000,
	"claude-3-haiku":       200000,
	"claude-3.5-sonnet":    200000,
	"gemini-1.5-pro":       1048576,
	"gemini-1.5-flash":     1048576,
	"gemini-2.0-flash":     1048576,
	"deepseek-chat":        65536,
	"deepseek-coder":       65536,
	"qwen-max":             32768,
	"qwen-plus":            131072,
	"qwen-turbo":           131072,
	"step-3.7-flash":       128000,
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

func EstimateTokenCount(text string) int {
	if len(text) == 0 {
		return 0
	}
	runeCount := 0
	for range text {
		runeCount++
	}
	return runeCount / 3
}
