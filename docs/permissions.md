# 工具权限系统（permissions）

> 这份文档描述 vortex 的全局工具权限层：它做什么、怎么配置（CLI 用户）、
> 怎么以库方式接入（下游开发者）、以及三个扩展点（自定义匹配器 / 确认 UI / 检查器）。
> 实现代码集中在 `agent/permission.go` 与 `tools/exec/matcher.go`。

---

## 1. 这是什么

Agent 能调用工具，就意味着模型的一句话可以变成一次 shell 执行、一次文件写入、一次
外部 API 调用。权限系统回答的问题是：**这次调用允不允许执行？要不要先问人？**

它的位置在框架核心（`agent/ask.go` 的 `execTool`）：Ask 循环每次要执行工具前，
先问权限检查器，返回放行才真正执行。因此它是**全局的**——内置工具
（`exec_command` / filesystem / memory）、MCP 工具、你自己的工具，一视同仁，
不需要每个工具自己实现审批。

设计参照 Claude Code 的权限模型：执行模式决定"没命中规则时问不问"，
allow/deny 规则做细粒度放行与拦截，deny 优先于一切。

```
模型发起 tool_calls
   │
   ▼
Skill allowed-tools 检查（skill.go，技能级白名单）
   │
   ▼
权限检查（permission.go）──── Deny ──► 错误文本回传模型（不重试），循环继续
   │                            │
   Allow                        ▼
   │                      确认 UI（confirm 模式）
   ▼
Hooks.OnBeforeToolCall（使用方钩子，仍可拦截）
   │
   ▼
Registry.Call → 工具真正执行
```

## 2. 功能总览

### 2.1 三种执行模式（Mode）

| 模式 | 行为 | 适用场景 |
|------|------|----------|
| `full_access` | 全部放行，不询问。**deny 规则仍然拦截** | 可信环境、CI |
| `confirm`（默认） | 放行规则未命中的调用，先询问用户 | 交互式终端 |
| `whitelist` | 仅 allow 规则命中的调用可执行，其余直接拒绝 | 无人值守、服务端 |

### 2.2 allow / deny 规则

规则是一个字符串：`工具名` 或 `工具名:参数模式`（按第一个冒号切分）。
工具名部分支持尾缀 `*` 通配：

| 规则 | 含义 |
|------|------|
| `read_file` | 放行/拒绝整个 `read_file` 工具（不限制参数） |
| `exec_command:git status*` | 仅当 shell 命令前缀匹配 `git status` 时命中 |
| `mcp__github` | github server 的**所有**工具（命名空间边界匹配） |
| `mcp__github__create_issue` | 单个 MCP 工具 |
| `mcp__github__*` | 同上，尾缀通配写法 |

参数模式的语义由**工具域自己解释**（见 [5.1](#51-为工具注册参数匹配器argmatcher)）：
框架不假设参数含义，`exec_command` 把模式理解为 shell 命令，文件工具可以理解为路径。
工具没有注册匹配器时，带参数模式的规则对它永远不会命中（只有纯工具名规则生效）。

MCP 工具名带命名空间：`mcp__<server>__<tool>`（server 名中非 `[A-Za-z0-9_]` 字符
清洗为 `_`）。因此规则可以精确到某个 server 或某个工具。

### 2.3 判定优先级

```
本会话已记住的允许（用户按过 a）  >  deny  >  allow  >  执行模式
```

- **deny 是绝对红线**：任何模式（含 `full_access`）下命中即拒绝。
- allow 命中 → 直接放行，不再询问。
- 都没命中 → 看模式：`full_access` 放行 / `whitelist` 拒绝 / `confirm` 询问。

### 2.4 交互确认（ApprovalUI）

`confirm` 模式下未放行的调用交给宿主注入的确认 UI。参考 CLI 的终端实现：

```
⚠ 需要确认 工具 exec_command 请求执行
  $ git push origin main
允许执行? [y]es / [n]o / [a]lways（本会话不再询问）:
```

- `y` 本次允许；`n` 拒绝；`a` 允许并**记住**——参数完全相同的后续调用不再询问
  （记忆粒度是"工具名 + 全部参数"，进程生命周期内有效）。
- 输入流结束（EOF）或会话已取消时视为拒绝。
- **无交互环境自动降级**：`ApprovalUI` 为 nil（如 vortex-serve）时，confirm 的
  询问直接按拒绝处理，拒绝原因会提示模型/用户改用 allow 规则、whitelist 模式或
  `full_access`。

### 2.5 拒绝如何回传模型

拒绝以 `nonRetryableError` 语义回传：**不会触发框架的指数退避重试**，错误文本
（含拒绝原因与"请勿重试相同调用"提示）作为 tool 消息进入历史，模型可以改用
其他方式或直接向用户说明。`Hooks.OnAfterToolCall` 会收到这次调用的错误，
可用于审计。

### 2.6 exec_command 的匹配语义（宽严刻意不同）

allow 与 deny 的失败方向相反：放行错放是事故，拦截错拦只是打扰。所以：

- **allow 从严**：复合命令（`;` `&&` `||` `|` 及换行，引号感知）拆段后**每一段**
  都要命中模式。`git status*` 放行不了 `git status; rm -rf /`。
- **deny 从宽**：对规范化后的整条命令做**词边界扫描**。藏在命令替换里的
  `echo $(rm -rf /)` 会被 `rm -rf*` 拦下；而无关词不误伤——`rm` 拦不住 `format`。

> ⚠️ 这是**字符串级过滤，不是沙箱**。它防模型误用，防不了蓄意构造的绕过
> （如 base64 解码后执行、解释器内联脚本）。高危环境请叠加操作系统级沙箱
> （容器、firejail 等）。

## 3. 参考 CLI 中的使用

### 3.1 配置文件

顶层 `permissions` 段（可选，不配置即 `confirm` 模式、无规则）：

```json
"permissions": {
  "mode": "confirm",
  "allow": [
    "read_file",
    "list_files",
    "exec_command:git status*",
    "mcp__github"
  ],
  "deny": [
    "exec_command:rm -rf*",
    "exec_command:mkfs*"
  ]
}
```

| 字段 | 说明 |
|------|------|
| `mode` | `full_access` / `confirm`（默认）/ `whitelist` |
| `allow` | 放行规则：命中的调用直接放行，不再询问 |
| `deny` | 拒绝规则：命中的调用直接拒绝，任何模式下生效 |

### 3.2 REPL 斜杠命令

| 命令 | 作用 |
|------|------|
| `/mode` | 查看当前执行模式与可选值 |
| `/mode full_access\|confirm\|whitelist` | 即时切换（仅本进程有效，不写回配置文件） |
| `/permissions` | 列出配置文件中的 allow / deny 规则 |

启动日志会打印当前模式与规则条数，便于确认权限配置已生效。

## 4. 库方式接入（下游 Go 代码）

与其他工具完全一致的惯例：**构造工具 → `registry.Add` → 注入 `Options.Permissions`**。
没有额外的接线步骤——exec_command 实现了 `agent.PermissionMatcherProvider`
自描述接口，`agent.New` 会自动把命令匹配器收集进 Checker，
带参数模式的权限规则随即按 shell 语义生效。

```go
package main

import (
    "github.com/capyflow/vortexagent/agent"
    "github.com/capyflow/vortexagent/tools/exec"
)

func main() {
    // 1. 构造权限检查器：执行模式 + 规则 + 确认 UI（无交互场景传 nil）
    perms, err := agent.NewChecker(agent.PermissionConfig{
        Mode:  agent.ModeConfirm,
        Allow: []string{"read_file", "exec_command:git status*"},
        Deny:  []string{"exec_command:rm -rf*"},
    }, myApprovalUI) // 无交互环境传 nil：confirm 未放行的调用自动拒绝
    if err != nil {
        panic(err) // 规则有错在启动时暴露，不带残缺策略运行
    }

    // 2. 像注册其他工具一样注册（工具自声明了权限匹配器，无需手工接线）
    if err := registry.Add(exec.NewExecTool(".")); err != nil { // workdir 空串=当前目录
        panic(err)
    }

    // 3. 权限检查器传入 Options，判定发生在 Ask 循环的工具分发点
    ag := agent.New(agent.Options{
        Provider:    provider,
        Registry:    registry,
        Model:       model,
        Permissions: perms, // 不设置 = 不做权限控制
    })
    _ = ag
}
```

工具名常量 `exec.ToolName`（`"exec_command"`）可用于拼接规则字符串。
注意：工具需在 `agent.New` 之前注册（收集发生在创建时），这也是框架的既有惯例。

不设置 `Permissions` 时框架行为与从前完全一致（零成本兼容）；要更省事，
也可以直接用 `config` 包加载与 CLI 同源的 JSON 配置，
把 `cfg.Permissions.Mode/Allow/Deny` 映射进 `agent.PermissionConfig`。

确认 UI 的参考实现在 `cmd/vortex/confirm.go`（终端问答），可直接照抄改造。

## 5. 扩展点

### 5.1 为工具接入参数匹配器（ArgMatcher）

自定义工具想支持 `工具名:参数模式` 规则，有两种方式：

**方式一（推荐）：实现自描述接口**，与内置工具完全一致，`registry.Add` 即完成接线：

```go
// 实现 agent.PermissionMatcherProvider（与 Overview() 渐进式披露同类的可选接口）
func (t *SendEmailTool) PermissionMatchers() (agent.ArgMatcher, agent.ArgMatcher) {
    return t.matchRecipient, nil // deny 位返回 nil 时退回 allow 匹配器
}

var _ agent.PermissionMatcherProvider = (*SendEmailTool)(nil) // 编译期断言
```

**方式二：手工注册**（动态匹配逻辑、或工具在框架外实现时）：

```go
// 签名：模式 + 本次调用参数 → 是否命中
type ArgMatcher func(pattern string, args map[string]any) bool

// 例：send_email 工具，模式匹配收件人域名
checker.RegisterMatcher("send_email", func(pattern string, args map[string]any) bool {
    to, _ := args["to"].(string)
    return strings.HasSuffix(to, pattern) // 规则: send_email:@company.com
})
```

- `RegisterMatcher`：**allow 语义**，实现应当从严（确定命中才返回 true）。
- `RegisterDenyMatcher`：**deny 语义**专用，实现应当从宽（可疑即命中）。
  未注册时 deny 规则退回 `RegisterMatcher` 的实现。
- 手工注册优先于工具自声明（自动收集发生在 `agent.New`，之后的手工调用覆盖它）。

### 5.2 自定义确认 UI（ApprovalUI）

`ApprovalUI` 是 `func(ctx, CallInfo) Decision`，适合接 GUI、企业 IM、Webhook 审批等：

```go
checker, _ := agent.NewChecker(pcfg, func(ctx context.Context, info agent.CallInfo) agent.Decision {
    // 展示 info.Tool 与 info.Args，异步等待审批人点击……
    allowed, remember := waitWebhookApproval(ctx, info) // 你的实现
    return agent.Decision{Allowed: allowed, Remember: remember}
})
```

返回 `Remember: true` 表示"这类调用以后别再问我"，Checker 会记住
（键 = 工具名 + 规范化 JSON 参数，哈希存储，进程生命周期）。
注意：Checker 在判定期间持锁，**并行的多个确认会依次弹出**而非并发轰炸——
自己实现 UI 时无需额外处理并发，但也不要在其中永久阻塞。

### 5.3 完全自定义检查器（PermissionChecker）

模式 + 规则不够用时（比如按角色、按时段、接 OPA 等策略引擎），实现接口即可，
框架只认接口：

```go
type PermissionChecker interface {
    // 返回判定与给人/模型看的拒绝原因（可为空）
    Check(ctx context.Context, info CallInfo) (Verdict, string)
}
```

`agent.Options.Permissions` 接受任何实现；`agent.Checker`（默认实现）本身也是
普通结构体，可以组合着用。

## 6. 行为细节与边界

- **检查顺序**：Skill `allowed-tools` → 权限 → `Hooks.OnBeforeToolCall` → 执行。
  被权限拒绝的调用不会到达 Hooks 的 Before 钩子；After 钩子照常触发（带错误）。
- **对所有工具一视同仁**：包括 `get_tool_schema`。使用渐进式披露模式 +
  `whitelist` 模式时，记得放行 `get_tool_schema`，否则模型拿不到工具 schema。
- **remember 的粒度**：整组参数必须完全一致（`timeout` 不同也算不同调用）。
  想做前缀级"总是允许"，请在配置里写 allow 规则，而不是靠按 `a`。
- **并发**：Checker 并发安全（内部互斥锁贯穿判定含 UI 询问），并行工具执行时
  确认提示天然串行；`SetMode` 可从其他 goroutine 安全调用（REPL 的 `/mode`）。
- **规则错误尽早失败**：`NewChecker` 对非法模式/规则返回错误（空串、缺工具名、
  纯 `*` 等），CLI 启动即退出；不要吞掉这个错误。

## 7. 测试

| 文件 | 覆盖 |
|------|------|
| `agent/permission_test.go` | 规则解析与报错、命名空间/通配匹配、优先级矩阵、full_access 下 deny 仍拦截、nil UI 降级、Remember 生效、工具自声明匹配器自动收集与手工覆盖、端到端拒绝/放行路径 |
| `tools/exec/matcher_test.go` | 命令拆分（引号/管道/后台符）、allow 前缀词边界、deny 词边界扫描与误伤用例 |
| `tools/exec/provider_test.go` | registry.Add 后权限匹配器自动接线（端到端） |
