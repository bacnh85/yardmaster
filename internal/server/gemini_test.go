package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/bacnh85/yardmaster/internal/auth"
	"github.com/bacnh85/yardmaster/internal/config"
	"github.com/bacnh85/yardmaster/internal/provider"
	"github.com/bacnh85/yardmaster/internal/proxy"
	"github.com/bacnh85/yardmaster/internal/store"
)

// geminiSrv harness: one anthropic upstream + one key, full server surface.
func geminiSrv(t *testing.T, providers []*config.Provider) *httptest.Server {
	t.Helper()
	st, err := store.Open(t.TempDir() + "/t.db")
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
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	reg := provider.New(cfg)
	p := proxy.NewProxy(reg, st, cfg.CostFor)
	p.Version = "test"
	srv := New(p, auth.NewKeyStore(cfg.Keys), st, "", "secretpw", "test")
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	return ts
}

// TestGeminiRouteAndAuth: the /v1beta route serves through the full wrap
// (auth + budget + allow), authenticates via x-goog-api-key, and a bad key
// 401s in the generic dialect (wrap's, before the gemini handler runs).
func TestGeminiRouteAndAuth(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/messages" {
			t.Errorf("upstream path: %s", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"id":"msg_1","type":"message","role":"assistant","model":"claude-x",` +
			`"content":[{"type":"text","text":"via-key"}],"stop_reason":"end_turn",` +
			`"usage":{"input_tokens":3,"output_tokens":2}}`))
	}))
	defer up.Close()

	ts := geminiSrv(t, []*config.Provider{
		{Name: "zai", BaseURL: up.URL, Wire: "anthropic", Models: []string{"claude-x"},
			Auth: config.AuthConf{Type: "static", Keys: []string{"zk"}}}})

	// x-goog-api-key authenticates the gemini-native route
	req, _ := http.NewRequest("POST", ts.URL+"/v1beta/models/claude-x:generateContent",
		strings.NewReader(`{"contents":[{"role":"user","parts":[{"text":"hi"}]}]}`))
	req.Header.Set("x-goog-api-key", "ar-agent")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("x-goog-api-key request: %d", resp.StatusCode)
	}
	var out map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	cands, _ := out["candidates"].([]any)
	if len(cands) != 1 {
		t.Fatalf("candidates: %v", cands)
	}
	parts, _ := cands[0].(map[string]any)["content"].(map[string]any)["parts"].([]any)
	if len(parts) == 0 || parts[0].(map[string]any)["text"] != "via-key" {
		t.Fatalf("parts: %v", parts)
	}

	// unknown key on the same path → 401
	req2, _ := http.NewRequest("POST", ts.URL+"/v1beta/models/claude-x:generateContent",
		strings.NewReader(`{"contents":[]}`))
	req2.Header.Set("x-goog-api-key", "ar-wrong")
	resp2, _ := http.DefaultClient.Do(req2)
	resp2.Body.Close()
	if resp2.StatusCode != 401 {
		t.Fatalf("bad key: %d, want 401", resp2.StatusCode)
	}

	// Bearer also authenticates (gemini-sdk sends Bearer when given a key)
	req3, _ := http.NewRequest("POST", ts.URL+"/v1beta/models/claude-x:streamGenerateContent?alt=sse",
		strings.NewReader(`{"contents":[{"role":"user","parts":[{"text":"hi"}]}]}`))
	req3.Header.Set("Authorization", "Bearer ar-agent")
	resp3, err := http.DefaultClient.Do(req3)
	if err != nil {
		t.Fatal(err)
	}
	defer resp3.Body.Close()
	if resp3.StatusCode != 200 {
		t.Fatalf("bearer on gemini route: %d", resp3.StatusCode)
	}
}

// TestGeminiAdvertisedModelsUnchanged: /v1/models advertisement follows the
// existing prefix rules for gemini providers — prefixed ids appear, and adding
// a gemini provider does not change the count for non-gemini providers.
func TestGeminiAdvertisedModelsUnchanged(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("no dispatch expected")
	}))
	defer up.Close()

	without := geminiSrv(t, []*config.Provider{
		{Name: "zai", BaseURL: up.URL, Wire: "anthropic", Models: []string{"claude-x"},
			Auth: config.AuthConf{Type: "static", Keys: []string{"zk"}}}})

	count := func(ts *httptest.Server) int {
		req, _ := http.NewRequest("GET", ts.URL+"/v1/models", nil)
		req.Header.Set("Authorization", "Bearer ar-agent")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		var out struct {
			Data []struct {
				ID string `json:"id"`
			} `json:"data"`
		}
		if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
			t.Fatal(err)
		}
		return len(out.Data)
	}

	base := count(without)
	if base != 1 {
		t.Fatalf("baseline advertised models: %d, want 1", base)
	}

	with := geminiSrv(t, []*config.Provider{
		{Name: "zai", BaseURL: up.URL, Wire: "anthropic", Models: []string{"claude-x"},
			Auth: config.AuthConf{Type: "static", Keys: []string{"zk"}}},
		{Name: "gem", Prefix: "g", BaseURL: up.URL, Wire: "gemini", Models: []string{"gem-pro"},
			Auth: config.AuthConf{Type: "static", Keys: []string{"gk"}}}})

	if got := count(with); got != base+1 {
		t.Fatalf("advertised models with gemini provider: %d, want %d (prefixed id added)", got, base+1)
	}
}

// TestGeminiListModels: GET /v1beta/models serves the Gemini CLI discovery
// shape (name + supportedGenerationMethods) behind the normal auth wrap.
func TestGeminiListModels(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("no dispatch expected")
	}))
	defer up.Close()

	ts := geminiSrv(t, []*config.Provider{
		{Name: "gem", BaseURL: up.URL, Wire: "gemini", Models: []string{"gem-pro"},
			Auth: config.AuthConf{Type: "static", Keys: []string{"gk"}}}})

	req, _ := http.NewRequest("GET", ts.URL+"/v1beta/models", nil)
	req.Header.Set("x-goog-api-key", "ar-agent")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("status %d", resp.StatusCode)
	}
	var out struct {
		Models []struct {
			Name                       string   `json:"name"`
			SupportedGenerationMethods []string `json:"supportedGenerationMethods"`
		} `json:"models"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	if len(out.Models) == 0 {
		t.Fatal("no models advertised")
	}
	found := false
	for _, m := range out.Models {
		if m.Name == "gem-pro" {
			found = true
			if len(m.SupportedGenerationMethods) != 2 ||
				m.SupportedGenerationMethods[0] != "generateContent" ||
				m.SupportedGenerationMethods[1] != "streamGenerateContent" {
				t.Fatalf("methods = %v", m.SupportedGenerationMethods)
			}
		}
	}
	if !found {
		t.Fatalf("gem-pro missing from %v", out.Models)
	}

	// a bad key must still 401 through the wrap
	req2, _ := http.NewRequest("GET", ts.URL+"/v1beta/models", nil)
	req2.Header.Set("x-goog-api-key", "nope")
	resp2, err := http.DefaultClient.Do(req2)
	if err != nil {
		t.Fatal(err)
	}
	resp2.Body.Close()
	if resp2.StatusCode != 401 {
		t.Fatalf("bad key: status %d, want 401", resp2.StatusCode)
	}
}
