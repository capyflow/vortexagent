package exec

import (
	"bytes"
	"context"
	"fmt"
	"os/exec"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/capyflow/vortexagent/agent"
)

const (
	DefaultTimeout = 30 * time.Second
	// MaxTimeout 限制模型可请求的最大超时：模型可能传超大值把调用方拖住
	// 数小时，上限封顶后超长任务应交给后台机制（如 TaskHub）。
	MaxTimeout = 10 * time.Minute
	// MaxOutputSize 单路输出（stdout/stderr）的硬上限：防止极端输出撑爆内存。
	MaxOutputSize = 100 * 1024 // 100KB
	// DefaultMaxOutputChars 工具结果进入会话历史前的默认截断上限（字符）。
	// 这是上下文源头节流的第一道闸——大输出根本不进上下文。0/未配置取此默认，
	// -1 表示不限制（不建议）。
	DefaultMaxOutputChars = 8000
)

// ToolName 是本工具在 Registry 中的名字，权限规则用工具名引用它
// （如 "exec_command:git status*"、"exec_command:rm -rf*"）。
const ToolName = "exec_command"

type ExecTool struct {
	workingDir string
	// maxOutput 结果进入历史前的截断上限（字符）：0 取 DefaultMaxOutputChars，
	// 负数不限。截断不影响命令本身的执行，完整输出若已归档可经
	// expand_chunk 回读（见 ctxengine）。
	maxOutput int
}

// Option 修改 ExecTool 配置。
type Option func(*ExecTool)

// WithMaxOutput 设置结果截断上限（字符）；负数不限，0 取默认。
func WithMaxOutput(n int) Option {
	return func(t *ExecTool) { t.maxOutput = n }
}

func NewExecTool(workingDir string, opts ...Option) *ExecTool {
	t := &ExecTool{workingDir: workingDir}
	for _, opt := range opts {
		opt(t)
	}
	return t
}

func (t *ExecTool) Name() string { return ToolName }

// PermissionMatchers 实现 agent.PermissionMatcherProvider 可选接口（与
// OverviewProvider 同类的自描述扩展点）：声明本工具如何解释权限规则里的
// 参数模式。Agent 创建时自动把匹配器接线进 Checker，调用方只需
// registry.Add(NewExecTool(...))，带参数模式的规则即可按 shell 语义生效。
func (t *ExecTool) PermissionMatchers() (agent.ArgMatcher, agent.ArgMatcher) {
	return CommandMatcher, CommandDenyMatcher
}

// 编译期断言：ExecTool 满足权限自描述接口。
var _ agent.PermissionMatcherProvider = (*ExecTool)(nil)

func (t *ExecTool) Description() string {
	return "执行 shell 命令并返回输出"
}

func (t *ExecTool) Overview() string {
	return "执行命令"
}

func (t *ExecTool) Schema() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"command": map[string]any{
				"type":        "string",
				"description": "要执行的 shell 命令",
			},
			"timeout": map[string]any{
				"type":        "number",
				"description": "超时时间（秒），默认 30 秒",
			},
		},
		"required": []string{"command"},
	}
}

func (t *ExecTool) Call(ctx context.Context, args map[string]any) (string, error) {
	command, _ := args["command"].(string)
	if command == "" {
		return "", fmt.Errorf("command 不能为空")
	}

	timeout := DefaultTimeout
	if v, ok := args["timeout"].(float64); ok && v > 0 {
		timeout = time.Duration(v) * time.Second
	}
	if timeout > MaxTimeout {
		timeout = MaxTimeout
	}

	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, "sh", "-c", command)
	cmd.Dir = t.workingDir

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	err := cmd.Run()

	var result strings.Builder
	if stdout.Len() > 0 {
		result.WriteString(truncateUTF8(stdout.String(), MaxOutputSize))
	}
	if stderr.Len() > 0 {
		output := truncateUTF8(stderr.String(), MaxOutputSize)
		if result.Len() > 0 {
			result.WriteString("\n")
		}
		result.WriteString("stderr: ")
		result.WriteString(output)
	}

	// 源头节流：进入历史的最终结果按上限截断（0 取默认，负数不限）。
	limit := t.maxOutput
	if limit == 0 {
		limit = DefaultMaxOutputChars
	}
	out := truncateWithMarker(result.String(), limit)

	if err != nil {
		if ctx.Err() == context.DeadlineExceeded {
			return out, fmt.Errorf("命令执行超时（%v）", timeout)
		}
		return out, fmt.Errorf("命令执行失败: %w", err)
	}

	if out == "" {
		return "命令执行成功（无输出）", nil
	}

	return out, nil
}

// truncateWithMarker 按 rune 上限截断并标注原始长度（max<0 不限制）。
func truncateWithMarker(s string, max int) string {
	if max < 0 {
		return s
	}
	rs := []rune(s)
	if len(rs) <= max {
		return s
	}
	return string(rs[:max]) + fmt.Sprintf("\n[…输出已截断，完整输出共 %d 字符]", len(rs))
}

// truncateUTF8 按字节上限截断并回退到完整的 rune 边界，
// 避免把多字节字符切成乱码（旧实现按字节硬切）。
func truncateUTF8(s string, max int) string {
	if len(s) <= max {
		return s
	}
	cut := max
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut] + "\n... (输出已截断)"
}
