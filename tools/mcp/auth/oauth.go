package auth

import (
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"
)

var (
	// ErrNoToken 表示会话里还没有 access token，需要走一次浏览器授权。
	ErrNoToken = errors.New("mcp auth: no access token")
	// ErrReauthRequired 表示 refresh 已不可用（无 refresh token 或 invalid_grant），
	// 需要用户重新授权。
	ErrReauthRequired = errors.New("mcp auth: reauthorization required")
	// ErrNotFound 由 Store 实现返回，表示该 key 还没有会话。
	ErrNotFound = errors.New("mcp auth: session not found")
)

// Challenge 是 WWW-Authenticate 中 Bearer 挑战的归一化结果。
type Challenge struct {
	Scheme              string
	ResourceMetadataURL string
	Scope               string
	Error               string
	ErrorDescription    string
	Raw                 string
}

// ParseChallenge 解析 MCP 端点返回的 WWW-Authenticate。规范要求客户端从
// resource_metadata 取资源元数据；没有该字段时调用方可以走 well-known 回退。
func ParseChallenge(header string) (*Challenge, error) {
	header = strings.TrimSpace(header)
	if header == "" {
		return nil, errors.New("mcp auth: empty WWW-Authenticate")
	}
	scheme, rest := header, ""
	if i := strings.IndexAny(header, " \t"); i >= 0 {
		scheme, rest = header[:i], strings.TrimSpace(header[i+1:])
	}
	if !strings.EqualFold(scheme, "Bearer") {
		// 头里可能列了多个挑战，找 Bearer 那一段。
		lower := strings.ToLower(header)
		i := strings.Index(lower, "bearer ")
		if i < 0 {
			return nil, fmt.Errorf("mcp auth: unsupported challenge scheme %q", scheme)
		}
		rest = strings.TrimSpace(header[i+len("bearer "):])
	}
	c := &Challenge{Scheme: "Bearer", Raw: header}
	for _, part := range splitAuthParams(rest) {
		k, v, ok := strings.Cut(part, "=")
		if !ok {
			continue
		}
		k = strings.ToLower(strings.TrimSpace(k))
		v = strings.TrimSpace(v)
		if len(v) >= 2 && v[0] == '"' && v[len(v)-1] == '"' {
			v = v[1 : len(v)-1]
			v = strings.ReplaceAll(v, `\"`, `"`)
			v = strings.ReplaceAll(v, `\\`, `\`)
		}
		switch k {
		case "resource_metadata":
			c.ResourceMetadataURL = v
		case "scope":
			c.Scope = v
		case "error":
			c.Error = v
		case "error_description":
			c.ErrorDescription = v
		}
	}
	return c, nil
}

// splitAuthParams 按不在引号内的逗号切分 auth-param；引号内的逗号属于值。
func splitAuthParams(s string) []string {
	var out []string
	var b strings.Builder
	inQuote, escaped := false, false
	for _, r := range s {
		switch {
		case escaped:
			b.WriteRune(r)
			escaped = false
		case r == '\\' && inQuote:
			b.WriteRune(r)
			escaped = true
		case r == '"':
			b.WriteRune(r)
			inQuote = !inQuote
		case r == ',' && !inQuote:
			if p := strings.TrimSpace(b.String()); p != "" {
				out = append(out, p)
			}
			b.Reset()
		default:
			b.WriteRune(r)
		}
	}
	if p := strings.TrimSpace(b.String()); p != "" {
		out = append(out, p)
	}
	return out
}

// ResourceMetadata 是 RFC 9728 的受保护资源元数据。
type ResourceMetadata struct {
	Resource               string   `json:"resource"`
	AuthorizationServers   []string `json:"authorization_servers"`
	ScopesSupported        []string `json:"scopes_supported,omitempty"`
	BearerMethodsSupported []string `json:"bearer_methods_supported,omitempty"`
	ResourceName           string   `json:"resource_name,omitempty"`
}

// ServerMetadata 是 RFC 8414（或 OIDC discovery）的授权服务器元数据。
type ServerMetadata struct {
	Issuer                                     string   `json:"issuer"`
	AuthorizationEndpoint                      string   `json:"authorization_endpoint"`
	TokenEndpoint                              string   `json:"token_endpoint"`
	RegistrationEndpoint                       string   `json:"registration_endpoint,omitempty"`
	RevocationEndpoint                         string   `json:"revocation_endpoint,omitempty"`
	ScopesSupported                            []string `json:"scopes_supported,omitempty"`
	ResponseTypesSupported                     []string `json:"response_types_supported,omitempty"`
	GrantTypesSupported                        []string `json:"grant_types_supported,omitempty"`
	TokenEndpointAuthMethodsSupported          []string `json:"token_endpoint_auth_methods_supported,omitempty"`
	CodeChallengeMethodsSupported              []string `json:"code_challenge_methods_supported,omitempty"`
	AuthorizationResponseISSParameterSupported bool     `json:"authorization_response_iss_parameter_supported,omitempty"`
}

// ClientRegistration 是一次客户端注册的结果（DCR 或手工填写）。
type ClientRegistration struct {
	ClientID                string `json:"client_id"`
	ClientSecret            string `json:"client_secret,omitempty"`
	TokenEndpointAuthMethod string `json:"token_endpoint_auth_method,omitempty"`
	RedirectURI             string `json:"redirect_uri,omitempty"`
	Scope                   string `json:"scope,omitempty"`
}

// Token 是授权服务器返回的令牌；ExpiresAt 在交换/刷新时按 expires_in 计算。
type Token struct {
	AccessToken  string    `json:"access_token"`
	TokenType    string    `json:"token_type,omitempty"`
	RefreshToken string    `json:"refresh_token,omitempty"`
	ExpiresIn    int64     `json:"expires_in,omitempty"`
	Scope        string    `json:"scope,omitempty"`
	ExpiresAt    time.Time `json:"expires_at,omitempty"`
}

// Expired 报告 token 是否已过期；skew 用于提前刷新（如 60s）。
func (t Token) Expired(now time.Time, skew time.Duration) bool {
	if t.AccessToken == "" {
		return true
	}
	if t.ExpiresAt.IsZero() {
		return false
	}
	return !now.Add(skew).Before(t.ExpiresAt)
}

// AuthorizationHeader 返回 "Bearer <token>"（token_type 缺省为 Bearer）。
func (t Token) AuthorizationHeader() string {
	if t.AccessToken == "" {
		return ""
	}
	typ := strings.TrimSpace(t.TokenType)
	if typ == "" {
		typ = "Bearer"
	} else if strings.EqualFold(typ, "bearer") {
		typ = "Bearer"
	}
	return typ + " " + t.AccessToken
}

// Session 是一台 MCP server 的授权会话：client 注册 + token + 刷新所需的端点。
// 存储位置由调用方决定（文件、Keychain 等）；包里不含任何持久化假设。
type Session struct {
	Resource            string             `json:"resource"`
	AuthorizationServer string             `json:"authorization_server"`
	TokenEndpoint       string             `json:"token_endpoint,omitempty"`
	RevocationEndpoint  string             `json:"revocation_endpoint,omitempty"`
	Client              ClientRegistration `json:"client"`
	Token               Token              `json:"token"`
}

// NeedsRefresh 报告会话是否需要在 now+skew 之前刷新。
func (s Session) NeedsRefresh(now time.Time, skew time.Duration) bool {
	return s.Token.Expired(now, skew)
}

// OAuthError 是授权服务器返回的 OAuth 错误。
type OAuthError struct {
	StatusCode  int
	Code        string
	Description string
}

func (e *OAuthError) Error() string {
	if e.Description != "" {
		return fmt.Sprintf("mcp auth: oauth %s: %s (HTTP %d)", e.Code, e.Description, e.StatusCode)
	}
	return fmt.Sprintf("mcp auth: oauth %s (HTTP %d)", e.Code, e.StatusCode)
}

// RegistrationError 表示服务商不支持动态客户端注册，需要手工提供 client 凭据，
// 并把 RedirectURI 加进服务商白名单。
type RegistrationError struct {
	RedirectURI string
	Reason      string
}

func (e *RegistrationError) Error() string {
	msg := "mcp auth: dynamic client registration is unavailable"
	if e.Reason != "" {
		msg += ": " + e.Reason
	}
	if e.RedirectURI != "" {
		msg += "; register a client manually and allow redirect URI " + e.RedirectURI
	}
	return msg
}

func ensureHTTPClient(hc *http.Client) *http.Client {
	if hc == nil {
		hc = &http.Client{Timeout: 30 * time.Second}
	}
	// 克隆一份再挂重定向校验：元数据/令牌地址来自服务端，不能让 302 把请求带去
	// 非 loopback 的 http 内网地址。调用方自定义的 Redirect 策略仍然保留。
	cp := *hc
	prev := hc.CheckRedirect
	cp.CheckRedirect = func(req *http.Request, via []*http.Request) error {
		if err := validateHTTPURL("redirect URL", req.URL.String()); err != nil {
			return err
		}
		if prev != nil {
			return prev(req, via)
		}
		if len(via) >= 10 {
			return errors.New("mcp auth: stopped after 10 redirects")
		}
		return nil
	}
	return &cp
}
