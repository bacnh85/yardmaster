package server

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/bacnh85/yardmaster/internal/auth"
	"github.com/bacnh85/yardmaster/internal/config"
	"github.com/bacnh85/yardmaster/internal/provider"
	"github.com/bacnh85/yardmaster/internal/proxy"
	"github.com/bacnh85/yardmaster/internal/store"
)

// Regression: the admin provider form rejected wire "classifier" ("wire must
// be openai, anthropic, or responses") even after config gained the wire —
// the dashboard could not add the typesafe / openrouter-classifier presets.
func TestProviderFormAcceptsClassifierWire(t *testing.T) {
	cfgPath := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(cfgPath, []byte("listen: \":0\"\nadmin_password: secretpw\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{
		Listen: ":0",
		Keys:   []*config.Key{{Key: "ar-agent", Name: "pi", Allow: []string{"*"}}},
	}
	cfg.Defaults()
	st, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	p := proxy.NewProxy(provider.New(cfg), st, cfg.CostFor)
	p.Version = "test"
	srv := New(p, auth.NewKeyStore(cfg.Keys), st, cfgPath, "secretpw", "test")
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()
	admin := func(method, path string, body string) (int, []byte) {
		req, _ := http.NewRequest(method, ts.URL+"/admin/api/"+path, strings.NewReader(body))
		req.SetBasicAuth("", "secretpw")
		req.Header.Set("Content-Type", "application/json")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b := new(bytes.Buffer)
		b.ReadFrom(resp.Body)
		return resp.StatusCode, b.Bytes()
	}

	// classifier wire accepted (curated models present)
	code, b := admin("POST", "providers", `{"name":"typesafe","wire":"classifier","base_url":"https://api.typesafe.ai/v1","models":["jev-latest"],"keys":["sk-ts"],"prefix":"jev"}`)
	if code != 200 {
		t.Fatalf("classifier provider add: %d %s", code, b)
	}
	// and it round-trips to disk with the wire intact
	var out struct {
		Providers []struct {
			Name string `json:"name"`
			Wire string `json:"wire"`
		} `json:"providers"`
	}
	code, b = admin("GET", "providers", "")
	if code != 200 || json.Unmarshal(b, &out) != nil {
		t.Fatalf("providers GET: %d %s", code, b)
	}
	found := false
	for _, pv := range out.Providers {
		if pv.Name == "typesafe" {
			found = true
			if pv.Wire != "classifier" {
				t.Fatalf("typesafe wire = %q, want classifier", pv.Wire)
			}
		}
	}
	if !found {
		t.Fatal("typesafe provider missing after add")
	}
	// curated-list rule still enforced through the form
	if code, _ := admin("POST", "providers", `{"name":"ts2","wire":"classifier","base_url":"https://api.typesafe.ai/v1","keys":["k"]}`); code != 400 {
		t.Fatalf("classifier provider without models accepted: %d", code)
	}
}
