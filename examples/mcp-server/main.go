// Command mcp-server 是一个示例 MCP server，演示如何为 vortex 编写自定义工具。
//
// 运行方式（在仓库根目录）：
//
//	go run ./examples/mcp-server
//
// 然后在 vortex.json 的 mcpServers 中声明：
//
//	"mcpServers": [{"name": "example-tools", "command": "go", "args": ["run", "./examples/mcp-server"]}]
//
// 启动 vortex 后，/tools 可以看到本 server 暴露的工具，agent 会自动调用它们。
// 任何语言（Python/Node/Go...）实现的 MCP server 都可以用同样的方式接入。
package main

import (
	"context"
	"fmt"
	"os"
	"time"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
)

func main() {
	srv := server.NewMCPServer("example-tools", "0.1.0")

	// get_time：返回当前时间。mcp.WithString 声明字符串参数，WithNumber 声明数值参数。
	srv.AddTool(
		mcp.NewTool(
			"get_time",
			mcp.WithDescription("获取当前时间，可选指定时区（如 Asia/Shanghai）"),
			mcp.WithString("timezone", mcp.Description("IANA 时区名，默认本地时区")),
		),
		func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			loc := time.Local
			if tz := req.GetString("timezone", ""); tz != "" {
				var err error
				loc, err = time.LoadLocation(tz)
				if err != nil {
					return mcp.NewToolResultError(fmt.Sprintf("未知时区 %q", tz)), nil
				}
			}
			return mcp.NewToolResultText(time.Now().In(loc).Format(time.RFC3339)), nil
		},
	)

	// add：两数相加，演示数值参数与算术逻辑。
	srv.AddTool(
		mcp.NewTool(
			"add",
			mcp.WithDescription("计算两个数字之和"),
			mcp.WithNumber("x", mcp.Required(), mcp.Description("第一个数字")),
			mcp.WithNumber("y", mcp.Required(), mcp.Description("第二个数字")),
		),
		func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			sum := req.GetFloat("x", 0) + req.GetFloat("y", 0)
			return mcp.NewToolResultText(fmt.Sprintf("%g", sum)), nil
		},
	)

	// 通过 stdio 对外服务（vortex 的 MCP 客户端会以子进程方式拉起本程序）。
	if err := server.ServeStdio(srv); err != nil {
		fmt.Fprintf(os.Stderr, "serve stdio: %v\n", err)
	}
}
