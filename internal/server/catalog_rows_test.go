package server

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/bacnh85/yardmaster/internal/auth"
	"github.com/bacnh85/yardmaster/internal/config"
	"github.com/bacnh85/yardmaster/internal/provider"
	"github.com/bacnh85/yardmaster/internal/proxy"
	"github.com/bacnh85/yardmaster/internal/store"
)

// TestCatalogRows merges two providers sharing one gateway base_url: the same
// catalog id from both merges into ONE row with two provider entries (canon
// dedupe), curated-only ids appear with exposed=true, and exposedOnly filters
// to rows at least one provider serves. Cache-stubbed — no network.
func TestCatalogRows(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	cfg := &config.Config{
		Listen: ":0",
		Keys:   []*config.Key{{Key: "k", Name: "pi", Allow: []string{"*"}}},
		Providers: []*config.Provider{
			{Name: "zen-chat", BaseURL: "https://gw.example/v1", Wire: "openai", Models: []string{"m-chat"}},
			{Name: "zen-claude", BaseURL: "https://gw.example/v1", Wire: "anthropic", Models: []string{"m-chat", "m-hidden"}},
		},
	}
	cfg.Defaults()
	cfg.Validate()
	p := proxy.NewProxy(provider.New(cfg), st, cfg.CostFor)
	srv := New(p, auth.NewKeyStore(cfg.Keys), st, "", "pw", "test")

	catalogCache.Store("https://gw.example/v1", catalogEntry{models: []ModelMeta{
		{ID: "m-chat", Family: "chat", Input: 1, Output: 2},
		{ID: "m-orphan", Family: "chat", Input: -1, Output: -1},
	}, until: time.Now().Add(time.Hour)})
	t.Cleanup(func() { catalogCache.Delete("https://gw.example/v1") })

	all := srv.catalogRows(t.Context(), false)
	// 3 rows: m-chat (cached catalog, both providers), m-hidden (curated by
	// zen-claude, absent from the cache → manual meta), m-orphan (cached only)
	if len(all) != 3 {
		t.Fatalf("want 3 deduped rows, got %d: %+v", len(all), all)
	}
	if all[0].ID != "m-chat" || all[1].ID != "m-hidden" || all[2].ID != "m-orphan" {
		t.Fatalf("rows must sort by id: %+v", all)
	}
	if len(all[0].Providers) != 2 {
		t.Fatalf("shared gateway id must merge into one row with 2 providers: %+v", all[0].Providers)
	}
	if !all[0].Providers[0].Exposed || !all[0].Providers[1].Exposed || !all[0].exposedAny() {
		t.Fatalf("m-chat exposed on both: %+v", all[0].Providers)
	}
	if !all[1].Manual || !all[1].exposedAny() {
		t.Fatalf("curated-only id must be manual + exposed: %+v", all[1])
	}
	if all[2].exposedAny() {
		t.Fatalf("m-orphan is in no provider's list: %+v", all[2])
	}

	exposed := srv.catalogRows(t.Context(), true)
	if len(exposed) != 2 || exposed[0].ID != "m-chat" || exposed[1].ID != "m-hidden" {
		t.Fatalf("exposedOnly must drop unexposed rows: %+v", exposed)
	}
}

// TestCatalogEndpoint: GET /admin/api/catalog returns the rows JSON, and the
// exposed=1 flag filters server-side.
func TestCatalogEndpoint(t *testing.T) {
	stubDev := httptest.NewServer(nil) // never reached: catalog is cache-stubbed
	defer stubDev.Close()
	oldURL := modelsDevURL
	modelsDevURL = stubDev.URL
	resetModelsDevCache()
	t.Cleanup(func() { modelsDevURL = oldURL; resetModelsDevCache() })

	st, err := store.Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	cfg := &config.Config{
		Listen: ":0",
		Keys:   []*config.Key{{Key: "k", Name: "pi", Allow: []string{"*"}}},
		Providers: []*config.Provider{
			{Name: "p1", BaseURL: "https://ep.example/v1", Wire: "openai", Models: []string{"m-a"}},
		},
	}
	cfg.Defaults()
	cfg.Validate()
	p := proxy.NewProxy(provider.New(cfg), st, cfg.CostFor)
	srv := New(p, auth.NewKeyStore(cfg.Keys), st, "", "pw", "test")
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()
	admin := func(path string) (int, string) {
		req, _ := http.NewRequest("GET", ts.URL+"/admin/api/"+path, nil)
		req.SetBasicAuth("", "pw")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(b)
	}
	catalogCache.Store("https://ep.example/v1", catalogEntry{models: []ModelMeta{
		{ID: "m-a", Family: "chat", Context: 1000000, Input: 0.5, Output: 1.5, Reasoning: true},
		{ID: "m-z", Family: "chat"},
	}, until: time.Now().Add(time.Hour)})
	t.Cleanup(func() { catalogCache.Delete("https://ep.example/v1") })

	code, body := admin("catalog?exposed=1")
	if code != 200 || !strings.Contains(body, `"m-a"`) {
		t.Fatalf("catalog?exposed=1: %d %s", code, body)
	}
	if strings.Contains(body, `"m-z"`) {
		t.Fatalf("unexposed row leaked into exposed=1: %s", body)
	}
	var got struct {
		Models []CatalogRow `json:"models"`
	}
	if err := json.Unmarshal([]byte(body), &got); err != nil {
		t.Fatal(err)
	}
	if len(got.Models) != 1 || got.Models[0].Context != 1000000 || !got.Models[0].Reasoning ||
		len(got.Models[0].Providers) != 1 || !got.Models[0].Providers[0].Exposed {
		t.Fatalf("row shape wrong: %+v", got.Models)
	}
	if code, _ = admin("catalog"); code != 200 {
		t.Fatalf("bare catalog: %d", code)
	}
}
