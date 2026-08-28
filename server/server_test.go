// 本文件测试 server 包：会话持久化（重启恢复）、同一会话的并发串行化、
// 会话列表。数据竞争由 -race 检出。
package server

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync"
	"testing"

	"github.com/capyflow/vortexagent/agent"
	"github.com/capyflow/vortexagent/agent/sessionstore"
	"github.com/capyflow/vortexagent/llm"
)

// countingProvider 记录每次请求携带的消息数，返回固定回答。
type countingProvider struct {
	mu       sync.Mutex
	lastReq  *llm.ChatRequest
	askCount int
}

func (p *countingProvider) Name() string       { return "fake" }
func (p *countingProvider) ContextWindow() int { return 128000 }

func (p *countingProvider) Chat(_ context.Context, req *llm.ChatRequest, _ func(llm.Delta) error) (*llm.ChatResponse, error) {
	p.mu.Lock()
	p.lastReq = req
	p.askCount++
	p.mu.Unlock()
	return &llm.ChatResponse{
		Message:      llm.NewTextMessage(llm.RoleAssistant, "回复"),
		FinishReason: "stop",
	}, nil
}

func newTestServer(t *testing.T, store sessionstore.Store) (*Server, *countingProvider, *httptest.Server) {
	t.Helper()
	prov := &countingProvider{}
	ag := agent.New(agent.Options{Provider: prov, Model: "m1"})
	srv := New(Config{Agent: ag, Store: store})
	ts := httptest.NewServer(srv.httpServer.Handler)
	t.Cleanup(ts.Close)
	return srv, prov, ts
}

func postChat(t *testing.T, ts *httptest.Server, sessionID, message string) ChatResponse {
	t.Helper()
	body, _ := json.Marshal(ChatRequest{SessionID: sessionID, Message: message})
	resp, err := http.Post(ts.URL+"/chat", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("POST /chat 失败: %v", err)
	}
	defer resp.Body.Close()
	var out ChatResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("解析响应失败: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("状态码 = %d, 响应: %+v", resp.StatusCode, out)
	}
	return out
}

// TestServer_SessionPersistsAcrossRestart 校验会话持久化：
// 第一个进程对话后，新进程（同一存储）按 session_id 恢复完整历史。
func TestServer_SessionPersistsAcrossRestart(t *testing.T) {
	store, err := sessionstore.NewJSON(filepath.Join(t.TempDir(), "sessions.json"))
	if err != nil {
		t.Fatal(err)
	}

	_, _, ts1 := newTestServer(t, store)
	first := postChat(t, ts1, "", "第一句")
	if first.SessionID == "" {
		t.Fatal("应返回 session_id")
	}

	// 模拟重启：新 Server + 新 provider，同一存储
	_, prov2, ts2 := newTestServer(t, store)
	postChat(t, ts2, first.SessionID, "第二句")

	// 第二次请求应携带完整历史：system + 第一轮(user+assistant) + 第二句 user = 4 条
	prov2.mu.Lock()
	defer prov2.mu.Unlock()
	if got := len(prov2.lastReq.Messages); got != 4 {
		t.Errorf("重启后历史条数 = %d, 期望 4（会话应从存储恢复）", got)
	}
}

// TestServer_ConcurrentSameSessionSerialized 校验同一会话的并发请求被串行化：
// 全部成功且互相看不到交错的半截状态（竞争由 -race 检出）。
func TestServer_ConcurrentSameSessionSerialized(t *testing.T) {
	_, prov, ts := newTestServer(t, nil)

	var wg sync.WaitGroup
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			out := postChat(t, ts, "", fmt.Sprintf("并发消息 %d", i))
			if out.Answer != "回复" {
				t.Errorf("回答 = %q", out.Answer)
			}
		}(i)
	}
	wg.Wait()

	prov.mu.Lock()
	defer prov.mu.Unlock()
	if prov.askCount != 10 {
		t.Errorf("请求数 = %d, 期望 10", prov.askCount)
	}
}

// TestServer_ListSessions 校验会话列表走锁保护的消息计数。
func TestServer_ListSessions(t *testing.T) {
	_, _, ts := newTestServer(t, nil)
	postChat(t, ts, "", "一条")

	resp, err := http.Get(ts.URL + "/sessions")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var sessions []SessionResponse
	if err := json.NewDecoder(resp.Body).Decode(&sessions); err != nil {
		t.Fatal(err)
	}
	if len(sessions) != 1 {
		t.Fatalf("会话数 = %d, 期望 1", len(sessions))
	}
	if sessions[0].Messages != 2 {
		t.Errorf("消息数 = %d, 期望 2（user+assistant）", sessions[0].Messages)
	}
}
