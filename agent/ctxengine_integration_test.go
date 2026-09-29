package agent

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/capyflow/vortexagent/agent/sessionstore"
	"github.com/capyflow/vortexagent/ctxengine"
	"github.com/capyflow/vortexagent/llm"
)

// scriptedProvider 按脚本逐步响应：每个 step 收到本轮请求，返回响应或错误。
type scriptedProvider struct {
	name  string
	steps []func(req *llm.ChatRequest) (*llm.ChatResponse, error)
	reqs  []*llm.ChatRequest
}

func (p *scriptedProvider) Name() string       { return p.name }
func (p *scriptedProvider) ContextWindow() int { return 128000 }

func (p *scriptedProvider) Chat(_ context.Context, req *llm.ChatRequest, _ func(llm.Delta) error) (*llm.ChatResponse, error) {
	p.reqs = append(p.reqs, req)
	if len(p.steps) == 0 {
		return nil, fmt.Errorf("脚本耗尽")
	}
	step := p.steps[0]
	p.steps = p.steps[1:]
	return step(req)
}

func answerStep(text string) func(*llm.ChatRequest) (*llm.ChatResponse, error) {
	return func(*llm.ChatRequest) (*llm.ChatResponse, error) {
		return &llm.ChatResponse{
			Message:      llm.NewTextMessage(llm.RoleAssistant, text),
			FinishReason: "stop",
		}, nil
	}
}

// toolStep 返回一次对指定工具的调用。
func toolStep(id, tool string, args map[string]any) func(*llm.ChatRequest) (*llm.ChatResponse, error) {
	return func(*llm.ChatRequest) (*llm.ChatResponse, error) {
		return &llm.ChatResponse{
			Message: llm.Message{
				Role:      llm.RoleAssistant,
				ToolCalls: []llm.ToolCall{{ID: id, Name: tool, Arguments: args}},
			},
			FinishReason: "tool_calls",
		}, nil
	}
}

// seedSession 预置 n 轮历史（每轮带主题词，供检索命中）。
func seedSession(t *testing.T, n int) *sessionstore.Session {
	t.Helper()
	s := sessionstore.NewSession("test-model")
	for i := 0; i < n; i++ {
		s.Add(llm.NewTextMessage(llm.RoleUser, fmt.Sprintf("第%d轮：排查第%d号 postgres 连接报错", i, i)))
		s.Add(llm.NewTextMessage(llm.RoleAssistant, fmt.Sprintf("第%d轮已解决：连接池配置改为 %d", i, i*10)))
	}
	return s
}

func newCtxTestEngine(t *testing.T) (*ctxengine.Engine, *ctxengine.FileSystemArchive) {
	t.Helper()
	archive, err := ctxengine.NewFileSystemArchive(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return ctxengine.New(archive, fakeHandoffSummarizer{}, ctxengine.Config{
		Model:            "test-model",
		KeepRecentBlocks: 2,
		MaxBlocks:        1000, // OnTurnDone 不自动触发，归档由测试显式驱动
	}), archive
}

// fakeHandoffSummarizer 确定性摘要器（不依赖 provider）。
type fakeHandoffSummarizer struct{}

func (fakeHandoffSummarizer) Summarize(ctx context.Context, msgs []llm.Message) (string, []string, error) {
	var topics []string
	for _, m := range msgs {
		if m.Role == llm.RoleUser {
			for _, c := range m.Content {
				if c.Type == llm.ContentText && strings.Contains(c.Text, "postgres") {
					topics = append(topics, c.Text)
				}
			}
		}
	}
	handoff := "【任务意图】排查 postgres 连接报错\n【当前状态】早期轮次的问题均已解决"
	if len(topics) > 0 {
		handoff += "\n【已确认事实】涉及片段：" + topics[0]
	}
	return handoff, nil, nil
}

// TestContextEngine_FullChain 归档 → 组装带交接摘要 → 模型经 search_history
// / expand_chunk 取回原文的完整链路（含会话经 context 注入工具）。
func TestContextEngine_FullChain(t *testing.T) {
	engine, archive := newCtxTestEngine(t)
	session := seedSession(t, 8)
	ctx := context.Background()

	// 1. 归档：活跃区只剩最近 2 块。
	if err := engine.Compact(ctx, session.ID, session); err != nil {
		t.Fatal(err)
	}
	if got := session.MessageCount(); got != 4 {
		t.Fatalf("归档后活跃消息数 = %d，期望 4", got)
	}
	blocks, _ := archive.ListBlocks(ctx, session.ID)
	if len(blocks) != 6 {
		t.Fatalf("归档块数 = %d，期望 6", len(blocks))
	}

	// 2. Ask：模型先调 search_history（验证工具经 context 拿到会话并命中归档），
	//    再调 expand_chunk 展开原文，最后作答。
	var searchResult, expandResult string
	provider := &scriptedProvider{name: "scripted", steps: []func(*llm.ChatRequest) (*llm.ChatResponse, error){
		toolStep("call-1", "search_history", map[string]any{"query": "postgres 连接报错"}),
		func(req *llm.ChatRequest) (*llm.ChatResponse, error) {
			// 第二步之前，检查上一轮工具结果已回传。
			for _, m := range req.Messages {
				if m.Role == llm.RoleTool {
					searchResult = m.Content[0].Text
				}
			}
			if !strings.Contains(searchResult, "expand_chunk") {
				return nil, fmt.Errorf("search_history 结果应提示 expand_chunk：%q", searchResult)
			}
			return toolStep("call-2", "expand_chunk", map[string]any{"chunk_id": extractChunkID(searchResult)})(req)
		},
		func(req *llm.ChatRequest) (*llm.ChatResponse, error) {
			for _, m := range req.Messages {
				if m.Role == llm.RoleTool && m.ToolCallID == "call-2" {
					expandResult = m.Content[0].Text
				}
			}
			if !strings.Contains(expandResult, "第1轮：排查第1号 postgres 连接报错") {
				return nil, fmt.Errorf("expand_chunk 应取回原文：%q", expandResult)
			}
			return answerStep("已根据归档原文回答")(req)
		},
	}}

	ag := New(Options{
		Provider:      provider,
		Registry:      mustRegistryWithCtxTools(t, engine),
		Model:         "test-model",
		ContextEngine: engine,
	})
	answer, err := ag.Ask(ctx, session, "上次第1号 postgres 报错是怎么解决的？")
	if err != nil {
		t.Fatal(err)
	}
	if answer != "已根据归档原文回答" {
		t.Fatalf("answer = %q", answer)
	}

	// 3. 请求组装断言：主 system 之后应注入【历史对话交接摘要】。
	first := provider.reqs[0].Messages
	if len(first) < 2 || first[0].Role != llm.RoleSystem {
		t.Fatalf("请求消息形态异常：%d 条", len(first))
	}
	if !strings.Contains(first[1].Content[0].Text, "历史对话交接摘要") {
		t.Fatalf("应注入交接摘要：%q", first[1].Content[0].Text)
	}
	if !strings.Contains(first[0].Content[0].Text, "search_history") {
		t.Fatalf("系统提示词应包含检索指引")
	}
}

// TestContextEngine_WindowErrorRelief 疑似超窗错误触发 L4 兜底：裁旧块重试一次。
func TestContextEngine_WindowErrorRelief(t *testing.T) {
	engine, archive := newCtxTestEngine(t)
	session := seedSession(t, 9)

	provider := &scriptedProvider{name: "scripted", steps: []func(*llm.ChatRequest) (*llm.ChatResponse, error){
		func(*llm.ChatRequest) (*llm.ChatResponse, error) {
			return nil, fmt.Errorf("request failed: This model's maximum context length is 8192 tokens")
		},
		answerStep("重试成功"),
	}}
	ag := New(Options{
		Provider:      provider,
		Registry:      NewRegistry(),
		Model:         "test-model",
		ContextEngine: engine,
	})
	answer, err := ag.Ask(context.Background(), session, "继续")
	if err != nil {
		t.Fatal(err)
	}
	if answer != "重试成功" {
		t.Fatalf("answer = %q", answer)
	}
	// 裁掉 1/3 = 3 块（6 条消息）；再计入本轮新增的用户消息与最终回答：
	// 18 - 6 + 1(用户) + 1(回答) = 14。
	if got := session.MessageCount(); got != 14 {
		t.Fatalf("Relieve 后消息数 = %d，期望 14", got)
	}
	blocks, _ := archive.ListBlocks(context.Background(), session.ID)
	if len(blocks) != 3 {
		t.Fatalf("Relieve 归档块数 = %d，期望 3", len(blocks))
	}
}

// TestContextEngine_SummaryInjectedAfterArchiveOnTurnDone Ask 成功后 OnTurnDone
// 异步归档生效（等待归档完成再断言）。
func TestContextEngine_SummaryInjectedAfterArchiveOnTurnDone(t *testing.T) {
	archive2, err := ctxengine.NewFileSystemArchive(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	// 小 MaxBlocks 让首轮 Ask 结束就满足触发条件。
	engine := ctxengine.New(archive2, fakeHandoffSummarizer{}, ctxengine.Config{
		Model: "test-model", KeepRecentBlocks: 2, MaxBlocks: 4,
	})

	session := seedSession(t, 10) // 10 块 > MaxBlocks 4
	provider := &scriptedProvider{name: "scripted", steps: []func(*llm.ChatRequest) (*llm.ChatResponse, error){
		answerStep("done"),
	}}
	ag := New(Options{
		Provider:      provider,
		Registry:      NewRegistry(),
		Model:         "test-model",
		ContextEngine: engine,
	})
	if _, err := ag.Ask(context.Background(), session, "最新问题"); err != nil {
		t.Fatal(err)
	}

	// 等待后台归档完成。
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if blocks, _ := archive2.ListBlocks(context.Background(), session.ID); len(blocks) > 0 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	blocks, _ := archive2.ListBlocks(context.Background(), session.ID)
	if len(blocks) == 0 {
		t.Fatal("Ask 成功后未触发后台归档")
	}
	if got := session.MessageCount(); got > 4+1 {
		t.Fatalf("归档后活跃消息数异常：%d", got)
	}
}

// mustRegistryWithCtxTools 注册 ctxengine 工具（本地 Tool 投影 → agent.Tool 适配）。
func mustRegistryWithCtxTools(t *testing.T, engine *ctxengine.Engine) *Registry {
	t.Helper()
	reg := NewRegistry()
	for _, tl := range ctxengine.NewTools(engine) {
		if err := reg.Add(toolAdapter{tl}); err != nil {
			t.Fatal(err)
		}
	}
	return reg
}

// toolAdapter 把 ctxengine.Tool（结构兼容）适配为 agent.Tool。
type toolAdapter struct {
	inner interface {
		Name() string
		Description() string
		Schema() map[string]any
		Call(ctx context.Context, args map[string]any) (string, error)
	}
}

func (a toolAdapter) Name() string           { return a.inner.Name() }
func (a toolAdapter) Description() string    { return a.inner.Description() }
func (a toolAdapter) Schema() map[string]any { return a.inner.Schema() }
func (a toolAdapter) Call(ctx context.Context, args map[string]any) (string, error) {
	return a.inner.Call(ctx, args)
}

// extractChunkID 从 search_history 输出里取第一个 "ID: " 后面的片段 ID。
func extractChunkID(out string) string {
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		line = strings.TrimPrefix(line, "- ")
		if strings.HasPrefix(line, "ID: ") {
			return strings.TrimSpace(strings.TrimPrefix(line, "ID: "))
		}
	}
	return ""
}
