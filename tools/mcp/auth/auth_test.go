package auth

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"
)

func TestParseChallenge(t *testing.T) {
	header := `Bearer realm="OAuth", resource_metadata="https://mcp.notion.com/.well-known/oauth-protected-resource/mcp", scope="default"`
	c, err := ParseChallenge(header)
	if err != nil {
		t.Fatal(err)
	}
	if c.Scheme != "Bearer" || c.ResourceMetadataURL != "https://mcp.notion.com/.well-known/oauth-protected-resource/mcp" || c.Scope != "default" {
		t.Fatalf("challenge 解析不符: %+v", c)
	}
	c, err = ParseChallenge(`Bearer error="invalid_token", error_description="token, expired"`)
	if err != nil || c.Error != "invalid_token" || c.ErrorDescription != "token, expired" {
		t.Fatalf("带逗号的 error_description 解析不符: %+v err=%v", c, err)
	}
	if _, err := ParseChallenge(""); err == nil {
		t.Fatal("空头应报错")
	}
}

func TestDiscover(t *testing.T) {
	a := newFakeAS(t, true)
	ctx := context.Background()

	rm, sm, err := Discover(ctx, a.srv.Client(), a.resourceURL(), a.metadataURL())
	if err != nil {
		t.Fatal(err)
	}
	if rm.Resource != a.resourceURL() || len(rm.AuthorizationServers) != 1 || rm.AuthorizationServers[0] != a.srv.URL {
		t.Fatalf("资源元数据不符: %+v", rm)
	}
	if sm.Issuer != a.srv.URL || sm.AuthorizationEndpoint != a.srv.URL+"/authorize" || sm.TokenEndpoint != a.srv.URL+"/token" {
		t.Fatalf("授权服务器元数据不符: %+v", sm)
	}

	// 没有 401 URL 时按 well-known 回退。
	rm2, _, err := Discover(ctx, a.srv.Client(), a.resourceURL(), "")
	if err != nil || rm2.Resource != rm.Resource {
		t.Fatalf("well-known 回退失败: %+v err=%v", rm2, err)
	}

	// challenge 里给出的 URL 优先/可用。
	h := fmt.Sprintf(`Bearer resource_metadata=%q, scope="default"`, a.metadataURL())
	c, err := ParseChallenge(h)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := Discover(ctx, a.srv.Client(), a.resourceURL(), c.ResourceMetadataURL); err != nil {
		t.Fatalf("按 challenge URL 发现失败: %v", err)
	}
}

func TestDiscoverRejectsBadIssuerAndPlainHTTP(t *testing.T) {
	a := newFakeAS(t, true)
	a.setIssuerOverride("https://other.example")
	if _, _, err := Discover(context.Background(), a.srv.Client(), a.resourceURL(), a.metadataURL()); err == nil || !strings.Contains(err.Error(), "issuer mismatch") {
		t.Fatalf("issuer 不匹配应报错: %v", err)
	}
	if _, _, err := Discover(context.Background(), http.DefaultClient, "http://192.0.2.1/mcp", ""); err == nil {
		t.Fatal("非 loopback 的 http 资源地址应被拒绝")
	}
}

func TestRegister(t *testing.T) {
	a := newFakeAS(t, true)
	ctx := context.Background()
	_, sm, err := Discover(ctx, a.srv.Client(), a.resourceURL(), a.metadataURL())
	if err != nil {
		t.Fatal(err)
	}
	redirect := "http://127.0.0.1:9999/callback"
	reg, err := Register(ctx, a.srv.Client(), sm, RegisterOptions{
		ClientName: "Friday", RedirectURI: redirect, Scopes: []string{"default"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if reg.ClientID != "test-client" || reg.TokenEndpointAuthMethod != "none" || reg.RedirectURI != redirect {
		t.Fatalf("DCR 结果不符: %+v", reg)
	}
	if a.dcrCalls.Load() != 1 {
		t.Fatalf("DCR 调用次数 = %d", a.dcrCalls.Load())
	}

	// 手工 client 不走 DCR。
	manual, err := Register(ctx, a.srv.Client(), sm, RegisterOptions{ClientID: "manual", RedirectURI: redirect})
	if err != nil || manual.ClientID != "manual" || manual.TokenEndpointAuthMethod != "none" {
		t.Fatalf("手工 client 不符: %+v err=%v", manual, err)
	}
	if a.dcrCalls.Load() != 1 {
		t.Fatalf("手工 client 不应触发 DCR: %d", a.dcrCalls.Load())
	}

	// 不支持 DCR 且没有手工 client → RegistrationError 带回调地址。
	noDCR := newFakeAS(t, false)
	_, sm2, err := Discover(ctx, noDCR.srv.Client(), noDCR.resourceURL(), noDCR.metadataURL())
	if err != nil {
		t.Fatal(err)
	}
	_, err = Register(ctx, noDCR.srv.Client(), sm2, RegisterOptions{ClientName: "Friday", RedirectURI: redirect})
	var re *RegistrationError
	if !errors.As(err, &re) || re.RedirectURI != redirect {
		t.Fatalf("应返回带回调地址的 RegistrationError: %v", err)
	}
}

func beginFlow(t *testing.T, a *fakeAS, opts FlowOptions) (*Flow, url.Values) {
	t.Helper()
	if opts.ResourceURL == "" {
		opts.ResourceURL = a.resourceURL()
	}
	if opts.HTTPClient == nil {
		opts.HTTPClient = a.srv.Client()
	}
	f, err := Begin(context.Background(), opts)
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	t.Cleanup(func() { _ = f.Close() })
	u, err := url.Parse(f.URL())
	if err != nil {
		t.Fatal(err)
	}
	q := u.Query()
	a.setExpectations(q.Get("code_challenge"), q.Get("redirect_uri"), q.Get("resource"))
	return f, q
}

func completeFlow(t *testing.T, a *fakeAS, f *Flow, state string, includeIss bool) *Session {
	t.Helper()
	cb := f.RedirectURI() + "?code=" + url.QueryEscape(a.getCode()) + "&state=" + url.QueryEscape(state)
	if includeIss {
		cb += "&iss=" + url.QueryEscape(a.srv.URL)
	}
	resp, err := http.Get(cb)
	if err != nil {
		t.Fatalf("回调 loopback: %v", err)
	}
	resp.Body.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	sess, err := f.Wait(ctx)
	if err != nil {
		t.Fatalf("Wait: %v", err)
	}
	return sess
}

func TestFlowPKCEAndExchange(t *testing.T) {
	a := newFakeAS(t, true)
	f, q := beginFlow(t, a, FlowOptions{ClientName: "Friday"})
	if q.Get("response_type") != "code" || q.Get("code_challenge_method") != "S256" {
		t.Fatalf("授权参数不符: %v", q)
	}
	if q.Get("client_id") != "test-client" || q.Get("redirect_uri") != f.RedirectURI() {
		t.Fatalf("client/redirect 不符: %v", q)
	}
	if q.Get("resource") != a.resourceURL() || q.Get("scope") != "default" {
		t.Fatalf("resource/scope 不符: %v", q)
	}
	sess := completeFlow(t, a, f, q.Get("state"), true)
	if sess.Token.AccessToken != "access-1" || sess.Token.RefreshToken != "refresh-1" {
		t.Fatalf("token 不符: %+v", sess.Token)
	}
	if sess.Resource != a.resourceURL() || sess.AuthorizationServer != a.srv.URL ||
		sess.TokenEndpoint != a.srv.URL+"/token" || sess.Client.ClientID != "test-client" {
		t.Fatalf("session 不符: %+v", sess)
	}
	if a.tokenCalls.Load() != 1 || a.dcrCalls.Load() != 1 {
		t.Fatalf("调用次数 token=%d dcr=%d", a.tokenCalls.Load(), a.dcrCalls.Load())
	}
}

func TestFlowStateMismatch(t *testing.T) {
	a := newFakeAS(t, true)
	f, _ := beginFlow(t, a, FlowOptions{ClientName: "Friday"})
	resp, err := http.Get(f.RedirectURI() + "?code=code-1&state=wrong")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if _, err := f.Wait(ctx); err == nil || !strings.Contains(err.Error(), "state mismatch") {
		t.Fatalf("state 不匹配应报错: %v", err)
	}
	if a.tokenCalls.Load() != 0 {
		t.Fatal("state 失败不应换 token")
	}
}

func TestFlowRegistrationRequired(t *testing.T) {
	a := newFakeAS(t, false)
	_, err := Begin(context.Background(), FlowOptions{
		ResourceURL: a.resourceURL(), ClientName: "Friday", HTTPClient: a.srv.Client(),
	})
	var re *RegistrationError
	if !errors.As(err, &re) || re.RedirectURI == "" {
		t.Fatalf("应返回带回调地址的 RegistrationError: %v", err)
	}
}

func TestFlowManualClient(t *testing.T) {
	a := newFakeAS(t, false)
	a.setClientID("manual")
	f, q := beginFlow(t, a, FlowOptions{ClientName: "Friday", ClientID: "manual"})
	if q.Get("client_id") != "manual" {
		t.Fatalf("手工 client_id 未生效: %v", q)
	}
	sess := completeFlow(t, a, f, q.Get("state"), true)
	if sess.Token.AccessToken != "access-1" || sess.Client.ClientID != "manual" {
		t.Fatalf("手工 client 授权失败: %+v", sess)
	}
	if a.dcrCalls.Load() != 0 {
		t.Fatalf("手工 client 不应触发 DCR: %d", a.dcrCalls.Load())
	}
}

func TestFlowConfidentialClient(t *testing.T) {
	a := newFakeAS(t, true)
	a.setConfidential(true)
	f, q := beginFlow(t, a, FlowOptions{ClientName: "Friday"})
	if q.Get("client_id") != "test-client" {
		t.Fatalf("授权 URL client_id 不符: %v", q)
	}
	sess := completeFlow(t, a, f, q.Get("state"), true)
	// 服务商只支持机密客户端时，DCR 请求 client_secret_basic，secret 由注册响应下发。
	if sess.Client.TokenEndpointAuthMethod != "client_secret_basic" || sess.Client.ClientSecret != "secret-1" {
		t.Fatalf("DCR 应选定 basic 并带回 secret: %+v", sess.Client)
	}
	tok, err := Refresh(context.Background(), a.srv.Client(), sess)
	if err != nil || tok.AccessToken != "access-2" {
		t.Fatalf("机密客户端刷新失败: %+v err=%v", tok, err)
	}

	// 换成公开客户端的表单凭据必须被拒绝——说明选路真的按注册结果走。
	bad := *sess
	bad.Client.TokenEndpointAuthMethod = "none"
	bad.Client.ClientSecret = ""
	if _, err := Refresh(context.Background(), a.srv.Client(), &bad); err == nil {
		t.Fatal("机密服务商不应接受公开客户端凭据")
	}
}

func TestDefaultClientAuthMethod(t *testing.T) {
	as := func(methods ...string) *ServerMetadata {
		return &ServerMetadata{TokenEndpointAuthMethodsSupported: methods}
	}
	cases := []struct {
		name   string
		secret string
		meta   *ServerMetadata
		want   string
	}{
		{"公开客户端优先 none", "", as("none", "client_secret_basic"), "none"},
		{"只支持 basic 时走 basic", "", as("client_secret_basic"), "client_secret_basic"},
		{"只支持 post 时走 post", "", as("client_secret_post"), "client_secret_post"},
		{"元数据不声明时兜底 none", "", as(), "none"},
		{"手工 secret 优先 basic", "s", as("none", "client_secret_post", "client_secret_basic"), "client_secret_basic"},
		{"手工 secret 只有 post 时走 post", "s", as("client_secret_post"), "client_secret_post"},
	}
	for _, c := range cases {
		if got := defaultClientAuthMethod(c.meta, c.secret); got != c.want {
			t.Errorf("%s: defaultClientAuthMethod = %q, want %q", c.name, got, c.want)
		}
	}
}

func TestRefreshAndEnsureToken(t *testing.T) {
	a := newFakeAS(t, true)
	f, q := beginFlow(t, a, FlowOptions{ClientName: "Friday"})
	sess := completeFlow(t, a, f, q.Get("state"), true)

	tok, err := Refresh(context.Background(), a.srv.Client(), sess)
	if err != nil {
		t.Fatal(err)
	}
	if tok.AccessToken != "access-2" || tok.RefreshToken != "refresh-2" {
		t.Fatalf("刷新 token 不符: %+v", tok)
	}
	sess.Token = *tok
	a.setRefreshToken("refresh-2")

	// 未过期：原样返回，不再请求。
	same, err := EnsureToken(context.Background(), a.srv.Client(), sess, time.Now(), time.Minute)
	if err != nil || same.Token.AccessToken != "access-2" {
		t.Fatalf("未过期不应刷新: %+v err=%v", same, err)
	}
	if a.refreshCalls.Load() != 1 {
		t.Fatalf("refresh 调用次数 = %d", a.refreshCalls.Load())
	}

	// 已过期：自动刷新。
	sess.Token.ExpiresAt = time.Now().Add(-time.Minute)
	next, err := EnsureToken(context.Background(), a.srv.Client(), sess, time.Now(), time.Minute)
	if err != nil || next.Token.AccessToken != "access-2" {
		t.Fatalf("过期应刷新: %+v err=%v", next, err)
	}
	if a.refreshCalls.Load() != 2 {
		t.Fatalf("refresh 调用次数 = %d", a.refreshCalls.Load())
	}

	// 没有 refresh token：要求重新授权。
	sess.Token.ExpiresAt = time.Now().Add(-time.Minute)
	sess.Token.RefreshToken = ""
	if _, err := EnsureToken(context.Background(), a.srv.Client(), sess, time.Now(), time.Minute); !errors.Is(err, ErrReauthRequired) {
		t.Fatalf("无 refresh token 应 ErrReauthRequired: %v", err)
	}

	// 服务端拒绝（invalid_grant，如 refresh token 已在别处轮换）同样归类为需重新授权。
	sess = completeFlow2(t, a)
	sess.Token.RefreshToken = "stale-refresh"
	_, err = Refresh(context.Background(), a.srv.Client(), sess)
	var oe *OAuthError
	if !errors.Is(err, ErrReauthRequired) || !errors.As(err, &oe) || oe.Code != "invalid_grant" {
		t.Fatalf("invalid_grant 应包装成 ErrReauthRequired: %v", err)
	}
	if _, err := EnsureToken(context.Background(), a.srv.Client(), sess, time.Now().Add(time.Hour), time.Minute); !errors.Is(err, ErrReauthRequired) {
		t.Fatalf("刷新被拒后 EnsureToken 应 ErrReauthRequired: %v", err)
	}
}

// completeFlow2 复用同一个 fakeAS 再走一次授权，拿到一份干净的会话。
func completeFlow2(t *testing.T, a *fakeAS) *Session {
	t.Helper()
	f, q := beginFlow(t, a, FlowOptions{ClientName: "Friday"})
	return completeFlow(t, a, f, q.Get("state"), true)
}

func TestRevoke(t *testing.T) {
	a := newFakeAS(t, true)
	f, q := beginFlow(t, a, FlowOptions{ClientName: "Friday"})
	sess := completeFlow(t, a, f, q.Get("state"), true)
	if err := Revoke(context.Background(), a.srv.Client(), sess, sess.Token.AccessToken, "access_token"); err != nil {
		t.Fatal(err)
	}
	if a.revokeCalls.Load() != 1 {
		t.Fatalf("revoke 调用次数 = %d", a.revokeCalls.Load())
	}
}

func TestLoopbackTimeoutAndError(t *testing.T) {
	l, err := NewLoopback(0)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Millisecond)
	defer cancel()
	if _, _, err := l.Wait(ctx, "state", ""); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("超时应返回 DeadlineExceeded: %v", err)
	}
	_ = l.Close()

	l2, err := NewLoopback(0)
	if err != nil {
		t.Fatal(err)
	}
	defer l2.Close()
	resp, err := http.Get(l2.RedirectURI() + "?error=access_denied&error_description=denied")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	_, _, err = l2.Wait(context.Background(), "state", "")
	var oe *OAuthError
	if !errors.As(err, &oe) || oe.Code != "access_denied" {
		t.Fatalf("回调错误应映射 OAuthError: %v", err)
	}
}

func TestMemoryStore(t *testing.T) {
	ctx := context.Background()
	s := NewMemoryStore()
	if _, err := s.Get(ctx, "notion"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("空 store 应 ErrNotFound: %v", err)
	}
	want := &Session{Resource: "https://mcp.notion.com/mcp", Token: Token{AccessToken: "a"}}
	if err := s.Put(ctx, "notion", want); err != nil {
		t.Fatal(err)
	}
	got, err := s.Get(ctx, "notion")
	if err != nil || got.Resource != want.Resource || got.Token.AccessToken != "a" {
		t.Fatalf("store 回读不符: %+v err=%v", got, err)
	}
	if err := s.Delete(ctx, "notion"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Get(ctx, "notion"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("删除后应 ErrNotFound: %v", err)
	}
}
