package auth

// Logto OIDC 授权码流程（go-oidc + oauth2）。
//
//	GET /api/login    → 302 Logto 授权页（state 防 CSRF）
//	GET /api/callback → code 换 token → 验 ID token → session cookie
//	kb owner_sub = ID token 的 sub（飞书登录身份）。

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"net/http"
	"strings"

	"github.com/coreos/go-oidc/v3/oidc"
	"golang.org/x/oauth2"
)

// OIDC 完整客户端（Enabled 时由 api.Server 持有）。
type OIDC struct {
	Config      OIDCConfig
	ExternalURL string // redirect_uri = {ExternalURL}/api/callback

	provider *oidc.Provider
	oauth2   *oauth2.Config
	verifier *oidc.IDTokenVerifier
}

// NewOIDC 构造（不发网络请求；discover 惰性在 Login 首次触发）。
func NewOIDC(cfg OIDCConfig, externalURL string) *OIDC {
	return &OIDC{Config: cfg, ExternalURL: externalURL}
}

// init 惰性 discovery（issuer/.well-known/openid-configuration）。
func (o *OIDC) init(ctx context.Context) error {
	if o.provider != nil {
		return nil
	}
	provider, err := oidc.NewProvider(ctx, o.Config.Issuer)
	if err != nil {
		return fmt.Errorf("oidc discovery %s: %w", o.Config.Issuer, err)
	}
	o.provider = provider
	o.verifier = provider.Verifier(&oidc.Config{ClientID: o.Config.ClientID})
	redirect := o.ExternalURL + "/api/callback"
	if o.ExternalURL == "" {
		// 未配置对外地址：从请求推导交给调用方（此处兜底 localhost）
		redirect = "http://localhost:8080/api/callback"
	}
	o.oauth2 = &oauth2.Config{
		ClientID:     o.Config.ClientID,
		ClientSecret: o.Config.ClientSecret,
		RedirectURL:  redirect,
		Scopes:       []string{oidc.ScopeOpenID, "profile"},
		Endpoint:     provider.Endpoint(),
	}
	return nil
}

// stateCookieName / state 参数（CSRF）。
const stateCookieName = "agmem_oidc_state"

func randomState() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

// HandleLogin GET /api/login：302 到 Logto 授权页。
func (o *OIDC) HandleLogin(w http.ResponseWriter, r *http.Request) (bool, error) {
	if err := o.init(r.Context()); err != nil {
		return false, err
	}
	state, err := randomState()
	if err != nil {
		return false, err
	}
	http.SetCookie(w, &http.Cookie{
		Name:     stateCookieName,
		Value:    state,
		Path:     "/",
		HttpOnly: true,
		Secure:   strings.HasPrefix(o.ExternalURL, "https://"),
		MaxAge:   600,
		SameSite: http.SameSiteLaxMode,
	})
	http.Redirect(w, r, o.oauth2.AuthCodeURL(state), http.StatusFound)
	return true, nil
}

// HandleCallback GET /api/callback：code 换 token，返回 sub；失败返回 error。
func (o *OIDC) HandleCallback(w http.ResponseWriter, r *http.Request) (string, error) {
	if err := o.init(r.Context()); err != nil {
		return "", err
	}
	ck, err := r.Cookie(stateCookieName)
	if err != nil || ck.Value == "" {
		return "", fmt.Errorf("缺少 state cookie")
	}
	if r.URL.Query().Get("state") != ck.Value {
		return "", fmt.Errorf("state 不匹配")
	}
	code := r.URL.Query().Get("code")
	if code == "" {
		return "", fmt.Errorf("缺少 code（err=%s）", r.URL.Query().Get("error"))
	}
	ctx := r.Context()
	tok, err := o.oauth2.Exchange(ctx, code)
	if err != nil {
		return "", fmt.Errorf("token exchange: %w", err)
	}
	rawID, ok := tok.Extra("id_token").(string)
	if !ok || rawID == "" {
		return "", fmt.Errorf("token 响应缺少 id_token")
	}
	idToken, err := o.verifier.Verify(ctx, rawID)
	if err != nil {
		return "", fmt.Errorf("id_token verify: %w", err)
	}
	var claims struct {
		Sub      string `json:"sub"`
		Username string `json:"username"`
	}
	if err := idToken.Claims(&claims); err != nil {
		return "", err
	}
	if claims.Sub == "" {
		return "", fmt.Errorf("id_token 无 sub")
	}
	// 用完即弃 state cookie
	http.SetCookie(w, &http.Cookie{Name: stateCookieName, Value: "", Path: "/", MaxAge: -1})
	return claims.Sub, nil
}
