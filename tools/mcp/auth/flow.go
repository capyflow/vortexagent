package auth

import (
	"context"
	"fmt"
	"net/http"
	"strings"
)

// FlowOptions 是一次浏览器授权所需的参数。
type FlowOptions struct {
	ResourceURL             string
	ResourceMetadataURL     string // 可选：401 challenge 里的 resource_metadata
	ClientName              string
	CallbackPort            int // 0 = 随机 loopback 端口
	Scopes                  []string
	ClientID                string // 手工注册时填写；为空则走 DCR
	ClientSecret            string
	TokenEndpointAuthMethod string
	HTTPClient              *http.Client
}

// Flow 是一次进行中的浏览器授权流程。调用方拿 URL() 打开浏览器，再 Wait() 等回调。
type Flow struct {
	opts     FlowOptions
	hc       *http.Client
	loop     *Loopback
	resource *ResourceMetadata
	as       *ServerMetadata
	client   *ClientRegistration
	pkce     PKCE
	state    string
	authURL  string
	scopes   []string
	finished bool
}

// Begin 完成发现 + 注册 + PKCE + 回调监听，返回待打开的授权 URL。
func Begin(ctx context.Context, opts FlowOptions) (*Flow, error) {
	if strings.TrimSpace(opts.ResourceURL) == "" {
		return nil, fmt.Errorf("mcp auth: resource URL is empty")
	}
	hc := ensureHTTPClient(opts.HTTPClient)
	loop, err := NewLoopback(opts.CallbackPort)
	if err != nil {
		return nil, err
	}
	f := &Flow{opts: opts, hc: hc, loop: loop}
	fail := func(err error) (*Flow, error) {
		_ = loop.Close()
		return nil, err
	}

	rm, as, err := Discover(ctx, hc, opts.ResourceURL, opts.ResourceMetadataURL)
	if err != nil {
		return fail(err)
	}
	f.resource, f.as = rm, as

	scopes := opts.Scopes
	if len(scopes) == 0 {
		scopes = rm.ScopesSupported
	}
	f.scopes = scopes

	client, err := Register(ctx, hc, as, RegisterOptions{
		ClientName:              opts.ClientName,
		RedirectURI:             loop.RedirectURI(),
		Scopes:                  scopes,
		ClientID:                opts.ClientID,
		ClientSecret:            opts.ClientSecret,
		TokenEndpointAuthMethod: opts.TokenEndpointAuthMethod,
	})
	if err != nil {
		return fail(err)
	}
	f.client = client

	pkce, err := NewPKCE()
	if err != nil {
		return fail(fmt.Errorf("mcp auth: generate PKCE: %w", err))
	}
	state, err := NewState()
	if err != nil {
		return fail(fmt.Errorf("mcp auth: generate state: %w", err))
	}
	f.pkce, f.state = pkce, state

	authURL, err := AuthorizeURL(as, AuthorizeOptions{
		ClientID:      client.ClientID,
		RedirectURI:   loop.RedirectURI(),
		Scopes:        scopes,
		State:         state,
		CodeChallenge: pkce.Challenge,
		Resource:      opts.ResourceURL,
	})
	if err != nil {
		return fail(err)
	}
	f.authURL = authURL
	return f, nil
}

// URL 返回需要交给系统浏览器打开的授权地址。
func (f *Flow) URL() string { return f.authURL }

// RedirectURI 返回本次流程使用的 loopback 回调地址。
func (f *Flow) RedirectURI() string { return f.loop.RedirectURI() }

// ServerMetadata 返回本次发现的授权服务器元数据（只读）。
func (f *Flow) ServerMetadata() *ServerMetadata { return f.as }

// ResourceMetadata 返回本次发现的受保护资源元数据（只读）。
func (f *Flow) ResourceMetadata() *ResourceMetadata { return f.resource }

// Wait 等待浏览器回调并换 token。成功返回可直接持久化的 Session。
func (f *Flow) Wait(ctx context.Context) (*Session, error) {
	if f == nil || f.loop == nil || f.as == nil || f.client == nil {
		return nil, fmt.Errorf("mcp auth: flow is not initialized")
	}
	if f.finished {
		return nil, fmt.Errorf("mcp auth: flow already finished")
	}
	f.finished = true
	defer f.loop.Close()

	code, _, err := f.loop.Wait(ctx, f.state, f.as.Issuer)
	if err != nil {
		return nil, err
	}
	token, err := Exchange(ctx, f.hc, f.as.TokenEndpoint, ExchangeOptions{
		Client:       f.client,
		RedirectURI:  f.loop.RedirectURI(),
		Code:         code,
		CodeVerifier: f.pkce.Verifier,
		Resource:     f.opts.ResourceURL,
	})
	if err != nil {
		return nil, err
	}
	return &Session{
		Resource:            f.opts.ResourceURL,
		AuthorizationServer: f.as.Issuer,
		TokenEndpoint:       f.as.TokenEndpoint,
		RevocationEndpoint:  f.as.RevocationEndpoint,
		Client:              *f.client,
		Token:               *token,
	}, nil
}

// Close 停止回调监听；成功后会自动关闭，重复调用无副作用。
func (f *Flow) Close() error {
	if f == nil || f.loop == nil {
		return nil
	}
	return f.loop.Close()
}
