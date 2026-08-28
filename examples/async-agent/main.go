// Command async-agent 演示 TaskHub 异步任务编排：主 agent 把任务分发给多个
// 子 agent 后立刻继续自己的工作（不被阻塞），子 agent 在后台 goroutine 里
// 各自执行，主 agent 之后再通过 task_wait 收取结果并汇总。
//
// 运行方式（在仓库根目录）：
//
//	export OPENAI_API_KEY=sk-...
//	go run ./examples/async-agent "我要采购 50 台打印机，先做个采购评估"
//
// 与 customer-service-agent（同步委派）的区别：
//
//	同步：主 agent 调用子 agent 工具时阻塞等待，拿到结果才继续——适合"先查再答"。
//	异步：主 agent 调 task_start 分发后循环立刻继续，可以接着分发更多任务或做
//	      其他事，最后 task_wait 一次性收取全部结果——适合"并行调研、最后汇总"。
//
// 并发机制都发生在框架内部：每个任务一个 goroutine，done channel 做完成信号，
// 缓冲 channel 做并发闸门；对模型暴露的只是 task_start / task_status /
// task_wait / task_cancel 四个普通工具。
package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"time"

	"github.com/capyflow/vortexagent/agent"
	"github.com/capyflow/vortexagent/agent/sessionstore"
	"github.com/capyflow/vortexagent/llm"
)

// ---- 领域工具（示例用假实现，带 1 秒延迟以体现并发的价值）----

// priceTool 供应商询价。
type priceTool struct{}

func (t *priceTool) Name() string { return "get_price" }
func (t *priceTool) Description() string {
	return "查询指定商品的供应商报价（较慢，约 1 秒）"
}
func (t *priceTool) Schema() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"item": map[string]any{"type": "string", "description": "商品名称"},
		},
		"required": []string{"item"},
	}
}
func (t *priceTool) Call(_ context.Context, args map[string]any) (string, error) {
	time.Sleep(time.Second) // 模拟慢速外部系统
	item, _ := args["item"].(string)
	return fmt.Sprintf("%s：A 供应商 ¥1200/台，B 供应商 ¥1150/台（满 50 台再打 95 折）", item), nil
}

// stockTool 库存查询。
type stockTool struct{}

func (t *stockTool) Name() string { return "get_stock" }
func (t *stockTool) Description() string {
	return "查询指定商品的当前库存与发货周期（较慢，约 1 秒）"
}
func (t *stockTool) Schema() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"item": map[string]any{"type": "string", "description": "商品名称"},
		},
		"required": []string{"item"},
	}
}
func (t *stockTool) Call(_ context.Context, args map[string]any) (string, error) {
	time.Sleep(time.Second) // 模拟慢速外部系统
	item, _ := args["item"].(string)
	return fmt.Sprintf("%s：A 供应商现货 80 台（3 天发货），B 供应商现货 30 台（7 天发货）", item), nil
}

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, "用法: go run ./examples/async-agent <任务>")
		os.Exit(1)
	}
	apiKey := os.Getenv("OPENAI_API_KEY")
	if apiKey == "" {
		fmt.Fprintln(os.Stderr, "请设置环境变量 OPENAI_API_KEY")
		os.Exit(1)
	}

	// 应用级 ctx：后台任务的生命周期锚点。Ctrl+C 时在途任务会被取消、优雅收尾。
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	provider, err := llm.NewProvider(llm.ProviderConfig{Name: "openai", APIKey: apiKey})
	if err != nil {
		fmt.Fprintln(os.Stderr, "创建 provider 失败:", err)
		os.Exit(1)
	}

	// 1. 两个专员子 agent：独立工具箱，各自在后台执行
	priceReg := agent.NewRegistry()
	if err := priceReg.Add(&priceTool{}); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	pricing := agent.New(agent.Options{
		Provider:     provider,
		Registry:     priceReg,
		MaxTokens:    1024,
		SystemPrompt: "你是询价专员。用 get_price 查询报价，返回简洁的比价结论。",
	})

	stockReg := agent.NewRegistry()
	if err := stockReg.Add(&stockTool{}); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	stock := agent.New(agent.Options{
		Provider:     provider,
		Registry:     stockReg,
		MaxTokens:    1024,
		SystemPrompt: "你是库存专员。用 get_stock 查询库存与发货周期，返回简洁结论。",
	})

	// 2. 任务中心：并发上限 2（两个专员可以真正并行）
	hub := agent.NewTaskHub(ctx, 2)
	if err := hub.Register("pricing", pricing); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	if err := hub.Register("stock", stock); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}

	// 3. 主 agent：工具箱里只有 hub 的四个任务工具，分发后继续自己的工作
	registry := agent.NewRegistry()
	for _, tool := range hub.Tools() {
		if err := registry.Add(tool); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
	}
	orchestrator := agent.New(agent.Options{
		Provider:      provider,
		Registry:      registry,
		ParallelTools: true, // 多个 task_start 可以在同一轮并发分发
		SystemPrompt: "你是采购调研主管。把询价与库存查询分别用 task_start 并行分发给 pricing 和 stock 专员，" +
			"然后立即用 task_wait 等待全部完成，基于返回的结果输出一份简短的采购评估。",
	})

	// 4. 执行
	session := sessionstore.NewSession("async-agent")
	answer, err := orchestrator.Ask(ctx, session, strings.Join(os.Args[1:], " "))
	fmt.Println()
	if err != nil {
		fmt.Fprintln(os.Stderr, "错误:", err)
	}

	// 5. 收尾：等待后台任务全部结束（未收取结果的任务也会跑完并落库，供下次查询）
	if err := hub.WaitAll(context.Background()); err != nil {
		fmt.Fprintln(os.Stderr, "警告: 仍有后台任务未结束:", err)
	}
	if err == nil {
		fmt.Println(answer)
	}
}
