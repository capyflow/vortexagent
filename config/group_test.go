package config

import (
	"path/filepath"
	"strings"
	"testing"
)

// TestGroupDir_layout 验证 group 目录与各子路径的拼接布局。
func TestGroupDir_layout(t *testing.T) {
	if got := GroupDir("a"); !strings.HasSuffix(got, filepath.Join(".vortex", "a")) {
		t.Errorf("GroupDir = %q, 期望以 .vortex/a 结尾", got)
	}
	if got := GroupDir(""); got != "" {
		t.Errorf("GroupDir(\"\") = %q, 期望空串", got)
	}
	if got := GroupDir("../escape"); got != "" {
		t.Errorf("含路径分隔符的 group 应返回空串, 实际 %q", got)
	}
	if got := GroupConfigPath("a"); filepath.Base(got) != "agent.json" {
		t.Errorf("GroupConfigPath = %q, 期望 agent.json", got)
	}
}

// TestResolveConfigPath_priority 验证配置路径的优先级：
// 显式 -config > group（运行时/烧录）> 烧录 DefaultPath > 报错。
func TestResolveConfigPath_priority(t *testing.T) {
	savedGroup, savedPath := DefaultGroup, DefaultPath
	defer func() { DefaultGroup, DefaultPath = savedGroup, savedPath }()
	DefaultGroup, DefaultPath = "", "/baked/path.json"

	path, group, err := ResolveConfigPath("/explicit/a.json", "")
	if err != nil || path != "/explicit/a.json" || group != "" {
		t.Errorf("显式路径: path=%q group=%q err=%v", path, group, err)
	}

	DefaultGroup = "g1"
	path, group, err = ResolveConfigPath("/explicit/a.json", "")
	if err != nil || group != "g1" {
		t.Errorf("显式路径应保留 group 用于默认值解析: path=%q group=%q err=%v", path, group, err)
	}

	path, group, err = ResolveConfigPath("", "g2")
	if err != nil || filepath.Base(path) != "agent.json" || !strings.Contains(path, "g2") || group != "g2" {
		t.Errorf("group 模式: path=%q group=%q err=%v", path, group, err)
	}

	path, group, err = ResolveConfigPath("", "")
	if err != nil || !strings.Contains(path, "g1") || filepath.Base(path) != "agent.json" || group != "g1" {
		t.Errorf("烧录 DefaultGroup 应优先于 DefaultPath: path=%q group=%q err=%v", path, group, err)
	}

	DefaultGroup = ""
	path, group, err = ResolveConfigPath("", "")
	if err != nil || path != "/baked/path.json" {
		t.Errorf("DefaultPath 兜底: path=%q group=%q err=%v", path, group, err)
	}

	DefaultPath = ""
	if _, _, err := ResolveConfigPath("", ""); err == nil {
		t.Error("全部未指定时应报错")
	}
}

// TestResolveGroupDefaults_fillsEmpty 验证留空路径填充进 group 目录、显式路径不动。
func TestResolveGroupDefaults_fillsEmpty(t *testing.T) {
	cfg := DefaultConfig()
	cfg.Session.Type = "json"
	cfg.Tools = &ToolsConfig{Memory: &MemoryToolConfig{Enabled: true}}
	cfg.Autonomous = &AutonomousConfig{Enabled: true}

	if err := cfg.ResolveGroupDefaults("rg1"); err != nil {
		t.Fatalf("ResolveGroupDefaults 返回错误: %v", err)
	}
	checks := []struct{ name, got, wantSuffix string }{
		{"session.file", cfg.Session.File, filepath.Join("rg1", "sessions.json")},
		{"memory.dir", cfg.Tools.Memory.Dir, filepath.Join("rg1", "memory")},
		{"goal_store.file", cfg.Autonomous.GoalStore.File, filepath.Join("rg1", "goals.json")},
	}
	for _, c := range checks {
		if !strings.HasSuffix(c.got, c.wantSuffix) {
			t.Errorf("%s = %q, 期望以 %s 结尾", c.name, c.got, c.wantSuffix)
		}
	}

	shared := "/data/shared-memory"
	cfg.Tools.Memory.Dir = shared
	if err := cfg.ResolveGroupDefaults("rg1"); err != nil {
		t.Fatalf("ResolveGroupDefaults 返回错误: %v", err)
	}
	if cfg.Tools.Memory.Dir != shared {
		t.Errorf("显式路径不应被覆盖: %q", cfg.Tools.Memory.Dir)
	}
}
