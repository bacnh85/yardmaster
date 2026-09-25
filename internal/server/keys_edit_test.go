package server

import (
	"encoding/json"
	"fmt"
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

// keysEditHarness: real config file + admin API + a two-key provider, for the
// keys-page data columns and the per-connection edit endpoints.
func keysEditHarness(t *testing.T) (*httptest.Server, *config.Config, *store.Store) {
	t.Helper()
	cfg := &config.Config{
		Listen: ":0",
		Keys: []*config.Key{
			{Key: "ar-agent", Name: "pi", Allow: []string{"*"}}, // legacy: no CreatedAt
		},
		Providers: []*config.Provider{
			{Name: "reg", BaseURL: "http://127.0.0.1:1", Wire: "openai", Preset: "ollama",
				Subscription: "free",
				Auth:         config.AuthConf{Type: "static", Keys: []string{"sk-one", "sk-two"},
					KeyLabels: []string{"OL one", "OL two"}}},
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
	cfgPath := filepath.Join(t.TempDir(), "config.yaml")
	if err := cfg.Save(cfgPath); err != nil {
		t.Fatal(err)
	}
	reg := provider.New(cfg)
	p := proxy.NewProxy(reg, st, cfg.CostFor)
	srv := New(p, auth.NewKeyStore(cfg.Keys), st, cfgPath, "secretpw", "test")
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	return ts, cfg, st
}

func adminDo(t *testing.T, ts *httptest.Server, method, path, body string) (int, []byte) {
	t.Helper()
	req, _ := http.NewRequest(method, ts.URL+"/admin/api/"+path, strings.NewReader(body))
	req.SetBasicAuth("", "secretpw")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, b
}

// inboundDo hits an auth-wrapped endpoint with a bearer key.
func inboundDo(t *testing.T, ts *httptest.Server, key string) int {
	t.Helper()
	req, _ := http.NewRequest("GET", ts.URL+"/v1/models", nil)
	req.Header.Set("Authorization", "Bearer "+key)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	return resp.StatusCode
}

// keysPageData: GET /admin/api/keys carries id (derived 32-hex), created_at
// (unix MS — assert > 1e12 to catch a seconds/ms regression) and last_used.
func TestKeysPageDataColumns(t *testing.T) {
	ts, _, _ := keysEditHarness(t)

	type keyRow struct {
		Name      string `json:"name"`
		ID        string `json:"id"`
		CreatedAt int64  `json:"created_at"`
		LastUsed  int64  `json:"last_used"`
		KeySuffix string `json:"key_suffix"`
	}
	list := func() map[string]keyRow {
		code, b := adminDo(t, ts, "GET", "keys", "")
		if code != 200 {
			t.Fatalf("keys GET: %d %s", code, b)
		}
		var out struct {
			Keys []keyRow `json:"keys"`
		}
		if err := json.Unmarshal(b, &out); err != nil {
			t.Fatalf("decode: %v\n%s", err, b)
		}
		m := map[string]keyRow{}
		for _, k := range out.Keys {
			m[k.Name] = k
		}
		return m
	}

	// create a fresh key → created_at is stamped (ms); legacy key stays 0
	if code, b := adminDo(t, ts, "POST", "keys", `{"name":"fresh"}`); code != 200 {
		t.Fatalf("create: %d %s", code, b)
	}
	m := list()
	fresh := m["fresh"]
	if len(fresh.ID) != 32 {
		t.Errorf("id %q, want 32 hex chars", fresh.ID)
	}
	if fresh.CreatedAt < 1e12 {
		t.Errorf("created_at %d, want unix ms (>1e12) — seconds/ms unit regression", fresh.CreatedAt)
	}
	if fresh.LastUsed != 0 {
		t.Errorf("last_used %d, want 0 (never used)", fresh.LastUsed)
	}
	// legacy key: id still derived (no schema migration needed), created_at 0
	pi := m["pi"]
	if len(pi.ID) != 32 || pi.ID == fresh.ID {
		t.Errorf("legacy id %q (fresh %q)", pi.ID, fresh.ID)
	}
	if pi.CreatedAt != 0 {
		t.Errorf("legacy created_at %d, want 0", pi.CreatedAt)
	}
	// id is stable across a rename (derived from the key, not the name)
	if code, b := adminDo(t, ts, "PUT", "keys/pi", `{"name":"pi-renamed"}`); code != 200 {
		t.Fatalf("rename: %d %s", code, b)
	}
	m = list()
	if m["pi-renamed"].ID != pi.ID {
		t.Errorf("rename changed id: %s → %s", pi.ID, m["pi-renamed"].ID)
	}
}

// rotation replaces the credential: id follows the new secret AND created_at
// is re-stamped (the rotated key's audit clock starts at the rotation, not the
// original issue date).
func TestKeyRotationRestampsCreatedAt(t *testing.T) {
	ts, _, _ := keysEditHarness(t)

	type keyRow struct {
		Name      string `json:"name"`
		ID        string `json:"id"`
		CreatedAt int64  `json:"created_at"`
	}
	before := time.Now().Add(-time.Second).UnixMilli()
	if code, b := adminDo(t, ts, "POST", "keys", `{"name":"rot"}`); code != 200 {
		t.Fatalf("create: %d %s", code, b)
	}
	get := func() keyRow {
		code, b := adminDo(t, ts, "GET", "keys", "")
		if code != 200 {
			t.Fatalf("keys GET: %d %s", code, b)
		}
		var out struct {
			Keys []keyRow `json:"keys"`
		}
		if err := json.Unmarshal(b, &out); err != nil {
			t.Fatalf("decode: %v", err)
		}
		for _, k := range out.Keys {
			if k.Name == "rot" {
				return k
			}
		}
		t.Fatal("rot key not found in keys GET")
		return keyRow{}
	}
	orig := get()
	time.Sleep(2 * time.Millisecond) // rotation re-stamps at ms granularity — same-ms rotate would trip the re-stamp assert
	if code, b := adminDo(t, ts, "PUT", "keys/rot", `{"key":"ar-rotated-secret"}`); code != 200 {
		t.Fatalf("rotate: %d %s", code, b)
	}
	after := get()
	if after.ID == orig.ID {
		t.Errorf("rotation kept id %s — id must follow the secret", after.ID)
	}
	if after.CreatedAt <= orig.CreatedAt || after.CreatedAt < before {
		t.Errorf("created_at %d after rotation (orig %d, floor %d) — must be re-stamped", after.CreatedAt, orig.CreatedAt, before)
	}
}

// key rotation via PUT /admin/api/keys/{name}: old key stops authenticating,
// new key works, and the derived id follows the secret.
func TestKeyRotation(t *testing.T) {
	ts, _, _ := keysEditHarness(t)

	code, b := adminDo(t, ts, "PUT", "keys/pi", `{"key":"ar-rotated"}`)
	if code != 200 {
		t.Fatalf("rotate: %d %s", code, b)
	}
	if got := inboundDo(t, ts, "ar-rotated"); got != 200 {
		t.Errorf("new key auth: %d, want 200", got)
	}
	// cfgs are hot-swapped by mutate(); the old secret must 401
	if got := inboundDo(t, ts, "ar-agent"); got != 401 {
		t.Errorf("old key auth: %d, want 401", got)
	}
	_, b = adminDo(t, ts, "GET", "keys", "")
	if !strings.Contains(string(b), `"name":"pi"`) {
		t.Fatalf("pi missing after rotate: %s", b)
	}
	// empty string keeps the stored secret (nil-pointer-keeps contract)
	if code, _ := adminDo(t, ts, "PUT", "keys/pi", `{"key":""}`); code != 200 {
		t.Error("empty key should be a no-op, not an error")
	}
	if got := inboundDo(t, ts, "ar-rotated"); got != 200 {
		t.Errorf("empty key wiped the secret: auth %d", got)
	}
}

// putKeysEdit: per-connection label + key edit, index-addressed with label
// alignment and duplicate rejection.
func TestProviderConnectionEdit(t *testing.T) {
	ts, _, _ := keysEditHarness(t)

	conns := func() []map[string]any {
		code, b := adminDo(t, ts, "GET", "providers", "")
		if code != 200 {
			t.Fatalf("providers GET: %d %s", code, b)
		}
		var out struct {
			Providers []struct {
				Name        string           `json:"name"`
				Connections []map[string]any `json:"connections"`
			} `json:"providers"`
		}
		if err := json.Unmarshal(b, &out); err != nil {
			t.Fatalf("decode: %v\n%s", err, b)
		}
		for _, p := range out.Providers {
			if p.Name == "reg" {
				return p.Connections
			}
		}
		t.Fatal("provider reg missing")
		return nil
	}

	// label-only edit: keys untouched, label replaced in place
	if code, b := adminDo(t, ts, "PUT", "providers/reg/keys/0", `{"label":"OL renamed"}`); code != 200 {
		t.Fatalf("label edit: %d %s", code, b)
	}
	c := conns()
	if len(c) != 2 || c[0]["label"] != "OL renamed" || c[1]["label"] != "OL two" {
		t.Fatalf("labels after edit: %+v", c)
	}
	if c[0]["suffix"] != "…sk-one" && c[0]["suffix"] != "sk-one" {
		t.Logf("suffix formatting: %v", c[0]["suffix"]) // informational; format owned by suffix()
	}
	// key + label edit on idx 1
	if code, b := adminDo(t, ts, "PUT", "providers/reg/keys/1", `{"key":"sk-new","label":"OL new"}`); code != 200 {
		t.Fatalf("key edit: %d %s", code, b)
	}
	c = conns()
	if len(c) != 2 || c[1]["label"] != "OL new" || !strings.HasSuffix(fmt.Sprint(c[1]["suffix"]), "sk-new") {
		t.Fatalf("after key edit: %+v", c)
	}
	// duplicate key rejected
	if code, b := adminDo(t, ts, "PUT", "providers/reg/keys/0", `{"key":"sk-new"}`); code != 400 || !strings.Contains(string(b), "already present") {
		t.Errorf("duplicate: %d %s", code, b)
	}
	// self-replacement of the same key is fine (no false duplicate)
	if code, b := adminDo(t, ts, "PUT", "providers/reg/keys/1", `{"key":"sk-new"}`); code != 200 {
		t.Errorf("self-replace: %d %s", code, b)
	}
	// out of range
	if code, b := adminDo(t, ts, "PUT", "providers/reg/keys/9", `{"label":"x"}`); code != 400 || !strings.Contains(string(b), "out of range") {
		t.Errorf("oor: %d %s", code, b)
	}
	// unknown provider
	if code, _ := adminDo(t, ts, "PUT", "providers/nope/keys/0", `{"label":"x"}`); code != 400 {
		t.Error("unknown provider should 400")
	}
	// clearing a label is legal (reviewer finding: the UI used to block it)
	if code, b := adminDo(t, ts, "PUT", "providers/reg/keys/0", `{"label":""}`); code != 200 {
		t.Fatalf("clear label: %d %s", code, b)
	}
	if c := conns(); c[0]["label"] != "Key 1" {
		t.Errorf("cleared label should fall back to \"Key 1\": %+v", c)
	}
	// unlabeled layout: PUT on a HIGH index pads KeyLabels so alignment holds
	// (the padding branch in the PUT handler: len(KeyLabels) < idx+1)
	if code, b := adminDo(t, ts, "PUT", "providers/reg/keys/1", `{"label":"only-second"}`); code != 200 {
		t.Fatalf("pad edit: %d %s", code, b)
	}
	c = conns()
	if len(c) != 2 || c[0]["label"] != "Key 1" || c[1]["label"] != "only-second" {
		t.Fatalf("padding misaligned labels: %+v", c)
	}
	// a follow-up edit of position 0 must not disturb position 1
	if code, b := adminDo(t, ts, "PUT", "providers/reg/keys/0", `{"label":"first"}`); code != 200 {
		t.Fatalf("follow-up edit: %d %s", code, b)
	}
	if c := conns(); c[0]["label"] != "first" || c[1]["label"] != "only-second" {
		t.Fatalf("labels after follow-up: %+v", c)
	}
}

// subscription endpoint: set, validate, clear — per provider entry.
func TestProviderSubscriptionEdit(t *testing.T) {
	ts, _, _ := keysEditHarness(t)

	subOf := func() string {
		_, b := adminDo(t, ts, "GET", "providers", "")
		var out struct {
			Providers []struct {
				Name         string `json:"name"`
				Subscription string `json:"subscription"`
			} `json:"providers"`
		}
		json.Unmarshal(b, &out)
		for _, p := range out.Providers {
			if p.Name == "reg" {
				return p.Subscription
			}
		}
		t.Fatal("provider reg missing")
		return ""
	}

	if subOf() != "free" {
		t.Fatalf("initial subscription %q", subOf())
	}
	if code, b := adminDo(t, ts, "PUT", "providers/reg/subscription", `{"plan":"max"}`); code != 200 {
		t.Fatalf("set plan: %d %s", code, b)
	}
	if subOf() != "max" {
		t.Errorf("plan after set: %q", subOf())
	}
	if code, b := adminDo(t, ts, "PUT", "providers/reg/subscription", `{"plan":"bogus"}`); code != 400 || !strings.Contains(string(b), "plan must be") {
		t.Errorf("invalid plan: %d %s", code, b)
	}
	if code, _ := adminDo(t, ts, "PUT", "providers/reg/subscription", `{"plan":""}`); code != 200 {
		t.Error("clearing should succeed")
	}
	if subOf() != "" {
		t.Errorf("plan after clear: %q", subOf())
	}
	if code, _ := adminDo(t, ts, "PUT", "providers/nope/subscription", `{"plan":"max"}`); code != 400 {
		t.Error("unknown provider should 400")
	}
}

// rename via PUT re-attributes request history so "last used" and the Usage
// breakdown follow the key instead of splitting into two buckets.
func TestKeyRenameReattributesHistory(t *testing.T) {
	ts, _, st := keysEditHarness(t)

	// record a request under the current key name; Submit is batched (500ms
	// flush tick), so wait until the row is queryable before renaming
	st.Submit(&store.Record{Ts: 1700000000000, Key: "pi", Model: "m", Provider: "p", Status: 200})
	deadline := time.Now().Add(3 * time.Second)
	for {
		last, err := st.KeyLastUsed()
		if err != nil {
			t.Fatal(err)
		}
		if last["pi"] == 1700000000000 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("record never flushed: %v", last)
		}
		time.Sleep(25 * time.Millisecond)
	}
	if code, b := adminDo(t, ts, "PUT", "keys/pi", `{"name":"pi-2"}`); code != 200 {
		t.Fatalf("rename: %d %s", code, b)
	}
	// KeyLastUsed goes through the store; the handler is best-effort but the
	// UPDATE is synchronous, so the re-attribution is visible immediately.
	last, err := st.KeyLastUsed()
	if err != nil {
		t.Fatal(err)
	}
	if last["pi-2"] != 1700000000000 {
		t.Errorf("history not re-attributed: %v", last)
	}
	if _, stale := last["pi"]; stale {
		t.Errorf("old key name still in history: %v", last)
	}
}

// Padding branch: a provider with keys but NO KeyLabels slice at all — a PUT
// on a high index must materialize placeholders so the label lands at idx and
// earlier positions keep their "Key N" fallback (reviewer finding: this branch
// had zero coverage because the harness always configured two labels).
func TestProviderConnectionEditPadsMissingLabels(t *testing.T) {
	cfg := &config.Config{
		Listen: ":0",
		Keys:   []*config.Key{{Key: "ar-agent", Name: "pi", Allow: []string{"*"}}},
		Providers: []*config.Provider{
			{Name: "nolel", BaseURL: "http://127.0.0.1:1", Wire: "openai",
				Auth: config.AuthConf{Type: "static", Keys: []string{"sk-a", "sk-b"}}}, // KeyLabels nil
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
	cfgPath := filepath.Join(t.TempDir(), "config.yaml")
	if err := cfg.Save(cfgPath); err != nil {
		t.Fatal(err)
	}
	reg := provider.New(cfg)
	p := proxy.NewProxy(reg, st, cfg.CostFor)
	srv := New(p, auth.NewKeyStore(cfg.Keys), st, cfgPath, "secretpw", "test")
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)

	if code, b := adminDo(t, ts, "PUT", "providers/nolel/keys/1", `{"label":"L1"}`); code != 200 {
		t.Fatalf("pad edit: %d %s", code, b)
	}
	code, b := adminDo(t, ts, "GET", "providers", "")
	if code != 200 {
		t.Fatalf("providers GET: %d %s", code, b)
	}
	var out struct {
		Providers []struct {
			Name        string           `json:"name"`
			Connections []map[string]any `json:"connections"`
		} `json:"providers"`
	}
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatal(err)
	}
	if len(out.Providers) != 1 || len(out.Providers[0].Connections) != 2 {
		t.Fatalf("connections: %+v", out.Providers)
	}
	// idx 0 keeps the "Key 1" fallback (placeholder ""), idx 1 gets the label
	if got := out.Providers[0].Connections[0]["label"]; got != "Key 1" {
		t.Errorf("idx 0 label %v, want Key 1 (padded placeholder)", got)
	}
	if got := out.Providers[0].Connections[1]["label"]; got != "L1" {
		t.Errorf("idx 1 label %v, want L1", got)
	}
	// and a later edit of idx 0 must not shift idx 1
	if code, b := adminDo(t, ts, "PUT", "providers/nolel/keys/0", `{"label":"L0"}`); code != 200 {
		t.Fatalf("idx 0 edit: %d %s", code, b)
	}
	_, b = adminDo(t, ts, "GET", "providers", "")
	json.Unmarshal(b, &out)
	if got := out.Providers[0].Connections[1]["label"]; got != "L1" {
		t.Errorf("idx 1 label after idx 0 edit: %v, want L1", got)
	}
}

// Per-connection enable/disable: PUT keys/<idx> {"disabled":bool} flags the
// key, GET providers reports it, routing (Registry.Resolve) skips the key, a
// label/key round-trip preserves the flag, and a full provider PUT with
// keyDisabled omitted keeps it.
func TestProviderConnectionDisable(t *testing.T) {
	ts, cfg, _ := keysEditHarness(t)

	connsOf := func(name string) []map[string]any {
		code, b := adminDo(t, ts, "GET", "providers", "")
		if code != 200 {
			t.Fatalf("providers GET: %d %s", code, b)
		}
		var out struct {
			Providers []struct {
				Name        string           `json:"name"`
				Connections []map[string]any `json:"connections"`
			} `json:"providers"`
		}
		json.Unmarshal(b, &out)
		for _, p := range out.Providers {
			if p.Name == name {
				return p.Connections
			}
		}
		t.Fatalf("provider %s missing", name)
		return nil
	}

	// disable key idx 0
	if code, b := adminDo(t, ts, "PUT", "providers/reg/keys/0", `{"disabled":true}`); code != 200 {
		t.Fatalf("disable: %d %s", code, b)
	}
	c := connsOf("reg")
	if c[0]["disabled"] != true || c[1]["disabled"] != false {
		t.Fatalf("flags after disable: %+v", c)
	}

	// routing must skip the disabled key: Resolve yields only sk-two's target.
	// (mutate() hot-swaps a fresh config into the server's own registry, so the
	// harness cfg pointer is stale — flag it directly to exercise Registry.)
	cfg.Providers[0].Auth.KeyDisabled = []bool{true, false}
	reg := provider.New(cfg)
	targets := reg.Resolve("m", []string{"*"})
	if len(targets) != 1 || targets[0].APIKey != "sk-two" {
		t.Fatalf("routing after disable: %+v, want only sk-two", targets)
	}

	// a label edit (key/label round-trip) must preserve the disabled flag
	if code, b := adminDo(t, ts, "PUT", "providers/reg/keys/0", `{"label":"OL one"}`); code != 200 {
		t.Fatalf("label edit: %d %s", code, b)
	}
	c = connsOf("reg")
	if c[0]["disabled"] != true {
		t.Fatalf("flag lost on label edit: %+v", c)
	}

	// full provider PUT (model toggles etc. — keyDisabled omitted) keeps flags
	if code, b := adminDo(t, ts, "PUT", "providers/reg", `{"name":"reg","wire":"openai","base_url":"http://127.0.0.1:1","models":["m"],"preset":"ollama","subscription":"free"}`); code != 200 {
		t.Fatalf("provider put: %d %s", code, b)
	}
	if c := connsOf("reg"); c[0]["disabled"] != true || c[1]["disabled"] != false {
		t.Fatalf("flags lost on provider PUT: %+v", c)
	}

	// re-enable
	if code, b := adminDo(t, ts, "PUT", "providers/reg/keys/0", `{"disabled":false}`); code != 200 {
		t.Fatalf("enable: %d %s", code, b)
	}
	cfg.Providers[0].Auth.KeyDisabled = []bool{false, false}
	targets = provider.New(cfg).Resolve("m", []string{"*"})
	if len(targets) != 2 {
		t.Fatalf("routing after re-enable: %d targets, want 2", len(targets))
	}
	if c := connsOf("reg"); c[0]["disabled"] != false {
		t.Fatalf("flag after re-enable: %+v", c)
	}
}
