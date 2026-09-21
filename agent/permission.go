// 本文件实现全局工具权限层：在 Ask 循环分发每次工具调用前统一判定
// 放行 / 询问 / 拒绝，覆盖所有工具（内置 exec / filesystem / memory、
// MCP 工具与使用方自定义工具）。设计参照 Claude Code 的权限模型：
//
//   - 执行模式（Mode）决定"没命中规则时问不问"：
//     full_access 全放行（deny 规则仍拦截）、confirm 询问用户（默认）、
//     whitelist 仅 allow 规则命中的调用可执行。
//   - allow / deny 规则：字符串 "工具名" 或 "工具名:参数模式"。参数模式的
//     语义由各工具域注册的 matcher 解释（如 exec_command 把模式解释为
//     shell 命令前缀），没有注册 matcher 的工具只按工具名匹配。
//   - 优先级：本会话已记住的允许 > deny > allow > 模式。
//     deny 是绝对红线，任何模式（含 full_access）下都拒绝。
//
// 交互确认由宿主注入的 ApprovalUI 完成（CLI 为终端问答，服务端通常为
// nil——此时 confirm 模式下未放行的调用一律降级为拒绝）。
package agent

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
)

// Verdict 是权限检查结论。
type Verdict int

const (
	VerdictDeny  Verdict = iota // 拒绝执行
	VerdictAllow                // 放行（含"用户确认后放行"）
)

// CallInfo 描述一次待执行的工具调用。
type CallInfo struct {
	Tool string         // 工具名，如 exec_command、mcp__github__create_issue
	Args map[string]any // 工具参数（来自模型 tool_calls 的原始值）
}

// Decision 是审批方（ApprovalUI）对一次询问的答复。
type Decision struct {
	Allowed  bool // 是否允许执行
	Remember bool // 本会话内记住该调用：参数完全相同的后续调用不再询问
}

// ApprovalUI 由宿主注入的交互确认回调。实现应展示调用详情并等待用户答复；
// 无交互能力的宿主（HTTP 服务、自治任务）传 nil。
type ApprovalUI func(ctx context.Context, info CallInfo) Decision

// PermissionChecker 是工具调用权限检查接口。Ask 循环在每次工具执行前调用，
// 返回 VerdictAllow 才会执行工具；reason 是给人（和模型）看的拒绝原因，可为空。
//
// 交互问询是 Checker 实现自己的事（默认实现在 Check 内部调用 ApprovalUI），
// 接口只有这一个方法，方便使用方注入完全自定义的策略。
type PermissionChecker interface {
	Check(ctx context.Context, info CallInfo) (Verdict, string)
}

// Mode 是执行模式：没命中 allow/deny 规则时的默认行为。
type Mode string

const (
	ModeFullAccess Mode = "full_access" // 全放行（deny 规则仍然拦截）
	ModeConfirm    Mode = "confirm"     // 询问用户（默认）
	ModeWhitelist  Mode = "whitelist"   // 仅 allow 规则命中的调用可执行
)

// ParseMode 解析执行模式，空串视为默认的 confirm。
func ParseMode(s string) (Mode, error) {
	switch m := Mode(strings.TrimSpace(s)); m {
	case "":
		return ModeConfirm, nil
	case ModeFullAccess, ModeConfirm, ModeWhitelist:
		return m, nil
	default:
		return "", fmt.Errorf("未知执行模式 %q（可选 full_access / confirm / whitelist）", s)
	}
}

// PermissionConfig 是 Checker 的构造配置，与 config 包的 JSON schema 解耦。
type PermissionConfig struct {
	Mode  Mode     // 缺省 confirm
	Allow []string // 放行规则："工具名" 或 "工具名:参数模式"
	Deny  []string // 拒绝规则：同上，任何模式下命中即拒
}

// ArgMatcher 把规则中的参数模式解释为具体工具的参数语义，返回该次调用的
// 参数是否命中模式。由工具域实现（如 tools/exec 的命令匹配器），注册到
// Checker 后，带参数模式的规则才对该工具生效。
type ArgMatcher func(pattern string, args map[string]any) bool

// toolRule 是解析后的规则。
type toolRule struct {
	tool     string // 工具名；尾缀 * 已剥离
	wildcard bool   // 规则工具名以 * 结尾：按原始前缀匹配
	pattern  string // 参数模式；空表示只按工具名匹配
	raw      string // 原始规则文本（报错与拒绝原因用）
}

// Checker 是 PermissionChecker 的默认实现：模式 + allow/deny 规则 + 参数
// matcher + 交互确认。并发安全；所有方法可从任意 goroutine 调用。
type Checker struct {
	mu sync.Mutex

	mode     Mode
	allow    []toolRule
	deny     []toolRule
	matchers map[string]ArgMatcher // allow 语义：全命中才算命中
	denyM    map[string]ArgMatcher // deny 语义：任一处命中即拒；未注册时退回 matchers
	allowed  map[string]bool       // Remember 记住的调用（本进程生命周期）
	ui       ApprovalUI
}

// NewChecker 校验并编译规则，返回可用的 Checker。规则格式错误会返回错误，
// 使用方应在启动时直接失败，而不是带着残缺策略运行。
func NewChecker(cfg PermissionConfig, ui ApprovalUI) (*Checker, error) {
	mode := cfg.Mode
	if mode == "" {
		mode = ModeConfirm
	}
	if mode != ModeFullAccess && mode != ModeConfirm && mode != ModeWhitelist {
		return nil, fmt.Errorf("未知执行模式 %q（可选 full_access / confirm / whitelist）", mode)
	}
	allow, err := parseRules(cfg.Allow)
	if err != nil {
		return nil, fmt.Errorf("allow 规则: %w", err)
	}
	deny, err := parseRules(cfg.Deny)
	if err != nil {
		return nil, fmt.Errorf("deny 规则: %w", err)
	}
	return &Checker{
		mode:     mode,
		allow:    allow,
		deny:     deny,
		matchers: map[string]ArgMatcher{},
		denyM:    map[string]ArgMatcher{},
		allowed:  map[string]bool{},
		ui:       ui,
	}, nil
}

// parseRules 编译规则列表；空列表合法（返回 nil）。
func parseRules(rules []string) ([]toolRule, error) {
	if len(rules) == 0 {
		return nil, nil
	}
	out := make([]toolRule, 0, len(rules))
	for _, r := range rules {
		parsed, err := parseRule(r)
		if err != nil {
			return nil, err
		}
		out = append(out, parsed)
	}
	return out, nil
}

// parseRule 解析单条规则："工具名" 或 "工具名:参数模式"（按第一个冒号切分）。
// 工具名支持尾缀 * 通配（mcp__github__*）；参数模式的含义由工具域 matcher 解释。
func parseRule(raw string) (toolRule, error) {
	r := strings.TrimSpace(raw)
	if r == "" {
		return toolRule{}, fmt.Errorf("规则不能为空字符串")
	}
	tool, pattern := r, ""
	if i := strings.Index(r, ":"); i >= 0 {
		tool, pattern = r[:i], r[i+1:]
	}
	tool = strings.TrimSpace(tool)
	if tool == "" || tool == "*" {
		return toolRule{}, fmt.Errorf("规则 %q 缺少工具名", raw)
	}
	wildcard := false
	if strings.HasSuffix(tool, "*") {
		wildcard = true
		tool = strings.TrimSuffix(tool, "*")
		if tool == "" {
			return toolRule{}, fmt.Errorf("规则 %q 缺少工具名", raw)
		}
	}
	return toolRule{tool: tool, wildcard: wildcard, pattern: strings.TrimSpace(pattern), raw: r}, nil
}

// RegisterMatcher 注册工具的参数模式匹配器（allow 语义：所有判定要求都
// 命中才算命中）。重复注册覆盖旧值。
func (c *Checker) RegisterMatcher(tool string, m ArgMatcher) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.matchers[tool] = m
}

// RegisterDenyMatcher 注册 deny 规则专用的匹配器。deny 语义应当偏宽
// （如对 shell 命令做整串词边界扫描，连命令替换里的危险片段也不放过）；
// 未注册时 deny 规则退回普通 matcher。
func (c *Checker) RegisterDenyMatcher(tool string, m ArgMatcher) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.denyM[tool] = m
}

// Mode 返回当前执行模式。
func (c *Checker) Mode() Mode {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.mode
}

// SetMode 运行时切换执行模式（如 REPL 的 /mode 命令），仅影响本进程。
func (c *Checker) SetMode(m Mode) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.mode = m
}

// Check 判定一次工具调用。判定顺序：已记住的允许 > deny > allow > 模式。
//
// 锁贯穿整个判定（含 UI 问询）：终端确认天然串行化——并行工具调用同时
// 触发确认时依次弹出，不会交错输出；SetMode 会短暂等待进行中的确认完成。
func (c *Checker) Check(ctx context.Context, info CallInfo) (Verdict, string) {
	c.mu.Lock()
	defer c.mu.Unlock()

	key := rememberKey(info)
	if c.allowed[key] {
		return VerdictAllow, ""
	}
	if r, ok := c.matchList(c.deny, info, c.denyMatcher(info.Tool)); ok {
		return VerdictDeny, fmt.Sprintf("命中 deny 规则 %q", r.raw)
	}
	if _, ok := c.matchList(c.allow, info, c.matchers[info.Tool]); ok {
		return VerdictAllow, ""
	}
	switch c.mode {
	case ModeFullAccess:
		return VerdictAllow, ""
	case ModeWhitelist:
		return VerdictDeny, "whitelist 模式仅执行 allow 规则命中的调用"
	default: // ModeConfirm
		if c.ui == nil {
			return VerdictDeny, "confirm 模式需要用户确认，但当前环境无交互界面（可配置 allow 规则、whitelist 模式或 full_access）"
		}
		d := c.ui(ctx, info)
		if !d.Allowed {
			return VerdictDeny, "用户拒绝执行"
		}
		if d.Remember {
			c.allowed[key] = true
		}
		return VerdictAllow, ""
	}
}

// matchList 依次尝试规则，返回首条命中的规则。带参数模式的规则只有在该
// 工具注册了 matcher 时才可能命中。
func (c *Checker) matchList(rules []toolRule, info CallInfo, m ArgMatcher) (toolRule, bool) {
	for _, r := range rules {
		if !ruleMatchesTool(r, info.Tool) {
			continue
		}
		if r.pattern == "" {
			return r, true
		}
		if m != nil && m(r.pattern, info.Args) {
			return r, true
		}
	}
	return toolRule{}, false
}

// denyMatcher 返回工具的 deny 语义 matcher，未注册专用 matcher 时退回
// 普通 matcher。
func (c *Checker) denyMatcher(tool string) ArgMatcher {
	if m, ok := c.denyM[tool]; ok {
		return m
	}
	return c.matchers[tool]
}

// ruleMatchesTool 判断规则的工具名部分是否覆盖 tool：
//   - 尾缀 * 通配：原始前缀匹配（mcp__github__* → mcp__github__任何）
//   - 否则：全等，或按命名空间边界前缀（mcp__github 覆盖 mcp__github__x，
//     即"整个 server"语义；exec 不会误伤 exec_command）
func ruleMatchesTool(r toolRule, tool string) bool {
	if r.wildcard {
		return strings.HasPrefix(tool, r.tool)
	}
	return tool == r.tool || strings.HasPrefix(tool, r.tool+"__")
}

// rememberKey 生成"本会话总是允许"的记忆键：工具名 + 规范化参数。
// encoding/json 对 map 的键排序是确定的，同一组参数序列化结果稳定。
func rememberKey(info CallInfo) string {
	b, err := json.Marshal(info.Args)
	if err != nil {
		b = []byte(fmt.Sprintf("%v", info.Args))
	}
	sum := sha256.Sum256(append([]byte(info.Tool), b...))
	return hex.EncodeToString(sum[:])
}
