package server

import (
	"encoding/json"
	"io"
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

// classifierFixture spins a mock System One upstream + a server exposing
// openrouter-classifier (or/typesafe/jev-1.13) and typesafe (jev/jev-latest).
func classifierFixture(t *testing.T) (*httptest.Server, *int) {
	t.Helper()
	hits := 0
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		if !strings.HasSuffix(r.URL.Path, "/systemone") {
			t.Errorf("upstream path = %q, want …/systemone", r.URL.Path)
		}
		if ah := r.Header.Get("Authorization"); ah != "Bearer sk-up" {
			t.Errorf("upstream auth = %q, want Bearer sk-up", ah)
		}
		var req map[string]any
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Errorf("upstream body decode: %v", err)
		}
		m, _ := req["model"].(string)
		if m != "typesafe/jev-1.13" && m != "jev-latest" {
			t.Errorf("upstream model = %v, want typesafe/jev-1.13 or jev-latest", req["model"])
		}
		if req["state"] == nil || req["questions"] == nil {
			t.Errorf("upstream body missing state/questions: %v", req)
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"model":   "typesafe/jev-1.13.0",
			"answers": map[string]any{"is_reversible": map[string]any{"type": "noul", "noul": 0.96}},
			"usage":   map[string]any{"input_tokens": 500, "output_tokens": 0},
		})
	}))
	t.Cleanup(up.Close)

	dbPath := t.TempDir() + "/test.db"
	st, err := store.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })

	cfg := &config.Config{
		Listen: ":0",
		Keys:   []*config.Key{{Key: "ar-agent", Name: "pi", Allow: []string{"*"}}},
		Providers: []*config.Provider{
			{Name: "openrouter-classifier", BaseURL: up.URL + "/api/v1", Wire: "classifier",
				Models: []string{"typesafe/jev-1.13"}, Prefix: "or",
				Auth: config.AuthConf{Type: "static", Keys: []string{"sk-up"}}},
			{Name: "typesafe", BaseURL: up.URL + "/v1", Wire: "classifier",
				Models: []string{"jev-latest"}, Prefix: "jev",
				Auth: config.AuthConf{Type: "static", Keys: []string{"sk-up"}}},
		},
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
	return ts, &hits
}

func TestClassifierEndToEnd(t *testing.T) {
	ts, hits := classifierFixture(t)
	body := `{"model":"jev/jev-latest","state":{"command":"bun test"},"questions":{"reversible":{"type":"noul","instructions":"Is this reversible?"}}}`

	for _, path := range []string{"/v1/systemone", "/v1/decisions", "/v1/classifier"} {
		*hits = 0
		req, _ := http.NewRequest("POST", ts.URL+path, strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer ar-agent")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("%s: %v", path, err)
		}
		if resp.StatusCode != 200 {
			t.Fatalf("%s: status %d", path, resp.StatusCode)
		}
		var out map[string]any
		json.NewDecoder(resp.Body).Decode(&out)
		resp.Body.Close()
		if out["answers"] == nil {
			t.Fatalf("%s: passthrough body lost answers: %v", path, out)
		}
		if *hits != 1 {
			t.Fatalf("%s: upstream hits = %d, want 1", path, *hits)
		}
	}

	// openrouter-style natural id resolves on the shared "or" prefix
	req, _ := http.NewRequest("POST", ts.URL+"/v1/systemone",
		strings.NewReader(`{"model":"or/typesafe/jev-1.13","state":"x","questions":{"q":{"type":"noul","instructions":"?"}}}`))
	req.Header.Set("Authorization", "Bearer ar-agent")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("or/typesafe/jev-1.13: status %d", resp.StatusCode)
	}

	// unknown model 404s
	req2, _ := http.NewRequest("POST", ts.URL+"/v1/systemone", strings.NewReader(`{"model":"nope","state":"x","questions":{}}`))
	req2.Header.Set("Authorization", "Bearer ar-agent")
	resp2, _ := http.DefaultClient.Do(req2)
	resp2.Body.Close()
	if resp2.StatusCode != 404 {
		t.Fatalf("unknown model: status %d, want 404", resp2.StatusCode)
	}

	// no key → 401
	resp3, _ := http.Post(ts.URL+"/v1/systemone", "application/json", strings.NewReader(`{"model":"jev/jev-latest"}`))
	resp3.Body.Close()
	if resp3.StatusCode != 401 {
		t.Fatalf("no key: status %d, want 401", resp3.StatusCode)
	}
}

func TestClassifierExcludedFromChatAdvertisement(t *testing.T) {
	ts, _ := classifierFixture(t)
	resp, err := http.Get(ts.URL + "/v1/models")
	if err != nil {
		t.Fatal(err)
	}
	var out struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	json.NewDecoder(resp.Body).Decode(&out)
	resp.Body.Close()
	for _, m := range out.Data {
		if strings.Contains(m.ID, "jev") || strings.Contains(m.ID, "typesafe") {
			t.Fatalf("classifier model %q advertised on /v1/models", m.ID)
		}
	}
}

func TestClassifierCatalogRowsAreCuratedOnly(t *testing.T) {
	ts, _ := classifierFixture(t)
	req, _ := http.NewRequest("GET", ts.URL+"/admin/api/catalog", nil)
	req.SetBasicAuth("", "secretpw")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	var out struct {
		Models []struct {
			ID       string `json:"id"`
			Family   string `json:"family"`
			Manual   bool   `json:"manual"`
			Checksum string `json:"checksum"`
			Props    []struct {
				Name  string `json:"name"`
				Wire  string `json:"wire"`
				Serve bool   `json:"exposed"`
			} `json:"providers"`
		} `json:"models"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	seen := map[string]string{}
	for _, m := range out.Models {
		seen[m.ID] = m.Family
		if m.Family == "classifier" && (m.Manual != true) {
			t.Errorf("classifier meta %q not marked manual", m.ID)
		}
	}
	for _, want := range []string{"typesafe/jev-1.13", "jev-latest"} {
		if seen[want] != "classifier" {
			t.Fatalf("catalog: id %q family = %q, want classifier (all rows: %v)", want, seen[want], seen)
		}
	}
}

func TestPlaygroundRejectsClassifierProvider(t *testing.T) {
	ts, _ := classifierFixture(t)
	body := `{"provider":"typesafe","model":"jev-latest","prompt":"hi"}`
	req, _ := http.NewRequest("POST", ts.URL+"/admin/api/playground", strings.NewReader(body))
	req.SetBasicAuth("", "secretpw")
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 400 {
		t.Fatalf("playground on classifier provider: status %d, want 400", resp.StatusCode)
	}
	b, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(b), "decision models") {
		t.Fatalf("playground error text unclear: %s", b)
	}
}
