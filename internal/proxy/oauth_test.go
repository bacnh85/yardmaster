package proxy

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/bacnh85/yardmaster/internal/auth"
	"github.com/bacnh85/yardmaster/internal/config"
	"github.com/bacnh85/yardmaster/internal/provider"
	"github.com/bacnh85/yardmaster/internal/store"
)

// oauthHarness wires a proxy against a fake token endpoint + upstream,
// returning the upstream's last received headers.
type oauthHarness struct {
	p       *Proxy
	upHdr   http.Header
	upCalls atomic.Int32
}

func newOAuthHarness(t *testing.T, kind, wire string) *oauthHarness {
	t.Helper()
	h := &oauthHarness{}

	// upstream: serves one chunk of its native wire, echoing received headers
	var upURL string
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h.upCalls.Add(1)
		h.upHdr = r.Header.Clone()
		if wire == "responses" {
			// codex backend: responses SSE
			w.Header().Set("Content-Type", "text/event-stream")
			f := w.(http.Flusher)
			ev := func(name string, data map[string]any) {
				b, _ := json.Marshal(data)
				fmt.Fprintf(w, "event: %s\ndata: %s\n\n", name, b)
			}
			ev("response.output_text.delta", map[string]any{"delta": "hey"})
			ev("response.completed", map[string]any{"response": map[string]any{
				"usage": map[string]any{"input_tokens": 3, "output_tokens": 1,
					"input_tokens_details": map[string]any{"cached_tokens": 1}}}})
			f.Flush()
			return
		}
		// anthropic wire (claude-code path)
		w.Header().Set("Content-Type", "text/event-stream")
		f := w.(http.Flusher)
		fmt.Fprintf(w, "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"usage\":{\"input_tokens\":2}}}\n\n")
		fmt.Fprintf(w, "event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"delta\":{\"type\":\"text_delta\",\"text\":\"hi\"}}\n\n")
		fmt.Fprintf(w, "event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n")
		f.Flush()
	}))
	upURL = up.URL
	t.Cleanup(up.Close)

	// token endpoint: issues a fresh access token
	tokSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		json.NewDecoder(r.Body).Decode(&body)
		if body["grant_type"] != "refresh_token" {
			t.Errorf("bad grant: %v", body)
		}
		json.NewEncoder(w).Encode(map[string]any{"access_token": "at-test", "refresh_token": "rt-new", "expires_in": 3600})
	}))
	t.Cleanup(tokSrv.Close)

	st, err := store.Open(t.TempDir() + "/t.db")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })

	cfg := &config.Config{Providers: []*config.Provider{{
		Name: "sub", BaseURL: upURL, Wire: wire, Models: []string{"m"},
		Auth: config.AuthConf{Type: "oauth", OAuth: []*config.OAuthAcct{{
			Name: "acc1", Kind: kind, RefreshTok: "rt-orig", AccountID: "acct-77",
			TokenEndpoint: tokSrv.URL + "/token", ClientID: "cid",
			ExpiresAt: time.Now().Add(-time.Minute).Unix(),
		}}},
	}}}
	pool := auth.NewOAuthPool(st)
	t.Cleanup(pool.Close)
	pool.RegisterConfig(cfg)

	h.p = NewProxy(provider.New(cfg), st, nil)
	h.p.Pool = pool
	return h
}

func TestOAuthCodexResponsesUpstream(t *testing.T) {
	h := newOAuthHarness(t, "codex", "responses")
	ts := httptest.NewServer(http.HandlerFunc(h.p.ServeChat))
	defer ts.Close()

	resp, err := http.Post(ts.URL, "application/json",
		strings.NewReader(`{"model":"m","stream":true,"messages":[{"role":"user","content":"yo"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("status %d", resp.StatusCode)
	}
	_, lines := readSSE(t, resp.Body)
	if len(lines) == 0 || lines[len(lines)-1] != "[DONE]" {
		t.Fatalf("lines: %v", lines)
	}
	joined := strings.Join(lines, "\n")
	if !strings.Contains(joined, `"content":"hey"`) {
		t.Fatalf("translated content missing: %s", joined)
	}
	if !strings.Contains(joined, `"prompt_tokens":3`) {
		t.Fatalf("usage missing: %s", joined)
	}
	// auth + codex headers on the upstream call
	if got := h.upHdr.Get("Authorization"); got != "Bearer at-test" {
		t.Fatalf("upstream auth: %q", got)
	}
	if got := h.upHdr.Get("chatgpt-account-id"); got != "acct-77" {
		t.Fatalf("account id header: %q", got)
	}
	if got := h.upHdr.Get("OpenAI-Beta"); got != "responses=experimental" {
		t.Fatalf("openai-beta: %q", got)
	}
	// request hit /responses
	if h.upCalls.Load() != 1 {
		t.Fatalf("upstream calls: %d", h.upCalls.Load())
	}
}

func TestOAuthClaudeCodeAnthropicWire(t *testing.T) {
	h := newOAuthHarness(t, "claude-code", "anthropic")
	ts := httptest.NewServer(http.HandlerFunc(h.p.ServeMessages)) // anthropic client
	defer ts.Close()

	resp, err := http.Post(ts.URL, "application/json",
		strings.NewReader(`{"model":"m","stream":true,"max_tokens":10,"messages":[{"role":"user","content":"yo"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("status %d", resp.StatusCode)
	}
	_, lines := readSSE(t, resp.Body)
	// anthropic passthrough (same wire): anthropic events flow through verbatim
	if len(lines) == 0 || !strings.Contains(strings.Join(lines, "\n"), "message_stop") {
		t.Fatalf("lines: %v", lines)
	}
	if got := h.upHdr.Get("Authorization"); got != "Bearer at-test" {
		t.Fatalf("upstream auth: %q", got)
	}
	if got := h.upHdr.Get("x-api-key"); got != "" {
		t.Fatalf("oauth on anthropic wire must not use x-api-key: %q", got)
	}
	if got := h.upHdr.Get("anthropic-beta"); got != "oauth-2025-04-20" {
		t.Fatalf("anthropic-beta: %q", got)
	}
}

func TestOAuthCooledAccountSkipped(t *testing.T) {
	h := newOAuthHarness(t, "codex", "responses")
	// cool the account before the request
	h.p.Pool.MarkResult("sub", "acc1", 429, "code 1302")
	ts := httptest.NewServer(http.HandlerFunc(h.p.ServeChat))
	defer ts.Close()

	resp, err := http.Post(ts.URL, "application/json",
		strings.NewReader(`{"model":"m","stream":true,"messages":[{"role":"user","content":"yo"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if h.upCalls.Load() != 0 {
		t.Fatal("cooled account must not be dispatched")
	}
	// all targets cooling → 429 + Retry-After (not a 502 that invites retries)
	if resp.StatusCode != 429 {
		t.Fatalf("want 429 when all targets cooling, got %d", resp.StatusCode)
	}
	if ra := resp.Header.Get("Retry-After"); ra == "" || ra == "0" {
		t.Fatalf("Retry-After = %q, want remaining cooldown seconds", ra)
	}
}

// Regression: a successful dispatch must report back to the pool, resetting
// the cooldown ladder — otherwise one stale 429 keeps re-cooling an account
// at the maximum backoff forever.
func TestOAuthSuccessResetsCooldownLadder(t *testing.T) {
	var calls atomic.Int32
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) == 1 { // first dispatch (account a1): quota error
			w.WriteHeader(429)
			w.Write([]byte(`{"error":{"code":1302}}`))
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		f := w.(http.Flusher)
		ev := func(name string, data map[string]any) {
			b, _ := json.Marshal(data)
			fmt.Fprintf(w, "event: %s\ndata: %s\n\n", name, b)
		}
		ev("response.output_text.delta", map[string]any{"delta": "ok"})
		ev("response.completed", map[string]any{"response": map[string]any{
			"usage": map[string]any{"input_tokens": 1, "output_tokens": 1}}})
		f.Flush()
	}))
	defer up.Close()

	st, err := store.Open(t.TempDir() + "/t.db")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	cfg := &config.Config{Providers: []*config.Provider{{
		Name: "sub", BaseURL: up.URL, Wire: "responses", Models: []string{"m"},
		Auth: config.AuthConf{Type: "oauth", OAuth: []*config.OAuthAcct{
			{Name: "a1", Kind: "codex", AccessTok: "at-a1"},
			{Name: "a2", Kind: "codex", AccessTok: "at-a2"},
		}},
	}}}
	pool := auth.NewOAuthPool(st)
	defer pool.Close()
	pool.RegisterConfig(cfg)
	p := NewProxy(provider.New(cfg), st, nil)
	p.Pool = pool
	ts := httptest.NewServer(http.HandlerFunc(p.ServeChat))
	defer ts.Close()

	post := func() int {
		resp, err := http.Post(ts.URL, "application/json",
			strings.NewReader(`{"model":"m","stream":true,"messages":[{"role":"user","content":"yo"}]}`))
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp.StatusCode
	}

	// request 1: a1 → 429 (cooled), failover to a2 → 200 (MarkResult 200)
	if code := post(); code != 200 {
		t.Fatalf("first request: %d", code)
	}
	s1 := pool.States("sub")["a1"]
	if !s1.Cooling || s1.LastError == "" {
		t.Fatalf("a1 should be cooling with an error: %+v", s1)
	}
	if s2 := pool.States("sub")["a2"]; s2.Cooling || s2.LastError != "" {
		t.Fatalf("a2 success must leave it clean: %+v", s2)
	}

	// request 2: a1 is skipped (cooling), a2 serves again
	if code := post(); code != 200 {
		t.Fatalf("second request: %d", code)
	}
	if n := calls.Load(); n != 3 { // a1 once, a2 twice
		t.Fatalf("upstream calls: %d (cooled a1 must not be re-dispatched)", n)
	}
}
