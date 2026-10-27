package auth

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
)

// PKCE 是 RFC 7636 的 S256 挑战对。
type PKCE struct {
	Verifier  string
	Challenge string
}

// NewPKCE 生成 32 字节随机 verifier（43 字符 base64url）与 S256 challenge。
func NewPKCE() (PKCE, error) {
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return PKCE{}, err
	}
	verifier := base64.RawURLEncoding.EncodeToString(raw)
	sum := sha256.Sum256([]byte(verifier))
	return PKCE{
		Verifier:  verifier,
		Challenge: base64.RawURLEncoding.EncodeToString(sum[:]),
	}, nil
}

// NewState 生成一次性 CSRF state。
func NewState() (string, error) {
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(raw), nil
}

// S256Challenge 计算任意 verifier 的 S256 challenge（测试与自定义 verifier 用）。
func S256Challenge(verifier string) (string, error) {
	if verifier == "" {
		return "", errors.New("mcp auth: empty code verifier")
	}
	sum := sha256.Sum256([]byte(verifier))
	return base64.RawURLEncoding.EncodeToString(sum[:]), nil
}
