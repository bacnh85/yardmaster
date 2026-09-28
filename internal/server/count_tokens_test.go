package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/bacnh85/yardmaster/internal/auth"
	"github.com/bacnh85/yardmaster/internal/config"
	"github.com/bacnh85/yardmaster/internal/provider"
	"github.com/bacnh85/yardmaster/internal/proxy"
	"github.com/bacnh85/yardmaster/internal/store"
)

const ctBody = `{"model":"glm-5.3","messages":[{"role":"user","content":"hello world, this is a long-ish message for the estimator fallback"}]}`

func postCountTokens(t *testing.T, ts *httptest.Server, body string) map[string]any {
	t.Helper()
	req, _ := http.NewRequest("POST", ts.URL+"/v1/messages/count_tokens", strings.NewReader(body))
	req.Header.Set("x-api-key", "ar-agent")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("count_tokens: status %d, want 200", resp.StatusCode)
	}
	var out map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	return out
}

func countTokensSrv(t *testing.T, providers []*config.Provider) *httptest.Server {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	cfg := &config.Config{
		Listen:    ":0",
		Keys:      []*config.Key{{Key: "ar-agent", Name: "pi", Allow: []string{"*"}}},
		Providers: providers,
	}
	cfg.Defaults()
	cfg.Validate()
	p := proxy.NewProxy(provider.New(cfg), st, cfg.CostFor)
	p.Version = "test"
	srv := New(p, auth.NewKeyStore(cfg.Keys), st, "", "pw", "test")
	return httptest.NewServer(srv.Handler())
}

// Anthropic upstream configured → the endpoint answers with the upstream's
// exact number, not the chars/4 estimate.
func TestCountTokensEndToEndAnthropicUpstream(t *testing.T) {
	var calls int32
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/messages/count_tokens" {
			t.Errorf("upstream path %q", r.URL.Path)
		}
		atomic.AddInt32(&calls, 1)
		w.Write([]byte(`{"input_tokens":321}`))
	}))
	defer up.Close()

	srv := countTokensSrv(t, []*config.Provider{
		{Name: "zai", BaseURL: up.URL, Wire: "anthropic", Models: []string{"glm-5.3"},
			Auth: config.AuthConf{Type: "static", Keys: []string{"sk-up"}}},
	})

	out := postCountTokens(t, srv, ctBody)
	if n, ok := out["input_tokens"].(float64); !ok || n != 321 {
		t.Fatalf("input_tokens = %v, want 321 from upstream", out["input_tokens"])
	}
	if atomic.LoadInt32(&calls) != 1 {
		t.Fatalf("upstream calls = %d, want 1", calls)
	}
}

// Only openai providers configured → falls back to the chars/4 estimate: no
// upstream traffic, and a number that differs from what the upstream path
// would have returned (321 here) — the estimate undercounts.
func TestCountTokensEndToEndEstimateFallback(t *testing.T) {
	var calls int32
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		w.Write([]byte(`{"input_tokens":321}`))
	}))
	defer up.Close()

	srv := countTokensSrv(t, []*config.Provider{
		{Name: "openai-only", BaseURL: up.URL, Wire: "openai", Models: []string{"glm-5.3"},
			Auth: config.AuthConf{Type: "static", Keys: []string{"sk-up"}}},
	})

	out := postCountTokens(t, srv, ctBody)
	n, ok := out["input_tokens"].(float64)
	if !ok || n <= 0 {
		t.Fatalf("input_tokens = %v, want a positive estimate", out["input_tokens"])
	}
	if n == 321 {
		t.Fatal("estimate collided with the upstream answer — fallback not distinguishable")
	}
	if atomic.LoadInt32(&calls) != 0 {
		t.Fatalf("openai upstream hit %d times, want 0", calls)
	}
}
