package server

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/bacnh85/yardmaster/internal/auth"
	"github.com/bacnh85/yardmaster/internal/config"
	"github.com/bacnh85/yardmaster/internal/provider"
	"github.com/bacnh85/yardmaster/internal/proxy"
	"github.com/bacnh85/yardmaster/internal/store"
)

const ccCreditsJSON = `{"credits":{"belowThreshold":false,"creditThreshold":0,"monthlyCredits":69.99,"purchasedCredits":0,"freeCredits":0},"windowLimits":{"limited":false,"exceeded":null,"fiveHour":{"used":0.5,"cap":14,"exceeded":false,"resetAt":1789918840825},"weekly":{"used":7,"cap":35,"exceeded":false,"resetAt":1790253079242}}}`

// redirectRT sends api.commandcode.ai requests to the test upstream so the
// handler's host-matched URL derivation is exercised end to end.
type redirectRT struct {
	base *url.URL
	hits *int
}

func (rt redirectRT) RoundTrip(req *http.Request) (*http.Response, error) {
	*rt.hits++
	req.URL.Scheme = rt.base.Scheme
	req.URL.Host = rt.base.Host
	return http.DefaultTransport.RoundTrip(req)
}

func resetQuotaCache(t *testing.T, hits *int, upURL string) *url.URL {
	t.Helper()
	u, err := url.Parse(upURL)
	if err != nil {
		t.Fatal(err)
	}
	origClient, origReport, origUntil := quotaClient, quotaReport, quotaUntil
	quotaClient = &http.Client{Timeout: 5 * time.Second, Transport: redirectRT{base: u, hits: hits}}
	quotaReport, quotaUntil = nil, time.Time{}
	t.Cleanup(func() { quotaClient, quotaReport, quotaUntil = origClient, origReport, origUntil })
	return u
}

func TestQuotaEndpoint(t *testing.T) {
	hits := 0
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/alpha/billing/credits" {
			t.Errorf("upstream path %s", r.URL.Path)
		}
		key := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		if key != "k1-secret" && key != "k2-secret" {
			t.Errorf("unexpected key %q", key)
		}
		w.Write([]byte(ccCreditsJSON))
	}))
	defer up.Close()
	resetQuotaCache(t, &hits, up.URL)

	dbPath := t.TempDir() + "/test.db"
	st, err := store.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	ccBase := "https://api.commandcode.ai/provider/v1"
	cfg := &config.Config{
		Listen: ":0",
		Keys:   []*config.Key{{Key: "ar-agent", Name: "pi", Allow: []string{"*"}}},
		Providers: []*config.Provider{
			{Name: "cmdcode", BaseURL: ccBase, Wire: "openai", Preset: "cmdcode",
				Auth: config.AuthConf{Type: "static", Keys: []string{"k1-secret", "k2-secret"}, KeyLabels: []string{"Alpha", "Beta"}}},
			{Name: "cmdcode-claude", BaseURL: ccBase, Wire: "anthropic", Preset: "cmdcode-claude",
				Auth: config.AuthConf{Type: "static", Keys: []string{"k1-secret"}}},
			{Name: "unrelated", BaseURL: up.URL, Wire: "openai", // wrong host → no quota source
				Auth: config.AuthConf{Type: "static", Keys: []string{"k1-secret"}}},
			{Name: "cmd-nokeys", BaseURL: ccBase, Wire: "openai"}, // no keys → skipped
		},
	}
	cfg.Defaults()
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	reg := provider.New(cfg)
	p := proxy.NewProxy(reg, st, cfg.CostFor)
	srv := New(p, auth.NewKeyStore(cfg.Keys), st, "", "secretpw", "test")
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	getQuota := func(query string) (string, map[string]any) {
		req, _ := http.NewRequest("GET", ts.URL+"/admin/api/quota"+query, nil)
		req.SetBasicAuth("x", "secretpw")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != 200 {
			t.Fatalf("quota status %d", resp.StatusCode)
		}
		b, err := io.ReadAll(resp.Body)
		if err != nil {
			t.Fatal(err)
		}
		body := string(b)
		var out map[string]any
		if err := json.Unmarshal([]byte(body), &out); err != nil {
			t.Fatalf("bad json: %v\n%s", err, body)
		}
		return body, out
	}

	body, out := getQuota("")
	quotas := out["quotas"].([]any)
	if len(quotas) != 1 {
		t.Fatalf("want 1 quota group, got %d: %s", len(quotas), body)
	}
	g := quotas[0].(map[string]any)
	if g["source"] != "commandcode" {
		t.Errorf("source %v", g["source"])
	}
	prov := g["providers"].([]any)
	if len(prov) != 2 || prov[0] != "cmdcode" || prov[1] != "cmdcode-claude" {
		t.Errorf("providers %v", prov)
	}
	accts := g["accounts"].([]any)
	if len(accts) != 2 { // k1 shared across the two wire entries → deduped
		t.Fatalf("want 2 accounts, got %d: %s", len(accts), body)
	}
	a1 := accts[0].(map[string]any)
	if a1["label"] != "Alpha" || a1["suffix"] != "…secret" {
		t.Errorf("account 1 label/suffix: %v / %v", a1["label"], a1["suffix"])
	}
	fh := a1["five_hour"].(map[string]any)
	if fh["used"].(float64) != 0.5 || fh["cap"].(float64) != 14 || fh["reset_at"].(float64) != 1789918840825 {
		t.Errorf("five_hour %v", fh)
	}
	wk := a1["weekly"].(map[string]any)
	if wk["cap"].(float64) != 35 {
		t.Errorf("weekly %v", wk)
	}
	if a1["monthly_credits"].(float64) != 69.99 {
		t.Errorf("monthly %v", a1["monthly_credits"])
	}
	if a1["monthly_total"].(float64) != 70 { // $14/$35 caps identify GOAT
		t.Errorf("monthly_total %v", a1["monthly_total"])
	}
	if strings.Contains(body, "k1-secret") || strings.Contains(body, "k2-secret") {
		t.Fatal("raw key leaked in response")
	}
	if hitsNow := *redirectHits(t); hitsNow != 2 {
		t.Fatalf("want 2 upstream hits after first call, got %d", hitsNow)
	}

	// cache: second call refetches nothing
	getQuota("")
	if n := *redirectHits(t); n != 2 {
		t.Fatalf("cache miss: hits %d", n)
	}
	// refresh=1 bypasses
	getQuota("?refresh=1")
	if n := *redirectHits(t); n != 4 {
		t.Fatalf("refresh bypass failed: hits %d", n)
	}
}

// redirectHits returns the hit counter registered by resetQuotaCache — the
// counter travels with quotaClient's transport, so read it from there.
func redirectHits(t *testing.T) *int {
	t.Helper()
	if rt, ok := quotaClient.Transport.(redirectRT); ok {
		return rt.hits
	}
	t.Fatal("quotaClient transport is not a redirectRT")
	return nil
}

// quotaTestHarness wires one single-key cmdcode provider against a delayable
// upstream; returns the admin test server and a delay setter (µs, atomic).
func quotaTestHarness(t *testing.T, hits *int) (*httptest.Server, func(int64)) {
	t.Helper()
	var delayUs atomic.Int64
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// hits are counted by the redirectRT transport — no count here
		if d := delayUs.Load(); d > 0 {
			time.Sleep(time.Duration(d) * time.Microsecond)
		}
		w.Write([]byte(ccCreditsJSON))
	}))
	t.Cleanup(up.Close)
	resetQuotaCache(t, hits, up.URL)

	cfg := &config.Config{
		Listen: ":0",
		Keys:   []*config.Key{{Key: "ar-agent", Name: "pi", Allow: []string{"*"}}},
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
	reg := provider.New(cfg)
	p := proxy.NewProxy(reg, st, cfg.CostFor)
	srv := New(p, auth.NewKeyStore(cfg.Keys), st, "", "secretpw", "test")
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	return ts, func(us int64) { delayUs.Store(us) }
}

func TestQuotaCancelDoesNotPoisonCache(t *testing.T) {
	hits := 0
	ts, setDelay := quotaTestHarness(t, &hits)
	adminGet := func(query string) string {
		req, _ := http.NewRequest("GET", ts.URL+"/admin/api/quota"+query, nil)
		req.SetBasicAuth("x", "secretpw")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return string(b)
	}

	// seed a good cached report
	if body := adminGet(""); strings.Contains(body, "context canceled") {
		t.Fatalf("seed report poisoned: %s", body)
	}
	if hits != 1 {
		t.Fatalf("seed build: hits %d", hits)
	}

	// slow the upstream, fire a request, cancel it mid-build
	setDelay(300_000) // 300ms
	ctx, cancel := context.WithCancel(context.Background())
	req, _ := http.NewRequestWithContext(ctx, "GET", ts.URL+"/admin/api/quota?refresh=1", nil)
	req.SetBasicAuth("x", "secretpw")
	done := make(chan error, 1)
	go func() {
		_, err := http.DefaultClient.Do(req)
		done <- err
	}()
	time.Sleep(60 * time.Millisecond)
	cancel()
	if err := <-done; err == nil {
		t.Error("expected client error after cancel")
	}
	setDelay(0)

	// the detached build still finishes and lands clean data in the cache
	deadline := time.Now().Add(3 * time.Second)
	for hits < 2 && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	body := adminGet("") // cached — no upstream hit (hits stay 2)
	if hits != 2 {
		t.Fatalf("post-cancel hits %d, want 2 (detached build must complete)", hits)
	}
	if strings.Contains(body, "context canceled") {
		t.Fatalf("canceled request poisoned the shared cache: %s", body)
	}
	if !strings.Contains(body, "monthly_credits") {
		t.Fatalf("cache lost good data after cancel: %s", body)
	}
}

func TestQuotaSingleflight(t *testing.T) {
	hits := 0
	ts, setDelay := quotaTestHarness(t, &hits)
	setDelay(150_000) // 150ms upstream

	const viewers = 3
	bodies := make([]string, viewers)
	var wg sync.WaitGroup
	start := time.Now()
	for i := range viewers {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			req, _ := http.NewRequest("GET", ts.URL+"/admin/api/quota", nil)
			req.SetBasicAuth("x", "secretpw")
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Error(err)
				return
			}
			b, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			bodies[i] = string(b)
		}(i)
	}
	wg.Wait()
	elapsed := time.Since(start)

	if hits != 1 {
		t.Fatalf("want exactly 1 upstream build for %d parallel viewers, hits %d", viewers, hits)
	}
	for i, b := range bodies {
		if b != bodies[0] {
			t.Errorf("viewer %d got a different report", i)
		}
	}
	// ~one fetch duration, not viewers×: generous 2× bound to stay CI-safe
	if elapsed > 400*time.Millisecond {
		t.Errorf("parallel viewers serialized: %v for 150ms upstream", elapsed)
	}
}

func TestCCMonthlyTotal(t *testing.T) {
	cases := []struct {
		hourly, weekly, want float64
	}{
		{3, 6, 10}, {14, 35, 70}, {16, 40, 80}, {45, 90, 150}, {90, 180, 300}, {12, 24, 40},
		{9, 9, 0}, // unknown caps → no total
	}
	for _, c := range cases {
		if got := ccMonthlyTotal(c.hourly, c.weekly); got != c.want {
			t.Errorf("ccMonthlyTotal(%v,%v) = %v, want %v", c.hourly, c.weekly, got, c.want)
		}
	}
}

func TestQuotaSourceMatcher(t *testing.T) {
	cases := map[string]string{
		"https://api.commandcode.ai/provider/v1": "commandcode",
		"https://api.commandcode.ai/other/path":  "commandcode",
		"http://API.commandcode.ai":              "commandcode", // host match is case-insensitive
		"https://api.example.com/v1":             "",
		"not a url":                              "",
	}
	for in, want := range cases {
		if got := quotaSource(in); got != want {
			t.Errorf("quotaSource(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestQuotaFetchError(t *testing.T) {
	orig := quotaClient
	t.Cleanup(func() { quotaClient = orig })

	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(503)
		w.Write([]byte("boom"))
	}))
	defer up.Close()
	quotaClient = up.Client()

	if _, err := fetchCommandCodeQuota(context.Background(), up.URL, "k"); err == nil || !strings.Contains(err.Error(), "503") {
		t.Errorf("want HTTP 503 error, got %v", err)
	}

	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("not json"))
	}))
	defer bad.Close()
	quotaClient = bad.Client()
	if _, err := fetchCommandCodeQuota(context.Background(), bad.URL, "k"); err == nil {
		t.Error("want decode error, got nil")
	}
}
