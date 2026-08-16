package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/capyflow/vortexagent/knowledge"
	"github.com/capyflow/vortexagent/llm"
)

// TestE2E_FullChain 端到端验证：OpenAI 兼容 provider + 知识库工具 + agent 循环。
//
// 流程：模型先返回 search_knowledge 工具调用 → agent 执行知识库检索 →
// 结果回传 → 模型基于检索结果给出最终回答。覆盖 pi 移植后的核心链路。
func TestE2E_FullChain(t *testing.T) {
	// 1. 构造知识库
	kbDir := t.TempDir()
	docs := filepath.Join(kbDir, "docs")
	if err := os.MkdirAll(docs, 0o755); err != nil {
		t.Fatal(err)
	}
	docPath := filepath.Join(docs, "vortex.md")
	docContent := "Vortex 是一个基于 Go 的智能文档助手，支持 MCP 自定义工具。"
	if err := os.WriteFile(docPath, []byte(docContent), 0o644); err != nil {
		t.Fatal(err)
	}

	// 2. 模拟 OpenAI 兼容服务器：先要求调用 search_knowledge，再基于工具结果回答
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Messages []struct {
				Role    string `json:"role"`
				Content any    `json:"content"`
			} `json:"messages"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Errorf("解析请求失败: %v", err)
		}
		// 最后一条消息是工具结果 → 基于结果回答
		last := req.Messages[len(req.Messages)-1]
		if last.Role == "tool" {
			result, _ := json.Marshal(last.Content)
			resp := map[string]any{
				"choices": []any{map[string]any{
					"message": map[string]any{
						"role":    "assistant",
						"content": "根据知识库：" + string(result),
					},
					"finish_reason": "stop",
				}},
				"usage": map[string]any{"prompt_tokens": 10, "completion_tokens": 5},
			}
			json.NewEncoder(w).Encode(resp)
			return
		}
		// 否则返回工具调用
		fmt.Fprint(w, `{"choices":[{"message":{"role":"assistant","content":"","tool_calls":[{"id":"call_1","type":"function","function":{"name":"search_knowledge","arguments":"{\"query\":\"Vortex\"}"}}]},"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":5,"completion_tokens":3}}`)
	}))
	defer srv.Close()

	// 3. 组装 provider + 知识库工具
	provider := llm.NewOpenAIProvider("fake-key", llm.WithBaseURL(srv.URL))
	reg := NewRegistry()
	kb := knowledge.NewKB([]string{kbDir})
	for _, tool := range knowledge.NewKBTools(kb) {
		if err := reg.Add(tool); err != nil {
			t.Fatal(err)
		}
	}

	ag := New(Options{Provider: provider, Registry: reg, Model: "fake-model"})
	session := NewSession("fake-model")

	answer, err := ag.Ask(context.Background(), session, "Vortex 是什么？")
	if err != nil {
		t.Fatalf("Ask 失败: %v", err)
	}
	// 工具结果内容应被模型基于知识库的回答承载
	if !strings.Contains(answer, "智能文档助手") {
		t.Errorf("回答应包含知识库检索内容，实际: %s", answer)
	}
	if !strings.Contains(answer, "MCP") {
		t.Errorf("回答应包含 MCP 关键字，实际: %s", answer)
	}
	// 会话历史应包含完整闭环：user → assistant(工具调用) → tool → assistant(最终)
	if len(session.Messages()) != 4 {
		t.Errorf("历史条数 = %d, 期望 4（user/assistant/tool/assistant）", len(session.Messages()))
	}
}
