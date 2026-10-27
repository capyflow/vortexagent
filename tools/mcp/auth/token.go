package auth

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// ExchangeOptions 是授权码换 token 的参数。
type ExchangeOptions struct {
	Client       *ClientRegistration
	RedirectURI  string
	Code         string
	CodeVerifier string
	Resource     string
}

// Exchange 用授权码 + PKCE verifier 换 token。
func Exchange(ctx context.Context, hc *http.Client, tokenEndpoint string, opts ExchangeOptions) (*Token, error) {
	if opts.Client == nil || strings.TrimSpace(opts.Client.ClientID) == "" {
		return nil, fmt.Errorf("mcp auth: missing client registration")
	}
	if strings.TrimSpace(opts.Code) == "" {
		return nil, fmt.Errorf("mcp auth: authorization code is empty")
	}
	if strings.TrimSpace(opts.CodeVerifier) == "" {
		return nil, fmt.Errorf("mcp auth: code verifier is empty")
	}
	if strings.TrimSpace(opts.RedirectURI) == "" {
		return nil, fmt.Errorf("mcp auth: redirect_uri is empty")
	}
	form := url.Values{}
	form.Set("grant_type", "authorization_code")
	form.Set("code", opts.Code)
	form.Set("redirect_uri", opts.RedirectURI)
	form.Set("code_verifier", opts.CodeVerifier)
	if strings.TrimSpace(opts.Resource) != "" {
		form.Set("resource", opts.Resource)
	}
	return requestToken(ctx, hc, tokenEndpoint, form, opts.Client)
}

// Refresh 用 refresh_token 换新 token；refresh token 轮换时沿用服务端返回值，
// 未返回则保留旧值。invalid_grant 映射为 ErrReauthRequired。
func Refresh(ctx context.Context, hc *http.Client, s *Session) (*Token, error) {
	if s == nil {
		return nil, ErrNoToken
	}
	if strings.TrimSpace(s.TokenEndpoint) == "" {
		return nil, fmt.Errorf("mcp auth: session has no token endpoint")
	}
	if strings.TrimSpace(s.Token.RefreshToken) == "" {
		return nil, ErrReauthRequired
	}
	form := url.Values{}
	form.Set("grant_type", "refresh_token")
	form.Set("refresh_token", s.Token.RefreshToken)
	if strings.TrimSpace(s.Resource) != "" {
		form.Set("resource", s.Resource)
	}
	tok, err := requestToken(ctx, hc, s.TokenEndpoint, form, &s.Client)
	if err != nil {
		var oe *OAuthError
		if errors.As(err, &oe) && oe.Code == "invalid_grant" {
			// 双 %w：调用方既能 errors.Is(ErrReauthRequired)，也能 errors.As 取回原始 OAuth 错误。
			return nil, fmt.Errorf("%w: %w", ErrReauthRequired, err)
		}
		return nil, err
	}
	if tok.RefreshToken == "" {
		tok.RefreshToken = s.Token.RefreshToken
	}
	if tok.Scope == "" {
		tok.Scope = s.Token.Scope
	}
	return tok, nil
}

// EnsureToken 返回可用会话：未过期原样返回；临近过期自动刷新；无 refresh token
// 或刷新被拒时返回 ErrReauthRequired。
func EnsureToken(ctx context.Context, hc *http.Client, s *Session, now time.Time, skew time.Duration) (*Session, error) {
	if s == nil || strings.TrimSpace(s.Token.AccessToken) == "" {
		return nil, ErrNoToken
	}
	if !s.NeedsRefresh(now, skew) {
		return s, nil
	}
	tok, err := Refresh(ctx, hc, s)
	if err != nil {
		return nil, err
	}
	next := *s
	next.Token = *tok
	return &next, nil
}

// Revoke 尽力撤销 token；服务端未提供 revocation_endpoint 时视为成功。
func Revoke(ctx context.Context, hc *http.Client, s *Session, token, tokenTypeHint string) error {
	if s == nil || strings.TrimSpace(s.RevocationEndpoint) == "" || strings.TrimSpace(token) == "" {
		return nil
	}
	if err := validateEndpoint("revocation_endpoint", s.RevocationEndpoint); err != nil {
		return err
	}
	form := url.Values{}
	form.Set("token", token)
	if tokenTypeHint != "" {
		form.Set("token_type_hint", tokenTypeHint)
	}
	req, err := formRequest(ctx, s.RevocationEndpoint, form, &s.Client)
	if err != nil {
		return err
	}
	resp, err := ensureHTTPClient(hc).Do(req)
	if err != nil {
		return fmt.Errorf("mcp auth: revoke token: %w", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("mcp auth: revoke token: HTTP %d: %s", resp.StatusCode, snippet(body))
	}
	return nil
}

func requestToken(ctx context.Context, hc *http.Client, endpoint string, form url.Values, reg *ClientRegistration) (*Token, error) {
	if err := validateEndpoint("token_endpoint", endpoint); err != nil {
		return nil, err
	}
	req, err := formRequest(ctx, endpoint, form, reg)
	if err != nil {
		return nil, err
	}
	resp, err := ensureHTTPClient(hc).Do(req)
	if err != nil {
		return nil, fmt.Errorf("mcp auth: token request: %w", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, fmt.Errorf("mcp auth: read token response: %w", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, parseOAuthError(resp.StatusCode, body)
	}
	// 少数实现 200 + error 字段。
	var probe struct {
		Error            string `json:"error"`
		ErrorDescription string `json:"error_description"`
	}
	if json.Unmarshal(body, &probe) == nil && probe.Error != "" {
		return nil, &OAuthError{StatusCode: resp.StatusCode, Code: probe.Error, Description: probe.ErrorDescription}
	}
	var tok Token
	if err := json.Unmarshal(body, &tok); err != nil {
		return nil, fmt.Errorf("mcp auth: decode token response: %w", err)
	}
	if strings.TrimSpace(tok.AccessToken) == "" {
		return nil, fmt.Errorf("mcp auth: token response missing access_token")
	}
	if tok.ExpiresIn > 0 {
		tok.ExpiresAt = time.Now().Add(time.Duration(tok.ExpiresIn) * time.Second)
	}
	return &tok, nil
}

func formRequest(ctx context.Context, endpoint string, form url.Values, reg *ClientRegistration) (*http.Request, error) {
	if reg == nil || strings.TrimSpace(reg.ClientID) == "" {
		return nil, fmt.Errorf("mcp auth: missing client registration")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, nil)
	if err != nil {
		return nil, fmt.Errorf("mcp auth: build token request: %w", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	if err := applyClientAuth(req, form, reg); err != nil {
		return nil, err
	}
	body := form.Encode()
	req.Body = io.NopCloser(strings.NewReader(body))
	req.ContentLength = int64(len(body))
	req.GetBody = func() (io.ReadCloser, error) {
		return io.NopCloser(strings.NewReader(body)), nil
	}
	return req, nil
}

func applyClientAuth(req *http.Request, form url.Values, reg *ClientRegistration) error {
	method := strings.TrimSpace(reg.TokenEndpointAuthMethod)
	if method == "" {
		if strings.TrimSpace(reg.ClientSecret) != "" {
			method = "client_secret_post"
		} else {
			method = "none"
		}
	}
	switch method {
	case "none":
		form.Set("client_id", reg.ClientID)
	case "client_secret_post":
		form.Set("client_id", reg.ClientID)
		if reg.ClientSecret != "" {
			form.Set("client_secret", reg.ClientSecret)
		}
	case "client_secret_basic":
		if reg.ClientSecret == "" {
			return fmt.Errorf("mcp auth: client_secret_basic requires a client secret")
		}
		// RFC 6749：client_id/secret 先做 application/x-www-form-urlencoded 再做 base64。
		user := url.QueryEscape(reg.ClientID)
		pass := url.QueryEscape(reg.ClientSecret)
		req.Header.Set("Authorization", "Basic "+base64.StdEncoding.EncodeToString([]byte(user+":"+pass)))
	default:
		return fmt.Errorf("mcp auth: unsupported token_endpoint_auth_method %q", method)
	}
	return nil
}

func parseOAuthError(status int, body []byte) error {
	var e struct {
		Error            string `json:"error"`
		ErrorDescription string `json:"error_description"`
	}
	if json.Unmarshal(body, &e) == nil && e.Error != "" {
		return &OAuthError{StatusCode: status, Code: e.Error, Description: e.ErrorDescription}
	}
	return fmt.Errorf("mcp auth: token endpoint: HTTP %d: %s", status, snippet(body))
}
