// Per-key monthly USD budgets: enforcement gate (429 budget_exceeded +
// Retry-After), count_tokens exemption, spend-cache TTL behavior, and the
// admin API's monthly_usd/month_spent/month_limit_pct key-row columns.
package server

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/bacnh85/yardmaster/internal/auth"
	"github.com/bacnh85/yardmaster/internal/config"
	"github.com/bacnh85/yardmaster/internal/provider"
	"github.com/bacnh85/yardmaster/internal/proxy"
	"github.com/bacnh85/yardmaster/internal/store"
)

// budgetHarness: server over a real (empty) store with the given inbound
// keys, exercised via /v1/models (auth-wrapped, no upstream traffic).
func budgetHarness(t *testing.T, keys []*config.Key) (*httptest.Server, *store.Store) {
	t.Helper()
	cfg := &config.Config{Listen: ":0", Keys: keys}
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
	return ts, st
}

// modelsDo GETs /v1/models with a bearer key, returning status and body.
func modelsDo(t *testing.T, ts *httptest.Server, key string) (int, []byte) {
	t.Helper()
	req, _ := http.NewRequest("GET", ts.URL+"/v1/models", nil)
	req.Header.Set("Authorization", "Bearer "+key)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, b
}

// countTokensDo POSTs an empty count_tokens body with a bearer key,
// returning the status and Retry-After header.
func countTokensDo(t *testing.T, ts *httptest.Server, key string) (int, string) {
	t.Helper()
	req, _ := http.NewRequest("POST", ts.URL+"/v1/messages/count_tokens", strings.NewReader(`{}`))
	req.Header.Set("Authorization", "Bearer "+key)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, resp.Body)
	return resp.StatusCode, resp.Header.Get("Retry-After")
}

// seedSpend records one costed request under keyName and waits for the async
// batch writer to flush it (Submit is buffered 500ms).
func seedSpend(t *testing.T, st *store.Store, keyName string, usd float64) {
	t.Helper()
	st.Submit(&store.Record{Ts: time.Now().UnixMilli(), Key: keyName, CostUSD: usd})
	deadline := time.Now().Add(3 * time.Second)
	for {
		spent, err := st.MonthSpend(keyName, monthStartUnixMS())
		if err != nil {
			t.Fatal(err)
		}
		if spent == usd {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("spend %v for %q never flushed", usd, keyName)
		}
		time.Sleep(25 * time.Millisecond)
	}
}

// over-budget key: 429 budget_exceeded + Retry-After counting down to the
// 1st of next month; unlimited key on the same server is unaffected;
// count_tokens stays 200 for the budgeted key.
func TestMonthlyBudgetExceeded429(t *testing.T) {
	ts, st := budgetHarness(t, []*config.Key{
		{Key: "ar-capped", Name: "capped", Allow: []string{"*"}, MonthlyUSD: 1.00},
		{Key: "ar-free", Name: "free", Allow: []string{"*"}},
	})
	seedSpend(t, st, "capped", 1.25)

	code, b := modelsDo(t, ts, "ar-capped")
	if code != 429 {
		t.Fatalf("capped key: want 429, got %d (%s)", code, b)
	}
	var out struct {
		Error struct {
			Type    string `json:"type"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatalf("decode: %v\n%s", err, b)
	}
	if out.Error.Type != "budget_exceeded" {
		t.Fatalf("error type %q, want budget_exceeded", out.Error.Type)
	}
	for _, want := range []string{"capped", "1.25", "1.00"} {
		if !strings.Contains(out.Error.Message, want) {
			t.Errorf("message %q missing %q", out.Error.Message, want)
		}
	}
	// sane Retry-After: strictly between now and the full month bound
	ra, err := strconv.Atoi(respHeader(t, ts, "ar-capped"))
	if err != nil {
		t.Fatalf("Retry-After not an int: %v", err)
	}
	maxS := int(time.Until(nextMonthStart()).Seconds())
	if ra < 1 || ra > maxS {
		t.Fatalf("Retry-After %d, want 1..%d (seconds until the 1st)", ra, maxS)
	}

	// unlimited key sails through
	if code, b := modelsDo(t, ts, "ar-free"); code != 200 {
		t.Fatalf("unlimited key: want 200, got %d (%s)", code, b)
	}

	// count_tokens exempt: budgeted key still gets its context-trim count
	if code, _ := countTokensDo(t, ts, "ar-capped"); code != 200 {
		t.Fatalf("count_tokens for capped key: want 200, got %d", code)
	}
}

// respHeader re-issues the request to read a header (429 body decode above
// consumes the response this helper pairs with).
func respHeader(t *testing.T, ts *httptest.Server, key string) string {
	t.Helper()
	req, _ := http.NewRequest("GET", ts.URL+"/v1/models", nil)
	req.Header.Set("Authorization", "Bearer "+key)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, resp.Body)
	return resp.Header.Get("Retry-After")
}

// exactly-at-budget blocks: the gate is >=, so a fully spent cap closes the
// key for the rest of the month.
func TestMonthlyBudgetAtLimitBlocks(t *testing.T) {
	ts, st := budgetHarness(t, []*config.Key{
		{Key: "ar-exact", Name: "exact", Allow: []string{"*"}, MonthlyUSD: 0.5},
	})
	seedSpend(t, st, "exact", 0.5)
	if code, b := modelsDo(t, ts, "ar-exact"); code != 429 {
		t.Fatalf("at-limit key: want 429, got %d (%s)", code, b)
	}
}

// cache behavior: a second gated call within the TTL must reuse the cached
// spend (mutation after the first read stays invisible until expiry), and
// budgetInvalidate drops the entry so the fresh value shows through.
func TestBudgetCacheTTLAndInvalidate(t *testing.T) {
	_, st := budgetHarness(t, []*config.Key{
		{Key: "ar-cache", Name: "cache", Allow: []string{"*"}, MonthlyUSD: 10},
	})
	srv := New(nil, nil, st, "", "secretpw", "test") // cache under test directly

	spent, err := srv.monthSpendCached("cache", monthStartUnixMS())
	if err != nil || spent != 0 {
		t.Fatalf("first read: %v, %v; want 0, nil", spent, err)
	}
	// cost lands via the batch writer AFTER the gate cached 0
	seedSpend(t, st, "cache", 7)

	spent, err = srv.monthSpendCached("cache", monthStartUnixMS())
	if err != nil || spent != 0 {
		t.Fatalf("within TTL: got %v, %v; want cached 0 (no requery)", spent, err)
	}

	// TTL expiry re-reads the store
	srv.budgets.mu.Lock()
	srv.budgets.spend["cache"] = spendCacheEntry{spend: 0, checkedAt: time.Now().Add(-budgetCacheTTL - time.Second)}
	srv.budgets.mu.Unlock()
	spent, err = srv.monthSpendCached("cache", monthStartUnixMS())
	if err != nil || spent != 7 {
		t.Fatalf("after TTL: got %v, %v; want fresh 7", spent, err)
	}

	// invalidate forces a fresh read too (delete/rename path)
	srv.budgetInvalidate("cache")
	if got := srv.budgets.spend["cache"]; got.checkedAt != (time.Time{}) {
		t.Fatalf("entry survived invalidate: %+v", got)
	}
	if spent, err = srv.monthSpendCached("cache", monthStartUnixMS()); err != nil || spent != 7 {
		t.Fatalf("after invalidate: got %v, %v; want fresh 7", spent, err)
	}
}

// admin API: PUT monthly_usd reflects in GET; pct is spend/cap; omitted
// (absent JSON key) when unlimited; negative rejected.
func TestAdminKeysBudgetFields(t *testing.T) {
	ts, _, _ := keysEditHarness(t)

	rowOf := func(name string) map[string]any {
		t.Helper()
		for _, k := range keysGet(t, ts) {
			if k["name"] == name {
				return k
			}
		}
		t.Fatalf("key %q missing from GET", name)
		return nil
	}

	// unlimited legacy key: no month_limit_pct in the row
	pi := rowOf("pi")
	if got, ok := pi["monthly_usd"].(float64); !ok || got != 0 {
		t.Fatalf("monthly_usd = %v, want 0", pi["monthly_usd"])
	}
	if _, present := pi["month_limit_pct"]; present {
		t.Fatalf("month_limit_pct %v present for unlimited key, want omitted", pi["month_limit_pct"])
	}
	if _, ok := pi["month_spent"].(float64); !ok {
		t.Fatalf("month_spent missing/non-numeric: %v", pi["month_spent"])
	}

	// set a cap via PUT, then verify GET + pct math
	if code, b := adminDo(t, ts, "PUT", "keys/pi", `{"monthly_usd":4}`); code != 200 {
		t.Fatalf("PUT monthly_usd: %d %s", code, b)
	}
	pi = rowOf("pi")
	if got := pi["monthly_usd"].(float64); got != 4 {
		t.Fatalf("monthly_usd after PUT = %v, want 4", got)
	}
	if pi["month_spent"].(float64) != 0 {
		t.Fatalf("month_spent = %v, want 0 (no spend seeded)", pi["month_spent"])
	}
	if got := pi["month_limit_pct"].(float64); got != 0 {
		t.Fatalf("month_limit_pct = %v, want 0 (0 spent of 4)", got)
	}

	// negative rejected on both write paths
	if code, _ := adminDo(t, ts, "PUT", "keys/pi", `{"monthly_usd":-1}`); code != 400 {
		t.Fatalf("negative PUT: want 400")
	}
	if code, _ := adminDo(t, ts, "POST", "keys", `{"name":"neg","monthly_usd":-5}`); code != 400 {
		t.Fatalf("negative POST: want 400")
	}
}

// keysGet decodes the admin keys list as generic rows.
func keysGet(t *testing.T, ts *httptest.Server) []map[string]any {
	t.Helper()
	code, b := adminDo(t, ts, "GET", "keys", "")
	if code != 200 {
		t.Fatalf("keys GET: %d %s", code, b)
	}
	var out struct {
		Keys []map[string]any `json:"keys"`
	}
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatalf("decode: %v\n%s", err, b)
	}
	return out.Keys
}

// pct reflects fresh month-to-date spend, including the over-limit >100 case,
// and stays present (not omitted) for a budgeted key.
func TestAdminKeysBudgetPctOverLimit(t *testing.T) {
	ts, cfg, st := keysEditHarness(t)

	// give the legacy key a budget directly in the loaded config
	for _, k := range cfg.Keys {
		if k.Name == "pi" {
			k.MonthlyUSD = 2
		}
	}
	// mutate+reload so the server sees it (same flow the dashboard uses)
	if code, b := adminDo(t, ts, "PUT", "keys/pi", `{"monthly_usd":2}`); code != 200 {
		t.Fatalf("PUT monthly_usd: %d %s", code, b)
	}
	seedSpend(t, st, "pi", 3)
	var pct float64
	var found bool
	for _, k := range keysGet(t, ts) {
		if k["name"] == "pi" {
			pct = k["month_limit_pct"].(float64)
			found = true
		}
	}
	if !found {
		t.Fatal("key pi missing from GET")
	}
	if pct <= 100 {
		t.Fatalf("month_limit_pct = %v, want >100 (3 spent of 2)", pct)
	}
	// sanity: exactly 150 for 3/2
	if pct < 149 || pct > 151 {
		t.Fatalf("month_limit_pct = %v, want ~150", pct)
	}
}

// a deleted key's cached spend must not leak to a later key reusing the name.
func TestBudgetCacheInvalidatedOnDelete(t *testing.T) {
	_, st := budgetHarness(t, []*config.Key{
		{Key: "ar-del", Name: "del", Allow: []string{"*"}, MonthlyUSD: 1},
	})
	srv := New(nil, nil, st, "", "secretpw", "test")
	seedSpend(t, st, "del", 5)
	if _, err := srv.monthSpendCached("del", monthStartUnixMS()); err != nil {
		t.Fatal(err)
	}
	srv.budgetInvalidate("del")
	if e, ok := srv.budgets.spend["del"]; ok {
		t.Fatalf("cache entry survived delete-path invalidate: %+v", e)
	}
}
