package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// DefaultGroup 构建时烧录的 agent group 名：
//
//	go build -ldflags "-X github.com/capyflow/vortexagent/config.DefaultGroup=my-agent"
//
// group 决定一组隔离的本地数据目录 ~/.vortex/<group>/：配置文件、会话、长期记忆、
// 自治目标、上下文卸载文件都默认落在其下（见 GroupDir 与 ResolveGroupDefaults），
// 一机多 agent 时各用各的 group 即天然隔离。运行时 -group 可覆盖烧录值。
var DefaultGroup string

// GroupDir 返回 group 的隔离数据目录 ~/.vortex/<group>；group 为空时返回空串。
// group 名不能含路径分隔符（防目录穿越）。
func GroupDir(group string) string {
	if group == "" {
		return ""
	}
	if err := validateGroup(group); err != nil {
		return ""
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".vortex", group)
}

// GroupConfigPath 返回 group 目录下的配置文件路径 ~/.vortex/<group>/agent.json。
func GroupConfigPath(group string) string {
	return filepath.Join(GroupDir(group), "agent.json")
}

// GroupSessionsFile 返回 group 目录下的会话存储文件路径。
func GroupSessionsFile(group string) string {
	return filepath.Join(GroupDir(group), "sessions.json")
}

// GroupMemoryDir 返回 group 目录下的长期记忆存储目录。
func GroupMemoryDir(group string) string {
	return filepath.Join(GroupDir(group), "memory")
}

// GroupGoalStoreFile 返回 group 目录下的自治目标存储文件路径。
func GroupGoalStoreFile(group string) string {
	return filepath.Join(GroupDir(group), "goals.json")
}

// GroupOffloadDir 返回 group 目录下的上下文卸载（归档）目录，
// 作为 sessionstore.NewFileSystemOffload 的 baseDir。
func GroupOffloadDir(group string) string {
	return filepath.Join(GroupDir(group), "offload")
}

// ResolveConfigPath 计算生效的配置文件路径与隔离 group。
// 优先级：显式 configPath（-config）> group（-group 或烧录 DefaultGroup）> 烧录 DefaultPath。
// 返回的 group 同时用于把配置中留空的本地状态路径解析进 group 目录（见 ResolveGroupDefaults）。
func ResolveConfigPath(configPath, group string) (path, effectiveGroup string, err error) {
	if group == "" {
		group = DefaultGroup
	}
	switch {
	case configPath != "":
		return configPath, group, nil
	case group != "":
		if err := validateGroup(group); err != nil {
			return "", "", err
		}
		return GroupConfigPath(group), group, nil
	case DefaultPath != "":
		return DefaultPath, "", nil
	default:
		return "", "", fmt.Errorf("未指定配置来源：构建时 -ldflags \"-X github.com/capyflow/vortexagent/config.DefaultGroup=组名\" 烧录，或运行时 -group / -config 传入")
	}
}

// ResolveGroupDefaults 把配置中留空的本地状态路径填充到 group 目录下，实现 group 级隔离：
// session.file → <group>/sessions.json，tools.memory.dir → <group>/memory，
// autonomous.goal_store.file → <group>/goals.json。显式指定的路径保持原样
// （可用于有意跨 agent 共享记忆等场景）。同时确保 group 目录存在。
// group 为空时不做任何事。
func (c *Config) ResolveGroupDefaults(group string) error {
	dir := GroupDir(group)
	if dir == "" {
		return nil
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("创建 group 数据目录 %s 失败: %w", dir, err)
	}
	if c.Session.Type == "json" && c.Session.File == "" {
		c.Session.File = GroupSessionsFile(group)
	}
	if c.Tools != nil && c.Tools.Memory != nil && c.Tools.Memory.Enabled && c.Tools.Memory.Dir == "" {
		c.Tools.Memory.Dir = GroupMemoryDir(group)
	}
	if c.Autonomous != nil && c.Autonomous.Enabled && c.Autonomous.GoalStore.File == "" {
		c.Autonomous.GoalStore.File = GroupGoalStoreFile(group)
	}
	return nil
}

func validateGroup(group string) error {
	if group == "" {
		return nil
	}
	if group == "." || group == ".." || filepath.Base(group) != group || strings.Contains(group, "\\") {
		return fmt.Errorf("无效的 agent group 名 %q：不能含路径分隔符", group)
	}
	return nil
}
