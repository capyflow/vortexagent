package agent

import (
	"context"
	"strings"
	"testing"

	"github.com/capyflow/vortexagent/agent/sessionstore"
	"github.com/capyflow/vortexagent/llm"
)

// exactArgsMatcher 是测试用参数匹配器：args["command"] 与模式完全相等才命中。
func exactArgsMatcher(pattern string, args map[string]any) bool {
	cmd, _ := args["command"].(string)
	return cmd == pattern
}

// allow 配置的便捷构造。
func newTestChecker(t *testing.T, cfg PermissionConfig, ui ApprovalUI) *Checker {
	t.Helper()
	c, err := NewChecker(cfg, ui)
	if err != nil {
		t.Fatalf("NewChecker() 错误: %v", err)
	}
	return c
}

func TestParseMode(t *testing.T) {
	valid := map[string]Mode{
		"":             ModeConfirm,
		"confirm":      ModeConfirm,
		"full_access":  ModeFullAccess,
		"whitelist":    ModeWhitelist,
		"  whitelist ": ModeWhitelist,
	}
	for in, want := range valid {
		got, err := ParseMode(in)
		if err != nil || got != want {
			t.Errorf("ParseMode(%q) = (%v, %v), 期望 (%v, nil)", in, got, err, want)
		}
	}
	if _, err := ParseMode("yolo"); err == nil {
		t.Error(`ParseMode("yolo") 应返回错误`)
	}
}

// TestNewCheckerRuleErrors 校验非法规则在构造时报错（启动即失败，不带残缺策略运行）。
func TestNewCheckerRuleErrors(t *testing.T) {
	bad := [][]string{
		{""},         // 空规则
		{":pattern"}, // 缺工具名
		{"*"},        // 纯通配等于放行一切，直接拒绝配置
		{" *"},       // 同上（带空白）
		{"ok", ""},   // 混有空规则
	}
	for _, rules := range bad {
		if _, err := NewChecker(PermissionConfig{Allow: rules}, nil); err == nil {
			t.Errorf("allow 规则 %v 应返回错误", rules)
		}
	}
	if _, err := NewChecker(PermissionConfig{Mode: "yolo"}, nil); err == nil {
		t.Error("非法模式应返回错误")
	}
	// 合法配置不报错
	if _, err := NewChecker(PermissionConfig{
		Mode:  ModeFullAccess,
		Allow: []string{"exec_command:git status*", "mcp__github__*"},
		Deny:  []string{"exec_command:rm -rf*"},
	}, nil); err != nil {
		t.Errorf("合法配置不应报错: %v", err)
	}
}

// TestChecker_NameRules 校验纯工具名规则与命名空间语义：
// mcp__github 覆盖整个 server 的工具（mcp__github__x），但不误伤 mcp__github2。
func TestChecker_NameRules(t *testing.T) {
	c := newTestChecker(t, PermissionConfig{
		Mode:  ModeWhitelist,
		Allow: []string{"read_file", "mcp__github"},
	}, nil)

	cases := []struct {
		tool string
		want Verdict
	}{
		{"read_file", VerdictAllow},
		{"mcp__github", VerdictAllow},
		{"mcp__github__create_issue", VerdictAllow}, // server 级规则覆盖
		{"mcp__github2__x", VerdictDeny},            // 命名空间边界不误伤
		{"mcp__gitlab__x", VerdictDeny},
		{"exec_command", VerdictDeny},
	}
	for _, tc := range cases {
		got, reason := c.Check(context.Background(), CallInfo{Tool: tc.tool})
		if got != tc.want {
			t.Errorf("Check(%q) = %v (reason %q), 期望 %v", tc.tool, got, reason, tc.want)
		}
	}
}

// TestChecker_WildcardRules 校验尾缀 * 通配规则。
func TestChecker_WildcardRules(t *testing.T) {
	c := newTestChecker(t, PermissionConfig{
		Mode:  ModeWhitelist,
		Allow: []string{"mcp__github__*"},
	}, nil)

	for _, tool := range []string{"mcp__github__create_issue", "mcp__github__a__b"} {
		if got, _ := c.Check(context.Background(), CallInfo{Tool: tool}); got != VerdictAllow {
			t.Errorf("Check(%q) 应放行", tool)
		}
	}
	if got, _ := c.Check(context.Background(), CallInfo{Tool: "mcp__gitlab__x"}); got != VerdictDeny {
		t.Error("mcp__gitlab__x 不应被 mcp__github__* 放行")
	}
}

// TestChecker_PatternRules 校验带参数模式的规则：只对注册了 matcher 的工具生效。
func TestChecker_PatternRules(t *testing.T) {
	c := newTestChecker(t, PermissionConfig{
		Mode:  ModeWhitelist,
		Allow: []string{"exec_command:git status"},
	}, nil)
	c.RegisterMatcher("exec_command", exactArgsMatcher)

	cases := []struct {
		info CallInfo
		want Verdict
	}{
		{CallInfo{Tool: "exec_command", Args: map[string]any{"command": "git status"}}, VerdictAllow},
		{CallInfo{Tool: "exec_command", Args: map[string]any{"command": "rm -rf /"}}, VerdictDeny},
		// 没有匹配参数时退回模式判定（whitelist → deny）
		{CallInfo{Tool: "exec_command", Args: map[string]any{"query": "x"}}, VerdictDeny},
	}
	for _, tc := range cases {
		if got, _ := c.Check(context.Background(), tc.info); got != tc.want {
			t.Errorf("Check(%+v) = %v, 期望 %v", tc.info, got, tc.want)
		}
	}
}

// TestChecker_Precedence 校验优先级：deny > allow > 模式，且 full_access 下 deny 仍拦截。
func TestChecker_Precedence(t *testing.T) {
	// deny 压过 allow 与 full_access（matcher 为全等语义，模式用完整命令）
	c := newTestChecker(t, PermissionConfig{
		Mode:  ModeFullAccess,
		Allow: []string{"exec_command:git status"},
		Deny:  []string{"exec_command:rm -rf /"},
	}, nil)
	c.RegisterMatcher("exec_command", exactArgsMatcher)

	if got, _ := c.Check(context.Background(), CallInfo{Tool: "exec_command", Args: map[string]any{"command": "git status"}}); got != VerdictAllow {
		t.Error("allow 规则在 full_access 下应放行")
	}
	verdict, reason := c.Check(context.Background(), CallInfo{Tool: "exec_command", Args: map[string]any{"command": "rm -rf /"}})
	if verdict != VerdictDeny {
		t.Errorf("deny 规则在 full_access 下仍应拒绝，实际 %v", verdict)
	}
	if !strings.Contains(reason, "deny") {
		t.Errorf("拒绝原因应说明命中 deny 规则: %q", reason)
	}

	// whitelist 模式：allow 命中放行，其余拒绝
	c2 := newTestChecker(t, PermissionConfig{Mode: ModeWhitelist, Allow: []string{"read_file"}}, nil)
	if got, _ := c2.Check(context.Background(), CallInfo{Tool: "read_file"}); got != VerdictAllow {
		t.Error("whitelist 模式下 allow 命中应放行")
	}
	got, reason := c2.Check(context.Background(), CallInfo{Tool: "exec_command"})
	if got != VerdictDeny || reason == "" {
		t.Errorf("whitelist 模式未命中应拒绝且带原因，实际 (%v, %q)", got, reason)
	}
}

// TestChecker_ConfirmUI 校验 confirm 模式的交互问询：nil UI 降级拒绝、
// 用户允许/拒绝、Remember 后相同调用不再询问。
func TestChecker_ConfirmUI(t *testing.T) {
	info := CallInfo{Tool: "exec_command", Args: map[string]any{"command": "go build ./..."}}

	// 无 UI（服务端场景）：降级为拒绝，且带可操作的原因
	c := newTestChecker(t, PermissionConfig{}, nil)
	got, reason := c.Check(context.Background(), info)
	if got != VerdictDeny || reason == "" {
		t.Errorf("confirm 无 UI 应拒绝且带原因，实际 (%v, %q)", got, reason)
	}

	// UI 拒绝
	denyCalls := 0
	askDeny := func(context.Context, CallInfo) Decision { denyCalls++; return Decision{} }
	c = newTestChecker(t, PermissionConfig{}, askDeny)
	if got, _ := c.Check(context.Background(), info); got != VerdictDeny {
		t.Error("用户拒绝后应返回 Deny")
	}

	// UI 允许 + Remember：相同参数第二次不再询问（UI 只被调用一次）
	allowCalls := 0
	askAllow := func(_ context.Context, got CallInfo) Decision {
		allowCalls++
		if got.Tool != info.Tool {
			t.Errorf("UI 收到工具 %q, 期望 %q", got.Tool, info.Tool)
		}
		return Decision{Allowed: true, Remember: true}
	}
	c = newTestChecker(t, PermissionConfig{}, askAllow)
	if got, _ := c.Check(context.Background(), info); got != VerdictAllow {
		t.Error("用户允许后应放行")
	}
	if got, _ := c.Check(context.Background(), info); got != VerdictAllow {
		t.Error("Remember 的调用应直接放行")
	}
	if allowCalls != 1 {
		t.Errorf("UI 应只被询问 1 次，实际 %d 次", allowCalls)
	}
	// 参数不同仍需询问
	other := CallInfo{Tool: "exec_command", Args: map[string]any{"command": "go test ./..."}}
	c.Check(context.Background(), other)
	if allowCalls != 2 {
		t.Errorf("参数不同的调用应再次询问，UI 共 %d 次，期望 2", allowCalls)
	}
}

// TestChecker_SetMode 校验运行时切换模式。
func TestChecker_SetMode(t *testing.T) {
	c := newTestChecker(t, PermissionConfig{}, nil)
	if c.Mode() != ModeConfirm {
		t.Errorf("默认模式应为 confirm, 实际 %s", c.Mode())
	}
	info := CallInfo{Tool: "exec_command", Args: map[string]any{"command": "x"}}
	if got, _ := c.Check(context.Background(), info); got != VerdictDeny {
		t.Error("confirm 无 UI 应拒绝")
	}
	c.SetMode(ModeFullAccess)
	if got, _ := c.Check(context.Background(), info); got != VerdictAllow {
		t.Error("full_access 应放行")
	}
}

// TestAsk_PermissionDeniesTool 端到端：权限拒绝以 nonRetryable 语义回传模型
// ——工具不执行、错误文本进入 tool 消息、循环继续到最终回答。
func TestAsk_PermissionDeniesTool(t *testing.T) {
	fp := &fakeProvider{name: "fake", toolName: "echo", maxRounds: 1}
	reg := NewRegistry()
	echo := &echoTool{}
	if err := reg.Add(echo); err != nil {
		t.Fatal(err)
	}
	c := newTestChecker(t, PermissionConfig{Mode: ModeWhitelist}, nil)
	ag := New(Options{
		Provider:    fp,
		Registry:    reg,
		Model:       "m1",
		Permissions: c,
	})
	session := sessionstore.NewSession("m1")

	if _, err := ag.Ask(context.Background(), session, "测试权限拒绝"); err != nil {
		t.Fatalf("Ask 失败: %v", err)
	}
	if echo.calls != 0 {
		t.Errorf("被拒绝的工具不应执行，实际执行 %d 次", echo.calls)
	}
	found := false
	for _, m := range session.Messages() {
		if m.Role == llm.RoleTool && strings.Contains(m.Content[0].Text, "被权限系统拒绝") {
			found = true
		}
	}
	if !found {
		t.Error("历史中应包含权限拒绝说明的 tool 消息")
	}
}

// TestAsk_PermissionAllowsTool 端到端：规则放行时工具正常执行。
func TestAsk_PermissionAllowsTool(t *testing.T) {
	fp := &fakeProvider{name: "fake", toolName: "echo", maxRounds: 1}
	reg := NewRegistry()
	echo := &echoTool{}
	if err := reg.Add(echo); err != nil {
		t.Fatal(err)
	}
	c := newTestChecker(t, PermissionConfig{Mode: ModeWhitelist, Allow: []string{"echo"}}, nil)
	ag := New(Options{
		Provider:    fp,
		Registry:    reg,
		Model:       "m1",
		Permissions: c,
	})

	if _, err := ag.Ask(context.Background(), sessionstore.NewSession("m1"), "测试放行"); err != nil {
		t.Fatalf("Ask 失败: %v", err)
	}
	if echo.calls != 1 {
		t.Errorf("放行的工具应执行 1 次, 实际 %d 次", echo.calls)
	}
}

// patternTool 是实现了权限自描述接口（PermissionMatcherProvider）的测试工具。
type patternTool struct{ allow ArgMatcher }

func (t *patternTool) Name() string        { return "pattern_tool" }
func (t *patternTool) Description() string { return "测试权限自描述" }
func (t *patternTool) Schema() map[string]any {
	return map[string]any{"type": "object"}
}
func (t *patternTool) Call(_ context.Context, _ map[string]any) (string, error) {
	return "ok", nil
}
func (t *patternTool) PermissionMatchers() (ArgMatcher, ArgMatcher) { return t.allow, nil }

// TestNew_CollectsToolMatchers 验证 agent.New 自动收集工具自声明的权限匹配器：
// 注册工具后无需手工接线，带参数模式的规则即按工具语义生效。
func TestNew_CollectsToolMatchers(t *testing.T) {
	reg := NewRegistry()
	matcher := func(pattern string, args map[string]any) bool {
		cmd, _ := args["command"].(string)
		return cmd == pattern || strings.HasPrefix(cmd, pattern+" ")
	}
	if err := reg.Add(&patternTool{allow: matcher}); err != nil {
		t.Fatal(err)
	}
	checker, err := NewChecker(PermissionConfig{
		Mode:  ModeWhitelist,
		Allow: []string{"pattern_tool:git status"},
	}, nil)
	if err != nil {
		t.Fatal(err)
	}

	_ = New(Options{Registry: reg, Permissions: checker})

	ctx := context.Background()
	if v, _ := checker.Check(ctx, CallInfo{Tool: "pattern_tool", Args: map[string]any{"command": "git status --short"}}); v != VerdictAllow {
		t.Error("自声明匹配器应已接线：git status --short 应命中 allow 规则")
	}
	if v, _ := checker.Check(ctx, CallInfo{Tool: "pattern_tool", Args: map[string]any{"command": "rm -rf /"}}); v != VerdictDeny {
		t.Error("未命中规则的调用应拒绝（whitelist 模式）")
	}

	// 手工注册优先于自声明（覆盖语义）
	checker.RegisterMatcher("pattern_tool", exactArgsMatcher)
	if v, _ := checker.Check(ctx, CallInfo{Tool: "pattern_tool", Args: map[string]any{"command": "git status --short"}}); v != VerdictDeny {
		t.Error("手工注册的匹配器应覆盖工具自声明（全等语义不命中带参命令）")
	}
}

// TestNew_CollectWithoutRegistrar 验证 Permissions 为 nil 或不支持收集的
// 自定义 Checker 时，收集过程静默跳过、不 panic。
func TestNew_CollectWithoutRegistrar(t *testing.T) {
	reg := NewRegistry()
	if err := reg.Add(&patternTool{}); err != nil {
		t.Fatal(err)
	}
	_ = New(Options{Registry: reg})
	_ = New(Options{Registry: reg, Permissions: customCheckerFunc(
		func(context.Context, CallInfo) (Verdict, string) { return VerdictAllow, "" })})
}

type customCheckerFunc func(ctx context.Context, info CallInfo) (Verdict, string)

func (f customCheckerFunc) Check(ctx context.Context, info CallInfo) (Verdict, string) {
	return f(ctx, info)
}
