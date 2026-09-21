package server

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/bacnh85/yardmaster/internal/auth"
	"github.com/bacnh85/yardmaster/internal/config"
	"github.com/bacnh85/yardmaster/internal/provider"
	"github.com/bacnh85/yardmaster/internal/proxy"
	"github.com/bacnh85/yardmaster/internal/store"
	"gopkg.in/yaml.v3"
)

// Deleting a provider must keep weighted-rr routes valid: weights stay
// index-aligned with the surviving chain, and routes whose chain empties are
// dropped — otherwise mutate→Validate 400s and the provider is undeletable.
func TestProviderDeleteTrimsRouteWeights(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(503) // never called; config surgery only
	}))
	defer up.Close()

	cfgPath := filepath.Join(t.TempDir(), "config.yaml")
	cfg := &config.Config{
		Listen: ":0",
		Keys:   []*config.Key{{Key: "ar-agent", Name: "pi", Allow: []string{"*"}}},
		Providers: []*config.Provider{
			{Name: "a", BaseURL: up.URL, Wire: "openai", Auth: config.AuthConf{Keys: []string{"k1"}}},
			{Name: "b", BaseURL: up.URL, Wire: "openai", Auth: config.AuthConf{Keys: []string{"k2"}}},
		},
		Routes: []*config.Route{
			{Match: "m", Chain: []string{"a", "b"}, Strategy: "weighted-rr", Weights: []int{3, 1}},
			{Match: "solo*", Chain: []string{"a"}, Strategy: "weighted-rr", Weights: []int{2}},
		},
	}
	b, _ := yaml.Marshal(cfg)
	if err := os.WriteFile(cfgPath, b, 0o600); err != nil {
		t.Fatal(err)
	}
	cfg.Defaults()
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}

	st, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	p := proxy.NewProxy(provider.New(cfg), st, cfg.CostFor)
	p.Version = "test"
	srv := New(p, auth.NewKeyStore(cfg.Keys), st, cfgPath, "pw", "test")
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()
	admin := func(method, path string) (int, []byte) {
		req, _ := http.NewRequest(method, ts.URL+"/admin/api/"+path, nil)
		req.SetBasicAuth("", "pw")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, b
	}

	if code, body := admin("DELETE", "providers/a"); code != 200 {
		t.Fatalf("delete provider a: %d %s", code, body)
	}

	_, body := admin("GET", "config")
	var dump struct {
		Routes []*config.Route `json:"routes"`
	}
	if err := json.Unmarshal(body, &dump); err != nil {
		t.Fatal(err)
	}
	if len(dump.Routes) != 1 {
		t.Fatalf("want 1 route (empty-chain route dropped), got %d: %s", len(dump.Routes), body)
	}
	rt := dump.Routes[0]
	if rt.Match != "m" || len(rt.Chain) != 1 || rt.Chain[0] != "b" {
		t.Fatalf("route m chain not trimmed: %s", body)
	}
	if len(rt.Weights) != 1 || rt.Weights[0] != 1 {
		t.Fatalf("route m weights not trimmed: %s", body)
	}

	// the trimmed config must still validate on disk (reload path)
	loaded, err := config.Load(cfgPath)
	if err != nil {
		t.Fatalf("saved config invalid: %v", err)
	}
	if len(loaded.Routes) != 1 || len(loaded.Routes[0].Weights) != 1 {
		t.Fatalf("reloaded routes wrong: %+v", loaded.Routes)
	}
}
