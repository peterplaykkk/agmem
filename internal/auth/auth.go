// Package auth：鉴权（Logto OIDC 飞书登录 + agmem 机器 Bearer token）。
package auth

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"net/http"
	"sync"
	"time"
)

// MachineAuth 校验 agmem 的 Bearer token（sha256 后恒时比较，避免明文驻留）。
type MachineAuth struct {
	sha256hex string
}

// NewMachineAuth 用明文 token 构造（token 来自 env AGMEM_MACHINE_TOKEN）。
func NewMachineAuth(token string) *MachineAuth {
	sum := sha256.Sum256([]byte(token))
	return &MachineAuth{sha256hex: hex.EncodeToString(sum[:])}
}

// Check 恒时比较 Bearer token。
func (m *MachineAuth) Check(r *http.Request) bool {
	tok := bearerToken(r)
	if tok == "" {
		return false
	}
	sum := sha256.Sum256([]byte(tok))
	return subtle.ConstantTimeCompare([]byte(hex.EncodeToString(sum[:])), []byte(m.sha256hex)) == 1
}

func bearerToken(r *http.Request) string {
	const p = "Bearer "
	h := r.Header.Get("Authorization")
	if len(h) > len(p) && h[:len(p)] == p {
		return h[len(p):]
	}
	return ""
}

// ReadonlyAuth 校验只读客户端 Bearer token（AGMEM_CLIENT_READONLY_TOKEN，允许多个）。
//
// 与 MachineAuth 同款 sha256 恒时比较；多令牌场景遍历全部候选、累积比较结果，
// 不做「命中即提前返回」的短路，避免通过响应时延区分令牌下标。令牌集合在构造时
// 定型，运行期只读。空集合的 Check 恒为 false（未配置时等于无只读身份）。
type ReadonlyAuth struct {
	sha256hex []string
}

// NewReadonlyAuth 用明文令牌集合构造（空/全空集合返回的对象 Check 恒 false）。
func NewReadonlyAuth(tokens []string) *ReadonlyAuth {
	a := &ReadonlyAuth{}
	for _, t := range tokens {
		if t == "" {
			continue
		}
		sum := sha256.Sum256([]byte(t))
		a.sha256hex = append(a.sha256hex, hex.EncodeToString(sum[:]))
	}
	return a
}

// Check 恒时比较 Bearer token：任一候选匹配即 true。
func (a *ReadonlyAuth) Check(r *http.Request) bool {
	if a == nil || len(a.sha256hex) == 0 {
		return false
	}
	tok := bearerToken(r)
	if tok == "" {
		return false
	}
	sum := sha256.Sum256([]byte(tok))
	got := hex.EncodeToString(sum[:])
	var hit int
	for _, want := range a.sha256hex {
		hit |= subtle.ConstantTimeCompare([]byte(got), []byte(want))
	}
	return hit == 1
}

// OIDCConfig Logto OIDC（飞书登录）配置。骨架期先固化配置面；
type OIDCConfig struct {
	Issuer       string
	ClientID     string
	ClientSecret string
}

// Enabled 是否配置了 OIDC（未配置时登录路由返回 503）。
func (c OIDCConfig) Enabled() bool { return c.Issuer != "" && c.ClientID != "" }

// Session 内存 session 存储（骨架期单副本可用；多副本需迁 Redis/DB——已记入待评审）。
type Session struct {
	mu    sync.Mutex
	store map[string]sessionEntry
	ttl   time.Duration
}

type sessionEntry struct {
	Subject string
	Expires time.Time
}

// NewSession 创建内存 session 存储。
func NewSession(ttl time.Duration) *Session {
	return &Session{store: map[string]sessionEntry{}, ttl: ttl}
}

// Put 写入 session，返回随机 session id。
func (s *Session) Put(subject string) (string, error) {
	b := make([]byte, 32)
	if _, err := randRead(b); err != nil {
		return "", err
	}
	id := hex.EncodeToString(b)
	s.mu.Lock()
	defer s.mu.Unlock()
	s.store[id] = sessionEntry{Subject: subject, Expires: time.Now().Add(s.ttl)}
	return id, nil
}

// Get 校验并返回 subject；过期或不存在返回 ""。
func (s *Session) Get(id string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	e, ok := s.store[id]
	if !ok || time.Now().After(e.Expires) {
		delete(s.store, id)
		return ""
	}
	return e.Subject
}

// randRead 抽离以便测试注入。
var randRead = func(b []byte) (int, error) {
	return rand.Read(b)
}
