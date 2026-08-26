package exec

import (
	"bytes"
	"context"
	"fmt"
	"os/exec"
	"strings"
	"time"
)

const (
	DefaultTimeout = 30 * time.Second
	MaxOutputSize  = 100 * 1024 // 100KB
)

type ExecTool struct {
	workingDir string
}

func NewExecTool(workingDir string) *ExecTool {
	return &ExecTool{workingDir: workingDir}
}

func (t *ExecTool) Name() string { return "exec_command" }

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
		output := stdout.String()
		if len(output) > MaxOutputSize {
			output = output[:MaxOutputSize] + "\n... (输出已截断)"
		}
		result.WriteString(output)
	}
	if stderr.Len() > 0 {
		output := stderr.String()
		if len(output) > MaxOutputSize {
			output = output[:MaxOutputSize] + "\n... (输出已截断)"
		}
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
