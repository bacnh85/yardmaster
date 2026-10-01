package server

import (
	"encoding/json"
	"net/http/httptest"
	"testing"

	"github.com/bacnh85/yardmaster/internal/auth"
	"github.com/bacnh85/yardmaster/internal/config"
	"github.com/bacnh85/yardmaster/internal/provider"
	"github.com/bacnh85/yardmaster/internal/proxy"
)

// routableHarness: providers + combos + a route alias covering every id class
// the picker must list — chat combos, route aliases, bare/prefixed curated ids,
// and decision (classifier) ids.
func routableHarness(t *testing.T) *httptest.Server {
	t.Helper()
	cfg := &config.Config{
		Listen: ":0",
		Keys:   []*config.Key{{Key: "ar-agent", Name: "pi", Allow: []string{"*"}}},
		Providers: []*config.Provider{
			{Name: "bare", BaseURL: "http://127.0.0.1:1", Wire: "openai",
				Auth: config.AuthConf{Type: "static", Keys: []string{"sk-a"}}, Models: []string{"glm-5.3"}},
			{Name: "pref", BaseURL: "http://127.0.0.1:2", Wire: "openai", Prefix: "cmd",
				Auth: config.AuthConf{Type: "static", Keys: []string{"sk-b"}}, Models: []string{"kimi-k3"}},
			{Name: "clf", BaseURL: "http://127.0.0.1:3", Wire: "classifier", Prefix: "clf",
				Auth: config.AuthConf{Type: "static", Keys: []string{"sk-c"}}, Models: []string{"jev-latest"}},
		},
		Routes: []*config.Route{{Match: "glm-*", Chain: []string{"bare"}}},
		Combos: []*config.Combo{
			{Name: "pool", Type: "chat", Members: []*config.ComboMember{{Provider: "bare", Model: "glm-5.3"}}},
			{Name: "jev", Type: "decision", Members: []*config.ComboMember{{Provider: "clf", Model: "jev-latest"}}},
		},
	}
	cfg.Defaults()
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	return adminServer(t, cfg)
}

// adminServer wires cfg into a test Server over the admin API (no store needed
// for read-only endpoints).
func adminServer(t *testing.T, cfg *config.Config) *httptest.Server {
	t.Helper()
	reg := provider.New(cfg)
	p := proxy.NewProxy(reg, nil, cfg.CostFor)
	srv := New(p, auth.NewKeyStore(cfg.Keys), nil, "", "secretpw", "test")
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	return ts
}

func TestRoutableEndpoint(t *testing.T) {
	ts := routableHarness(t)
	code, body := adminDo(t, ts, "GET", "routable", "")
	if code != 200 {
		t.Fatalf("status %d: %s", code, body)
	}
	var got struct {
		Models []struct {
			ID     string `json:"id"`
			Family string `json:"family"`
		} `json:"models"`
	}
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatal(err)
	}
	byID := map[string]string{}
	for _, m := range got.Models {
		byID[m.ID] = m.Family
	}
	// chat: chat combo + route alias (glm-* → glm) + bare + prefixed curated
	for id, want := range map[string]string{
		"combo/pool":     "chat",
		"glm":            "chat", // route alias, trailing * trimmed
		"glm-5.3":        "chat", // bare provider advertises the bare id
		"cmd/kimi-k3":    "chat", // prefixed provider advertises ONLY prefix/model
		"combo/jev":      "decision",
		"clf/jev-latest": "decision", // classifier provider, prefixed
	} {
		if byID[id] != want {
			t.Errorf("%s: family %q, want %q (all: %v)", id, byID[id], want, byID)
		}
	}
	// the bare form of the prefixed provider's model must NOT be advertised
	if fam, ok := byID["kimi-k3"]; ok {
		t.Errorf("kimi-k3 listed with family %q — prefixed providers advertise prefix/model only", fam)
	}
}
