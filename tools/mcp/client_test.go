package mcp

import (
	"context"
	"fmt"
	"io"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"

	vllm "github.com/capyflow/vortexagent/llm"
)

// connectCtx 返回一个 30 秒超时的上下文，避免测试挂起。
func connectCtx() (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), 30*time.Second)
}

// helperConfig 构造以 helper-process 模式拉起测试 MCP server 的配置。
//
// mode 为 "stdio" 时 server 正常应答；为 "silent" 时只读 stdin 不应答，
// 用于握手超时测试。
func helperConfig(mode string) ServerConfig {
	return ServerConfig{
		Name:    "test-server",
		Command: os.Args[0],
		Args:    []string{"-test.run=TestHelperProcess"},
		Env: map[string]string{
			"GO_WANT_HELPER_PROCESS": "1",
			"MCP_HELPER_MODE":        mode,
		},
	}
}

// TestHelperProcess 是经典的 helper-process 测试模式：本测试二进制以
// -test.run=TestHelperProcess 被被测客户端以子进程方式拉起；当环境变量
// GO_WANT_HELPER_PROCESS=1 时进入 MCP server 角色，通过 stdio 提供协议服务。
func TestHelperProcess(t *testing.T) {
	if os.Getenv("GO_WANT_HELPER_PROCESS") != "1" {
		return // 常规测试运行，直接返回
	}
	switch os.Getenv("MCP_HELPER_MODE") {
	case "silent":
		// 只读 stdin 不响应任何请求，用于握手超时测试
		io.Copy(io.Discard, os.Stdin)
		os.Exit(0)
	default:
		serveTestServer()
		os.Exit(0)
	}
}

// serveTestServer 以 mcp-go server 端实现测试用 MCP server，通过 stdio 对外服务。
func serveTestServer() {
	srv := server.NewMCPServer("test-mcp-server", "0.1.0")

	// echo：原样回显传入文本，验证字符串参数传递
	srv.AddTool(
		mcp.NewTool(
			"echo",
			mcp.WithDescription("原样回显传入的文本"),
			mcp.WithString("text", mcp.Required(), mcp.Description("要回显的文本")),
		),
		func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			return mcp.NewToolResultText("echo: " + req.GetString("text", "")), nil
		},
	)

	// add：两个数字相加，验证数值参数传递
	srv.AddTool(
		mcp.NewTool(
			"add",
			mcp.WithDescription("计算两个数字之和"),
			mcp.WithNumber("x", mcp.Required(), mcp.Description("第一个数字")),
			mcp.WithNumber("y", mcp.Required(), mcp.Description("第二个数字")),
		),
		func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			return mcp.NewToolResultText(fmt.Sprintf("%g", req.GetFloat("x", 0)+req.GetFloat("y", 0))), nil
		},
	)

	// fail：模拟工具执行失败（isError 标记），验证错误上报
	srv.AddTool(
		mcp.NewTool("fail", mcp.WithDescription("总是执行失败")),
		func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			return mcp.NewToolResultError("故意的失败"), nil
		},
	)

	if err := server.ServeStdio(srv); err != nil {
		fmt.Fprintf(os.Stderr, "serve stdio: %v\n", err)
		os.Exit(1)
	}
}

// connectHelper 连接测试 MCP server 并注册清理。
func connectHelper(t *testing.T, mode string) *Client {
	t.Helper()
	ctx, cancel := connectCtx()
	t.Cleanup(cancel)

	c, err := Connect(ctx, helperConfig(mode))
	if err != nil {
		t.Fatalf("Connect() 错误: %v", err)
	}
	t.Cleanup(func() { c.Close() })
	return c
}

// findTool 按名称查找工具，缺失时直接失败。
func findTool(t *testing.T, c *Client, name string) *Tool {
	t.Helper()
	for _, tool := range c.Tools() {
		if tool.Name() == name {
			return tool
		}
	}
	t.Fatalf("缺少工具 %q，实际有: %v", name, c.toolNames())
	return nil
}

func (c *Client) toolNames() []string {
	names := make([]string, 0, len(c.tools))
	for _, tool := range c.tools {
		names = append(names, tool.Name())
	}
	return names
}

// TestConnectAndListTools 验证 tools/list：工具数量、名称、描述与 schema 正确。
func TestConnectAndListTools(t *testing.T) {
	c := connectHelper(t, "stdio")

	tools := c.Tools()
	if len(tools) != 3 {
		t.Fatalf("期望 3 个工具，实际 %d 个: %v", len(tools), c.toolNames())
	}

	echo := findTool(t, c, "echo")
	if got := echo.Description(); got != "原样回显传入的文本" {
		t.Errorf("echo.Description() = %q", got)
	}
	// schema 原样保留：type=object，properties.text.type=string，required 含 text
	if got := echo.Schema()["type"]; got != "object" {
		t.Errorf("echo schema.type = %v", got)
	}
	props, _ := echo.Schema()["properties"].(map[string]any)
	textProp, _ := props["text"].(map[string]any)
	if textProp["type"] != "string" {
		t.Errorf("echo schema properties.text.type = %v", textProp["type"])
	}
	required, _ := echo.Schema()["required"].([]any)
	if !slices.Contains(required, "text") {
		t.Errorf("echo schema required = %v", required)
	}
}

// TestCallTool 验证 tools/call：参数传递、结果返回与错误上报。
func TestCallTool(t *testing.T) {
	c := connectHelper(t, "stdio")
	ctx, cancel := connectCtx()
	defer cancel()

	// echo：字符串参数传递与结果返回
	got, err := findTool(t, c, "echo").Call(ctx, map[string]any{"text": "hello mcp"})
	if err != nil {
		t.Fatalf("echo.Call() 错误: %v", err)
	}
	if got != "echo: hello mcp" {
		t.Errorf("echo.Call() = %q", got)
	}

	// add：数值参数传递与结果返回
	got, err = findTool(t, c, "add").Call(ctx, map[string]any{"x": 2.0, "y": 3.0})
	if err != nil {
		t.Fatalf("add.Call() 错误: %v", err)
	}
	if got != "5" {
		t.Errorf("add.Call() = %q", got)
	}

	// fail：工具执行错误（isError 标记）转为 error 返回
	_, err = findTool(t, c, "fail").Call(ctx, nil)
	if err == nil {
		t.Fatal("fail.Call() 应返回错误")
	}
	if !strings.Contains(err.Error(), "故意的失败") {
		t.Errorf("fail.Call() 错误信息 = %v", err)
	}

	// 调用 server 上不存在的工具：协议级错误
	ghost := &Tool{name: "ghost", client: c.mcp}
	if _, err := ghost.Call(ctx, map[string]any{}); err == nil {
		t.Error("未知工具的 Call() 应返回错误")
	}
}

// TestClose 验证 Close：正常退出、幂等、关闭后调用失败。
func TestClose(t *testing.T) {
	ctx, cancel := connectCtx()
	defer cancel()

	c, err := Connect(ctx, helperConfig("stdio"))
	if err != nil {
		t.Fatalf("Connect() 错误: %v", err)
	}

	if err := c.Close(); err != nil {
		t.Fatalf("第一次 Close() 错误: %v", err)
	}
	if err := c.Close(); err != nil {
		t.Fatalf("第二次 Close() 错误: %v", err)
	}

	// Close 后调用工具应失败（传输已关闭、子进程已退出）
	if _, err := c.Tools()[0].Call(ctx, map[string]any{"text": "x"}); err == nil {
		t.Error("Close 后调用工具应失败")
	}
}

// TestConnectProcessStartError 验证进程启动失败返回明确错误。
func TestConnectProcessStartError(t *testing.T) {
	ctx, cancel := connectCtx()
	defer cancel()

	cfg := ServerConfig{Name: "ghost-server", Command: "/nonexistent/mcp-server-binary"}
	_, err := Connect(ctx, cfg)
	if err == nil {
		t.Fatal("Connect() 应返回错误")
	}
	if !strings.Contains(err.Error(), "启动 stdio 传输失败") {
		t.Errorf("错误信息未包含启动上下文: %v", err)
	}
}

// TestConnectHandshakeTimeout 验证握手超时返回明确错误：
// 以 2 秒超时连接 silent 模式的 server（只读不应答），应返回握手失败错误。
func TestConnectHandshakeTimeout(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	_, err := Connect(ctx, helperConfig("silent"))
	if err == nil {
		t.Fatal("Connect() 应返回错误")
	}
	if !strings.Contains(err.Error(), "握手失败") {
		t.Errorf("错误信息未包含握手上下文: %v", err)
	}
}

// TestToToolParams 验证 MCP 工具到 llm.ToolParam 的转换。
func TestToToolParams(t *testing.T) {
	c := connectHelper(t, "stdio")

	params := ToToolParams(c.Tools())
	if len(params) != 3 {
		t.Fatalf("ToToolParams() 长度 = %d", len(params))
	}
	byName := make(map[string]vllm.ToolParam, len(params))
	for _, p := range params {
		byName[p.Name] = p
	}

	echo := byName["echo"]
	if echo.Name != "echo" {
		t.Errorf("name = %q", echo.Name)
	}
	if echo.Description != "原样回显传入的文本" {
		t.Errorf("description = %q", echo.Description)
	}
	if echo.Schema["type"] != "object" {
		t.Errorf("schema.type = %v", echo.Schema["type"])
	}
	if _, ok := echo.Schema["properties"]; !ok {
		t.Error("schema.properties 缺失")
	}
}
