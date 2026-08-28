// Command customer-service-agent 演示如何用子 agent 原语（agent.NewSubagentTool）
// 搭建一个"总-分"结构的智能客服：总机 agent 负责理解意图并委派，
// 各专员子 agent 拥有独立的系统提示词与工具箱。
//
// 运行方式（在仓库根目录）：
//
//	export OPENAI_API_KEY=sk-...
//	go run ./examples/customer-service-agent "我上个月买的数据线用了三天就坏了，要求退款"
//
// 结构：
//
//	                  ┌────────────────────────┐
//	用户问题 ────────▶ │  总机 agent（orchestrator）│  工具：仅两个子 agent
//	                  └───────┬────────┬───────┘
//	                  售前咨询 │        │ 售后工单
//	                  ┌───────▼──┐  ┌──▼─────────┐
//	                  │ 售前 agent │  │ 售后 agent    │
//	                  │ 工具：FAQ  │  │ 工具：订单/退款 │
//	                  └──────────┘  └────────────┘
//
// 关键点：对总机 agent 而言，每个专员子 agent 只是 Registry 里的一个普通工具——
// 权限钩子、并行执行、渐进式披露等框架机制对子 agent 原样生效；
// 子 agent 的中间过程（思考、工具轮次）不进入总机的上下文，只回收最终答复。
package main

import (
	"context"
	"fmt"
	"os"
	"strings"

	"github.com/capyflow/vortexagent/agent"
	"github.com/capyflow/vortexagent/agent/sessionstore"
	"github.com/capyflow/vortexagent/llm"
)

// ---- 领域工具（示例用假实现，真实场景替换为调用业务系统）----

// faqTool 售前 FAQ 检索。
type faqTool struct{}

func (t *faqTool) Name() string { return "search_faq" }
func (t *faqTool) Description() string {
	return "检索产品常见问题（价格、规格、发货时效）"
}
func (t *faqTool) Schema() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"question": map[string]any{"type": "string", "description": "要检索的问题"},
		},
		"required": []string{"question"},
	}
}
func (t *faqTool) Call(_ context.Context, args map[string]any) (string, error) {
	return "FAQ：标准版 ¥99 / 专业版 ¥299；满 ¥79 包邮，48 小时内发货；7 天无理由退货。", nil
}

// orderTool 售后订单查询。
type orderTool struct{}

func (t *orderTool) Name() string        { return "query_order" }
func (t *orderTool) Description() string { return "按订单号查询订单状态" }
func (t *orderTool) Schema() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"order_id": map[string]any{"type": "string", "description": "订单号，如 A123"},
		},
		"required": []string{"order_id"},
	}
}
func (t *orderTool) Call(_ context.Context, args map[string]any) (string, error) {
	id, _ := args["order_id"].(string)
	return fmt.Sprintf("订单 %s：已签收 25 天，金额 ¥99，支持退款", id), nil
}

// refundTool 售后退款登记。
type refundTool struct{}

func (t *refundTool) Name() string { return "register_refund" }
func (t *refundTool) Description() string {
	return "为订单登记退款申请（有副作用，需先查询订单确认）"
}
func (t *refundTool) Schema() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"order_id": map[string]any{"type": "string", "description": "订单号"},
			"reason":   map[string]any{"type": "string", "description": "退款原因"},
		},
		"required": []string{"order_id", "reason"},
	}
}
func (t *refundTool) Call(_ context.Context, args map[string]any) (string, error) {
	id, _ := args["order_id"].(string)
	return fmt.Sprintf("订单 %s 的退款申请已登记，预计 3 个工作日到账", id), nil
}

// mustAdd 批量注册工具，失败即退出（示例从简）。
func mustAdd(registry *agent.Registry, tools ...agent.Tool) {
	for _, t := range tools {
		if err := registry.Add(t); err != nil {
			fmt.Fprintln(os.Stderr, "注册工具失败:", err)
			os.Exit(1)
		}
	}
}

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, "用法: go run ./examples/customer-service-agent <客户问题>")
		os.Exit(1)
	}
	apiKey := os.Getenv("OPENAI_API_KEY")
	if apiKey == "" {
		fmt.Fprintln(os.Stderr, "请设置环境变量 OPENAI_API_KEY")
		os.Exit(1)
	}

	// 1. LLM provider（三个 agent 共用一个，也可按专员用不同模型）
	provider, err := llm.NewProvider(llm.ProviderConfig{Name: "openai", APIKey: apiKey})
	if err != nil {
		fmt.Fprintln(os.Stderr, "创建 provider 失败:", err)
		os.Exit(1)
	}

	// 2. 派生专员子 agent：每个专员 = 独立提示词 + 独立工具箱
	presalesReg := agent.NewRegistry()
	mustAdd(presalesReg, &faqTool{})
	presales := agent.New(agent.Options{
		Provider:     provider,
		Registry:     presalesReg,
		MaxTokens:    1024,
		SystemPrompt: "你是售前客服专员。只回答产品咨询，用 search_faq 查证后再回答，不确定时如实说不知道。",
	})

	aftersalesReg := agent.NewRegistry()
	mustAdd(aftersalesReg, &orderTool{}, &refundTool{})
	aftersales := agent.New(agent.Options{
		Provider:      provider,
		Registry:      aftersalesReg,
		MaxTokens:     1024,
		MaxIterations: 6,
		SystemPrompt:  "你是售后客服专员。处理退款前必须先用 query_order 确认订单存在，再登记退款；无法处理的工单如实告知用户。",
	})

	// 3. 总机 agent：工具箱里只有两个"专员工具"
	orchestratorReg := agent.NewRegistry()
	mustAdd(orchestratorReg,
		agent.NewSubagentTool("presales", "售前咨询专员：产品价格、规格、发货时效等问题", presales),
		agent.NewSubagentTool("aftersales", "售后专员：查订单、退款、换货等工单问题", aftersales),
	)
	orchestrator := agent.New(agent.Options{
		Provider: provider,
		Registry: orchestratorReg,
		SystemPrompt: "你是客服总机。根据客户问题委派给合适的专员，把客户诉求完整写进 task；" +
			"汇总专员的答复，用友好的口吻回复客户。",
	})

	// 4. 接待客户（会话挂在总机 agent 上）
	session := sessionstore.NewSession("customer-service")
	answer, err := orchestrator.Ask(context.Background(), session, strings.Join(os.Args[1:], " "))
	fmt.Println()
	if err != nil {
		fmt.Fprintln(os.Stderr, "错误:", err)
		os.Exit(1)
	}
	fmt.Println(answer)
}
