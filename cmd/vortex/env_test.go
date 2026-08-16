package main

import (
	"os"
	"path/filepath"
	"testing"
)

func writeEnv(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), ".env")
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

// TestLoadDotEnv_setsMissingKeys 验证新键写入环境、已有键不被覆盖。
func TestLoadDotEnv_setsMissingKeys(t *testing.T) {
	t.Setenv("ALREADY_SET", "old")
	path := writeEnv(t, "ALREADY_SET=new\nNEW_KEY=hello\n")

	if err := loadDotEnv(path); err != nil {
		t.Fatalf("loadDotEnv 返回错误: %v", err)
	}
	if got := os.Getenv("NEW_KEY"); got != "hello" {
		t.Errorf("NEW_KEY = %q, 期望 hello", got)
	}
	if got := os.Getenv("ALREADY_SET"); got != "old" {
		t.Errorf("ALREADY_SET 不应被覆盖, 实际 %q", got)
	}
}

// TestLoadDotEnv_skipsCommentsAndEmptyLines 验证注释行与空行被忽略。
func TestLoadDotEnv_skipsCommentsAndEmptyLines(t *testing.T) {
	path := writeEnv(t, "# 注释\n\n  \nKEY=value\n")
	if err := loadDotEnv(path); err != nil {
		t.Fatalf("loadDotEnv 返回错误: %v", err)
	}
	if got := os.Getenv("KEY"); got != "value" {
		t.Errorf("KEY = %q, 期望 value", got)
	}
}

// TestLoadDotEnv_missingFile 验证文件不存在时静默成功。
func TestLoadDotEnv_missingFile(t *testing.T) {
	if err := loadDotEnv(filepath.Join(t.TempDir(), "nope.env")); err != nil {
		t.Errorf("文件不存在应返回 nil, 实际 %v", err)
	}
}

// TestLoadDotEnv_malformedLine 验证非法行报错并带行号。
func TestLoadDotEnv_malformedLine(t *testing.T) {
	path := writeEnv(t, "GOOD=1\nBAD LINE\n")
	if err := loadDotEnv(path); err == nil {
		t.Error("非法行应返回错误")
	}
}
