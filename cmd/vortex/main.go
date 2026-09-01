// Command vortex 是基于本框架的参考 CLI（reference app）：一个通用 REPL agent。
//
// 它演示了如何用框架组装一个完整的 agent 应用：配置驱动、可选扩展
// （知识库 / MCP 工具）、可选会话持久化（重启后继续上次对话）。
// 你自己的应用可以直接以库的方式使用框架（见 examples/ 下的最小示例）。
//
// 交互模式：输入问题回车提问，支持以下斜杠命令：
//
//	/help    显示帮助
//	/tools   列出当前可用的工具
//	/clear   清空当前会话历史
//	/exit    退出
package main

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"time"

	"github.com/capyflow/vortexagent/agent"
	"github.com/capyflow/vortexagent/agent/sessionstore"
	"github.com/capyflow/vortexagent/llm"
	"github.com/capyflow/vortexagent/tools/exec"
	"github.com/capyflow/vortexagent/tools/filesystem"
	"github.com/capyflow/vortexagent/tools/knowledge"
	"github.com/capyflow/vortexagent/tools/mcp"
	"github.com/capyflow/vortexagent/tools/memory"
)

func main() {
	configPath := flag.String("config", "", "配置文件路径（默认 ~/.vortex/agent.json）")
	flag.Parse()

	if *configPath == "" {
		home, _ := os.UserHomeDir()
		*configPath = filepath.Join(home, ".vortex", "agent.json")
	}

	configDir := filepath.Dir(*configPath)
	if err := os.MkdirAll(configDir, 0o755); err != nil {
		fmt.Fprintln(os.Stderr, "错误: 创建配置目录失败:", err)
		os.Exit(1)
	}

	if _, err := os.Stat(*configPath); os.IsNotExist(err) {
		cfg := interactiveSetup()
		if err := saveConfig(*configPath, cfg); err != nil {
			fmt.Fprintln(os.Stderr, "错误: 保存配置失败:", err)
			os.Exit(1)
		}
		fmt.Printf("配置已保存到 %s\n", *configPath)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	cfg, err := LoadConfig(*configPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, "错误:", err)
		os.Exit(1)
	}

	// 0. 加载 .env（若存在）：填充未设置的环境变量，不覆盖已有值
	if err := loadDotEnv(".env"); err != nil {
		fmt.Fprintln(os.Stderr, "警告: 加载 .env 失败:", err)
	}

	// 1. 创建 LLM provider
	var apiKey string
	if cfg.Provider.APIKey != "" {
		apiKey = cfg.Provider.APIKey
	} else if cfg.Provider.APIKeyEnv != "" {
		apiKey = os.Getenv(cfg.Provider.APIKeyEnv)
		if apiKey == "" {
			apiKey = os.Getenv(defaultAPIKeyEnv(cfg.Provider.Name))
		}
	} else {
		apiKey = os.Getenv(defaultAPIKeyEnv(cfg.Provider.Name))
	}
	if apiKey == "" {
		fmt.Fprintln(os.Stderr, "错误: 未找到 API 密钥，请在配置文件中填写 apiKey 或设置环境变量")
		os.Exit(1)
	}
	provider, err := llm.NewProvider(llm.ProviderConfig{
		Name:        cfg.Provider.Name,
		APIKey:      apiKey,
		BaseURL:     cfg.Provider.BaseURL,
		Model:       cfg.Provider.Model,
		MaxTokens:   cfg.Provider.MaxTokens,
		Temperature: cfg.Provider.Temperature,
	})
	if err != nil {
		fmt.Fprintln(os.Stderr, "错误:", err)
		os.Exit(1)
	}

	// 2. 注册工具：内置工具（可选）+ 知识库 + MCP
	registry := agent.NewRegistry()
	if cfg.Tools != nil {
		registerBuiltinTools(registry, cfg.Tools)
	}
	if len(cfg.Knowledge) > 0 {
		kb := knowledge.NewKB(cfg.Knowledge)
		for _, t := range knowledge.NewKBTools(kb) {
			if err := registry.Add(t); err != nil {
				fmt.Fprintln(os.Stderr, "警告:", err)
			}
		}
		fmt.Printf("已加载知识库: %s\n", strings.Join(cfg.Knowledge, ", "))
	}

	var mcpClients []*mcp.Client
	for _, srv := range cfg.MCPServers {
		client, err := mcp.Connect(ctx, mcp.ServerConfig{
			Name:    srv.Name,
			Command: srv.Command,
			Args:    srv.Args,
			Env:     srv.Env,
		})
		if err != nil {
			fmt.Fprintf(os.Stderr, "警告: 连接 MCP server %s 失败: %v\n", srv.Name, err)
			continue
		}
		mcpClients = append(mcpClients, client)
		for _, t := range client.Tools() {
			if err := registry.Add(t); err != nil {
				fmt.Fprintf(os.Stderr, "警告: 注册 MCP 工具 %s 失败: %v\n", t.Name(), err)
			}
		}
		fmt.Printf("已连接 MCP server %s（%d 个工具）\n", srv.Name, len(client.Tools()))
	}
	defer func() {
		for _, c := range mcpClients {
			c.Close()
		}
	}()

	// 3. 会话存储
	session := sessionstore.NewSession(cfg.Provider.Model)
	var store sessionstore.Store
	var lockManager *sessionstore.LockManager

	switch cfg.Session.Type {
	case "postgres":
		if cfg.Session.Postgres == nil {
			fmt.Fprintln(os.Stderr, "错误: session.type=postgres 但未配置 session.postgres")
			os.Exit(1)
		}
		pgStore, perr := sessionstore.NewPostgres(cfg.Session.Postgres.DSN, generateDeviceID())
		if perr != nil {
			fmt.Fprintln(os.Stderr, "错误:", perr)
			os.Exit(1)
		}
		store = pgStore

		ttl := 30 * time.Second
		if cfg.Session.Postgres.LockTTL > 0 {
			ttl = time.Duration(cfg.Session.Postgres.LockTTL) * time.Second
		}
		renewal := 10 * time.Second
		if cfg.Session.Postgres.LockRenewal > 0 {
			renewal = time.Duration(cfg.Session.Postgres.LockRenewal) * time.Second
		}
		lockManager = sessionstore.NewLockManager(pgStore, session.ID, ttl, renewal)

		fmt.Printf("已连接 PostgreSQL（锁 TTL: %v, 续期间隔: %v）\n", ttl, renewal)
	case "json":
		if cfg.Session.File == "" {
			fmt.Fprintln(os.Stderr, "错误: session.type=json 但未配置 session.file")
			os.Exit(1)
		}
		sessionFile := expandPath(cfg.Session.File)
		jsonStore, jerr := sessionstore.NewJSON(sessionFile)
		if jerr != nil {
			fmt.Fprintln(os.Stderr, "错误:", jerr)
			os.Exit(1)
		}
		store = jsonStore
		prev, lerr := jsonStore.LoadLatest(ctx)
		if lerr != nil {
			fmt.Fprintf(os.Stderr, "警告: 恢复会话失败: %v\n", lerr)
		} else if prev != nil {
			session = prev
			fmt.Printf("已恢复上次会话（%d 条历史）\n", len(session.Messages()))
		}
	default:
		store = sessionstore.NewMemory()
	}

	// 4. 创建 agent 并进入交互循环
	var firstDelta bool = true
	ag := agent.New(agent.Options{
		Provider:     provider,
		Registry:     registry,
		Model:        cfg.Provider.Model,
		Thinking:     cfg.Provider.Thinking,
		MaxTokens:    cfg.Provider.MaxTokens,
		SystemPrompt: cfg.SystemPrompt,
		Store:        store,
		OnDelta: func(d llm.Delta) {
			if d.Thinking != "" {
				fmt.Fprintf(os.Stderr, "\033[90m%s\033[0m", d.Thinking)
				return
			}
			if firstDelta {
				fmt.Println()
				firstDelta = false
			}
			fmt.Print(d.Text)
		},
	})

	fmt.Printf("vortex 通用 agent 已启动（provider=%s, model=%s, 工具: %s）\n",
		provider.Name(), modelName(cfg), strings.Join(registry.Names(), ", "))
	fmt.Println("输入问题开始对话，/help 查看命令。")

	if lockManager != nil {
		acquired, lerr := lockManager.Acquire(ctx)
		if lerr != nil {
			fmt.Fprintf(os.Stderr, "警告: 获取会话锁失败: %v\n", lerr)
		} else if !acquired {
			fmt.Println("警告: 会话已被其他设备锁定，当前为只读模式")
		} else {
			fmt.Println("已获取会话锁（自动续期中）")
		}
		defer func() {
			if lockManager.IsHeld() {
				lockManager.Release(context.Background())
				fmt.Println("已释放会话锁")
			}
		}()
	}

	scanner := bufio.NewScanner(os.Stdin)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	exit := false
	for {
		// Ctrl+C（SIGINT）会取消 ctx：中断当前请求后直接结束会话。
		// 注意 signal.NotifyContext 会一直拦截信号，若不在此退出，
		// 后续 Ctrl+C 无法再触发默认终止行为，用户会被困在 REPL 里。
		if ctx.Err() != nil {
			fmt.Println()
			break
		}
		fmt.Print("\n> ")
		if !scanner.Scan() {
			break
		}
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		if handleCommand(line, session, store, registry, &exit) {
			if exit {
				break
			}
			continue
		}

		fmt.Println()
		_, err := ag.Ask(ctx, session, line)
		fmt.Println()
		if err != nil {
			fmt.Fprintf(os.Stderr, "\n错误: %v\n", err)
		}
	}
	if err := scanner.Err(); err != nil {
		fmt.Fprintln(os.Stderr, "读取输入失败:", err)
	}
}

// registerBuiltinTools 按配置注册内置工具（exec / filesystem）。
func registerBuiltinTools(registry *agent.Registry, cfg *ToolsConfig) {
	if cfg.Exec != nil && cfg.Exec.Enabled {
		workdir := cfg.Exec.Workdir
		if workdir == "" {
			workdir = "."
		}
		if err := registry.Add(exec.NewExecTool(workdir)); err != nil {
			fmt.Fprintln(os.Stderr, "警告:", err)
		} else {
			fmt.Printf("已启用内置工具: exec_command（工作目录 %s，建议配合权限钩子限制命令）\n", workdir)
		}
	}
	if cfg.Filesystem != nil && cfg.Filesystem.Enabled {
		root := cfg.Filesystem.Root
		if root == "" {
			root = "."
		}
		tools := []agent.Tool{
			filesystem.NewReadFileTool(root),
			filesystem.NewWriteFileTool(root),
			filesystem.NewEditFileTool(root),
			filesystem.NewListFilesTool(root),
		}
		for _, t := range tools {
			if err := registry.Add(t); err != nil {
				fmt.Fprintln(os.Stderr, "警告:", err)
			}
		}
		fmt.Printf("已启用内置工具: read_file / write_file / edit_file / list_files（根目录 %s）\n", root)
	}
	if cfg.Memory != nil && cfg.Memory.Enabled {
		dir := expandPath(cfg.Memory.Dir)
		if dir == "" {
			home, _ := os.UserHomeDir()
			dir = filepath.Join(home, ".vortex", "memory")
		}
		memStore, err := memory.NewJSONStore(filepath.Join(dir, "memories.json"))
		if err != nil {
			fmt.Fprintln(os.Stderr, "警告: 启用长期记忆失败:", err)
		} else {
			for _, t := range memory.NewTools(memStore, nil) {
				if err := registry.Add(t); err != nil {
					fmt.Fprintln(os.Stderr, "警告:", err)
				}
			}
			fmt.Printf("已启用内置工具: 长期记忆六件套（存储 %s）\n", filepath.Join(dir, "memories.json"))
		}
	}
}

// handleCommand 处理斜杠命令，返回 (是否已处理, 是否退出)。
func handleCommand(line string, session *sessionstore.Session, store sessionstore.Store, registry *agent.Registry, exit *bool) bool {
	switch {
	case line == "/exit" || line == "/quit":
		*exit = true
		return true
	case line == "/help":
		fmt.Println("命令: /help 帮助  /tools 工具列表  /clear 清空历史")
		fmt.Println("      /sessions 会话列表  /new 新建会话  /switch ID 切换会话  /delete ID 删除会话")
		fmt.Println("      /exit 退出")
		return true
	case line == "/tools":
		if len(registry.Names()) == 0 {
			fmt.Println("当前没有可用工具")
			return true
		}
		for _, name := range registry.Names() {
			t, _ := registry.Get(name)
			fmt.Printf("  %s - %s\n", name, t.Description())
		}
		return true
	case line == "/clear":
		session.Clear()
		fmt.Println("会话历史已清空")
		return true
	case line == "/sessions":
		handleSessions(session, store)
		return true
	case line == "/new":
		handleNewSession(session, store)
		return true
	case strings.HasPrefix(line, "/switch "):
		id := strings.TrimSpace(strings.TrimPrefix(line, "/switch"))
		handleSwitchSession(session, store, id)
		return true
	case strings.HasPrefix(line, "/delete "):
		id := strings.TrimSpace(strings.TrimPrefix(line, "/delete"))
		handleDeleteSession(session, store, id)
		return true
	}
	return false
}

// handleSessions 列出所有会话。
func handleSessions(current *sessionstore.Session, store sessionstore.Store) {
	if store == nil {
		fmt.Println("未启用会话持久化（配置 sessionFile 才能管理多个会话）")
		return
	}
	ctx := context.Background()
	sessions, err := store.List(ctx)
	if err != nil {
		fmt.Fprintf(os.Stderr, "获取会话列表失败: %v\n", err)
		return
	}
	if len(sessions) == 0 {
		fmt.Println("暂无会话")
		return
	}
	fmt.Printf("共 %d 个会话:\n", len(sessions))
	for _, s := range sessions {
		marker := "  "
		if s.ID == current.ID {
			marker = "→ "
		}
		fmt.Printf("%s%s  模型: %s  消息: %d  更新: %s\n",
			marker, s.ID, s.Model, len(s.History),
			s.UpdatedAt.Format("2006-01-02 15:04:05"))
	}
}

// handleNewSession 创建新会话并切换。
func handleNewSession(current *sessionstore.Session, store sessionstore.Store) {
	newSess := sessionstore.NewSession(current.Model)
	if store != nil {
		if err := store.Save(context.Background(), newSess); err != nil {
			fmt.Fprintf(os.Stderr, "保存新会话失败: %v\n", err)
			return
		}
	}
	current.Reset(newSess)
	fmt.Printf("已创建新会话: %s\n", current.ID)
}

// handleSwitchSession 切换到指定会话。
func handleSwitchSession(current *sessionstore.Session, store sessionstore.Store, id string) {
	if store == nil {
		fmt.Println("未启用会话持久化")
		return
	}
	if id == "" {
		fmt.Println("用法: /switch <会话ID>")
		return
	}
	sess, err := store.Load(context.Background(), id)
	if err != nil {
		fmt.Fprintf(os.Stderr, "加载会话失败: %v\n", err)
		return
	}
	if sess == nil {
		fmt.Printf("会话不存在: %s\n", id)
		return
	}
	current.Reset(sess)
	fmt.Printf("已切换到会话: %s（%d 条历史）\n", current.ID, len(current.History))
}

// handleDeleteSession 删除指定会话。
func handleDeleteSession(current *sessionstore.Session, store sessionstore.Store, id string) {
	if store == nil {
		fmt.Println("未启用会话持久化")
		return
	}
	if id == "" {
		fmt.Println("用法: /delete <会话ID>")
		return
	}
	if id == current.ID {
		fmt.Println("不能删除当前会话，请先切换到其他会话")
		return
	}
	if err := store.Delete(context.Background(), id); err != nil {
		fmt.Fprintf(os.Stderr, "删除会话失败: %v\n", err)
		return
	}
	fmt.Printf("已删除会话: %s\n", id)
}

// modelName 返回展示用的模型名。
func modelName(cfg *Config) string {
	if cfg.Provider.Model != "" {
		return cfg.Provider.Model
	}
	return "默认"
}

// generateDeviceID 生成唯一的设备标识符。
func generateDeviceID() string {
	hostname, _ := os.Hostname()
	var b [4]byte
	rand.Read(b[:])
	return fmt.Sprintf("%s-%s", hostname, hex.EncodeToString(b[:]))
}

func expandPath(path string) string {
	if strings.HasPrefix(path, "~") {
		home, _ := os.UserHomeDir()
		return filepath.Join(home, path[1:])
	}
	return path
}

func interactiveSetup() *Config {
	scanner := bufio.NewScanner(os.Stdin)

	fmt.Println("=== Vortex Agent 配置向导 ===")
	fmt.Println()

	fmt.Println("选择 API 接口规范:")
	fmt.Println("  1. OpenAI 兼容（适用于 DeepSeek/Qwen/智谱/Moonshot 等国内厂商）")
	fmt.Println("  2. Anthropic 原生接口")
	fmt.Println("  3. Gemini 原生接口")
	fmt.Print("请输入 (1-3, 默认 1): ")
	scanner.Scan()
	providerChoice := strings.TrimSpace(scanner.Text())
	if providerChoice == "" {
		providerChoice = "1"
	}

	var providerName string
	switch providerChoice {
	case "2":
		providerName = "anthropic"
	case "3":
		providerName = "gemini"
	default:
		providerName = "openai"
	}

	fmt.Printf("\n当前接口规范: %s\n", providerName)

	var apiKeyEnv, model, baseURL string

	for {
		fmt.Print("\nAPI 密钥环境变量名 (默认 ")
		fmt.Print(defaultAPIKeyEnv(providerName))
		fmt.Print("): ")
		scanner.Scan()
		apiKeyEnv = strings.TrimSpace(scanner.Text())
		if apiKeyEnv == "" {
			apiKeyEnv = defaultAPIKeyEnv(providerName)
		}

		fmt.Print("\n模型名称: ")
		scanner.Scan()
		model = strings.TrimSpace(scanner.Text())

		if providerName == "openai" {
			fmt.Print("\nAPI 地址 (如 https://api.deepseek.com): ")
		} else {
			fmt.Print("\nAPI 地址: ")
		}
		scanner.Scan()
		baseURL = strings.TrimSpace(scanner.Text())

		if model == "" || baseURL == "" {
			fmt.Println("\n错误: 模型名称和 API 地址不能为空，请重新填写")
			continue
		}

		break
	}

	home, _ := os.UserHomeDir()
	defaultSessionFile := filepath.Join(home, ".vortex", "sessions.json")

	cfg := &Config{}
	cfg.Provider.Name = providerName
	cfg.Provider.APIKeyEnv = apiKeyEnv
	cfg.Provider.Model = model
	cfg.Provider.BaseURL = baseURL
	cfg.Session.Type = "json"
	cfg.Session.File = defaultSessionFile

	return cfg
}

func saveConfig(path string, cfg *Config) error {
	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, data, 0o600)
}
