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

// TestSkill_AllowedToolsEnforced 校验 allowed-tools 双重生效：
// 模型只看到允许列表内的工具声明；越权调用也会被拦截且不执行。
func TestSkill_AllowedToolsEnforced(t *testing.T) {
	echo := &echoTool{}
	secret := &namedToolWithCount{namedTool: namedTool{name: "secret"}}
	registry := NewRegistry()
	if err := registry.Add(echo); err != nil {
		t.Fatal(err)
	}
	if err := registry.Add(secret); err != nil {
		t.Fatal(err)
	}

	provider := &scriptProvider{name: "skill", script: []llm.ChatResponse{
		toolCallResp("secret", map[string]any{}),
		textResp("收到错误后收尾"),
	}}
	skill := &Skill{
		Metadata: SkillMetadata{
			Name:         "limited",
			AllowedTools: []string{"echo"},
		},
	}
	ag := New(Options{Provider: provider, Registry: registry, Model: "m"})

	_, err := ag.Ask(context.Background(), sessionstore.NewSession("m"), "触发", WithSkill(skill))
	if err != nil {
		t.Fatalf("越权调用应回传错误文本而非 Ask 失败: %v", err)
	}
	if secret.CallCount() != 0 {
		t.Errorf("越权工具不应被执行，实际 %d 次", secret.CallCount())
	}

	// 工具声明应被过滤：模型只看到 echo
	declared := map[string]bool{}
	for _, t2 := range provider.lastReq.Tools {
		declared[t2.Name] = true
	}
	if !declared["echo"] || declared["secret"] {
		t.Errorf("工具声明应只包含 echo: %v", declared)
	}
}

// TestSkill_ModelAndTemperature 校验 skill 的 model 与 temperature 覆盖请求参数。
func TestSkill_ModelAndTemperature(t *testing.T) {
	provider := &mockProviderForSkill{
		response: &llm.ChatResponse{
			Message: llm.Message{Role: llm.RoleAssistant, Content: []llm.Content{{Type: llm.ContentText, Text: "ok"}}},
		},
	}
	ag := New(Options{Provider: provider, Model: "default-model"})
	temp := 0.2
	skill := &Skill{
		Metadata: SkillMetadata{
			Name:        "review",
			Model:       "skill-model",
			Temperature: &temp,
		},
	}

	if _, err := ag.Ask(context.Background(), sessionstore.NewSession("m"), "问题", WithSkill(skill)); err != nil {
		t.Fatal(err)
	}
	if provider.lastRequest.Model != "skill-model" {
		t.Errorf("请求模型 = %q, 期望 skill-model", provider.lastRequest.Model)
	}
	if provider.lastRequest.Temperature == nil || *provider.lastRequest.Temperature != 0.2 {
		t.Errorf("请求温度 = %v, 期望 0.2", provider.lastRequest.Temperature)
	}
}

// namedToolWithCount 是带调用计数的命名工具。
type namedToolWithCount struct {
	namedTool
	calls int
}

func (t *namedToolWithCount) Call(ctx context.Context, args map[string]any) (string, error) {
	t.calls++
	return t.namedTool.Call(ctx, args)
}

func (t *namedToolWithCount) CallCount() int { return t.calls }

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
