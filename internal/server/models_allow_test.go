package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/bacnh85/yardmaster/internal/auth"
	"github.com/bacnh85/yardmaster/internal/config"
	"github.com/bacnh85/yardmaster/internal/provider"
	"github.com/bacnh85/yardmaster/internal/proxy"
	"github.com/bacnh85/yardmaster/internal/store"
)

// allowHarness: chat models glm-5.3 + glm-5.3-flash, decision model
// cmd/typesafe/jev — enough shape to cover all three listing endpoints.
func allowHarness(t *testing.T, keys []*config.Key) *httptest.Server {
	t.Helper()
	cfg := &config.Config{
		Listen: ":0",
		Keys:   keys,
		Providers: []*config.Provider{
			{Name: "msh", BaseURL: "http://127.0.0.1:1", Wire: "openai", Models: []string{"glm-5.3", "glm-5.3-flash"},
				Auth: config.AuthConf{Type: "static", Keys: []string{"sk-up"}}},
			{Name: "ts", BaseURL: "http://127.0.0.1:1", Wire: "classifier", Models: []string{"typesafe/jev"},
				Auth: config.AuthConf{Type: "static", Keys: []string{"sk-up"}}},
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

// modelIDs GETs path with a bearer key and returns the advertised ids.
func modelIDs(t *testing.T, ts *httptest.Server, key, path string) []string {
	t.Helper()
	req, _ := http.NewRequest("GET", ts.URL+path, nil)
	req.Header.Set("Authorization", "Bearer "+key)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("%s: %d", path, resp.StatusCode)
	}
	var out struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
		Models []struct {
			Name string `json:"name"`
		} `json:"models"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	if out.Models != nil { // gemini shape
		ids := make([]string, len(out.Models))
		for i, m := range out.Models {
			ids[i] = m.Name
		}
		return ids
	}
	ids := make([]string, len(out.Data))
	for i, m := range out.Data {
		ids[i] = m.ID
	}
	return ids
}

func idsOf(got []string) map[string]bool {
	m := map[string]bool{}
	for _, id := range got {
		m[id] = true
	}
	return m
}

// TestModelsListRespectsKeyAllow: a scoped key's /v1/models, /v1beta/models
// and /v1/systemone/models listings mirror exactly what Registry.Resolve
// would route for that key — the same KeyAllowed matcher, same ids.
func TestModelsListRespectsKeyAllow(t *testing.T) {
	ts := allowHarness(t, []*config.Key{
		{Key: "ar-scope", Name: "scoped", Allow: []string{"glm-*"}},
		{Key: "ar-all", Name: "wide", Allow: []string{"*"}},
	})

	// scoped key: glm glob admits the two chat glm ids only
	scoped := idsOf(modelIDs(t, ts, "ar-scope", "/v1/models"))
	if !scoped["glm-5.3"] || !scoped["glm-5.3-flash"] || len(scoped) != 2 {
		t.Fatalf("scoped /v1/models = %v, want exactly the two glm ids", scoped)
	}
	if got := idsOf(modelIDs(t, ts, "ar-scope", "/v1beta/models")); len(got) != 2 {
		t.Fatalf("scoped /v1beta/models = %v, want the glm pair", got)
	}
	if got := modelIDs(t, ts, "ar-scope", "/v1/systemone/models"); len(got) != 0 {
		t.Fatalf("scoped /v1/systemone/models = %v, want empty", got)
	}

	// pairing: routing denies what listing omits — exact-id allow, not listed
	if targets := provider.New(nil).Resolve("kimi-k3", []string{"glm-*"}); len(targets) != 0 {
		t.Fatalf("routing allowed kimi-k3: %v", targets)
	}

	// wide key: regression guard — everything still advertised
	wide := idsOf(modelIDs(t, ts, "ar-all", "/v1/models"))
	if !wide["glm-5.3"] || len(wide) != 2 {
		t.Fatalf("wide /v1/models = %v, want glm pair", wide)
	}
	if got := idsOf(modelIDs(t, ts, "ar-all", "/v1/systemone/models")); !got["typesafe/jev"] || len(got) != 1 {
		t.Fatalf("wide /v1/systemone/models = %v, want typesafe/jev", got)
	}
}