package server

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/bacnh85/yardmaster/internal/auth"
	"github.com/bacnh85/yardmaster/internal/config"
	"github.com/bacnh85/yardmaster/internal/proxy"
	"github.com/bacnh85/yardmaster/internal/provider"
	"github.com/bacnh85/yardmaster/internal/store"
)

// wildcardHarness mirrors prod openrouter: prefixed wildcard chat provider
// (empty Models) + a curated classifier sibling on the same prefix.
func wildcardHarness(t *testing.T) *httptest.Server {
	t.Helper()
	cfg := &config.Config{
		Listen: ":0",
		Keys:   []*config.Key{{Key: "ar-x", Name: "pi", Allow: []string{"*"}}},
		Providers: []*config.Provider{
			{Name: "openrouter", Prefix: "or", BaseURL: "https://openrouter.ai/api/v1", Wire: "openai",
				Auth: config.AuthConf{Type: "static", Keys: []string{"sk-or-x"}}},
			{Name: "or-clf", Prefix: "or", BaseURL: "https://openrouter.ai/api/v1", Wire: "classifier",
				Models: []string{"typesafe/jev-1.13"}, Auth: config.AuthConf{Type: "static", Keys: []string{"sk-or-x"}}},
		},
	}
	cfg.Defaults()
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	reg := provider.New(cfg)
	p := proxy.NewProxy(reg, st, cfg.CostFor)
	srv := New(p, auth.NewKeyStore(cfg.Keys), st, "", "secretpw", "test")
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	return ts
}

func TestWildcardPrefixModelsEndpoint(t *testing.T) {
	// cold cache (WarmCatalogs hasn't run): wildcard provider advertises nothing
	ts := wildcardHarness(t)
	if ids := modelIDs(t, ts, "ar-x", "/v1/models"); len(ids) != 0 {
		t.Fatalf("cold cache must advertise nothing, got %v", ids)
	}

	// seed the catalog cache the way WarmCatalogs would
	catalogCache.Store("https://openrouter.ai/api/v1", catalogEntry{
		until: time.Now().Add(time.Hour),
		models: []ModelMeta{
			{ID: "deepseek/deepseek-v4.1-flash", Context: 131072, Input: 0.3, Output: 1.2},
			{ID: "z-ai/glm-5.3", Context: 131072, Input: 0.6, Output: 2.2},
		},
	})
	ids := modelIDs(t, ts, "ar-x", "/v1/models")
	want := map[string]bool{"or/deepseek/deepseek-v4.1-flash": false, "or/z-ai/glm-5.3": false}
	for _, id := range ids {
		if _, ok := want[id]; ok {
			want[id] = true
		}
		if strings.HasPrefix(id, "or/typesafe") {
			t.Fatalf("classifier id leaked onto chat /v1/models: %v", ids)
		}
	}
	for id, seen := range want {
		if !seen {
			t.Fatalf("%s missing from /v1/models: %v", id, ids)
		}
	}

	// metadata rides the advertisement (context_length from the catalog meta)
	req, _ := http.NewRequest("GET", ts.URL+"/v1/models", nil)
	req.Header.Set("Authorization", "Bearer ar-x")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var out struct {
		Data []struct {
			ID            string `json:"id"`
			ContextLength int    `json:"context_length"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	for _, m := range out.Data {
		if m.ID == "or/deepseek/deepseek-v4.1-flash" && m.ContextLength != 131072 {
			t.Fatalf("or/deepseek context_length = %d, want 131072", m.ContextLength)
		}
	}
}

// A wildcard-prefixed request must RESOLVE (reach the upstream attempt) —
// the dead base_url turns that into a 502-style failure, never 404 unknown-model.
func TestWildcardPrefixResolveNotUnknown(t *testing.T) {
	ts := wildcardHarness(t)
	body := `{"model":"or/deepseek/deepseek-v4.1-flash","messages":[{"role":"user","content":"hi"}]}`
	req, _ := http.NewRequest("POST", ts.URL+"/v1/chat/completions", bytes.NewBufferString(body))
	req.Header.Set("Authorization", "Bearer ar-x")
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == 404 {
		t.Fatalf("wildcard prefixed id 404'd — resolve broken")
	}
}
