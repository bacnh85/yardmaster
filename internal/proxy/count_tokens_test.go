package proxy

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/bacnh85/yardmaster/internal/auth"
	"github.com/bacnh85/yardmaster/internal/config"
	"github.com/bacnh85/yardmaster/internal/provider"
)

const countTokensBody = `{"model":"glm-5.3","messages":[{"role":"user","content":"hello world, this is a long-ish message for the estimator"}]}`

// TestCountTokensUpstreamPassthrough: an anthropic-wire provider must answer
// from its /v1/messages/count_tokens endpoint with the mapped model id and
// the same auth headers the chat path uses.
func TestCountTokensUpstreamPassthrough(t *testing.T) {
	var gotPath, gotAuth, gotVer string
	var gotBody map[string]any
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotAuth = r.Header.Get("x-api-key")
		gotVer = r.Header.Get("anthropic-version")
		json.NewDecoder(r.Body).Decode(&gotBody)
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"input_tokens":123}`))
	}))
	defer up.Close()

	cfg := &config.Config{Providers: []*config.Provider{
		{Name: "zai", Prefix: "zai", BaseURL: up.URL + "/v1", Wire: "anthropic", Models: []string{"glm-5.3"},
			ModelMap: map[string]string{"glm-5.3": "glm-5.3-flash"},
			Auth:     config.AuthConf{Type: "static", Keys: []string{"sk-up"}}}}}
	p := NewProxy(provider.New(cfg), nil, nil)

	n, ok := p.CountTokens(context.Background(), []byte(countTokensBody))
	if !ok || n != 123 {
		t.Fatalf("CountTokens = (%d, %v), want (123, true)", n, ok)
	}
	if gotPath != "/v1/messages/count_tokens" {
		t.Fatalf("upstream path %q, want /v1/messages/count_tokens (zen-style /v1 base must not double)", gotPath)
	}
	if gotAuth != "sk-up" {
		t.Fatalf("x-api-key %q, want sk-up", gotAuth)
	}
	if gotVer != "2023-06-01" {
		t.Fatalf("anthropic-version %q, want 2023-06-01", gotVer)
	}
	if gotBody["model"] != "glm-5.3-flash" {
		t.Fatalf("upstream model %v, want mapped glm-5.3-flash", gotBody["model"])
	}
}

// Upstream 500 must degrade to (0, false) — the server falls back to the
// estimate — and must NOT trip cooldown breakers (count_tokens is advisory).
func TestCountTokensUpstreamErrorNoCooldownMark(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(500)
		w.Write([]byte(`{"error":"boom"}`))
	}))
	defer up.Close()

	cfg := &config.Config{Providers: []*config.Provider{
		{Name: "p", BaseURL: up.URL, Wire: "anthropic", Models: []string{"m"},
			Auth: config.AuthConf{Type: "static", Keys: []string{"sk"}}}}}
	p := NewProxy(provider.New(cfg), nil, nil)

	if n, ok := p.CountTokens(context.Background(), []byte(countTokensBody)); ok {
		t.Fatalf("CountTokens = (%d, true), want ok=false on 500", n)
	}
	if p.Cd.Cooling("p", "sk") {
		t.Fatal("500 marked the key cooling — advisory path must never trip breakers")
	}
}

// Openai-wire-only config: no anthropic target exists, so nothing is sent
// upstream and the count falls back.
func TestCountTokensNoAnthropicTarget(t *testing.T) {
	var calls atomic.Int32
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.Write([]byte(`{"input_tokens":999}`))
	}))
	defer up.Close()

	cfg := &config.Config{Providers: []*config.Provider{
		{Name: "openai-only", BaseURL: up.URL, Wire: "openai", Models: []string{"glm-5.3"},
			Auth: config.AuthConf{Type: "static", Keys: []string{"sk"}}}}}
	p := NewProxy(provider.New(cfg), nil, nil)

	if n, ok := p.CountTokens(context.Background(), []byte(countTokensBody)); ok {
		t.Fatalf("CountTokens = (%d, true), want ok=false with no anthropic provider", n)
	}
	if calls.Load() != 0 {
		t.Fatalf("openai upstream hit %d times, want 0", calls.Load())
	}
}

// A cooled anthropic key is skipped in favor of a healthy one; if all are
// cooled the count falls back without touching the upstream.
func TestCountTokensCooledTargetSkipped(t *testing.T) {
	var calls int32
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		if r.Header.Get("x-api-key") == "sk-hot" {
			// a broken skip would dispatch on the cooled key; the 500 makes
			// that outcome visible (calls would be 2 and the result would
			// still be true via the fallback target)
			w.WriteHeader(500)
			return
		}
		w.Write([]byte(`{"input_tokens":7}`))
	}))
	defer up.Close()

	cfg := &config.Config{Providers: []*config.Provider{
		{Name: "p", BaseURL: up.URL, Wire: "anthropic", Models: []string{"m"},
			Auth: config.AuthConf{Type: "static", Keys: []string{"sk-hot", "sk-cold"}}}}}
	p := NewProxy(provider.New(cfg), nil, nil)
	p.Cd.Mark429("p", "sk-hot", "60") // rotation tries this key first — must be skipped, not marked again

	n, ok := p.CountTokens(context.Background(), []byte(`{"model":"m","messages":[{"role":"user","content":"hi"}]}`))
	if !ok || n != 7 {
		t.Fatalf("CountTokens = (%d, %v), want (7, true) via the healthy key", n, ok)
	}
	if atomic.LoadInt32(&calls) != 1 {
		t.Fatalf("upstream calls = %d, want 1", atomic.LoadInt32(&calls))
	}
	if !p.Cd.Cooling("p", "sk-hot") || p.Cd.Cooling("p", "sk-cold") {
		t.Fatal("cooldown marks changed on the advisory path")
	}
}

// Malformed bodies (bad json, missing model/messages) fall back without any
// upstream traffic.
func TestCountTokensBadBody(t *testing.T) {
	cfg := &config.Config{Providers: []*config.Provider{
		{Name: "p", BaseURL: "http://127.0.0.1:1", Wire: "anthropic", Models: []string{"m"},
			Auth: config.AuthConf{Type: "static", Keys: []string{"sk"}}}}}
	p := NewProxy(provider.New(cfg), nil, nil)

	for name, body := range map[string]string{
		"bad json":     `{model:`,
		"no messages":  `{"model":"m"}`,
		"model not id": `{"messages":[]}`,
		"empty model":  `{"model":"","messages":[]}`,
	} {
		if _, ok := p.CountTokens(context.Background(), []byte(body)); ok {
			t.Fatalf("%s: want ok=false", name)
		}
	}
}

// claude-code oauth accounts authenticate count_tokens with the same bearer +
// anthropic-beta headers as chat (routed through setOAuthAuth).
func TestCountTokensOAuthClaudeCode(t *testing.T) {
	var gotAuth, gotBeta string
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		gotBeta = r.Header.Get("anthropic-beta")
		w.Write([]byte(`{"input_tokens":42}`))
	}))
	defer up.Close()
	tokSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{"access_token": "at-ct", "refresh_token": "rt-new", "expires_in": 3600})
	}))
	defer tokSrv.Close()

	st := mustStore(t)
	cfg := &config.Config{Providers: []*config.Provider{{
		Name: "sub", BaseURL: up.URL, Wire: "anthropic", Models: []string{"m"},
		Auth: config.AuthConf{Type: "oauth", OAuth: []*config.OAuthAcct{{
			Name: "acc1", Kind: "claude-code", RefreshTok: "rt-orig",
			TokenEndpoint: tokSrv.URL + "/token", ClientID: "cid",
			ExpiresAt: time.Now().Add(-time.Minute).Unix(),
		}}},
	}}}
	pool := auth.NewOAuthPool(st)
	t.Cleanup(pool.Close)
	pool.RegisterConfig(cfg)
	p := NewProxy(provider.New(cfg), st, nil)
	p.Pool = pool

	n, ok := p.CountTokens(context.Background(), []byte(`{"model":"m","messages":[{"role":"user","content":"hi"}]}`))
	if !ok || n != 42 {
		t.Fatalf("CountTokens = (%d, %v), want (42, true)", n, ok)
	}
	if gotAuth != "Bearer at-ct" {
		t.Fatalf("Authorization %q, want bearer at-ct", gotAuth)
	}
	if !strings.Contains(gotBeta, "oauth-2025-04-20") {
		t.Fatalf("anthropic-beta %q, want oauth beta marker", gotBeta)
	}
}

// A caller deadline tighter than the 10s cap must be respected.
func TestCountTokensRespectsCallerDeadline(t *testing.T) {
	released := make(chan struct{})
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-released // hold until the client gives up
	}))
	defer up.Close()
	defer close(released)

	cfg := &config.Config{Providers: []*config.Provider{
		{Name: "p", BaseURL: up.URL, Wire: "anthropic", Models: []string{"m"},
			Auth: config.AuthConf{Type: "static", Keys: []string{"sk"}}}}}
	p := NewProxy(provider.New(cfg), nil, nil)

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	start := time.Now()
	if _, ok := p.CountTokens(ctx, []byte(`{"model":"m","messages":[]}`)); ok {
		t.Fatal("want ok=false when the upstream hangs")
	}
	if el := time.Since(start); el > 5*time.Second {
		t.Fatalf("returned after %v — caller deadline was ignored", el)
	}
}
