package agent

import (
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"
)

// SkillMetadata 是 SKILL.md 的 YAML frontmatter
type SkillMetadata struct {
	Name         string   `yaml:"name"`
	Description  string   `yaml:"description"`
	Model        string   `yaml:"model,omitempty"`
	AllowedTools []string `yaml:"allowed-tools,omitempty"`
	Temperature  *float64 `yaml:"temperature,omitempty"`
	ArgumentHint string   `yaml:"argument-hint,omitempty"`
}

// Skill 表示一个完整的技能定义
type Skill struct {
	Metadata SkillMetadata
	Content  string // SKILL.md 的 Markdown 内容
	Path     string // 文件路径
	Scope    string // "project" | "user"
}

// SkillManager 管理 skill 的发现和加载
type SkillManager struct {
	searchDirs []string
	skills     map[string]*Skill
}

// NewSkillManager 创建 skill 管理器
func NewSkillManager(workDir string) *SkillManager {
	dirs := []string{
		filepath.Join(workDir, ".vortex", "skills"),
		filepath.Join(os.Getenv("HOME"), ".vortex", "skills"),
	}
	return &SkillManager{
		searchDirs: dirs,
		skills:     make(map[string]*Skill),
	}
}

// Discover 扫描所有目录，发现可用 skills
func (m *SkillManager) Discover() error {
	for _, dir := range m.searchDirs {
		if _, err := os.Stat(dir); os.IsNotExist(err) {
			continue
		}

		entries, err := os.ReadDir(dir)
		if err != nil {
			continue
		}

		for _, entry := range entries {
			if !entry.IsDir() {
				continue
			}

			skillPath := filepath.Join(dir, entry.Name(), "SKILL.md")
			skill, err := LoadSkill(skillPath, m.getScope(dir))
			if err != nil {
				continue
			}

			if existing, ok := m.skills[skill.Metadata.Name]; !ok ||
				(skill.Scope == "project" && existing.Scope == "user") {
				m.skills[skill.Metadata.Name] = skill
			}
		}
	}
	return nil
}

// LoadSkill 从文件加载 skill
func LoadSkill(path, scope string) (*Skill, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}

	content := string(data)
	meta, body, err := parseFrontmatter(content)
	if err != nil {
		return nil, err
	}

	return &Skill{
		Metadata: meta,
		Content:  body,
		Path:     path,
		Scope:    scope,
	}, nil
}

// parseFrontmatter 解析 --- 包裹的 YAML frontmatter
func parseFrontmatter(content string) (SkillMetadata, string, error) {
	var meta SkillMetadata

	if !strings.HasPrefix(content, "---") {
		return meta, content, nil
	}

	parts := strings.SplitN(content, "---", 3)
	if len(parts) < 3 {
		return meta, content, nil
	}

	err := yaml.Unmarshal([]byte(parts[1]), &meta)
	if err != nil {
		return meta, "", err
	}

	return meta, strings.TrimSpace(parts[2]), nil
}

// Get 返回指定名称的 skill
func (m *SkillManager) Get(name string) (*Skill, bool) {
	skill, ok := m.skills[name]
	return skill, ok
}

// List 返回所有可用 skills
func (m *SkillManager) List() []*Skill {
	result := make([]*Skill, 0, len(m.skills))
	for _, skill := range m.skills {
		result = append(result, skill)
	}
	return result
}

func (m *SkillManager) getScope(dir string) string {
	if strings.Contains(dir, ".vortex") && !strings.HasPrefix(dir, os.Getenv("HOME")) {
		return "project"
	}
	return "user"
}
