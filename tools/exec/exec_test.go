// 本文件测试 exec 工具：正常执行、stderr 汇总、超时、
// 超时上限封顶与 UTF-8 安全截断。
package exec

import (
	"context"
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

func TestExec_Basic(t *testing.T) {
	tool := NewExecTool(".")
	out, err := tool.Call(context.Background(), map[string]any{"command": "echo hello-vortex"})
	if err != nil {
		t.Fatalf("执行失败: %v", err)
	}
	if !strings.Contains(out, "hello-vortex") {
		t.Errorf("输出 = %q", out)
	}
}

func TestExec_Timeout(t *testing.T) {
	tool := NewExecTool(".")
	start := time.Now()
	_, err := tool.Call(context.Background(), map[string]any{
		"command": "sleep 5",
		"timeout": float64(0.5),
	})
	if err == nil {
		t.Fatal("超时命令应报错")
	}
	if !strings.Contains(err.Error(), "超时") {
		t.Errorf("错误信息 = %v, 应包含超时说明", err)
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Errorf("应在 0.5s 左右超时，实际 %v", elapsed)
	}
}

// TestExec_TruncateUTF8Safe 校验截断落在 rune 边界上（中文不被切成乱码）。
func TestExec_TruncateUTF8Safe(t *testing.T) {
	tool := NewExecTool(".")
	// 约 100KB 的中文输出，100KB 上限大概率落在多字节字符中间
	out, err := tool.Call(context.Background(), map[string]any{
		"command": "yes '你好世界，测试输出截断' | tr -d '\\n' | head -c 200000",
	})
	if err != nil {
		t.Fatalf("执行失败: %v", err)
	}
	if !utf8.ValidString(out) {
		t.Error("截断后的输出应是合法 UTF-8")
	}
	if !strings.Contains(out, "输出已截断") {
		t.Errorf("输出应包含截断标记: len=%d", len(out))
	}
	if len(out) > MaxOutputSize+utf8.UTFMax+len("\n... (输出已截断)") {
		t.Errorf("截断后长度 %d 超出上限附近", len(out))
	}
}

func TestExec_TruncateUTF8Unit(t *testing.T) {
	// 在多字节字符中间截断：应回退到完整 rune
	s := strings.Repeat("汉", 10) // 30 字节
	got := truncateUTF8(s, 28)
	if !utf8.ValidString(strings.TrimSuffix(got, "\n... (输出已截断)")) {
		t.Errorf("截断结果不是合法 UTF-8: %q", got)
	}
	if !strings.HasSuffix(got, "(输出已截断)") {
		t.Errorf("截断结果应带标记: %q", got)
	}
	if len(s) <= 28 {
		t.Fatal("测试用例应超过上限")
	}
}
