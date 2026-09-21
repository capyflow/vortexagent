// 本文件提供下游项目的一站式注册入口：一条语句把 exec_command 工具注册进
// Registry，并把命令匹配器接线进权限 Checker，使带参数模式的权限规则
// （如 "exec_command:git status*"）按 shell 命令语义生效。
//
//	perms, _ := agent.NewChecker(agent.PermissionConfig{
//	    Mode:  agent.ModeConfirm,
//	    Deny:  []string{"exec_command:rm -rf*"},
//	}, nil)
//	exec.Register(registry, perms, ".")            // 工具 + 权限匹配一次接线
//	ag := agent.New(agent.Options{Permissions: perms, ...})
package exec

import (
	"fmt"

	"github.com/capyflow/vortexagent/agent"
)

// ToolName 是本工具在 Registry 中的名字，权限规则用工具名引用它
// （如 "exec_command:git status*"、"exec_command:rm -rf*"）。
const ToolName = "exec_command"

// Register 把 exec_command 工具注册进 reg，并把命令匹配器接线进 perms。
//
// perms 传 nil 表示只注册工具、不接权限匹配（工具仍可被模型调用且不受
// 参数模式规则约束——仅在确认不需要权限控制时这么做）。workdir 是命令
// 执行目录，传空串表示进程当前目录。
//
// 工具本身不携带任何审批逻辑：放行/询问/拒绝的判定统一发生在
// agent.Checker（经 agent.Options.Permissions 注入），见 docs/permissions.md。
func Register(reg *agent.Registry, perms *agent.Checker, workdir string) error {
	if reg == nil {
		return fmt.Errorf("exec: registry 不能为空")
	}
	if err := reg.Add(NewExecTool(workdir)); err != nil {
		return err
	}
	if perms != nil {
		perms.RegisterMatcher(ToolName, CommandMatcher)         // allow 语义（从严）
		perms.RegisterDenyMatcher(ToolName, CommandDenyMatcher) // deny 语义（从宽）
	}
	return nil
}
