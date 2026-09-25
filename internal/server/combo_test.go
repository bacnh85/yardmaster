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

// Combos admin CRUD + provider-delete scrub: PUT replaces the list (mutate→
// Validate→save→reload), GET reads it back, deleting a member provider trims
// members (combo dropped when the pool empties), and usage aggregates rows.
func TestCombosAPI(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(503) // config surgery + analytics only; no dispatch
	}))
	defer up.Close()

	cfgPath := filepath.Join(t.TempDir(), "config.yaml")
	cfg := &config.Config{
		Listen: ":0",
		Keys:   []*config.Key{{Key: "ar-agent", Name: "pi", Allow: []string{"*"}}},
		Providers: []*config.Provider{
			{Name: "a", BaseURL: up.URL, Wire: "openai", Auth: config.AuthConf{Keys: []string{"k1"}, KeyLabels: []string{"main"}}},
			{Name: "b", BaseURL: up.URL, Wire: "openai", Auth: config.AuthConf{Keys: []string{"k2"}}},
		},
		Combos: []*config.Combo{{
			Name: "m", Model: "m",
			Members: []*config.ComboMember{{Provider: "a"}, {Provider: "b"}},
		}},
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
	admin := func(method, path, body string) (int, []byte) {
		var rd io.Reader
		if body != "" {
			rd = io.NopCloser(jsonReader(body))
		}
		req, _ := http.NewRequest(method, ts.URL+"/admin/api/"+path, rd)
		req.SetBasicAuth("", "pw")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		rb, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, rb
	}

	// GET returns the config combo
	code, body := admin("GET", "combos", "")
	if code != 200 {
		t.Fatalf("GET combos: %d %s", code, body)
	}
	var list struct {
		Combos []*config.Combo `json:"combos"`
	}
	if err := json.Unmarshal(body, &list); err != nil || len(list.Combos) != 1 {
		t.Fatalf("GET combos: %s", body)
	}

	// PUT replaces: invalid combo (unknown provider) → 400, config untouched
	code, body = admin("PUT", "combos", `[{"name":"x","model":"m","members":[{"provider":"zzz"}]}]`)
	if code != 400 {
		t.Fatalf("PUT invalid combo: %d %s", code, body)
	}
	// PUT valid: two combos
	code, body = admin("PUT", "combos", `[{"name":"x","model":"m","members":[{"provider":"a","keys":["main"]},{"provider":"b"}]},{"name":"y","model":"m2","members":[{"provider":"b"}]}]`)
	if code != 200 {
		t.Fatalf("PUT combos: %d %s", code, body)
	}
	_, body = admin("GET", "combos", "")
	if err := json.Unmarshal(body, &list); err != nil || len(list.Combos) != 2 || list.Combos[0].Members[0].Keys[0] != "main" {
		t.Fatalf("PUT round-trip: %s", body)
	}

	// usage endpoint aggregates (empty table → empty list, still 200)
	code, body = admin("GET", "combos/usage?hours=24", "")
	if code != 200 {
		t.Fatalf("GET combos/usage: %d %s", code, body)
	}

	// delete provider a → member scrubbed; combo y (b only) survives, x keeps b
	if code, body := admin("DELETE", "providers/a", ""); code != 200 {
		t.Fatalf("delete provider a: %d %s", code, body)
	}
	_, body = admin("GET", "combos", "")
	if err := json.Unmarshal(body, &list); err != nil {
		t.Fatal(err)
	}
	if len(list.Combos) != 2 {
		t.Fatalf("want both combos alive, got %d: %s", len(list.Combos), body)
	}
	if names := list.Combos[0].Members; len(names) != 1 || names[0].Provider != "b" {
		t.Fatalf("combo x member not scrubbed: %s", body)
	}
}

func jsonReader(s string) io.Reader { return io.NopCloser(newStrReader(s)) }

type strReader struct{ s string }

func (r *strReader) Read(p []byte) (int, error) {
	if len(r.s) == 0 {
		return 0, io.EOF
	}
	n := copy(p, r.s)
	r.s = r.s[n:]
	return n, nil
}
func newStrReader(s string) *strReader { return &strReader{s: s} }
