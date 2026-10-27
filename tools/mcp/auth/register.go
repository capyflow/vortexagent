package auth

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
)

// RegisterOptions 描述一次客户端注册。ClientID 非空时跳过 DCR，直接使用手工凭据；
// 否则要求授权服务器提供 registration_endpoint。
type RegisterOptions struct {
	ClientName              string
	RedirectURI             string
	Scopes                  []string
	ClientID                string
	ClientSecret            string
	TokenEndpointAuthMethod string
}

// Register 返回可用的客户端注册。服务商不支持 DCR 时返回 *RegistrationError，
// 其中带出需要加白的回调地址。
func Register(ctx context.Context, hc *http.Client, as *ServerMetadata, opts RegisterOptions) (*ClientRegistration, error) {
	if as == nil {
		return nil, fmt.Errorf("mcp auth: missing authorization server metadata")
	}
	if err := validateEndpoint("redirect_uri", opts.RedirectURI); err != nil {
		return nil, err
	}
	scope := strings.Join(opts.Scopes, " ")
	if strings.TrimSpace(opts.ClientID) != "" {
		method := strings.TrimSpace(opts.TokenEndpointAuthMethod)
		if method == "" {
			method = defaultClientAuthMethod(as, opts.ClientSecret)
		}
		return &ClientRegistration{
			ClientID:                strings.TrimSpace(opts.ClientID),
			ClientSecret:            strings.TrimSpace(opts.ClientSecret),
			TokenEndpointAuthMethod: method,
			RedirectURI:             opts.RedirectURI,
			Scope:                   scope,
		}, nil
	}
	if strings.TrimSpace(as.RegistrationEndpoint) == "" {
		return nil, &RegistrationError{
			RedirectURI: opts.RedirectURI,
			Reason:      "authorization server metadata has no registration_endpoint",
		}
	}
	if err := validateEndpoint("registration_endpoint", as.RegistrationEndpoint); err != nil {
		return nil, err
	}
	method := strings.TrimSpace(opts.TokenEndpointAuthMethod)
	if method == "" {
		method = defaultClientAuthMethod(as, opts.ClientSecret)
	}
	name := strings.TrimSpace(opts.ClientName)
	if name == "" {
		name = "vortexagent"
	}
	body := map[string]any{
		"client_name":                name,
		"redirect_uris":              []string{opts.RedirectURI},
		"grant_types":                []string{"authorization_code", "refresh_token"},
		"response_types":             []string{"code"},
		"token_endpoint_auth_method": method,
	}
	if scope != "" {
		body["scope"] = scope
	}
	payload, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, as.RegistrationEndpoint, bytes.NewReader(payload))
	if err != nil {
		return nil, fmt.Errorf("mcp auth: build registration request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	resp, err := ensureHTTPClient(hc).Do(req)
	if err != nil {
		return nil, fmt.Errorf("mcp auth: register client: %w", err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, fmt.Errorf("mcp auth: read registration response: %w", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("mcp auth: register client: HTTP %d: %s", resp.StatusCode, snippet(data))
	}
	var reg struct {
		ClientID                string `json:"client_id"`
		ClientSecret            string `json:"client_secret"`
		TokenEndpointAuthMethod string `json:"token_endpoint_auth_method"`
		Scope                   string `json:"scope"`
	}
	if err := json.Unmarshal(data, &reg); err != nil {
		return nil, fmt.Errorf("mcp auth: decode registration response: %w", err)
	}
	if strings.TrimSpace(reg.ClientID) == "" {
		return nil, fmt.Errorf("mcp auth: registration response missing client_id")
	}
	if reg.TokenEndpointAuthMethod != "" {
		method = reg.TokenEndpointAuthMethod
	}
	outScope := reg.Scope
	if outScope == "" {
		outScope = scope
	}
	return &ClientRegistration{
		ClientID:                reg.ClientID,
		ClientSecret:            reg.ClientSecret,
		TokenEndpointAuthMethod: method,
		RedirectURI:             opts.RedirectURI,
		Scope:                   outScope,
	}, nil
}

func defaultClientAuthMethod(as *ServerMetadata, secret string) string {
	supports := func(method string) bool {
		for _, m := range as.TokenEndpointAuthMethodsSupported {
			if m == method {
				return true
			}
		}
		return false
	}
	if secret != "" {
		if supports("client_secret_basic") {
			return "client_secret_basic"
		}
		if supports("client_secret_post") {
			return "client_secret_post"
		}
		return "client_secret_post"
	}
	if supports("none") {
		return "none"
	}
	// 服务商只支持机密客户端时，DCR 仍可请求 basic/post，由注册响应下发 secret。
	if supports("client_secret_basic") {
		return "client_secret_basic"
	}
	if supports("client_secret_post") {
		return "client_secret_post"
	}
	return "none"
}
