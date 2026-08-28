// 本文件测试 filesystem 工具的路径安全：越界、绝对路径、前缀混淆、
// 符号链接逃逸都必须被拒绝，root 内的正常读写不受影响。
package filesystem

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func newTestRoot(t *testing.T) string {
	t.Helper()
	return t.TempDir()
}

func mustWrite(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestReadFile_InRoot(t *testing.T) {
	root := newTestRoot(t)
	mustWrite(t, filepath.Join(root, "docs", "a.md"), "# hello")

	tool := NewReadFileTool(root)
	for _, path := range []string{"docs/a.md", "./docs/a.md", filepath.Join(root, "docs/a.md")} {
		out, err := tool.Call(context.Background(), map[string]any{"path": path})
		if err != nil {
			t.Fatalf("读取 %s 失败: %v", path, err)
		}
		if out != "# hello" {
			t.Errorf("读取 %s 内容 = %q", path, out)
		}
	}
}

// TestReadFile_RejectsOutsidePaths 校验各类越界路径都被拒绝。
func TestReadFile_RejectsOutsidePaths(t *testing.T) {
	root := newTestRoot(t)
	secret := t.TempDir() // root 之外的真实文件
	mustWrite(t, filepath.Join(secret, "passwd"), "top-secret")

	tool := NewReadFileTool(root)
	for _, path := range []string{
		"/etc/passwd", // 绝对路径直达（旧实现直接放行）
		"../" + filepath.Base(secret) + "/passwd", // ../ 逃逸
		filepath.Join(secret, "passwd"),           // 绝对路径指向 root 外文件
	} {
		if _, err := tool.Call(context.Background(), map[string]any{"path": path}); err == nil {
			t.Errorf("路径 %s 应被拒绝", path)
		}
	}
}

// TestReadFile_RejectsPrefixConfusion 校验前缀混淆：root=/a/proj 时 /a/proj-evil 不可读。
func TestReadFile_RejectsPrefixConfusion(t *testing.T) {
	base := t.TempDir()
	root := filepath.Join(base, "proj")
	sibling := filepath.Join(base, "proj-evil")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	mustWrite(t, filepath.Join(sibling, "secret.txt"), "leak")

	tool := NewReadFileTool(root)
	if _, err := tool.Call(context.Background(), map[string]any{"path": filepath.Join(sibling, "secret.txt")}); err == nil {
		t.Error("同级前缀目录应被拒绝（HasPrefix 漏洞）")
	}
}

// TestReadFile_RejectsSymlinkEscape 校验符号链接逃逸：root 内的链接指向 root 外。
func TestReadFile_RejectsSymlinkEscape(t *testing.T) {
	root := newTestRoot(t)
	secret := t.TempDir()
	mustWrite(t, filepath.Join(secret, "passwd"), "top-secret")
	if err := os.Symlink(filepath.Join(secret, "passwd"), filepath.Join(root, "leak")); err != nil {
		t.Skip("无法创建符号链接:", err)
	}

	tool := NewReadFileTool(root)
	if _, err := tool.Call(context.Background(), map[string]any{"path": "leak"}); err == nil {
		t.Error("指向 root 外的符号链接应被拒绝")
	}
}

func TestWriteFile_InRootAndRejectsEscape(t *testing.T) {
	root := newTestRoot(t)
	tool := NewWriteFileTool(root)

	out, err := tool.Call(context.Background(), map[string]any{"path": "sub/new.txt", "content": "ok"})
	if err != nil || !strings.Contains(out, "new.txt") {
		t.Fatalf("root 内写入失败: %v, %q", err, out)
	}
	data, err := os.ReadFile(filepath.Join(root, "sub", "new.txt"))
	if err != nil || string(data) != "ok" {
		t.Fatalf("写入内容不符: %v, %q", err, data)
	}

	outside := t.TempDir()
	for _, path := range []string{
		filepath.Join(outside, "evil.txt"),
		"../" + filepath.Base(outside) + "/evil.txt",
	} {
		if _, err := tool.Call(context.Background(), map[string]any{"path": path, "content": "x"}); err == nil {
			t.Errorf("写入 %s 应被拒绝", path)
		}
	}
}

func TestEditFile_ReplaceAndAmbiguity(t *testing.T) {
	root := newTestRoot(t)
	mustWrite(t, filepath.Join(root, "a.txt"), "hello world")
	tool := NewEditFileTool(root)

	if _, err := tool.Call(context.Background(), map[string]any{
		"path": "a.txt", "old_string": "world", "new_string": "Go",
	}); err != nil {
		t.Fatalf("编辑失败: %v", err)
	}
	data, _ := os.ReadFile(filepath.Join(root, "a.txt"))
	if string(data) != "hello Go" {
		t.Errorf("编辑结果 = %q", data)
	}

	mustWrite(t, filepath.Join(root, "b.txt"), "x x x")
	if _, err := tool.Call(context.Background(), map[string]any{
		"path": "b.txt", "old_string": "x", "new_string": "y",
	}); err == nil {
		t.Error("多处匹配应报错")
	}
}

func TestListFiles(t *testing.T) {
	root := newTestRoot(t)
	mustWrite(t, filepath.Join(root, "a.txt"), "a")
	if err := os.MkdirAll(filepath.Join(root, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}

	tool := NewListFilesTool(root)
	out, err := tool.Call(context.Background(), nil)
	if err != nil {
		t.Fatalf("列目录失败: %v", err)
	}
	if !strings.Contains(out, "a.txt") || !strings.Contains(out, "sub/") {
		t.Errorf("列目录结果 = %q", out)
	}
}
