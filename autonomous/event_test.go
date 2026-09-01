package autonomous

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

func TestFileWatcher_AddRemoveWatch(t *testing.T) {
	var emitted []Event
	emit := func(e Event) {
		emitted = append(emitted, e)
	}

	w := NewFileWatcher(emit, 100*time.Millisecond)

	w.AddWatch("goal-1", "*.txt")
	if w.WatchCount() != 1 {
		t.Errorf("监听数应为 1, got %d", w.WatchCount())
	}

	w.AddWatch("goal-2", "*.go")
	if w.WatchCount() != 2 {
		t.Errorf("监听数应为 2, got %d", w.WatchCount())
	}

	w.RemoveWatch("goal-1")
	if w.WatchCount() != 1 {
		t.Errorf("监听数应为 1, got %d", w.WatchCount())
	}
}

func TestFileWatcher_DetectsChanges(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "test.txt")

	// 创建初始文件
	if err := os.WriteFile(file, []byte("initial"), 0o644); err != nil {
		t.Fatal(err)
	}

	var emitted []Event
	var mu sync.Mutex
	emit := func(e Event) {
		mu.Lock()
		emitted = append(emitted, e)
		mu.Unlock()
	}

	w := NewFileWatcher(emit, 50*time.Millisecond)
	w.AddWatch("goal-1", filepath.Join(dir, "*.txt"))

	ctx, cancel := context.WithCancel(context.Background())
	w.Watch(ctx)
	defer func() {
		cancel()
		w.Stop()
	}()

	// 等待首次扫描记录 modtime
	time.Sleep(100 * time.Millisecond)

	// 修改文件
	if err := os.WriteFile(file, []byte("modified"), 0o644); err != nil {
		t.Fatal(err)
	}

	// 等待检测
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		mu.Lock()
		count := len(emitted)
		mu.Unlock()
		if count > 0 {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(emitted) == 0 {
		t.Error("应检测到文件变化事件")
	}
}

func TestWebhookHandler(t *testing.T) {
	var emitted []Event
	emit := func(e Event) {
		emitted = append(emitted, e)
	}

	handler := NewWebhookHandler(emit)

	// 创建请求
	body := map[string]string{"repo": "my-repo", "ref": "main"}
	bodyJSON, _ := json.Marshal(body)

	req := httptest.NewRequest(http.MethodPost, "/webhook/github.push", bytes.NewReader(bodyJSON))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	handler.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("状态码应为 200, got %d", w.Code)
	}

	if len(emitted) != 1 {
		t.Fatalf("应发出 1 个事件, got %d", len(emitted))
	}

	event := emitted[0]
	if event.Type != "github.push" {
		t.Errorf("事件类型应为 github.push, got %q", event.Type)
	}
	if event.Data["repo"] != "my-repo" {
		t.Errorf("事件数据应包含 repo=my-repo, got %v", event.Data["repo"])
	}
}

func TestWebhookHandler_InvalidMethod(t *testing.T) {
	handler := NewWebhookHandler(func(e Event) {})
	req := httptest.NewRequest(http.MethodGet, "/webhook/test", nil)
	w := httptest.NewRecorder()

	handler.ServeHTTP(w, req)

	if w.Code != http.StatusMethodNotAllowed {
		t.Errorf("状态码应为 405, got %d", w.Code)
	}
}

func TestWebhookHandler_EmptyBody(t *testing.T) {
	var emitted []Event
	handler := NewWebhookHandler(func(e Event) {
		emitted = append(emitted, e)
	})

	req := httptest.NewRequest(http.MethodPost, "/webhook/test", nil)
	w := httptest.NewRecorder()

	handler.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("状态码应为 200, got %d", w.Code)
	}

	if len(emitted) != 1 {
		t.Fatalf("应发出 1 个事件, got %d", len(emitted))
	}
}

func TestWebhookHandler_NoEventType(t *testing.T) {
	handler := NewWebhookHandler(func(e Event) {})
	req := httptest.NewRequest(http.MethodPost, "/webhook/", nil)
	w := httptest.NewRecorder()

	handler.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("状态码应为 400, got %d", w.Code)
	}
}

func TestFormatWebhookEvent(t *testing.T) {
	event := Event{
		Type: "github.push",
		Data: map[string]any{
			"repo":  "my-repo",
			"_time": "2026-01-01T00:00:00Z",
		},
	}

	result := formatWebhookEvent(event)
	if result == "" {
		t.Error("格式化结果不应为空")
	}
}

var _ http.Handler = (*WebhookHandler)(nil)
