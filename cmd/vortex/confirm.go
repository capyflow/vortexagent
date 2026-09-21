// 本文件实现 CLI 的交互确认 UI（agent.ApprovalUI）：工具调用触发权限询问时，
// 在终端展示调用详情并等待用户答复 y / n / a。
//
// 提示输出走 stderr：stdout 上是模型的流式回复，确认提示不应与之混排。
package main

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"

	"github.com/capyflow/vortexagent/agent"
)

// approvalArgsLimit 限制确认界面展示的参数长度，超长截断。
const approvalArgsLimit = 600

// terminalApprovalUI 返回基于终端的确认 UI。workdir 仅用于展示 exec_command
// 的执行目录（与工具的实际工作目录保持一致，由调用方传入）。
func terminalApprovalUI(workdir string) agent.ApprovalUI {
	return func(ctx context.Context, info agent.CallInfo) agent.Decision {
		fmt.Fprintf(os.Stderr, "\n\033[33m⚠ 需要确认\033[0m 工具 %s 请求执行\n", info.Tool)
		fmt.Fprintf(os.Stderr, "%s\n", describeArgs(info))

		reader := bufio.NewReader(os.Stdin)
		for {
			if ctx.Err() != nil {
				return agent.Decision{} // 会话已取消：视为拒绝
			}
			fmt.Fprint(os.Stderr, "允许执行? [y]es / [n]o / [a]lways（本会话不再询问）: ")
			line, err := reader.ReadString('\n')
			if err != nil {
				fmt.Fprintln(os.Stderr)
				return agent.Decision{} // 输入流结束（EOF 等）：视为拒绝
			}
			switch strings.ToLower(strings.TrimSpace(line)) {
			case "y", "yes":
				return agent.Decision{Allowed: true}
			case "n", "no":
				return agent.Decision{}
			case "a", "always":
				return agent.Decision{Allowed: true, Remember: true}
			default:
				fmt.Fprintln(os.Stderr, "请输入 y、n 或 a")
			}
		}
	}
}

// describeArgs 生成展示用的参数描述：exec_command 展示命令本身，
// 其余工具展示截断后的 JSON。
func describeArgs(info agent.CallInfo) string {
	if info.Tool == "exec_command" {
		if cmd, _ := info.Args["command"].(string); cmd != "" {
			return fmt.Sprintf("  $ %s", truncateLines(cmd, approvalArgsLimit))
		}
	}
	b, err := json.Marshal(info.Args)
	if err != nil {
		return "  (参数无法展示)"
	}
	s := string(b)
	if len(s) > approvalArgsLimit {
		s = s[:approvalArgsLimit] + " ...（已截断）"
	}
	return fmt.Sprintf("  参数: %s", s)
}

// truncateLines 把多行命令压成缩进的单行展示并限制长度。
func truncateLines(s string, max int) string {
	s = strings.ReplaceAll(strings.TrimSpace(s), "\n", " ⏎ ")
	if len(s) > max {
		s = s[:max] + " ...（已截断）"
	}
	return s
}
