// Package mcp 实现 Model Context Protocol（MCP）客户端。
//
// MCP（https://modelcontextprotocol.io）为 agent 提供统一协议，用于自动发现
// 并调用用户自定义的 MCP server（任意语言编写）暴露的工具。本包负责：
//   - 启动外部 MCP server 子进程（stdio 传输）并完成 initialize 握手
//   - 通过 tools/list 拉取工具声明，包装为统一的 Tool 接口
//   - 通过 tools/call 执行工具调用，返回文本结果
//
// 用户只需在配置文件中声明 server 的命令、参数与环境变量，agent 即可自动
// 发现并调用其工具，是"用户自定义功能"的核心入口。
package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"sync"
	"time"

	"github.com/mark3labs/mcp-go/client"
	"github.com/mark3labs/mcp-go/client/transport"
	mcp "github.com/mark3labs/mcp-go/mcp"

	vllm "github.com/capyflow/vortexagent/llm"
)

// handshakeTimeout 是 initialize 握手的最长等待时间；期间 server 无响应即报错。
const handshakeTimeout = 30 * time.Second

// clientInfo 是握手时向 server 声明的客户端信息。
var clientInfo = mcp.Implementation{Name: "vortexagent", Version: "0.1.0"}

// ServerConfig 描述一个用户自定义 MCP server 的进程配置（从配置文件加载）。
type ServerConfig struct {
	Name    string            // 唯一名称，用于日志与错误上下文
	Command string            // 可执行文件路径（任意语言编写的 MCP server）
	Args    []string          // 启动参数
	Env     map[string]string // 注入子进程的环境变量（会继承父进程环境）
}

// Client 管理一个 MCP server 子进程的连接与工具调用。
type Client struct {
	name         string             // server 名称（来自 ServerConfig）
	mcp          *client.Client     // 底层 mcp-go 客户端
	cancel       context.CancelFunc // 取消后终止子进程（exec.CommandContext）
	supportTools bool               // 握手后 server 是否声明了 tools 能力
	tools        []*Tool            // Connect 时拉取并包装好的工具列表
	once         sync.Once          // 保证 Close 只执行一次
	closeErr     error              // Close 的结果，供多次调用时复用
}

// Connect 启动 MCP server 子进程并完成 initialize 握手与 tools/list 拉取。
//
// ctx 控制握手超时与取消；任何失败都会清理已启动的子进程。返回的 Client
// 使用完毕必须调用 Close 释放子进程。
func Connect(ctx context.Context, cfg ServerConfig) (*Client, error) {
	if cfg.Command == "" {
		return nil, fmt.Errorf("mcp %s: 未配置 Command", cfg.Name)
	}

	// 派生可取消上下文：Close 时调用 cancel 即向子进程发送终止信号
	procCtx, cancel := context.WithCancel(ctx)
	mcpClient := client.NewClient(transport.NewStdioWithOptions(
		cfg.Command, envSlice(cfg.Env), cfg.Args,
	))
	c := &Client{name: cfg.Name, mcp: mcpClient, cancel: cancel}

	// 启动子进程并建立 stdio 管道
	if err := mcpClient.Start(procCtx); err != nil {
		c.Close() // 清理已建立的管道
		return nil, fmt.Errorf("mcp %s: 启动 stdio 传输失败: %w", cfg.Name, err)
	}

	// 握手超时兜底：即使调用方 ctx 无截止时间，也保证握手有限时
	handshakeCtx, cancelHandshake := context.WithTimeout(ctx, handshakeTimeout)
	defer cancelHandshake()

	if err := c.initialize(handshakeCtx); err != nil {
		c.Close()
		return nil, err
	}

	tools, err := c.listTools(handshakeCtx)
	if err != nil {
		c.Close()
		return nil, err
	}
	c.tools = tools
	return c, nil
}

// initialize 完成 MCP initialize 握手并记录 server 的工具能力。
//
// 注：MCP 协议中 tools 是 server 能力（ServerCapabilities.Tools），客户端
// capabilities 无需声明；握手后根据 server 返回值判断其是否支持工具。
func (c *Client) initialize(ctx context.Context) error {
	req := mcp.InitializeRequest{}
	req.Params.ProtocolVersion = mcp.LATEST_PROTOCOL_VERSION
	req.Params.ClientInfo = clientInfo
	req.Params.Capabilities = mcp.ClientCapabilities{}

	result, err := c.mcp.Initialize(ctx, req)
	if err != nil {
		return fmt.Errorf("mcp %s: initialize 握手失败: %w", c.name, err)
	}
	c.supportTools = result.Capabilities.Tools != nil
	return nil
}

// listTools 通过 tools/list 拉取工具声明并包装为 Tool 列表。
func (c *Client) listTools(ctx context.Context) ([]*Tool, error) {
	if !c.supportTools {
		return nil, nil // server 未声明工具能力：视为无工具
	}
	result, err := c.mcp.ListTools(ctx, mcp.ListToolsRequest{})
	if err != nil {
		return nil, fmt.Errorf("mcp %s: tools/list 失败: %w", c.name, err)
	}
	tools := make([]*Tool, 0, len(result.Tools))
	for _, mt := range result.Tools {
		tools = append(tools, &Tool{
			name:        mt.Name,
			description: mt.Description,
			schema:      inputSchemaToMap(mt.InputSchema),
			client:      c.mcp,
		})
	}
	return tools, nil
}

// Tools 返回该 server 暴露的所有工具（Connect 时已拉取）。
func (c *Client) Tools() []*Tool { return c.tools }

// Close 终止 MCP server 子进程并释放连接资源。
//
// 先取消进程上下文（exec.CommandContext 会向子进程发送终止信号），再关闭
// stdio 管道并等待进程退出，避免残留僵尸进程。Close 可安全地多次调用。
func (c *Client) Close() error {
	c.once.Do(func() {
		c.cancel()
		err := c.mcp.Close()
		// cancel 导致的进程退出属于预期的关闭路径，不算错误：
		//   - 进程被终止时 Wait 返回 *exec.ExitError（signal: killed）
		//   - exec.CommandContext 在 ctx 已取消且进程已终止时返回 context.Canceled
		var exitErr *exec.ExitError
		if err != nil && !errors.Is(err, context.Canceled) && !errors.As(err, &exitErr) {
			c.closeErr = err
		}
	})
	return c.closeErr
}

// Tool 包装单个 MCP 工具，方法签名与 agent.Tool 接口一致。
//
// Go 接口是结构化的：本包不 import agent 包，只要方法签名匹配即可被
// agent 的 Tool 接口（Name/Description/Schema/Call）接受。
type Tool struct {
	name        string         // 工具名称
	description string         // 工具说明，模型据此决定是否调用
	schema      map[string]any // 参数 JSON Schema（tools/list 的 inputSchema 原样保留）
	client      *client.Client // 所属 MCP 客户端，用于 tools/call
}

// Name 返回工具名称。
func (t *Tool) Name() string { return t.name }

// Description 返回工具说明。
func (t *Tool) Description() string { return t.description }

// Schema 返回参数 JSON Schema（object 类型），供模型生成参数。
func (t *Tool) Schema() map[string]any { return t.schema }

// Call 通过 tools/call 调用 MCP server 执行工具，返回拼接后的文本结果。
//
// 并发安全：底层 mcp-go 客户端为每个请求分配独立的原子请求 ID 并维护独立
// 的响应通道，天然支持并发调用，故此处无需额外加锁。
func (t *Tool) Call(ctx context.Context, args map[string]any) (string, error) {
	result, err := t.client.CallTool(ctx, mcp.CallToolRequest{
		Params: mcp.CallToolParams{Name: t.name, Arguments: args},
	})
	if err != nil {
		return "", fmt.Errorf("mcp 工具 %s: 调用失败: %w", t.name, err)
	}
	text := concatContent(result.Content)
	if result.IsError {
		// 工具执行错误以 isError 标记返回（而非协议错误），需转为 error 上报
		return "", fmt.Errorf("mcp 工具 %s: 执行返回错误: %s", t.name, text)
	}
	return text, nil
}

// inputSchemaToMap 将 MCP 工具声明的 inputSchema 原样转换为 map 形式，
// 作为 Tool.Schema 暴露给 LLM（llm.ToolParam.Schema 即 map[string]any）。
func inputSchemaToMap(schema mcp.ToolInputSchema) map[string]any {
	data, err := json.Marshal(schema)
	if err != nil {
		return map[string]any{"type": "object"} // 兜底：数据来自 JSON 反序列化，理论不可达
	}
	var m map[string]any
	if err := json.Unmarshal(data, &m); err != nil {
		return map[string]any{"type": "object"} // 兜底：同上
	}
	return m
}

// concatContent 将 tools/call 结果的 content 数组拼接为一段文本。
// text 类型取 text 字段；image 等非文本类型给出忽略提示。
func concatContent(contents []mcp.Content) string {
	var sb strings.Builder
	for _, content := range contents {
		if sb.Len() > 0 {
			sb.WriteByte('\n')
		}
		switch c := content.(type) {
		case mcp.TextContent:
			sb.WriteString(c.Text)
		case mcp.ImageContent:
			fmt.Fprintf(&sb, "[image 内容已忽略: mimeType=%s]", c.MIMEType)
		default:
			sb.WriteString("[非文本内容已忽略]")
		}
	}
	return sb.String()
}

// ToToolParams 将 MCP 工具列表转换为 LLM 层统一使用的工具声明
// （llm.ToolParam），供 Chat 请求暴露给模型。
func ToToolParams(tools []*Tool) []vllm.ToolParam {
	params := make([]vllm.ToolParam, 0, len(tools))
	for _, t := range tools {
		params = append(params, vllm.ToolParam{
			Name:        t.name,
			Description: t.description,
			Schema:      t.schema,
		})
	}
	return params
}

// envSlice 将 map 形式的注入环境变量转换为 exec.Cmd 需要的 "KEY=VALUE" 列表，
// 追加到子进程继承的父环境之上（mcp-go stdio 传输的默认行为）。
func envSlice(env map[string]string) []string {
	if len(env) == 0 {
		return nil
	}
	slice := make([]string, 0, len(env))
	for k, v := range env {
		slice = append(slice, k+"="+v)
	}
	return slice
}
