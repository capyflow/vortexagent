// 本文件测试 token 估算启发式：CJK 按字计、拉丁按字符/4 计，
// 以及中文文本不再被显著低估。
package llm

import (
	"strings"
	"testing"
)

func TestEstimateTokenCount(t *testing.T) {
	cases := []struct {
		name string
		text string
		want int
	}{
		{"空文本", "", 0},
		{"纯中文", "你好世界", 4},                   // 1 字 1 token
		{"中文标点", "你好，世界！", 6},                // 标点也是 1 token
		{"纯英文", "hello world", 2},            // 11 字符 / 4
		{"混合", "用 Go 写的 agent 框架", 5 + 11/4}, // 5 CJK（用写的框架）+ 11 其他字符
	}
	for _, c := range cases {
		if got := EstimateTokenCount(c.text); got != c.want {
			t.Errorf("%s: EstimateTokenCount(%q) = %d, 期望 %d", c.name, c.text, got, c.want)
		}
	}
}

// TestEstimateTokenCount_ChineseNotUnderestimated 校验修复目标：
// 中文文本的估算必须显著高于旧的 rune/3 算法（旧算法 60 字只估 20）。
func TestEstimateTokenCount_ChineseNotUnderestimated(t *testing.T) {
	long := strings.Repeat("上下文长度估算", 12) // 84 个 CJK 字符
	if got := EstimateTokenCount(long); got != 84 {
		t.Errorf("中文估算 = %d, 期望 84（1 字 1 token）", got)
	}
}
