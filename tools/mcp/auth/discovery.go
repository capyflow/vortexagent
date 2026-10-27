package auth

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
)

// Discover 发现 MCP 资源对应的授权服务器：
//
//  1. 优先使用 401 challenge 里的 resource_metadata URL；
//  2. 否则按 RFC 9728 尝试 well-known（含路径变体）；
//  3. 读取授权服务器元数据（RFC 8414 / OIDC discovery），校验 issuer 与端点。
//
// 返回的 ServerMetadata.Issuer 一定非空；调用方据此做回调 iss 校验。
func Discover(ctx context.Context, hc *http.Client, resourceURL, resourceMetadataURL string) (*ResourceMetadata, *ServerMetadata, error) {
	if err := validateHTTPURL("resource URL", resourceURL); err != nil {
		return nil, nil, err
	}
	u, err := url.Parse(strings.TrimSpace(resourceURL))
	if err != nil {
		return nil, nil, fmt.Errorf("mcp auth: invalid resource URL %q", resourceURL)
	}
	hc = ensureHTTPClient(hc)

	candidates := make([]string, 0, 4)
	if strings.TrimSpace(resourceMetadataURL) != "" {
		candidates = append(candidates, strings.TrimSpace(resourceMetadataURL))
	}
	candidates = append(candidates, wellKnownResourceURLs(u)...)

	var rm ResourceMetadata
	var lastErr error
	for _, candidate := range dedupStrings(candidates) {
		var m ResourceMetadata
		if err := fetchJSON(ctx, hc, candidate, &m); err != nil {
			lastErr = err
			continue
		}
		if len(m.AuthorizationServers) == 0 {
			lastErr = fmt.Errorf("mcp auth: %s has no authorization_servers", candidate)
			continue
		}
		rm = m
		break
	}
	if len(rm.AuthorizationServers) == 0 {
		return nil, nil, fmt.Errorf("mcp auth: protected resource metadata not found: %w", lastErr)
	}

	asURL := strings.TrimRight(strings.TrimSpace(rm.AuthorizationServers[0]), "/")
	if err := validateHTTPURL("authorization server", asURL); err != nil {
		return nil, nil, err
	}
	au, err := url.Parse(asURL)
	if err != nil {
		return nil, nil, fmt.Errorf("mcp auth: invalid authorization server %q", rm.AuthorizationServers[0])
	}

	var sm ServerMetadata
	lastErr = nil
	for _, candidate := range wellKnownServerURLs(au) {
		var m ServerMetadata
		if err := fetchJSON(ctx, hc, candidate, &m); err != nil {
			lastErr = err
			continue
		}
		if m.AuthorizationEndpoint == "" || m.TokenEndpoint == "" {
			lastErr = fmt.Errorf("mcp auth: %s missing authorization/token endpoint", candidate)
			continue
		}
		sm = m
		break
	}
	if sm.AuthorizationEndpoint == "" || sm.TokenEndpoint == "" {
		return nil, nil, fmt.Errorf("mcp auth: authorization server metadata not found for %s: %w", asURL, lastErr)
	}
	if sm.Issuer == "" {
		sm.Issuer = asURL
	}
	if !sameIssuer(sm.Issuer, asURL) {
		return nil, nil, fmt.Errorf("mcp auth: issuer mismatch: metadata says %q, resource points to %q", sm.Issuer, asURL)
	}
	if err := validateEndpoint("authorization_endpoint", sm.AuthorizationEndpoint); err != nil {
		return nil, nil, err
	}
	if err := validateEndpoint("token_endpoint", sm.TokenEndpoint); err != nil {
		return nil, nil, err
	}
	if sm.RegistrationEndpoint != "" {
		if err := validateEndpoint("registration_endpoint", sm.RegistrationEndpoint); err != nil {
			return nil, nil, err
		}
	}
	if sm.RevocationEndpoint != "" {
		if err := validateEndpoint("revocation_endpoint", sm.RevocationEndpoint); err != nil {
			return nil, nil, err
		}
	}
	return &rm, &sm, nil
}

func fetchJSON(ctx context.Context, hc *http.Client, rawURL string, out any) error {
	if err := validateHTTPURL("metadata URL", rawURL); err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return fmt.Errorf("mcp auth: build metadata request: %w", err)
	}
	req.Header.Set("Accept", "application/json")
	resp, err := hc.Do(req)
	if err != nil {
		return fmt.Errorf("mcp auth: fetch metadata %s: %w", rawURL, err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return fmt.Errorf("mcp auth: read metadata %s: %w", rawURL, err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("mcp auth: metadata %s: HTTP %d: %s", rawURL, resp.StatusCode, snippet(body))
	}
	if err := json.Unmarshal(body, out); err != nil {
		return fmt.Errorf("mcp auth: decode metadata %s: %w", rawURL, err)
	}
	return nil
}

func wellKnownResourceURLs(u *url.URL) []string {
	origin := u.Scheme + "://" + u.Host
	out := make([]string, 0, 2)
	if p := strings.TrimSuffix(u.EscapedPath(), "/"); p != "" && p != "/" {
		out = append(out, origin+"/.well-known/oauth-protected-resource"+p)
	}
	return append(out, origin+"/.well-known/oauth-protected-resource")
}

func wellKnownServerURLs(as *url.URL) []string {
	origin := as.Scheme + "://" + as.Host
	path := strings.TrimSuffix(as.EscapedPath(), "/")
	base := origin + "/.well-known/oauth-authorization-server"
	out := make([]string, 0, 4)
	if path != "" && path != "/" {
		// RFC 8414 的路径插入形态，以及部分服务商的直拼形态。
		out = append(out, base+path)
		out = append(out, strings.TrimRight(as.String(), "/")+"/.well-known/oauth-authorization-server")
		out = append(out, strings.TrimRight(as.String(), "/")+"/.well-known/openid-configuration")
	} else {
		out = append(out, base)
		out = append(out, origin+"/.well-known/openid-configuration")
	}
	return out
}

func validateEndpoint(name, raw string) error {
	if err := validateHTTPURL(name, raw); err != nil {
		return err
	}
	return nil
}

// validateHTTPURL 要求远程端点使用 HTTPS；仅 loopback（本地开发/测试）允许 http。
// 元数据 URL 来自 MCP 服务端，不能让一个远端 MCP 把客户端引去请求内网 http 服务。
func validateHTTPURL(name, raw string) error {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") {
		return fmt.Errorf("mcp auth: invalid %s %q", name, raw)
	}
	if u.Scheme == "http" && !isLoopbackHost(u.Hostname()) {
		return fmt.Errorf("mcp auth: %s must use https (http is only allowed on loopback): %q", name, raw)
	}
	return nil
}

func isLoopbackHost(host string) bool {
	host = strings.ToLower(strings.Trim(host, "[]"))
	return host == "localhost" || host == "127.0.0.1" || host == "::1"
}

func sameIssuer(a, b string) bool {
	return strings.TrimRight(strings.TrimSpace(a), "/") == strings.TrimRight(strings.TrimSpace(b), "/")
}

func dedupStrings(in []string) []string {
	seen := map[string]bool{}
	out := make([]string, 0, len(in))
	for _, s := range in {
		s = strings.TrimSpace(s)
		if s == "" || seen[s] {
			continue
		}
		seen[s] = true
		out = append(out, s)
	}
	return out
}

func snippet(body []byte) string {
	s := strings.TrimSpace(string(body))
	if len(s) > 240 {
		s = s[:240] + "…"
	}
	return s
}
