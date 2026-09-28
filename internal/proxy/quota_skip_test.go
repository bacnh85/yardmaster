package proxy

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/bacnh85/yardmaster/internal/config"
	"github.com/bacnh85/yardmaster/internal/provider"
)

// tsOf wraps a proxy in a test HTTP server.
func tsOf(p *Proxy) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(p.ServeChat))
}

// chatOnce posts a minimal non-streaming chat completion through p and
// returns the assistant content of the response.
func chatOnce(t *testing.T, p *Proxy) string {
	t.Helper()
	resp, err := http.Post(tsOf(p).URL, "application/json",
		strings.NewReader(`{"model":"m","stream":false}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 {
		t.Fatalf("status %d: %s", resp.StatusCode, body)
	}
	var out struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		t.Fatalf("bad json: %v", err)
	}
	return out.Choices[0].Message.Content
}

// Quota-aware routing: targets whose provider reports exhausted billing
// windows are skipped like cooling targets (no upstream attempt burned), the
// chain continues; when EVERY target is quota-skipped the request must still
// be served (fail-open on stale quota data) by attempting the first skipped
// target. Nil QuotaExhausted / flag-off providers keep plain routing.
func TestQuotaExhaustedSkip(t *testing.T) {
	var callsA, callsB atomic.Int32
	upA := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		callsA.Add(1)
		w.WriteHeader(200)
		w.Write([]byte(`{"id":"x","object":"chat.completion","model":"m","choices":[{"index":0,"message":{"role":"assistant","content":"from-a"},"finish_reason":"stop"}],"usage":{}}`))
	}))
	defer upA.Close()
	upB := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		callsB.Add(1)
		w.WriteHeader(200)
		w.Write([]byte(`{"id":"x","object":"chat.completion","model":"m","choices":[{"index":0,"message":{"role":"assistant","content":"from-b"},"finish_reason":"stop"}],"usage":{}}`))
	}))
	defer upB.Close()

	newCfg := func(skipA bool) *config.Config {
		return &config.Config{Providers: []*config.Provider{
			{Name: "a", BaseURL: upA.URL, Wire: "openai", Models: []string{"m"}, SkipWhenExhausted: skipA,
				Auth: config.AuthConf{Keys: []string{"k1"}}},
			{Name: "b", BaseURL: upB.URL, Wire: "openai", Models: []string{"m"},
				Auth: config.AuthConf{Keys: []string{"k2"}}},
		}}
	}

	// a exhausted + flag on → skipped without an upstream attempt, b serves
	p := NewProxy(provider.New(newCfg(true)), nil, nil)
	p.QuotaExhausted = func(provider string) bool { return provider == "a" }
	if got := chatOnce(t, p); got != "from-b" {
		t.Fatalf("exhausted provider a: got %q, want failover to b", got)
	}
	if callsA.Load() != 0 {
		t.Fatalf("exhausted provider a got %d upstream calls, want 0", callsA.Load())
	}
	if callsB.Load() != 1 {
		t.Fatalf("provider b calls: %d, want 1", callsB.Load())
	}

	// every target exhausted + flag on → fail-open: a attempted and serves
	// (single-provider chain so ALL targets are quota-skipped)
	callsA.Store(0)
	singleCfg := &config.Config{Providers: []*config.Provider{
		{Name: "a", BaseURL: upA.URL, Wire: "openai", Models: []string{"m"}, SkipWhenExhausted: true,
			Auth: config.AuthConf{Keys: []string{"k1"}}},
	}}
	p2 := NewProxy(provider.New(singleCfg), nil, nil)
	p2.QuotaExhausted = func(string) bool { return true }
	if got := chatOnce(t, p2); got != "from-a" {
		t.Fatalf("all-exhausted fail-open: got %q, want a attempted and serving", got)
	}
	if callsA.Load() != 1 {
		t.Fatalf("fail-open provider a calls: %d, want 1", callsA.Load())
	}

	// flag off on a → a attempted normally even though quota hook reports it
	callsA.Store(0)
	callsB.Store(0)
	callsB.Store(0)
	p3 := NewProxy(provider.New(newCfg(false)), nil, nil)
	p3.QuotaExhausted = func(string) bool { return true }
	if got := chatOnce(t, p3); got != "from-a" {
		t.Fatalf("flag off: got %q, want a serving normally", got)
	}
	if callsA.Load() != 1 {
		t.Fatalf("flag-off provider a calls: %d, want 1", callsA.Load())
	}

	// nil QuotaExhausted → plain routing, no quota consulted
	p4 := NewProxy(provider.New(newCfg(true)), nil, nil)
	if got := chatOnce(t, p4); got != "from-a" {
		t.Fatalf("nil hook: got %q, want a serving", got)
	}
}

// Fail-open must honor the combo model override: a combo member that is
// quota-skipped has to dispatch its member-specific upstream id, not the
// virtual combo id, and price the natural model.
func TestQuotaFailOpenComboOverride(t *testing.T) {
	var gotModel atomic.Value
	var upCalls atomic.Int32
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upCalls.Add(1)
		var body map[string]any
		json.NewDecoder(r.Body).Decode(&body)
		gotModel.Store(body["model"])
		w.Write([]byte(`{"id":"x","object":"chat.completion","model":"member-id","choices":[{"index":0,"message":{"role":"assistant","content":"from-member"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1000000,"completion_tokens":0}}`))
	}))
	defer up.Close()

	cfg := &config.Config{
		Providers: []*config.Provider{
			{Name: "a", BaseURL: up.URL, Wire: "openai", SkipWhenExhausted: true,
				Auth: config.AuthConf{Keys: []string{"k1"}}},
		},
		Combos: []*config.Combo{{
			Name: "pool", Model: "member-id",
			Members: []*config.ComboMember{{Provider: "a", Model: "member-id"}},
		}},
	}
	p := NewProxy(provider.New(cfg), nil, func(m string) config.Cost {
		if m == "member-id" {
			return config.Cost{Input: 1} // $1/Mtok → 1M tok = $1
		}
		return config.Cost{} // combo id / unknown → free
	})
	p.QuotaExhausted = func(string) bool { return true }

	// request the combo id; all its targets are quota-skipped → fail open
	ts := httptest.NewServer(http.HandlerFunc(p.ServeChat))
	defer ts.Close()
	resp, err := http.Post(ts.URL, "application/json",
		strings.NewReader(`{"model":"combo/pool","stream":false,"messages":[{"role":"user","content":"hi"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("status %d", resp.StatusCode)
	}
	if m, _ := gotModel.Load().(string); m != "member-id" {
		t.Fatalf("upstream model = %q, want member-id (combo override ignored on fail-open)", m)
	}
	if upCalls.Load() != 1 {
		t.Fatalf("upstream calls = %d, want 1", upCalls.Load())
	}
	// cost header is only set on non-stream passthrough responses; this
	// upstream is same-wire so it should carry the natural-model price
	if got := resp.Header.Get("X-Yardmaster-Cost-Usd"); got != "1.0000" {
		t.Fatalf("cost header = %q, want 1.0000 (priced on the natural model)", got)
	}
}
