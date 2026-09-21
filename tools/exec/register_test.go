package exec

import (
	"context"
	"testing"

	"github.com/capyflow/vortexagent/agent"
)

// TestRegister 验证一站式注册：工具进入 Registry，命令匹配器接线进 Checker——
// 带参数模式的规则在 whitelist 模式下按 shell 语义放行/拒绝。
func TestRegister(t *testing.T) {
	reg := agent.NewRegistry()
	checker, err := agent.NewChecker(agent.PermissionConfig{
		Mode:  agent.ModeWhitelist,
		Allow: []string{ToolName + ":git status*"},
		Deny:  []string{ToolName + ":rm -rf*"},
	}, nil)
	if err != nil {
		t.Fatalf("NewChecker() 错误: %v", err)
	}

	if err := Register(reg, checker, "."); err != nil {
		t.Fatalf("Register() 错误: %v", err)
	}
	if _, ok := reg.Get(ToolName); !ok {
		t.Fatalf("工具 %q 应已注册", ToolName)
	}

	ctx := context.Background()
	// matcher 接线成功：allow 前缀规则命中（工具未注册匹配器时该规则不可能命中）
	if v, _ := checker.Check(ctx, agent.CallInfo{Tool: ToolName, Args: map[string]any{"command": "git status --short"}}); v != agent.VerdictAllow {
		t.Error("git status --short 应被 allow 规则放行（匹配器未接线则会拒绝）")
	}
	// deny 语义从宽：复合命令任一段命中即拒
	if v, _ := checker.Check(ctx, agent.CallInfo{Tool: ToolName, Args: map[string]any{"command": "ls; rm -rf /"}}); v != agent.VerdictDeny {
		t.Error("复合命令含 rm -rf 应被 deny 规则拒绝")
	}
	// 未命中规则：whitelist 模式直接拒绝
	if v, _ := checker.Check(ctx, agent.CallInfo{Tool: ToolName, Args: map[string]any{"command": "git push"}}); v != agent.VerdictDeny {
		t.Error("whitelist 模式下未命中规则的命令应拒绝")
	}
}

// TestRegister_NilChecker 验证 perms 为 nil 时仅注册工具，不报错。
func TestRegister_NilChecker(t *testing.T) {
	reg := agent.NewRegistry()
	if err := Register(reg, nil, ""); err != nil {
		t.Fatalf("Register(nil checker) 错误: %v", err)
	}
	tool, ok := reg.Get(ToolName)
	if !ok {
		t.Fatalf("工具 %q 应已注册", ToolName)
	}
	if tool.Name() != ToolName {
		t.Errorf("工具名 = %q, 期望 %q", tool.Name(), ToolName)
	}
}

// TestRegister_Duplicate 验证重复注册返回重名错误（Registry 的重名保护）。
func TestRegister_Duplicate(t *testing.T) {
	reg := agent.NewRegistry()
	if err := Register(reg, nil, "."); err != nil {
		t.Fatalf("第一次 Register() 错误: %v", err)
	}
	if err := Register(reg, nil, "."); err == nil {
		t.Error("重复注册应返回重名错误")
	}
}

// TestRegister_NilRegistry 验证 registry 为空时报错而非 panic。
func TestRegister_NilRegistry(t *testing.T) {
	if err := Register(nil, nil, "."); err == nil {
		t.Error("nil registry 应返回错误")
	}
}
