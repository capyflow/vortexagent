package autonomous

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// FileWatcher 监听文件变化并发送事件。
type FileWatcher struct {
	mu       sync.Mutex
	watches  map[string]string // goalID -> glob pattern
	emit     func(Event)
	cancel   context.CancelFunc
	wg       sync.WaitGroup
	interval time.Duration
}

// NewFileWatcher 创建文件监听器。
// emit 是事件发送回调，interval 是轮询间隔。
func NewFileWatcher(emit func(Event), interval time.Duration) *FileWatcher {
	if interval <= 0 {
		interval = 30 * time.Second
	}
	return &FileWatcher{
		watches:  make(map[string]string),
		emit:     emit,
		interval: interval,
	}
}

// AddWatch 添加文件监听（goalID -> glob pattern）。
func (w *FileWatcher) AddWatch(goalID, pattern string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.watches[goalID] = pattern
}

// RemoveWatch 移除文件监听。
func (w *FileWatcher) RemoveWatch(goalID string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	delete(w.watches, goalID)
}

// WatchCount 返回监听数量。
func (w *FileWatcher) WatchCount() int {
	w.mu.Lock()
	defer w.mu.Unlock()
	return len(w.watches)
}

// Start 启动监听循环。
func (w *FileWatcher) Watch(ctx context.Context) {
	ctx, cancel := context.WithCancel(ctx)
	w.cancel = cancel

	w.wg.Add(1)
	go func() {
		defer w.wg.Done()

		// 记录文件修改时间，只在变化时触发
		modTimes := make(map[string]time.Time)

		ticker := time.NewTicker(w.interval)
		defer ticker.Stop()

		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				w.checkFiles(modTimes)
			}
		}
	}()
}

// Stop 停止监听。
func (w *FileWatcher) Stop() {
	if w.cancel != nil {
		w.cancel()
	}
	w.wg.Wait()
}

// checkFiles 检查所有监听的文件。
func (w *FileWatcher) checkFiles(modTimes map[string]time.Time) {
	w.mu.Lock()
	watches := make(map[string]string, len(w.watches))
	for k, v := range w.watches {
		watches[k] = v
	}
	w.mu.Unlock()

	for _, pattern := range watches {
		matches, err := filepath.Glob(pattern)
		if err != nil {
			log.Printf("[vortex-autonomous] glob %q 失败: %v", pattern, err)
			continue
		}

		for _, path := range matches {
			info, err := os.Stat(path)
			if err != nil {
				continue
			}

			prevMod := modTimes[path]
			if !prevMod.IsZero() && info.ModTime().After(prevMod) {
				// 文件有变化
				w.emit(Event{
					Type:      "file_change",
					Source:    path,
					Data:      map[string]any{"op": "modify", "pattern": pattern},
					Timestamp: time.Now(),
				})
			}
			modTimes[path] = info.ModTime()
		}
	}
}

// WebhookHandler 处理外部 webhook 请求。
type WebhookHandler struct {
	emit func(Event)
}

// NewWebhookHandler 创建 webhook 处理器。
func NewWebhookHandler(emit func(Event)) *WebhookHandler {
	return &WebhookHandler{emit: emit}
}

// ServeHTTP 实现 http.Handler 接口。
// 路由: POST /webhook/{type}
// 请求体为 JSON，作为 Event.Data 存储。
func (h *WebhookHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "仅支持 POST", http.StatusMethodNotAllowed)
		return
	}

	// 从路径提取事件类型
	// 路径格式: /webhook/{type}
	path := strings.TrimPrefix(r.URL.Path, "/webhook/")
	eventType := strings.TrimSpace(path)
	if eventType == "" {
		http.Error(w, "请在路径中指定事件类型，如 /webhook/github.push", http.StatusBadRequest)
		return
	}

	// 解析请求体
	var data map[string]any
	if err := json.NewDecoder(r.Body).Decode(&data); err != nil {
		// 空 body 也允许
		data = make(map[string]any)
	}

	// 添加标准字段
	data["_source"] = r.RemoteAddr
	data["_time"] = time.Now().Format(time.RFC3339)
	data["_user_agent"] = r.UserAgent()

	event := Event{
		Type:      eventType,
		Source:    r.RemoteAddr,
		Data:      data,
		Timestamp: time.Now(),
	}

	h.emit(event)

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{
		"status": "ok",
		"event":  eventType,
	})
}

// formatWebhookEvent 格式化 webhook 事件数据为可读字符串。
func formatWebhookEvent(event Event) string {
	var parts []string
	for k, v := range event.Data {
		if strings.HasPrefix(k, "_") {
			continue // 跳过内部字段
		}
		parts = append(parts, fmt.Sprintf("%s=%v", k, v))
	}
	if len(parts) == 0 {
		return fmt.Sprintf("事件 %s (无数据)", event.Type)
	}
	return fmt.Sprintf("事件 %s: %s", event.Type, strings.Join(parts, ", "))
}
