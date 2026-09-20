package server

import (
	"bytes"
	"encoding/json"
	"io"
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

	"gopkg.in/yaml.v3"
)

// Review round: key labels stay index-aligned across delete, and PUT preserves
// disabled/keyLabels when the client omits them.
func TestKeyLabelsAndDisabledPreservation(t *testing.T) {
	cfgPath := filepath.Join(t.TempDir(), "config.yaml")
	cfg := &config.Config{
		Listen: ":0",
		Keys:   []*config.Key{{Key: "ar-agent", Name: "pi", Allow: []string{"*"}}},
		Providers: []*config.Provider{
			{Name: "reg", BaseURL: "http://127.0.0.1:1", Wire: "openai", Preset: "opencode-go",
				Disabled: true,
				Auth: config.AuthConf{Type: "static", Keys: []string{"sk-one", "sk-two"},
					KeyLabels: []string{"", "backup"}}},
		},
	}
	b, _ := yaml.Marshal(cfg)
	if err := os.WriteFile(cfgPath, b, 0o600); err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	cfg.Defaults()
	cfg.Validate()
	p := proxy.NewProxy(provider.New(cfg), st, nil)
	p.Version = "test"
	srv := New(p, auth.NewKeyStore(cfg.Keys), st, cfgPath, "secretpw", "test")
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()
	admin := func(method, path string, body io.Reader) (int, []byte) {
		req, _ := http.NewRequest(method, ts.URL+"/admin/api/"+path, body)
		req.SetBasicAuth("", "secretpw")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, b
	}
	conns := func() []map[string]any {
		code, b := admin("GET", "providers", nil)
		if code != 200 {
			t.Fatalf("providers GET: %d", code)
		}
		var out struct {
			Providers []struct {
				Name        string           `json:"name"`
				Disabled    bool             `json:"disabled"`
				Connections []map[string]any `json:"connections"`
			} `json:"providers"`
		}
		json.Unmarshal(b, &out)
		for _, pr := range out.Providers {
			if pr.Name == "reg" {
				return pr.Connections
			}
		}
		t.Fatal("provider reg missing")
		return nil
	}

	// reviewer acceptance: POST label a, POST label b, DELETE keys/0 → label[0] == "b"
	if code, body := admin("POST", "providers/reg/keys", strings.NewReader(`{"key":"sk-a","label":"a"}`)); code != 200 {
		t.Fatalf("add a: %d %s", code, body)
	}
	if code, body := admin("POST", "providers/reg/keys", strings.NewReader(`{"key":"sk-b","label":"b"}`)); code != 200 {
		t.Fatalf("add b: %d %s", code, body)
	}
	// reviewer acceptance: labels stay index-aligned through deletes.
	// keys: [sk-one, sk-two, sk-a, sk-b] labels: ["", backup, a, b]
	if code, body := admin("DELETE", "providers/reg/keys/2", nil); code != 200 { // delete sk-a
		t.Fatalf("delete idx 2: %d %s", code, body)
	}
	c := conns()
	if len(c) != 3 || c[0]["label"] != "Key 1" || c[1]["label"] != "backup" || c[2]["label"] != "b" {
		t.Fatalf("labels misaligned after delete: %+v", c)
	}

	// deleting idx 0 shifts labels up: connections[0].label == "backup"
	if code, _ := admin("DELETE", "providers/reg/keys/0", nil); code != 200 {
		t.Fatal("delete idx 0")
	}
	c = conns()
	if len(c) != 2 || c[0]["label"] != "backup" || c[1]["label"] != "b" {
		t.Fatalf("labels after delete idx 0: %+v", c)
	}

	// PUT omitting disabled/keyLabels preserves them (high/low review findings)
	if code, body := admin("PUT", "providers/reg", strings.NewReader(
		`{"wire":"openai","base_url":"http://127.0.0.1:1","models":["m"]}`)); code != 200 {
		t.Fatalf("put: %d %s", code, body)
	}
	code, b := admin("GET", "providers", nil)
	if code != 200 || !bytes.Contains(b, []byte(`"disabled":true`)) {
		t.Fatalf("PUT without disabled re-enabled the provider: %s", b)
	}
	if !bytes.Contains(b, []byte(`"label":"backup"`)) || !bytes.Contains(b, []byte(`"label":"b"`)) {
		t.Fatalf("PUT without keyLabels wiped labels: %s", b)
	}

	// PUT with explicit disabled:false re-enables
	if code, body := admin("PUT", "providers/reg", strings.NewReader(
		`{"wire":"openai","base_url":"http://127.0.0.1:1","models":["m"],"disabled":false}`)); code != 200 {
		t.Fatalf("put enable: %d %s", code, body)
	}
	if code, b := admin("GET", "providers", nil); code != 200 || bytes.Contains(b, []byte(`"disabled":true`)) {
		t.Fatalf("explicit disabled:false not applied: %s", b)
	}
}
