package auth

import (
	"context"
	"crypto/subtle"
	"fmt"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"
)

// DefaultCallbackPath 是 loopback 回调路径；注册到授权服务器的 redirect_uri 与之一致。
const DefaultCallbackPath = "/callback"

// Loopback 是一次性本地回调服务：只监听 127.0.0.1，收到浏览器授权结果后即可关闭。
type Loopback struct {
	listener  net.Listener
	server    *http.Server
	url       string
	result    chan callbackResult
	closeOnce sync.Once
}

type callbackResult struct {
	code  string
	state string
	iss   string
	err   error
}

// NewLoopback 在 127.0.0.1 上监听回调；port=0 表示随机空闲端口。
func NewLoopback(port int) (*Loopback, error) {
	if port < 0 || port > 65535 {
		return nil, fmt.Errorf("mcp auth: invalid callback port %d", port)
	}
	ln, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", port))
	if err != nil {
		return nil, fmt.Errorf("mcp auth: listen loopback callback: %w", err)
	}
	l := &Loopback{
		listener: ln,
		url:      "http://" + ln.Addr().String() + DefaultCallbackPath,
		result:   make(chan callbackResult, 1),
	}
	mux := http.NewServeMux()
	mux.HandleFunc(DefaultCallbackPath, l.handle)
	l.server = &http.Server{
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
		WriteTimeout:      10 * time.Second,
	}
	go func() {
		_ = l.server.Serve(ln)
	}()
	return l, nil
}

// RedirectURI 返回要注册/拼进授权 URL 的完整回调地址。
func (l *Loopback) RedirectURI() string { return l.url }

// Wait 阻塞等待授权回调，校验 state（以及可选的 iss）后返回授权码。
func (l *Loopback) Wait(ctx context.Context, expectedState, expectedIssuer string) (code, iss string, err error) {
	if strings.TrimSpace(expectedState) == "" {
		return "", "", fmt.Errorf("mcp auth: empty expected state")
	}
	select {
	case res := <-l.result:
		if res.err != nil {
			return "", "", res.err
		}
		if subtle.ConstantTimeCompare([]byte(res.state), []byte(expectedState)) != 1 {
			return "", "", fmt.Errorf("mcp auth: callback state mismatch")
		}
		if expectedIssuer != "" && res.iss != "" && !sameIssuer(res.iss, expectedIssuer) {
			return "", "", fmt.Errorf("mcp auth: callback issuer mismatch: %q", res.iss)
		}
		return res.code, res.iss, nil
	case <-ctx.Done():
		return "", "", fmt.Errorf("mcp auth: waiting for authorization callback: %w", ctx.Err())
	}
}

// Close 停止回调服务；可重复调用。
func (l *Loopback) Close() error {
	var err error
	l.closeOnce.Do(func() {
		if l.server != nil {
			err = l.server.Close()
		}
	})
	return err
}

func (l *Loopback) handle(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	q := r.URL.Query()
	if code := strings.TrimSpace(q.Get("error")); code != "" {
		msg := strings.TrimSpace(q.Get("error_description"))
		l.deliver(callbackResult{err: &OAuthError{Code: code, Description: msg}})
		writeCallbackPage(w, false)
		return
	}
	code := strings.TrimSpace(q.Get("code"))
	state := strings.TrimSpace(q.Get("state"))
	if code == "" || state == "" {
		l.deliver(callbackResult{err: fmt.Errorf("mcp auth: callback missing code or state")})
		writeCallbackPage(w, false)
		return
	}
	l.deliver(callbackResult{code: code, state: state, iss: strings.TrimSpace(q.Get("iss"))})
	writeCallbackPage(w, true)
}

func (l *Loopback) deliver(res callbackResult) {
	select {
	case l.result <- res:
	default:
	}
	// 给浏览器一点时间把结果页写完，再关 listener；Flow 也会 Close，幂等。
	go func() {
		time.Sleep(150 * time.Millisecond)
		_ = l.Close()
	}()
}

func writeCallbackPage(w http.ResponseWriter, ok bool) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Content-Security-Policy", "default-src 'none'; style-src 'unsafe-inline'")
	title, message := "授权完成", "可以关闭此页面，回到应用继续。"
	if !ok {
		title, message = "授权失败", "请回到应用查看原因并重试。"
	}
	fmt.Fprintf(w, `<!doctype html><html lang="zh-CN"><head><meta charset="utf-8"><title>%s</title>
<style>html,body{height:100%%;margin:0}body{display:flex;flex-direction:column;align-items:center;justify-content:center;gap:12px;background:#171717;color:#e8e8e8;font-family:-apple-system,BlinkMacSystemFont,"PingFang SC",sans-serif}h1{font-size:20px;font-weight:600;margin:0}p{font-size:13px;color:#a1a1aa;margin:0}</style>
</head><body><h1>%s</h1><p>%s</p></body></html>`, title, title, message)
}
