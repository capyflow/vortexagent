package config

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

	if err := LoadDotEnv(path); err != nil {
		t.Fatalf("LoadDotEnv 返回错误: %v", err)
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
	if err := LoadDotEnv(path); err != nil {
		t.Fatalf("LoadDotEnv 返回错误: %v", err)
	}
	if got := os.Getenv("KEY"); got != "value" {
		t.Errorf("KEY = %q, 期望 value", got)
	}
}

// TestLoadDotEnv_missingFile 验证文件不存在时静默成功。
func TestLoadDotEnv_missingFile(t *testing.T) {
	if err := LoadDotEnv(filepath.Join(t.TempDir(), "nope.env")); err != nil {
		t.Errorf("文件不存在应返回 nil, 实际 %v", err)
	}
}

// TestLoadDotEnv_malformedLine 验证非法行报错并带行号。
func TestLoadDotEnv_malformedLine(t *testing.T) {
	path := writeEnv(t, "GOOD=1\nBAD LINE\n")
	if err := LoadDotEnv(path); err == nil {
		t.Error("非法行应返回错误")
	}
}

// TestLoadConfig_missingFile 验证配置文件不存在时返回默认配置。
func TestLoadConfig_missingFile(t *testing.T) {
	cfg, err := LoadConfig(filepath.Join(t.TempDir(), "nope.json"))
	if err != nil {
		t.Fatalf("LoadConfig 返回错误: %v", err)
	}
	if cfg.Provider.Name != "openai" {
		t.Errorf("Provider.Name = %q, 期望默认值 openai", cfg.Provider.Name)
	}
}

// TestLoadConfig_roundTrip 验证结构化字段能正确序列化与回读。
func TestLoadConfig_roundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "agent.json")
	src := `{
	  "provider": {"name": "anthropic", "model": "claude-x", "thinking": true},
	  "session": {"type": "json", "file": "~/s.json"},
	  "autonomous": {"enabled": true, "max_sleep_minutes": 5,
	    "goal_store": {"type": "json", "file": "~/g.json"},
	    "goals": [{"title": "巡检", "schedule_type": "cron", "cron": "0 8 * * *"}]}
	}`
	if err := os.WriteFile(path, []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("LoadConfig 返回错误: %v", err)
	}
	if cfg.Provider.Name != "anthropic" || !cfg.Provider.Thinking {
		t.Errorf("provider 解析不符: %+v", cfg.Provider)
	}
	if cfg.Session.File != "~/s.json" {
		t.Errorf("session.file = %q", cfg.Session.File)
	}
	if cfg.Autonomous == nil || !cfg.Autonomous.Enabled || cfg.Autonomous.MaxSleepMin != 5 {
		t.Fatalf("autonomous 解析不符: %+v", cfg.Autonomous)
	}
	if _, err := GoalFromConfig(cfg.Autonomous.Goals[0]); err != nil {
		t.Errorf("GoalFromConfig 返回错误: %v", err)
	}
}
