package agent

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/capyflow/vortexagent/agent/sessionstore"
	"github.com/capyflow/vortexagent/llm"
)

func TestSkillManager_Discover(t *testing.T) {
	tmpDir := t.TempDir()

	skillDir := filepath.Join(tmpDir, ".vortex", "skills", "test-skill")
	if err := os.MkdirAll(skillDir, 0755); err != nil {
		t.Fatal(err)
	}

	skillContent := `---
name: test-skill
description: "测试技能"
---

# Test Skill

这是一个测试技能。
`
	if err := os.WriteFile(filepath.Join(skillDir, "SKILL.md"), []byte(skillContent), 0644); err != nil {
		t.Fatal(err)
	}

	mgr := NewSkillManager(tmpDir)
	if err := mgr.Discover(); err != nil {
		t.Fatal(err)
	}

	skill, ok := mgr.Get("test-skill")
	if !ok {
		t.Fatal("未找到 test-skill")
	}

	if skill.Metadata.Name != "test-skill" {
		t.Errorf("期望 name=test-skill, 实际 %s", skill.Metadata.Name)
	}

	if skill.Metadata.Description != "测试技能" {
		t.Errorf("期望 description=测试技能, 实际 %s", skill.Metadata.Description)
	}

	if skill.Scope != "project" {
		t.Errorf("期望 scope=project, 实际 %s", skill.Scope)
	}
}

func TestWithSkill(t *testing.T) {
	skill := &Skill{
		Metadata: SkillMetadata{
			Name:        "code-review",
			Description: "代码审查",
		},
		Content: "# Code Review\n审查代码质量",
	}

	provider := &mockProviderForSkill{
		response: &llm.ChatResponse{
			Message: llm.Message{
				Role:    llm.RoleAssistant,
				Content: []llm.Content{{Type: llm.ContentText, Text: "审查完成"}},
			},
		},
	}

	registry := NewRegistry()
	ag := New(Options{
		Provider: provider,
		Registry: registry,
	})

	session := sessionstore.NewSession("test")
	answer, err := ag.Ask(context.Background(), session, "审查代码", WithSkill(skill))
	if err != nil {
		t.Fatal(err)
	}

	if answer != "审查完成" {
		t.Errorf("期望 answer=审查完成, 实际 %s", answer)
	}

	if provider.lastRequest == nil {
		t.Fatal("未收到请求")
	}

	msgs := provider.lastRequest.Messages
	if len(msgs) == 0 {
		t.Fatal("消息为空")
	}

	systemMsg := msgs[0]
	if systemMsg.Role != llm.RoleSystem {
		t.Errorf("期望 system 角色, 实际 %s", systemMsg.Role)
	}

	content := textOf(systemMsg)
	if !strings.Contains(content, "code-review") {
		t.Errorf("系统提示词应包含 skill 内容, 实际: %s", content)
	}

	if !strings.Contains(content, "审查代码质量") {
		t.Errorf("系统提示词应包含 skill body, 实际: %s", content)
	}
}

func TestWithoutSkill(t *testing.T) {
	provider := &mockProviderForSkill{
		response: &llm.ChatResponse{
			Message: llm.Message{
				Role:    llm.RoleAssistant,
				Content: []llm.Content{{Type: llm.ContentText, Text: "普通回答"}},
			},
		},
	}

	registry := NewRegistry()
	ag := New(Options{
		Provider: provider,
		Registry: registry,
	})

	session := sessionstore.NewSession("test")
	answer, err := ag.Ask(context.Background(), session, "你好")
	if err != nil {
		t.Fatal(err)
	}

	if answer != "普通回答" {
		t.Errorf("期望 answer=普通回答, 实际 %s", answer)
	}

	msgs := provider.lastRequest.Messages
	systemMsg := msgs[0]
	content := textOf(systemMsg)

	if strings.Contains(content, "<skill-instruction>") {
		t.Error("不传 skill 时不应包含 skill 内容")
	}
}

type mockProviderForSkill struct {
	response    *llm.ChatResponse
	lastRequest *llm.ChatRequest
}

func (m *mockProviderForSkill) Name() string      { return "mock" }
func (m *mockProviderForSkill) ContextWindow() int { return 4096 }

func (m *mockProviderForSkill) Chat(ctx context.Context, req *llm.ChatRequest, onDelta func(llm.Delta) error) (*llm.ChatResponse, error) {
	m.lastRequest = req
	return m.response, nil
}
