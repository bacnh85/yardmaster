package server

// GET /v1/usage tests: JSON projection of the shared quota report (windows +
// credits), slug mapping, aggregate fallback, and the per-key usage permission
// (default ON, revocable via PUT /admin/api/keys/{name}).

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/bacnh85/yardmaster/internal/auth"
	"github.com/bacnh85/yardmaster/internal/config"
	"github.com/bacnh85/yardmaster/internal/provider"
	"github.com/bacnh85/yardmaster/internal/proxy"
	"github.com/bacnh85/yardmaster/internal/store"
)

func TestUsageSlugMap(t *testing.T) {
	cases := map[string]string{
		"zai": "zai", "command-code": "commandcode", "cmd": "commandcode",
		"commandcode": "commandcode", "ds": "deepseek", "deepseek": "deepseek",
		"ocg": "opencode", "opencode-go": "opencode", "opencode": "opencode",
		"glm-5.3-flash": "", "": "",
	}
	for in, want := range cases {
		if got := sourceForSlug(in); got != want {
			t.Errorf("sourceForSlug(%q) = %q, want %q", in, got, want)
		}
	}
	if slugFor("commandcode") != "command-code" || slugFor("zai") != "zai" || slugFor("deepseek") != "deepseek" {
		t.Error("slugFor mismatch")
	}
}

func TestUsageWindowMath(t *testing.T) {
	// pct windows: Used IS the percentage
	pct := &QuotaWindow{Used: 47, Cap: 100, Unit: "pct"}
	if got := windowUsedPct(pct); got != 47 {
		t.Errorf("pct used %d, want 47", got)
	}
	// USD windows: used/cap
	usd := &QuotaWindow{Used: 7, Cap: 35}
	if got := windowUsedPct(usd); got != 20 {
		t.Errorf("usd used %d, want 20", got)
	}
	// clamped
	if got := windowUsedPct(&QuotaWindow{Used: 5, Cap: 4}); got != 100 {
		t.Errorf("over cap %d, want 100", got)
	}
	w := toUsageWindow(&QuotaWindow{Used: 0.5, Cap: 14, ResetAt: 1789918840825})
	if w.RemainingPct != 96 || w.ResetAt != 1789918840825 { // 100*0.5/14 = 3.57 → 4 used
		t.Errorf("toUsageWindow %+v", w)
	}
}

func TestUsageSourceProjection(t *testing.T) {
	prov := []string{"zai"}
	pq := &ProviderQuota{Source: "zai", Providers: []string{"zai"}, Accounts: []QuotaAccount{
		{Label: "A", FiveHour: &QuotaWindow{Used: 10, Cap: 100, Unit: "pct"}, Weekly: &QuotaWindow{Used: 90, Cap: 100, Unit: "pct"}},
		{Label: "B", FiveHour: &QuotaWindow{Used: 50, Cap: 100, Unit: "pct"}, MonthlyCredits: ptrF(3.5)},
	}}
	out := sourceUsage(pq, prov)
	if out.Provider != "zai" {
		t.Errorf("provider %q", out.Provider)
	}
	// worst (highest used) account wins per window, independently
	if w := out.Windows["session"]; w == nil || w.RemainingPct != 50 {
		t.Errorf("session %+v, want 50 remaining", out.Windows["session"])
	}
	if w := out.Windows["weekly"]; w == nil || w.RemainingPct != 10 {
		t.Errorf("weekly %+v, want 10 remaining", out.Windows["weekly"])
	}
	if out.Credits == nil || out.Credits.Currency != "USD" || out.Credits.Balance != 3.5 {
		t.Errorf("credits %+v", out.Credits)
	}

	// no windows, CNY balance (deepseek-style)
	ds := &ProviderQuota{Source: "deepseek", Accounts: []QuotaAccount{{MonthlyCredits: ptrF(88), Currency: "CNY"}}}
	out = sourceUsage(ds, prov)
	if out.Windows != nil || out.Credits == nil || out.Credits.Currency != "CNY" || out.Credits.Balance != 88 {
		t.Errorf("deepseek projection: windows=%v credits=%+v", out.Windows, out.Credits)
	}

	// opencode: all three pct windows project (live API reports monthly)
	oc := &ProviderQuota{Source: "opencode", Accounts: []QuotaAccount{
		{FiveHour: &QuotaWindow{Used: 1, Cap: 100, Unit: "pct"}, Weekly: &QuotaWindow{Used: 100, Cap: 100, Unit: "pct"}, Monthly: &QuotaWindow{Used: 80, Cap: 100, Unit: "pct"}},
	}}
	out = sourceUsage(oc, prov)
	if w := out.Windows["monthly"]; w == nil || w.RemainingPct != 20 {
		t.Errorf("monthly %+v, want 20 remaining", out.Windows["monthly"])
	}
	// commandcode carries no explicit Monthly — no monthly window may appear
	cc := &ProviderQuota{Source: "commandcode", Accounts: []QuotaAccount{
		{FiveHour: &QuotaWindow{Used: 5, Cap: 100, Unit: "pct"}, MonthlyCredits: ptrF(69.99)},
	}}
	out = sourceUsage(cc, prov)
	if out.Windows["monthly"] != nil {
		t.Errorf("commandcode monthly %+v, want none", out.Windows["monthly"])
	}

	// monthly-only account (oc API omitted rolling/weekly → ocPctWindow nil):
	// monthly must still project, never be dropped for lack of session/weekly
	only := &ProviderQuota{Source: "opencode", Accounts: []QuotaAccount{
		{Monthly: &QuotaWindow{Used: 80, Cap: 100, Unit: "pct"}},
	}}
	out = sourceUsage(only, prov)
	if w := out.Windows["monthly"]; w == nil || w.RemainingPct != 20 {
		t.Errorf("monthly-only %+v, want 20 remaining", out.Windows["monthly"])
	}
	if out.Windows["session"] != nil || out.Windows["weekly"] != nil {
		t.Errorf("monthly-only fabricated windows: %+v", out.Windows)
	}
	// and the aggregate must surface it too
	agg := aggregateUsage([]ProviderQuota{*only}, prov)
	if w := agg.Windows["monthly"]; w == nil || w.RemainingPct != 20 {
		t.Errorf("aggregate monthly-only %+v, want 20", agg.Windows["monthly"])
	}
}

func TestUsageAggregateWorstCase(t *testing.T) {
	prov := []string{"zai", "command-code"}
	rep := []ProviderQuota{
		{Source: "zai", Accounts: []QuotaAccount{{FiveHour: &QuotaWindow{Used: 5, Cap: 100, Unit: "pct"}, Weekly: &QuotaWindow{Used: 90, Cap: 100, Unit: "pct"}}}},
		{Source: "commandcode", Accounts: []QuotaAccount{{FiveHour: &QuotaWindow{Used: 50, Cap: 100, Unit: "pct"}, MonthlyCredits: ptrF(69.99)}}},
	}
	out := aggregateUsage(rep, prov)
	// session: min(95, 50) = 50; weekly: only zai has one → 10
	if w := out.Windows["session"]; w == nil || w.RemainingPct != 50 {
		t.Errorf("aggregate session %+v, want 50", out.Windows["session"])
	}
	if w := out.Windows["weekly"]; w == nil || w.RemainingPct != 10 {
		t.Errorf("aggregate weekly %+v, want 10", out.Windows["weekly"])
	}
	// monthly: only opencode carries one → worst = its own
	rep[0].Accounts[0].Monthly = &QuotaWindow{Used: 20, Cap: 100, Unit: "pct"}
	out = aggregateUsage(rep, prov)
	if w := out.Windows["monthly"]; w == nil || w.RemainingPct != 80 {
		t.Errorf("aggregate monthly %+v, want 80", out.Windows["monthly"])
	}
	if out.Credits == nil || out.Credits.Balance != 69.99 {
		t.Errorf("aggregate credits %+v", out.Credits)
	}
	if out.Provider != "" {
		t.Errorf("aggregate provider %q, want empty", out.Provider)
	}
}

// Reviewer case: the aggregate must not silently pick an arbitrary currency —
// credits follow the worst-session source, not config order.
func TestUsageAggregateCreditsDeterministic(t *testing.T) {
	// deepseek (CNY 88, no windows) listed FIRST, commandcode (USD, windows) second
	rep := []ProviderQuota{
		{Source: "deepseek", Accounts: []QuotaAccount{{MonthlyCredits: ptrF(88), Currency: "CNY"}}},
		{Source: "commandcode", Accounts: []QuotaAccount{{FiveHour: &QuotaWindow{Used: 50, Cap: 100, Unit: "pct"}, MonthlyCredits: ptrF(69.99)}}},
	}
	out := aggregateUsage(rep, []string{"deepseek", "command-code"})
	if out.Credits == nil || out.Credits.Currency != "USD" || out.Credits.Balance != 69.99 {
		t.Errorf("credits %+v, want USD 69.99 (worst-session source, not first-found)", out.Credits)
	}

	// reversed config order must not change the pick
	rep[0], rep[1] = rep[1], rep[0]
	out = aggregateUsage(rep, []string{"deepseek", "command-code"})
	if out.Credits == nil || out.Credits.Currency != "USD" {
		t.Errorf("reversed: credits %+v, want USD 69.99", out.Credits)
	}

	// exactly one source with a balance (and no windows of its own): still shown
	rep = []ProviderQuota{
		{Source: "zai", Accounts: []QuotaAccount{{FiveHour: &QuotaWindow{Used: 5, Cap: 100, Unit: "pct"}}}},
		{Source: "deepseek", Accounts: []QuotaAccount{{MonthlyCredits: ptrF(17.29)}}},
	}
	out = aggregateUsage(rep, []string{"zai", "deepseek"})
	if out.Credits == nil || out.Credits.Balance != 17.29 {
		t.Errorf("single-credits-source %+v, want 17.29", out.Credits)
	}

	// two sources with balances, neither owns the worst session → omitted
	// rather than guessing a currency
	rep = []ProviderQuota{
		{Source: "zai", Accounts: []QuotaAccount{{FiveHour: &QuotaWindow{Used: 5, Cap: 100, Unit: "pct"}}}},
		{Source: "deepseek", Accounts: []QuotaAccount{{MonthlyCredits: ptrF(17.29)}}},
		{Source: "commandcode", Accounts: []QuotaAccount{{MonthlyCredits: ptrF(69.99)}}},
	}
	out = aggregateUsage(rep, []string{"zai", "deepseek", "command-code"})
	if out.Credits != nil {
		t.Errorf("ambiguous credits %+v, want nil", out.Credits)
	}
}

func ptrF(v float64) *float64 { return &v }

// usageTestHarness: one cmdcode provider (live upstream via redirectRT) plus a
// key whose usage permission the tests flip through the admin PUT endpoint.
func usageTestHarness(t *testing.T) *httptest.Server {
	t.Helper()
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(ccCreditsJSON))
	}))
	t.Cleanup(up.Close)
	hits := 0
	resetQuotaCache(t, &hits, up.URL)

	cfg := &config.Config{
		Listen: ":0",
		Keys: []*config.Key{
			{Key: "ar-agent", Name: "pi", Allow: []string{"*"}},                          // Usage nil → allowed
			{Key: "ar-revoked", Name: "nouse", Allow: []string{"*"}, Usage: ptrB(false)}, // explicitly revoked
		},
		Providers: []*config.Provider{
			{Name: "cmdcode", BaseURL: "https://api.commandcode.ai/provider/v1", Wire: "openai", Preset: "cmdcode",
				Auth: config.AuthConf{Type: "static", Keys: []string{"k1-secret"}}},
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
	// real config file so PUT /admin/api/keys/{name} can mutate+reload
	cfgPath := filepath.Join(t.TempDir(), "config.yaml")
	if err := cfg.Save(cfgPath); err != nil {
		t.Fatal(err)
	}
	reg := provider.New(cfg)
	p := proxy.NewProxy(reg, st, cfg.CostFor)
	srv := New(p, auth.NewKeyStore(cfg.Keys), st, cfgPath, "secretpw", "test")
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	return ts
}

func ptrB(v bool) *bool { return &v }

func TestUsageEndpoint(t *testing.T) {
	ts := usageTestHarness(t)

	get := func(key, query string) (int, string) {
		req, _ := http.NewRequest("GET", ts.URL+"/v1/usage"+query, nil)
		req.Header.Set("Authorization", "Bearer "+key)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(b)
	}

	// default-ON key, provider-scoped: cmdcode windows → pct
	code, body := get("ar-agent", "?provider=command-code")
	if code != 200 {
		t.Fatalf("status %d: %s", code, body)
	}
	var rep usageReport
	if err := json.Unmarshal([]byte(body), &rep); err != nil {
		t.Fatalf("bad json: %v\n%s", err, body)
	}
	if rep.Provider != "command-code" {
		t.Errorf("provider %q", rep.Provider)
	}
	if w := rep.Windows["session"]; w == nil || w.RemainingPct != 96 || w.ResetAt != 1789918840825 {
		t.Errorf("session %+v, want 96 remaining", rep.Windows["session"])
	}
	if w := rep.Windows["weekly"]; w == nil || w.RemainingPct != 80 {
		t.Errorf("weekly %+v, want 80 remaining", rep.Windows["weekly"])
	}
	if rep.Credits == nil || rep.Credits.Currency != "USD" || rep.Credits.Balance != 69.99 {
		t.Errorf("credits %+v", rep.Credits)
	}
	if len(rep.Providers) != 1 || rep.Providers[0] != "command-code" {
		t.Errorf("providers %v", rep.Providers)
	}

	// alias slug hits the same source
	if code, _ := get("ar-agent", "?provider=cmd"); code != 200 {
		t.Errorf("cmd alias status %d", code)
	}

	// no provider → aggregate (single source here), provider empty
	code, body = get("ar-agent", "")
	if code != 200 {
		t.Fatalf("aggregate status %d: %s", code, body)
	}
	rep = usageReport{}
	if err := json.Unmarshal([]byte(body), &rep); err != nil {
		t.Fatalf("bad json: %v", err)
	}
	if rep.Provider != "" || rep.Windows["session"] == nil || rep.Windows["session"].RemainingPct != 96 {
		t.Errorf("aggregate %+v", rep)
	}

	// unknown provider (opencode-go has no upstream usage API)
	code, body = get("ar-agent", "?provider=ocg")
	if code != 404 || !strings.Contains(body, "unknown provider") {
		t.Errorf("ocg: %d %s", code, body)
	}

	// revoked key → 403 (both scoped and aggregate)
	code, body = get("ar-revoked", "?provider=zai")
	if code != 403 || !strings.Contains(body, "usage permission revoked") {
		t.Errorf("revoked: %d %s", code, body)
	}
	if code, body := get("ar-revoked", ""); code != 403 {
		t.Errorf("revoked aggregate: %d %s", code, body)
	}

	// bad key → 401 from the auth wrap
	if code, _ := get("ar-wrong", ""); code != 401 {
		t.Errorf("bad key status %d", code)
	}
}

// Reviewer acceptance: a restricted key (allow patterns) must not read the
// windows, balances, or roster of upstreams it can never route to.
func TestUsageAllowScoped(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(ccCreditsJSON))
	}))
	t.Cleanup(up.Close)
	hits := 0
	resetQuotaCache(t, &hits, up.URL)

	cfg := &config.Config{
		Listen: ":0",
		Keys: []*config.Key{
			{Key: "ar-scoped", Name: "cmd-only", Allow: []string{"cmd/*"}},
		},
		Providers: []*config.Provider{
			{Name: "cmdcode", Prefix: "cmd", BaseURL: "https://api.commandcode.ai/provider/v1", Wire: "openai", Preset: "cmdcode",
				Models: []string{"deepseek-v4-flash"},
				Auth:   config.AuthConf{Type: "static", Keys: []string{"k1-secret"}}},
			// same gateway, wildcard model list — must NOT broaden visibility
			{Name: "cmdcode-claude", Prefix: "cmd", BaseURL: "https://api.commandcode.ai/anthropic", Wire: "anthropic",
				Auth: config.AuthConf{Type: "static", Keys: []string{"k1-secret"}}},
			{Name: "deepseek", Prefix: "ds", BaseURL: "https://api.deepseek.com", Wire: "openai", Preset: "deepseek",
				Models: []string{"deepseek-flash"},
				Auth:   config.AuthConf{Type: "static", Keys: []string{"ds-key"}}},
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
	srv := New(p, auth.NewKeyStore(cfg.Keys), st, "", "pw", "test")
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)

	get := func(query string) (int, usageReport) {
		req, _ := http.NewRequest("GET", ts.URL+"/v1/usage"+query, nil)
		req.Header.Set("Authorization", "Bearer ar-scoped")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		var rep usageReport
		if resp.StatusCode == 200 {
			if err := json.Unmarshal(b, &rep); err != nil {
				t.Fatalf("bad json: %v\n%s", err, b)
			}
		}
		return resp.StatusCode, rep
	}

	// own upstream: visible with full report
	code, rep := get("?provider=command-code")
	if code != 200 || rep.Provider != "command-code" || rep.Credits == nil {
		t.Errorf("command-code: %d %+v", code, rep)
	}
	// out-of-scope upstream: same 404 as unknown — existence not leaked
	code, _ = get("?provider=deepseek")
	if code != 404 {
		t.Errorf("deepseek: %d, want 404 (out of allow scope)", code)
	}
	// aggregate + roster contain no out-of-scope data
	code, rep = get("")
	if code != 200 {
		t.Fatalf("aggregate: %d", code)
	}
	if len(rep.Providers) != 1 || rep.Providers[0] != "command-code" {
		t.Errorf("roster %v, want [command-code] only", rep.Providers)
	}
	if rep.Windows["session"] == nil {
		t.Errorf("aggregate lost own windows: %+v", rep.Windows)
	}

	// bare-pattern keys: allow ["deepseek-flash"] admits the bare id, which the
	// wildcard cmdcode-claude (models: []) also catches — so the cmdcode source
	// is genuinely routable for this key and therefore visible (visibility
	// mirrors Registry.Resolve, it does not guess intent).
	cfg.Keys[0].Allow = []string{"deepseek-flash"}
	reg.Reload(cfg)
	code, rep = get("?provider=deepseek")
	if code != 200 || rep.Provider != "deepseek" {
		t.Errorf("bare pattern: %d %+v", code, rep)
	}
	if code, _ := get("?provider=command-code"); code != 200 {
		t.Errorf("bare pattern + wildcard gateway source: want 200 (routable)")
	}

	// fully-claimed patterns ("ds/*", a known prefix) stay locked to their
	// namespace even with a wildcard provider present
	cfg.Keys[0].Allow = []string{"ds/*"}
	reg.Reload(cfg)
	code, rep = get("?provider=deepseek")
	if code != 200 || rep.Provider != "deepseek" {
		t.Errorf("claimed prefix ds: %d %+v", code, rep)
	}
	if code, _ := get("?provider=command-code"); code != 404 {
		t.Errorf("claimed prefix ds vs command-code: want 404")
	}

	// ── routes: visibility mirrors Resolve's routes-first chain selection ──
	// A route "cmd/ds*" → [cmdcode-claude] (wildcard member) is genuinely
	// routable by a key allowed "cmd/ds*" — the wildcard member must surface
	// the commandcode source even though its Models list is empty.
	cfg.Routes = []*config.Route{{Match: "cmd/ds*", Chain: []string{"cmdcode-claude"}}}
	cfg.Keys[0].Allow = []string{"cmd/ds*"}
	reg.Reload(cfg)
	code, rep = get("?provider=command-code")
	if code != 200 || rep.Provider != "command-code" {
		t.Errorf("route chain wildcard member: %d %+v", code, rep)
	}
	// key NOT allowed any model the route matches → still 404
	cfg.Keys[0].Allow = []string{"ds/*"}
	reg.Reload(cfg)
	if code, _ := get("?provider=command-code"); code != 404 {
		t.Errorf("route exists but key out of route scope: want 404")
	}
	// chain NOT naming the queried source's provider (deepseek) → invisible
	cfg.Routes = []*config.Route{{Match: "cmd/ds*", Chain: []string{"cmdcode"}}}
	cfg.Keys[0].Allow = []string{"cmd/ds*"}
	reg.Reload(cfg)
	if code, _ := get("?provider=deepseek"); code != 404 {
		t.Errorf("route chains other provider: deepseek want 404")
	}
	// route to a DISABLED provider → not reachable
	cfg.Routes = []*config.Route{{Match: "cmd/ds*", Chain: []string{"cmdcode-claude"}}}
	cfg.Providers[1].Disabled = true
	reg.Reload(cfg)
	if code, _ := get("?provider=command-code"); code != 404 {
		t.Errorf("route to disabled provider: want 404")
	}
	cfg.Providers[1].Disabled = false
	cfg.Routes = nil
	reg.Reload(cfg)
}

func TestUsagePermissionEdit(t *testing.T) {
	ts := usageTestHarness(t)

	admin := func(method, path, body string) *http.Response {
		req, _ := http.NewRequest(method, ts.URL+"/admin/api"+path, strings.NewReader(body))
		req.SetBasicAuth("x", "secretpw")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		return resp
	}
	keyGet := func(key string) int {
		req, _ := http.NewRequest("GET", ts.URL+"/v1/usage", nil)
		req.Header.Set("Authorization", "Bearer "+key)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp.StatusCode
	}

	// flip pi's usage off via PUT, confirm 403, keys listing shows it, flip back
	resp := admin("PUT", "/keys/pi", `{"usage": false}`)
	if resp.StatusCode != 200 {
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		t.Fatalf("PUT usage=false: %d %s", resp.StatusCode, b)
	}
	resp.Body.Close()
	if code := keyGet("ar-agent"); code != 403 {
		t.Errorf("after revoke status %d, want 403", code)
	}

	resp = admin("GET", "/keys", "")
	var list struct {
		Keys []struct {
			Name  string `json:"name"`
			Usage *bool  `json:"usage"`
		} `json:"keys"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&list); err != nil {
		t.Fatalf("keys decode: %v", err)
	}
	resp.Body.Close()
	var found *bool
	for _, k := range list.Keys {
		if k.Name == "pi" {
			found = k.Usage
		}
	}
	if found == nil || *found {
		t.Errorf("keys listing usage = %v, want false", found)
	}

	resp = admin("PUT", "/keys/pi", `{"usage": true}`)
	resp.Body.Close()
	if code := keyGet("ar-agent"); code != 200 {
		t.Errorf("after re-allow status %d, want 200", code)
	}

	// rename via PUT + unknown key error
	resp = admin("PUT", "/keys/nope", `{"usage": true}`)
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 400 || !strings.Contains(string(b), "no key named") {
		t.Errorf("unknown key edit: %d %s", resp.StatusCode, b)
	}
}
