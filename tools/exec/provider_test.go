package exec

import (
	"context"
	"testing"

	"github.com/capyflow/vortexagent/agent"
)

// TestPermissionMatchersAutoWiring 端到端验证自描述接线：ExecTool 实现了
// agent.PermissionMatcherProvider，调用方只需 registry.Add（与其他工具
// 完全一致的惯例），agent.New 即自动把命令匹配器接进 Checker，
// 带参数模式的权限规则随即按 shell 语义生效。
func TestPermissionMatchersAutoWiring(t *testing.T) {
	reg := agent.NewRegistry()
	checker, err := agent.NewChecker(agent.PermissionConfig{
		Mode:  agent.ModeWhitelist,
		Allow: []string{ToolName + ":git status*"},
		Deny:  []string{ToolName + ":rm -rf*"},
	}, nil)
	if err != nil {
		t.Fatalf("NewChecker() 错误: %v", err)
	}
	if err := reg.Add(NewExecTool(".")); err != nil {
		t.Fatalf("registry.Add() 错误: %v", err)
	}

	_ = agent.New(agent.Options{Registry: reg, Permissions: checker})

	ctx := context.Background()
	// allow 语义（从严）：前缀词边界命中
	if v, _ := checker.Check(ctx, agent.CallInfo{Tool: ToolName, Args: map[string]any{"command": "git status --short"}}); v != agent.VerdictAllow {
		t.Error("git status --short 应被 allow 规则放行（自声明匹配器未接线则会拒绝）")
	}
	// deny 语义（从宽）：复合命令任一段命中即拒
	if v, _ := checker.Check(ctx, agent.CallInfo{Tool: ToolName, Args: map[string]any{"command": "ls; rm -rf /"}}); v != agent.VerdictDeny {
		t.Error("复合命令含 rm -rf 应被 deny 规则拒绝")
	}
	// 未命中规则：whitelist 模式直接拒绝
	if v, _ := checker.Check(ctx, agent.CallInfo{Tool: ToolName, Args: map[string]any{"command": "git push"}}); v != agent.VerdictDeny {
		t.Error("whitelist 模式下未命中规则的命令应拒绝")
	}
}
