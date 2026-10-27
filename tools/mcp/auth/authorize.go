package auth

import (
	"fmt"
	"net/url"
	"strings"
)

// AuthorizeOptions 是构造授权 URL 的参数。
type AuthorizeOptions struct {
	ClientID      string
	RedirectURI   string
	Scopes        []string
	State         string
	CodeChallenge string
	Resource      string
	// Extra 用于服务商特有的附加参数（如 audience）；同名参数不会覆盖标准字段。
	Extra url.Values
}

// AuthorizeURL 按 RFC 6749/7636/8707 拼授权地址：code + PKCE S256 + state + resource。
func AuthorizeURL(as *ServerMetadata, opts AuthorizeOptions) (string, error) {
	if as == nil {
		return "", fmt.Errorf("mcp auth: missing authorization server metadata")
	}
	if strings.TrimSpace(as.AuthorizationEndpoint) == "" {
		return "", fmt.Errorf("mcp auth: authorization_endpoint is empty")
	}
	if strings.TrimSpace(opts.ClientID) == "" {
		return "", fmt.Errorf("mcp auth: client_id is empty")
	}
	if strings.TrimSpace(opts.RedirectURI) == "" {
		return "", fmt.Errorf("mcp auth: redirect_uri is empty")
	}
	if strings.TrimSpace(opts.State) == "" || strings.TrimSpace(opts.CodeChallenge) == "" {
		return "", fmt.Errorf("mcp auth: state and code challenge are required")
	}
	u, err := url.Parse(as.AuthorizationEndpoint)
	if err != nil || u.Host == "" {
		return "", fmt.Errorf("mcp auth: invalid authorization_endpoint %q", as.AuthorizationEndpoint)
	}
	q := u.Query()
	for k, values := range opts.Extra {
		for _, v := range values {
			q.Add(k, v)
		}
	}
	q.Set("response_type", "code")
	q.Set("client_id", opts.ClientID)
	q.Set("redirect_uri", opts.RedirectURI)
	q.Set("state", opts.State)
	q.Set("code_challenge", opts.CodeChallenge)
	q.Set("code_challenge_method", "S256")
	if len(opts.Scopes) > 0 {
		q.Set("scope", strings.Join(opts.Scopes, " "))
	}
	if strings.TrimSpace(opts.Resource) != "" {
		q.Set("resource", opts.Resource)
	}
	u.RawQuery = q.Encode()
	return u.String(), nil
}
