package filesystem

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestReadFile_SourceThrottling 大文件读取进历史前被截断并标注原始长度。
func TestReadFile_SourceThrottling(t *testing.T) {
	root := t.TempDir()
	big := filepath.Join(root, "big.txt")
	content := strings.Repeat("内容", 5000) // 10000 字符
	if err := os.WriteFile(big, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}

	tool := NewReadFileTool(root, WithMaxContent(100))
	out, err := tool.Call(context.Background(), map[string]any{"path": "big.txt"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "完整内容共 10000 字符") {
		t.Fatalf("缺少截断标注：%s", out)
	}
	if len([]rune(out)) > 200 {
		t.Fatalf("截断后仍过长：%d 字符", len([]rune(out)))
	}

	// 不限制模式。
	unlimited := NewReadFileTool(root, WithMaxContent(-1))
	out2, err := unlimited.Call(context.Background(), map[string]any{"path": "big.txt"})
	if err != nil {
		t.Fatal(err)
	}
	if len([]rune(out2)) != 10000 {
		t.Fatalf("不限制模式应返回全文：%d", len([]rune(out2)))
	}
}
