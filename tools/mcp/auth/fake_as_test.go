package auth

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync"
	"sync/atomic"
	"testing"
)

// fakeClientSecret 是机密客户端模式下 DCR 下发的 client_secret。
const fakeClientSecret = "secret-1"

// fakeAS 是一个最小的授权服务器：元数据、DCR、token、refresh、revoke。
// 授权页不真正跳转；测试直接解析授权 URL 后回调 loopback，模拟用户同意。
type fakeAS struct {
	t   *testing.T
	srv *httptest.Server

	dcr bool

	mu               sync.Mutex
	issuerOverride   string
	clientID         string
	code             string
	challenge        string
	expectedRedirect string
	expectedResource string
	refreshToken     string
	confidential     bool

	dcrCalls     atomic.Int32
	tokenCalls   atomic.Int32
	refreshCalls atomic.Int32
	revokeCalls  atomic.Int32
}

func newFakeAS(t *testing.T, dcr bool) *fakeAS {
	t.Helper()
	a := &fakeAS{t: t, dcr: dcr, clientID: "test-client", code: "code-1", refreshToken: "refresh-1"}
	mux := http.NewServeMux()
	mux.HandleFunc("/.well-known/oauth-protected-resource/mcp", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{
			"resource":              a.resourceURL(),
			"authorization_servers": []string{a.srv.URL},
			"scopes_supported":      []string{"default"},
			"resource_name":         "Test MCP",
		})
	})
	mux.HandleFunc("/.well-known/oauth-authorization-server", func(w http.ResponseWriter, r *http.Request) {
		a.mu.Lock()
		issuer := a.srv.URL
		if a.issuerOverride != "" {
			issuer = a.issuerOverride
		}
		dcr := a.dcr
		confidential := a.confidential
		a.mu.Unlock()
		methods := []string{"none"}
		if confidential {
			methods = []string{"client_secret_basic", "client_secret_post"}
		}
		meta := map[string]any{
			"issuer":                                         issuer,
			"authorization_endpoint":                         a.srv.URL + "/authorize",
			"token_endpoint":                                 a.srv.URL + "/token",
			"revocation_endpoint":                            a.srv.URL + "/revoke",
			"scopes_supported":                               []string{"default"},
			"response_types_supported":                       []string{"code"},
			"grant_types_supported":                          []string{"authorization_code", "refresh_token"},
			"token_endpoint_auth_methods_supported":          methods,
			"code_challenge_methods_supported":               []string{"S256"},
			"authorization_response_iss_parameter_supported": true,
		}
		if dcr {
			meta["registration_endpoint"] = a.srv.URL + "/register"
		}
		writeJSON(w, http.StatusOK, meta)
	})
	mux.HandleFunc("/register", func(w http.ResponseWriter, r *http.Request) {
		a.dcrCalls.Add(1)
		var req struct {
			ClientName              string   `json:"client_name"`
			RedirectURIs            []string `json:"redirect_uris"`
			TokenEndpointAuthMethod string   `json:"token_endpoint_auth_method"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeOAuthError(w, http.StatusBadRequest, "invalid_client_metadata", err.Error())
			return
		}
		if req.ClientName == "" || len(req.RedirectURIs) == 0 {
			writeOAuthError(w, http.StatusBadRequest, "invalid_client_metadata", "missing client_name/redirect_uris")
			return
		}
		resp := map[string]any{
			"client_id":                  a.getClientID(),
			"token_endpoint_auth_method": req.TokenEndpointAuthMethod,
		}
		if a.isConfidential() {
			if req.TokenEndpointAuthMethod != "client_secret_basic" && req.TokenEndpointAuthMethod != "client_secret_post" {
				writeOAuthError(w, http.StatusBadRequest, "invalid_client_metadata", "confidential AS requires a secret method")
				return
			}
			resp["client_secret"] = fakeClientSecret
		}
		writeJSON(w, http.StatusCreated, resp)
	})
	mux.HandleFunc("/token", func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			writeOAuthError(w, http.StatusBadRequest, "invalid_request", err.Error())
			return
		}
		switch r.Form.Get("grant_type") {
		case "authorization_code":
			a.tokenCalls.Add(1)
			code, challenge, redirect, resource := a.expectations()
			if r.Form.Get("code") != code {
				writeOAuthError(w, http.StatusBadRequest, "invalid_grant", "bad code")
				return
			}
			if r.Form.Get("redirect_uri") != redirect {
				writeOAuthError(w, http.StatusBadRequest, "invalid_grant", "redirect_uri mismatch")
				return
			}
			if r.Form.Get("resource") != resource {
				writeOAuthError(w, http.StatusBadRequest, "invalid_target", "resource mismatch")
				return
			}
			if err := a.checkClientAuth(r); err != nil {
				writeOAuthError(w, http.StatusBadRequest, "invalid_client", err.Error())
				return
			}
			got, _ := S256Challenge(r.Form.Get("code_verifier"))
			if challenge == "" || got != challenge {
				writeOAuthError(w, http.StatusBadRequest, "invalid_grant", "PKCE mismatch")
				return
			}
			writeJSON(w, http.StatusOK, map[string]any{
				"access_token":  "access-1",
				"token_type":    "Bearer",
				"expires_in":    3600,
				"refresh_token": "refresh-1",
				"scope":         "default",
			})
		case "refresh_token":
			a.refreshCalls.Add(1)
			if err := a.checkClientAuth(r); err != nil {
				writeOAuthError(w, http.StatusBadRequest, "invalid_client", err.Error())
				return
			}
			if r.Form.Get("refresh_token") != a.getRefreshToken() {
				writeOAuthError(w, http.StatusBadRequest, "invalid_grant", "bad refresh token")
				return
			}
			if r.Form.Get("resource") != a.getExpectedResource() {
				writeOAuthError(w, http.StatusBadRequest, "invalid_target", "resource mismatch")
				return
			}
			writeJSON(w, http.StatusOK, map[string]any{
				"access_token":  "access-2",
				"token_type":    "Bearer",
				"expires_in":    3600,
				"refresh_token": "refresh-2",
				"scope":         "default",
			})
		default:
			writeOAuthError(w, http.StatusBadRequest, "unsupported_grant_type", "")
		}
	})
	mux.HandleFunc("/revoke", func(w http.ResponseWriter, r *http.Request) {
		a.revokeCalls.Add(1)
		w.WriteHeader(http.StatusOK)
	})
	a.srv = httptest.NewServer(mux)
	t.Cleanup(a.srv.Close)
	return a
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeOAuthError(w http.ResponseWriter, status int, code, desc string) {
	writeJSON(w, status, map[string]any{"error": code, "error_description": desc})
}

func (a *fakeAS) getCode() string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.code
}

func (a *fakeAS) getClientID() string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.clientID
}

func (a *fakeAS) setClientID(id string) {
	a.mu.Lock()
	a.clientID = id
	a.mu.Unlock()
}

func (a *fakeAS) getRefreshToken() string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.refreshToken
}

func (a *fakeAS) setRefreshToken(tok string) {
	a.mu.Lock()
	a.refreshToken = tok
	a.mu.Unlock()
}

func (a *fakeAS) setIssuerOverride(issuer string) {
	a.mu.Lock()
	a.issuerOverride = issuer
	a.mu.Unlock()
}

func (a *fakeAS) setExpectations(challenge, redirect, resource string) {
	a.mu.Lock()
	a.challenge, a.expectedRedirect, a.expectedResource = challenge, redirect, resource
	a.mu.Unlock()
}

func (a *fakeAS) expectations() (code, challenge, redirect, resource string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.code, a.challenge, a.expectedRedirect, a.expectedResource
}

func (a *fakeAS) getExpectedResource() string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.expectedResource
}

func (a *fakeAS) isConfidential() bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.confidential
}

func (a *fakeAS) setConfidential(v bool) {
	a.mu.Lock()
	a.confidential = v
	a.mu.Unlock()
}

// checkClientAuth 校验 token 请求携带的客户端凭据，形态必须与注册时声明的
// token_endpoint_auth_method 一致：公开客户端走表单 client_id，机密客户端走
// Basic 头或表单 client_id+client_secret。
func (a *fakeAS) checkClientAuth(r *http.Request) error {
	clientID := a.getClientID()
	if !a.isConfidential() {
		if r.Header.Get("Authorization") != "" {
			return errors.New("public client must not use client_secret_basic")
		}
		if r.Form.Get("client_id") != clientID {
			return errors.New("client_id mismatch")
		}
		if r.Form.Get("client_secret") != "" {
			return errors.New("public client must not send a client_secret")
		}
		return nil
	}
	if user, pass, ok := r.BasicAuth(); ok {
		// RFC 6749 §2.3.1：先 application/x-www-form-urlencoded，再 base64。
		id, err1 := url.QueryUnescape(user)
		secret, err2 := url.QueryUnescape(pass)
		if err1 != nil || err2 != nil {
			return errors.New("malformed basic credentials")
		}
		if r.Form.Get("client_id") != "" {
			return errors.New("client_secret_basic must not repeat client_id in the form")
		}
		if id != clientID || secret != fakeClientSecret {
			return errors.New("basic credentials mismatch")
		}
		return nil
	}
	if r.Form.Get("client_id") != clientID {
		return errors.New("client_id mismatch")
	}
	if r.Form.Get("client_secret") != fakeClientSecret {
		return errors.New("client_secret mismatch")
	}
	return nil
}

func (a *fakeAS) resourceURL() string { return a.srv.URL + "/mcp" }

func (a *fakeAS) metadataURL() string {
	return a.srv.URL + "/.well-known/oauth-protected-resource/mcp"
}
