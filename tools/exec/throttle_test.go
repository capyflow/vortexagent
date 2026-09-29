package exec

import (
	"context"
	"strings"
	"testing"
)

// TestTruncateWithMarker 截断带原始长度标注；max<0 不限制。
func TestTruncateWithMarker(t *testing.T) {
	long := strings.Repeat("字", 100)
	got := truncateWithMarker(long, 50)
	if !strings.HasPrefix(got, strings.Repeat("字", 50)) {
		t.Fatalf("截断内容错误")
	}
	if !strings.Contains(got, "完整输出共 100 字符") {
		t.Fatalf("缺少原始长度标注：%s", got)
	}
	if got := truncateWithMarker(long, -1); got != long {
		t.Fatal("负数上限应不限制")
	}
	if got := truncateWithMarker(long, 100); got != long {
		t.Fatal("恰好等于上限不应截断")
	}
}

// TestCall_SourceThrottling 超长命令输出进历史前被截断。
func TestCall_SourceThrottling(t *testing.T) {
	tool := NewExecTool(".", WithMaxOutput(200))
	out, err := tool.Call(context.Background(), map[string]any{
		"command": "printf 'a%.0s' $(seq 1 5000)",
	})
	if err != nil {
		// seq 在部分环境不可用；退而用循环。
		out, err = tool.Call(context.Background(), map[string]any{
			"command": "i=0; while [ $i -lt 5000 ]; do printf a; i=$((i+1)); done",
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	if !strings.Contains(out, "完整输出共 5000 字符") {
		t.Fatalf("输出未被节流：%d 字节", len(out))
	}
	if len([]rune(out)) > 300 {
		t.Fatalf("截断后仍过长：%d 字符", len([]rune(out)))
	}

	// 不限制模式。
	unlimited := NewExecTool(".", WithMaxOutput(-1))
	out2, err := unlimited.Call(context.Background(), map[string]any{
		"command": "i=0; while [ $i -lt 5000 ]; do printf a; i=$((i+1)); done",
	})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out2, "已截断") {
		t.Fatal("不限制模式不应截断")
	}
}
