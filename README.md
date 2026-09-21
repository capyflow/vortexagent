English | [简体中文](README.zh-CN.md)

# Vortex — A General-Purpose Agent Framework in Go

Vortex is a **general-purpose agent framework** written in Go. It provides unified LLM access,
an agent loop, a tool system, lifecycle hooks, and session storage — everything you need to build
agents for any domain: document Q&A, coding assistants, ops bots… Domain capabilities come entirely
from pluggable **tools** and prompts; the framework itself is domain-agnostic.

- **Framework core**: `llm/` (unified LLM protocol with multi-vendor adapters), `agent/` (runtime: loop / tools / sessions / hooks / storage)
- **Built-in extensions**: `tools/mcp/` (MCP client for custom tools written in any language), `tools/knowledge/` (local document knowledge base), `tools/exec/` and `tools/filesystem/` (shell execution and file access, enabled via the CLI's `tools` config)
- **Reference apps**: `cmd/vortex/` (general-purpose REPL CLI), `examples/` (framework usage examples)

The architecture borrows the layering ideas of
[earendil-works/pi](https://github.com/earendil-works/pi), a TS coding-agent toolkit with 91k stars.

> **Want to learn agent development?** The in-repo docs are written in Chinese:
> - Quick overview & getting started: [docs/usage.md](docs/usage.md) — what the project is, its strengths, and feature guides
> - Beginners: start with [docs/tutorial.md](docs/tutorial.md) — build your own agent in 10 hands-on lessons
> - Design deep-dive: [docs/architecture.md](docs/architecture.md) — framework layering principles and design decisions
> - Docs home: [docs/README.md](docs/README.md)

## Quick Start

### Option 1: Use as a library (recommended)

Import the framework packages directly and assemble an agent in four steps
(full code in `examples/minimal-agent`):

```go
provider, _ := llm.NewProvider(llm.ProviderConfig{Name: "openai", APIKey: apiKey})

registry := agent.NewRegistry()
registry.Add(&myTool{})          // just implement the agent.Tool interface

ag := agent.New(agent.Options{
    Provider: provider,
    Registry: registry,
    OnDelta:  func(d llm.Delta) { fmt.Print(d.Text) },
})

answer, _ := ag.Ask(ctx, agent.NewSession("default"), "your question")
```

### Option 2: Run the reference CLI

```bash
# 1. Configure an API key (pick one, matching provider.name in vortex.json)
#    You can also put the key in a .env file at the project root (one KEY=VALUE per line;
#    already-set environment variables take precedence)
export OPENAI_API_KEY=sk-...        # OpenAI-compatible (DeepSeek/Qwen/Zhipu, etc.)
# export ANTHROPIC_API_KEY=sk-ant-...
# export GEMINI_API_KEY=...

# 2. Run (first launch enters an interactive setup wizard; -group decides the data
#    directory ~/.vortex/my-agent/)
go run ./cmd/vortex -group my-agent
#    Prefer not to use a group? Point at a config file explicitly:
#    go run ./cmd/vortex -config path/to/agent.json
```

Interacting: type a question and press enter. `/tools` lists available tools, `/clear` wipes
history, `/exit` (or `/quit`) quits.

### Option 3: Run the examples

```bash
export OPENAI_API_KEY=sk-...
go run ./examples/minimal-agent "what time is it?"              # minimal agent (built-in time tool)
go run ./examples/knowledge-agent "which vendors are supported?" # document Q&A agent (knowledge base)
go run ./examples/customer-service-agent "my cable is broken, refund please" # support agent (sub-agent orchestration)
go run ./examples/async-agent "procure 50 printers for evaluation" # async task orchestration (fan-out + collect)
go run ./examples/mcp-server                                     # MCP server template (custom tools)
```

## Architecture

```
cmd/vortex/        Reference app: general-purpose REPL CLI (config-driven, wires up all framework capabilities)
cmd/vortex-serve/  Reference app: HTTP server mode (SSE streaming + webhooks, optional autonomous agent)
examples/          Framework usage examples (minimal / knowledge / customer-service / async / mcp-server)
├──────────────────── Framework core (the packages your app depends on) ────────────────────
llm/               Unified LLM API
  protocol.go      Unified message / tool / streaming protocol
  openai.go        OpenAI-compatible layer (DeepSeek, Qwen, Zhipu, etc.)
  anthropic.go     Anthropic Messages API (incl. thinking mode)
  gemini.go        Google Gemini API
agent/             Agent runtime
  agent.go         Agent core & configuration (Options / New / default prompt)
  ask.go           The Ask loop: LLM → tool calls → execute → repeat until the final answer
  tool.go          Tool interface & registry
  session.go       Conversation message history
  hooks.go         Lifecycle hooks (logging / telemetry / interception)
  store.go         Session storage abstraction (in-memory / JSON file implementations)
autonomous/        Autonomous agent: goal storage & scheduling (cron / interval / one-shot); runs alongside vortex-serve
config/            Config file schema & loading (agent group data isolation under ~/.vortex/<group>/)
├──────────────────── Built-in extensions (optional; register as needed) ────────────────────
tools/mcp/         MCP client: connect to MCP servers written in any language
tools/knowledge/   Local document knowledge base: directory scan + full-text keyword search tools
tools/exec/        Shell command execution tool (timeout cap, safe output truncation)
tools/filesystem/  File read/write toolset (paths confined to a root, prevents escape)
```

### The agent loop (agent/ask.go)

```
User asks → call LLM with history + tool declarations
  → tool calls returned? → execute tools (hooks may intercept) → feed results back → continue
  → final answer returned → done
```

The loop is capped at 10 rounds (`Options.MaxIterations`) to prevent runaway cycles.

## Framework capabilities

| Capability | Description |
|------|------|
| `llm.Provider` | Unified LLM interface with three built-in adapters — openai (compatible) / anthropic / gemini; switching vendors is a one-line config change |
| `agent.New` + `Ask` | Core loop: multi-turn tool scheduling, automatic history rollback on failure, empty-answer protection |
| `agent.Tool` + `Registry` | Structured interface — implement 4 methods to become a tool; duplicate-name protection and deterministic tool-declaration ordering |
| `agent.Hooks` | Lifecycle hooks: `OnMessage` / `OnLLMCall` (token usage & latency) / `OnBeforeToolCall` (can block) / `OnAfterToolCall` / `OnError` / `OnFinish` |
| `agent.Checker` | Global tool permissions: execution modes (full_access / confirm / whitelist) + allow/deny rules + interactive approval UI; every tool (incl. MCP) is intercepted in the Ask loop, and deny applies in every mode |
| `agent.NewSubagentTool` | Sub-agent primitive: wrap a derived Agent as a tool; the parent delegates a task and only receives the final answer — the foundation of multi-agent orchestration |
| `agent.TaskHub` | Async task orchestration: `task_start` dispatches background tasks (goroutine + channel concurrency) while the main agent keeps working; `task_status` / `task_wait` / `task_cancel` manage them |
| `agent.WithJSONMode` | Structured output: forces the reply to be valid JSON (native mapping on OpenAI/Gemini, system-prompt constraint on Anthropic) |
| Skill system | `SKILL.md` discovery & loading; `allowed-tools` (tool whitelist, enforced twice), `model` and `temperature` metadata all take effect |
| `agent.SessionStore` | Session persistence abstraction with in-memory and JSON file implementations; plugging in SQLite/Redis only takes 3 methods |
| `tools/mcp` | MCP client: spawns subprocess servers, auto-discovers and registers their tools |
| `tools/exec` | Built-in shell tool (exec_command): timeout & output truncation guard; self-describing permission matchers (`PermissionMatcherProvider`) — after `registry.Add`, patterned permission rules apply with shell semantics |
| `tools/knowledge` | Optional extension: document retrieval tools (`search_knowledge` / `read_document`, with path-escape protection) |

## Configuration file (reference CLI only)

An **agent group** is the recommended way to manage configuration and data isolation: a group
decides an independent local data directory `~/.vortex/<group>/` where the config, sessions,
long-term memory, autonomous goals, and context-offload files live by default:

```
~/.vortex/<group>/agent.json      config file
~/.vortex/<group>/sessions.json   session storage (when session.type=json and file is empty)
~/.vortex/<group>/memory/         long-term memory (when tools.memory.dir is empty)
~/.vortex/<group>/goals.json      autonomous goals (when autonomous.goal_store.file is empty)
~/.vortex/<group>/offload/        context offload archive (when wiring up FileSystemOffload)
```

Config resolution order: explicit `-config` path > group (`-group` at runtime or baked in at
build time) > baked-in `DefaultPath`:

```bash
# Specify a group at runtime
go run ./cmd/vortex -group my-agent
go run ./cmd/vortex-serve -group my-agent -addr :8080

# Bake the group name in at build time — the binary then needs no arguments
# The bake target is DefaultGroup in the framework's config package. Downstream projects can
# bake the same variable with their own build command, or read config.DefaultGroup in code.
go build -ldflags "-X github.com/capyflow/vortexagent/config.DefaultGroup=my-agent" -o my-agent ./cmd/vortex
./my-agent
```

**Empty path fields in the config file are resolved into the group directory automatically**;
explicitly specified paths are kept as-is (useful for intentionally sharing data across
agents). Startup fails when neither a group nor a path is given.

| Field | Description |
|------|------|
| `provider.name` | `openai` / `anthropic` / `gemini` |
| `provider.apiKeyEnv` | Environment variable holding the API key; falls back to the default name |
| `provider.baseURL` | Custom endpoint (for OpenAI-compatible vendors) |
| `provider.model` | Model name |
| `provider.thinking` | Enable thinking mode (e.g. DeepSeek R1 / Claude) |
| `knowledge` | Knowledge base root directories (optional; registers search/read tools) |
| `tools` | Built-in tool switches (optional): `tools.exec` enables shell execution, `tools.filesystem` enables file access (paths confined to a root), `tools.memory` enables long-term memory — all off by default |
| `permissions` | Global tool permissions (optional): execution mode + allow/deny rules — see "Tool permissions & approval" below |
| `mcpServers` | MCP server list; connected and their tools registered automatically at startup (optional) |
| `systemPrompt` | Custom system prompt |
| `session` | Session persistence (optional): `session.type` is `memory` (default) / `json` / `postgres`; `session.file` is the JSON storage path — with it, conversations survive restarts |

### Multi-agent deployment on one machine

Run multiple agents on one machine by giving each its own group — config, sessions, memory,
goals, and offload files stay naturally isolated under their own `~/.vortex/<group>/`
(no per-path configuration needed; the JSON session store rewrites the whole file, so sharing
a path would lose data — the group layout rules that out by construction):

```bash
vortex -group agent-a -addr :8081
vortex -group agent-b -addr :8082
```

Alternatively, build one binary per agent with the group name baked in (see above). Write
explicit paths in the config only when **intentionally sharing** a kind of data (e.g. several
agents sharing one memory store). For multiple `vortex-serve` instances, use distinct ports;
`.env` is loaded from the process working directory, so give each agent its own working directory.

## Tool permissions & approval

Every tool — built-in or MCP — passes a global permission check before each execution
(`agent.Checker`, enforced at the tool-dispatch point of the Ask loop). The execution mode
decides what happens when no rule matches:

| Mode | Behavior |
|------|------|
| `full_access` | Allow everything, no prompting (deny rules still block) |
| `confirm` (default) | Calls not matched by allow rules prompt the user: y allow / n deny / a always allow this session |
| `whitelist` | Only calls matched by allow rules run; everything else is rejected |

```json
"permissions": {
  "mode": "confirm",
  "allow": ["read_file", "list_files", "exec_command:git status*", "mcp__github"],
  "deny": ["exec_command:rm -rf*", "exec_command:mkfs*"]
}
```

- **Rule format**: `tool` or `tool:pattern` (split at the first colon); tool names support a
  trailing `*` wildcard. MCP tool names are namespaced: `mcp__<server>` covers an entire server,
  `mcp__<server>__<tool>` targets a single tool
- **exec_command patterns are interpreted as shell commands**, deliberately asymmetric:
  allow is strict — compound commands (`;` `&&` `||` `|`) must match segment by segment, so
  `git status*` cannot approve `git status; rm -rf /`; deny is loose — a word-boundary scan of
  the whole command catches `echo $(rm -rf /)` hidden inside command substitution, while
  unrelated words (like `format` vs `rm`) don't trip it
- **Precedence**: session-remembered approvals > deny > allow > mode; deny applies in every
  mode (including full_access)
- `/mode` views/switches the execution mode and `/permissions` lists rules in the REPL
- In headless environments (vortex-serve), confirm prompts automatically degrade to rejection;
  configure allow rules, the whitelist mode, or full_access for calls that must run unattended
- This is string-level filtering that guards against model misuse — it cannot stop deliberately
  crafted bypasses; pair it with an OS-level sandbox in high-risk environments

For the full feature description and integration guide (library usage, custom matchers /
approval UI / checkers), see [docs/permissions.md](docs/permissions.md) (Chinese).

## Build your own agent with the framework

1. **Write a tool**: implement the four `agent.Tool` methods (`Name / Description / Schema / Call`)
   and register it in the `Registry`. Tools are the only source of agent capabilities — the
   framework ships no business tools of its own.
2. **Integrate custom tools (MCP)**: implement an MCP server in any language (see the
   `examples/mcp-server` template) or declare one under the CLI's `mcpServers`; the agent
   discovers and calls them automatically. Off-the-shelf Python/Node MCP servers (filesystem,
   fetch, …) work too.
3. **Add a knowledge base (optional)**: register `knowledge.NewKBTools(kb)` for document
   retrieval; without it, the agent knows nothing about your documents.
4. **Observe & intercept**: use `agent.Hooks` to log messages, block dangerous tool calls
   (permission control), collect telemetry, and report errors.
5. **Persist sessions**: set `Options.Store` and every Ask auto-saves; `JSONSessionStore`
   gives "resume the last conversation after restart".
6. **Switch model vendors**: change `Name` in `llm.NewProvider` — the loop and tools stay untouched.
7. **Multi-agent orchestration (sub-agents)**: build one Agent per specialist scenario (its own
   system prompt and toolbox) and register it into the dispatcher agent's Registry with
   `agent.NewSubagentTool` (synchronous delegation, result in the same turn) or `agent.TaskHub`
   (async dispatch, results collected later) — this is how support-desk and coding-agent
   hub-and-spoke structures are derived (see `examples/customer-service-agent` and
   `examples/async-agent`).
8. **Reuse config loading (optional)**: the `config` package exposes the same schema the
   reference CLI uses — `config.LoadConfig(path)` parses straight into `config.Config`
   (provider / session / tools / autonomous, etc.), with `config.ExpandPath`,
   `config.LoadDotEnv` and `config.GoalFromConfig` alongside. For data isolation use groups:
   bake `config.DefaultGroup` at build time, resolve `~/.vortex/<group>/agent.json` via
   `config.ResolveConfigPath`, then let `cfg.ResolveGroupDefaults(group)` fill the empty
   session/memory/goal/offload paths into the group directory.

## Development

```bash
go build ./...            # build
go test ./...             # all tests (incl. end-to-end integration tests; no real API needed)
go run ./cmd/vortex -config vortex/deploy_agent/my-agent.json   # run the reference CLI locally
```

### Testing strategy

- `llm/*_test.go`: httptest mocks of each vendor API — request serialization & response parsing
- `agent/loop_test.go`: fake provider covering the tool loop, rollback, empty answers, etc.
- `agent/hooks_test.go`: hook ordering and interception behavior
- `agent/store_test.go`: session storage CRUD, snapshot semantics, JSON round-trips
- `agent/e2e_test.go`: full-stack end-to-end (mock OpenAI + knowledge base + tool loop)
- `tools/mcp/client_test.go`: spawns a real subprocess MCP server to verify the protocol handshake

## Roadmap

- [ ] Context compaction (summarize over-long history; the counterpart of pi's agent-harness)
- [ ] Vector retrieval extension (replacing knowledge's internals, or as a new extension)
- [ ] Multi-session management & branching
- [ ] RPC / server mode (in-process integration)
- [ ] More built-in extensions (shell, http, filesystem, …)

## License

MIT
