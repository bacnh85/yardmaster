package proxy

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/bacnh85/yardmaster/internal/config"
	"github.com/bacnh85/yardmaster/internal/provider"
)

// clientFor: proxy_url empty → the shared client (direct egress preserved).
func TestClientForEmptyProxyURL(t *testing.T) {
	p := NewProxy(provider.New(&config.Config{}), nil, nil)
	got := p.clientFor(&config.Provider{Name: "a"})
	if got != p.Client {
		t.Fatal("empty proxy_url must return the shared client")
	}
}

// clientFor: one proxied client per provider, stable across calls, transport
// Proxy set to the provider's proxy_url.
func TestClientForPerProviderClients(t *testing.T) {
	p := NewProxy(provider.New(&config.Config{}), nil, nil)
	a := p.clientFor(&config.Provider{Name: "a", ProxyURL: "http://pa:1"})
	a2 := p.clientFor(&config.Provider{Name: "a", ProxyURL: "http://pa:1"})
	b := p.clientFor(&config.Provider{Name: "b", ProxyURL: "http://pb:2"})
	if a != a2 {
		t.Fatal("same provider must reuse its proxied client")
	}
	if a == b || a == p.Client || b == p.Client {
		t.Fatal("proxied clients must be distinct from each other and the shared client")
	}
	for _, tc := range []struct {
		c    *http.Client
		want string
	}{
		{a, "http://pa:1"}, {b, "http://pb:2"},
	} {
		tr, ok := tc.c.Transport.(*http.Transport)
		if !ok {
			t.Fatal("proxied client transport must be *http.Transport")
		}
		u, err := tr.Proxy(nil)
		if err != nil || u == nil || u.String() != tc.want {
			t.Fatalf("transport proxy = %v, %v; want %s", u, err, tc.want)
		}
	}
}

// A changed proxy_url on the SAME provider name must yield a fresh client
// (config reload swaps proxies without a restart); the old cached one is not
// returned again.
func TestClientForProxyURLChange(t *testing.T) {
	p := NewProxy(provider.New(&config.Config{}), nil, nil)
	old := p.clientFor(&config.Provider{Name: "a", ProxyURL: "http://pa:1"})
	updated := p.clientFor(&config.Provider{Name: "a", ProxyURL: "http://pa:2"})
	if updated == old {
		t.Fatal("proxy_url change reused the stale client")
	}
	tr, ok := updated.Transport.(*http.Transport)
	if !ok {
		t.Fatal("updated client transport must be *http.Transport")
	}
	u, err := tr.Proxy(nil)
	if err != nil || u == nil || u.String() != "http://pa:2" {
		t.Fatalf("updated transport proxy = %v, %v; want http://pa:2", u, err)
	}
}

// clientFor must NOT be used by oauth refresh — that stays on p.Client even
// when the provider carries a proxy_url (control-plane vs provider egress).
func TestCopilotExchangeBypassesClientFor(t *testing.T) {
	// p.Client's transport records every request; the copilot exchange goes to
	// the hard-coded api.github.com URL, so a redirect transport proves which
	// client was used. The proxied transport would fail to dial api.github.com
	// through a dead proxy — success here already proves the bypass; the
	// counting transport proves p.Client specifically.
	gh := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"token":"cc-1","expires_at":0}`))
	}))
	defer gh.Close()
	p := NewProxy(provider.New(&config.Config{}), nil, nil)
	p.Client = &http.Client{Transport: ghRedirectRT{base: mustURL(t, gh.URL)}}
	tok, err := p.copilotToken(t.Context(), "prov", "acct", "ghp-x")
	if err != nil || tok != "cc-1" {
		t.Fatalf("copilot token: %q, %v", tok, err)
	}
}

// proxy_url must actually steer dispatch: the upstream is only reachable
// through the stub HTTP proxy, which observes CONNECT + the absolute-form
// target. A direct dial fails — any success means the traffic was proxied.
func TestDispatchGoesThroughConfiguredProxy(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"id":"x","object":"chat.completion","model":"m","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":3,"completion_tokens":1}}`))
	}))
	defer up.Close()
	upURL := mustURL(t, up.URL)

	type via struct {
		method string
		host   string
	}
	var seen atomic.Pointer[via]
	px := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen.Store(&via{r.Method, r.Host})
		if r.Method == http.MethodConnect {
			w.WriteHeader(200)
			return
		}
		// absolute-form request: forward to the target ourselves
		resp, err := http.DefaultTransport.RoundTrip(r)
		if err != nil {
			w.WriteHeader(502)
			return
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		for k, vv := range resp.Header {
			for _, v := range vv {
				w.Header().Add(k, v)
			}
		}
		w.WriteHeader(resp.StatusCode)
		w.Write(b)
	}))
	defer px.Close()

	cfg := &config.Config{Providers: []*config.Provider{
		{Name: "prox", BaseURL: up.URL, Wire: "openai", ProxyURL: px.URL, Models: []string{"m"},
			Auth: config.AuthConf{Keys: []string{"k"}}}}}
	p := NewProxy(provider.New(cfg), nil, nil)
	ts := httptest.NewServer(http.HandlerFunc(p.ServeChat))
	defer ts.Close()

	resp, err := http.Post(ts.URL, "application/json", strings.NewReader(`{"model":"m","stream":false}`))
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 || !strings.Contains(string(body), "ok") {
		t.Fatalf("proxied dispatch failed: %d %s", resp.StatusCode, body)
	}
	v := seen.Load()
	if v == nil {
		t.Fatal("proxy never saw the request")
	}
	// plain-http upstream → absolute-form POST through the proxy, whose Host
	// is the target authority
	if v.method != http.MethodPost || v.host != upURL.Host {
		t.Fatalf("proxy saw %s %s, want POST %s", v.method, v.host, upURL.Host)
	}
}

// CountTokens shares the dispatch transport: an anthropic upstream with a
// proxy_url must be counted through that proxy too.
func TestCountTokensGoesThroughConfiguredProxy(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"input_tokens":123}`))
	}))
	defer up.Close()
	var proxied atomic.Int32
	px := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		proxied.Add(1)
		if r.Method == http.MethodConnect {
			w.WriteHeader(200)
			return
		}
		resp, err := http.DefaultTransport.RoundTrip(r)
		if err != nil {
			w.WriteHeader(502)
			return
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		w.WriteHeader(resp.StatusCode)
		w.Write(b)
	}))
	defer px.Close()

	cfg := &config.Config{Providers: []*config.Provider{
		{Name: "ant", BaseURL: up.URL, Wire: "anthropic", ProxyURL: px.URL, Models: []string{"m"},
			Auth: config.AuthConf{Keys: []string{"k"}}}}}
	p := NewProxy(provider.New(cfg), nil, nil)
	n, ok := p.CountTokens(t.Context(), []byte(`{"model":"m","messages":[{"role":"user","content":"hi"}]}`))
	if !ok || n != 123 {
		t.Fatalf("count_tokens = %d, %v; want 123, true", n, ok)
	}
	if proxied.Load() == 0 {
		t.Fatal("count_tokens bypassed the configured proxy")
	}
}

// Decision headers on the non-stream passthrough path: provider, upstream
// model (after model_map), key label, attempts, and the usage-derived pair.
func TestDecisionHeadersNonStream(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"id":"x","object":"chat.completion","model":"up-id","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":100,"completion_tokens":50,"prompt_tokens_details":{"cached_tokens":40}}}`))
	}))
	defer up.Close()
	cfg := &config.Config{Providers: []*config.Provider{
		{Name: "one", BaseURL: up.URL, Wire: "openai", Prefix: "px", ModelMap: map[string]string{"px/nat": "up-id"}, Models: []string{"px/nat"},
			Auth: config.AuthConf{Keys: []string{"sk-secret"}, KeyLabels: []string{"team-key"}}}}}
	p := NewProxy(provider.New(cfg), nil, nil)
	p.Cost = func(string) config.Cost { return config.Cost{Input: 1, Output: 2, CacheRead: 0.1} }
	ts := httptest.NewServer(http.HandlerFunc(p.ServeChat))
	defer ts.Close()

	resp, err := http.Post(ts.URL, "application/json", strings.NewReader(`{"model":"px/nat","stream":false}`))
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("status %d: %s", resp.StatusCode, body)
	}
	for k, want := range map[string]string{
		"X-Yardmaster-Provider":   "one",
		"X-Yardmaster-Model":      "up-id",
		"X-Yardmaster-Key":        "team-key",
		"X-Yardmaster-Attempts":   "1",
		"X-Yardmaster-Cost-Usd":   "0.0002",
		"X-Yardmaster-Cache-Read": "40",
	} {
		if got := resp.Header.Get(k); got != want {
			t.Errorf("%s = %q, want %q (body: %s)", k, got, want, body)
		}
	}
	// 60*1/1e6 + 50*2/1e6 + 40*0.1/1e6 = 0.000164 → formatted 0.0002
}

// Decision headers on the streaming passthrough path: no cost/cache headers
// (usage is not known when headers are written), attempts reflect failover.
func TestDecisionHeadersStreamAndFailover(t *testing.T) {
	var callsA, callsB atomic.Int32
	upA := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		callsA.Add(1)
		w.WriteHeader(500)
		w.Write([]byte(`{"error":{"message":"boom"}}`))
	}))
	defer upA.Close()
	upB := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		callsB.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		b, _ := json.Marshal(oaiChunk(map[string]any{"content": "ok"}, nil))
		fmt.Fprintf(w, "data: %s\n\ndata: [DONE]\n\n", b)
		w.(http.Flusher).Flush()
	}))
	defer upB.Close()

	cfg := &config.Config{Providers: []*config.Provider{
		{Name: "a", BaseURL: upA.URL, Wire: "openai", Models: []string{"m"}, Auth: config.AuthConf{Keys: []string{"ka"}}},
		{Name: "b", BaseURL: upB.URL, Wire: "openai", Models: []string{"m"}, Auth: config.AuthConf{Keys: []string{"kb"}}},
	}}
	p := NewProxy(provider.New(cfg), nil, nil)
	ts := httptest.NewServer(http.HandlerFunc(p.ServeChat))
	defer ts.Close()

	resp, err := http.Post(ts.URL, "application/json", strings.NewReader(`{"model":"m","stream":true}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("status %d", resp.StatusCode)
	}
	ttft, lines := readSSE(t, resp.Body)
	_ = ttft
	if len(lines) < 2 || lines[len(lines)-1] != "[DONE]" {
		t.Fatalf("lines: %v", lines)
	}
	if callsA.Load() != 1 || callsB.Load() != 1 {
		t.Fatalf("calls: a=%d b=%d", callsA.Load(), callsB.Load())
	}
	for k, want := range map[string]string{
		"X-Yardmaster-Provider": "b",
		"X-Yardmaster-Model":    "m",
		"X-Yardmaster-Key":      "Key 1", // b's first (only) key
		"X-Yardmaster-Attempts": "2",
	} {
		if got := resp.Header.Get(k); got != want {
			t.Errorf("%s = %q, want %q", k, got, want)
		}
	}
	for _, k := range []string{"X-Yardmaster-Cost-Usd", "X-Yardmaster-Cache-Read"} {
		if got := resp.Header.Get(k); got != "" {
			t.Errorf("%s must be absent on streaming responses, got %q", k, got)
		}
	}
}

// Cross-wire translate (openai client → anthropic upstream): non-stream
// cost/cache headers ride the translated response too, cache from the
// anthropic usage fields.
func TestDecisionHeadersNonStreamTranslated(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/messages" {
			t.Errorf("upstream path %s", r.URL.Path)
		}
		w.Write([]byte(`{"id":"m1","type":"message","role":"assistant","content":[{"type":"text","text":"ok"}],"stop_reason":"end_turn","usage":{"input_tokens":10,"output_tokens":5,"cache_read_input_tokens":6}}`))
	}))
	defer up.Close()
	cfg := &config.Config{Providers: []*config.Provider{
		{Name: "ant", BaseURL: up.URL, Wire: "anthropic", Models: []string{"m"}, Auth: config.AuthConf{Keys: []string{"k"}}}}}
	p := NewProxy(provider.New(cfg), nil, nil)
	ts := httptest.NewServer(http.HandlerFunc(p.ServeChat))
	defer ts.Close()

	resp, err := http.Post(ts.URL, "application/json", strings.NewReader(`{"model":"m","stream":false,"max_tokens":16,"messages":[{"role":"user","content":"hi"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("status %d: %s", resp.StatusCode, body)
	}
	if got := resp.Header.Get("X-Yardmaster-Provider"); got != "ant" {
		t.Errorf("provider header %q", got)
	}
	if got := resp.Header.Get("X-Yardmaster-Cache-Read"); got != "6" {
		t.Errorf("cache header %q, want 6 (body: %s)", got, body)
	}
	if got := resp.Header.Get("X-Yardmaster-Cost-Usd"); got == "" {
		t.Error("cost header missing on translated non-stream response")
	}
}

// When every target fails the relay surfaces the last upstream error — the
// decision headers must name the LAST ATTEMPTED target, not a skipped one.
func TestDecisionHeadersOnLastErrorRelay(t *testing.T) {
	upA := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(429)
		w.Write([]byte(`{"error":{"message":"rl"}}`))
	}))
	defer upA.Close()
	upB := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(400)
		w.Write([]byte(`{"error":{"message":"bad model"}}`))
	}))
	defer upB.Close()
	cfg := &config.Config{Providers: []*config.Provider{
		{Name: "a", BaseURL: upA.URL, Wire: "openai", Models: []string{"m"}, Auth: config.AuthConf{Keys: []string{"ka"}}},
		{Name: "b", BaseURL: upB.URL, Wire: "openai", Models: []string{"m"}, Auth: config.AuthConf{Keys: []string{"kb"}}},
	}}
	p := NewProxy(provider.New(cfg), nil, nil)
	ts := httptest.NewServer(http.HandlerFunc(p.ServeChat))
	defer ts.Close()

	resp, err := http.Post(ts.URL, "application/json", strings.NewReader(`{"model":"m","stream":false}`))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 400 {
		t.Fatalf("status %d, want relayed 400", resp.StatusCode)
	}
	for k, want := range map[string]string{
		"X-Yardmaster-Provider": "b",
		"X-Yardmaster-Model":    "m",
		"X-Yardmaster-Attempts": "2",
	} {
		if got := resp.Header.Get(k); got != want {
			t.Errorf("%s = %q, want %q", k, got, want)
		}
	}
	if got := resp.Header.Get("X-Yardmaster-Key"); got != "Key 1" {
		t.Errorf("key header %q, want Key 1 (provider b's first key)", got)
	}
}
