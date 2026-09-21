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
	MaxTimeout    = 10 * time.Minute
	MaxOutputSize = 100 * 1024 // 100KB
)

// ToolName 是本工具在 Registry 中的名字，权限规则用工具名引用它
// （如 "exec_command:git status*"、"exec_command:rm -rf*"）。
const ToolName = "exec_command"

type ExecTool struct {
	workingDir string
}

func NewExecTool(workingDir string) *ExecTool {
	return &ExecTool{workingDir: workingDir}
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

	if err != nil {
		if ctx.Err() == context.DeadlineExceeded {
			return result.String(), fmt.Errorf("命令执行超时（%v）", timeout)
		}
		return result.String(), fmt.Errorf("命令执行失败: %w", err)
	}

	if result.Len() == 0 {
		return "命令执行成功（无输出）", nil
	}

	return result.String(), nil
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
