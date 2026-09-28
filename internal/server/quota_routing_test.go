package server

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/bacnh85/yardmaster/internal/auth"
	"github.com/bacnh85/yardmaster/internal/config"
	"github.com/bacnh85/yardmaster/internal/provider"
	"github.com/bacnh85/yardmaster/internal/proxy"
	"github.com/bacnh85/yardmaster/internal/store"
)

// Quota-aware routing end to end: the cmdcode billing stub reports
// limited:true (account exhausted), the config provider opts in with
// skip_when_exhausted, so a chat request must dispatch to the non-opted
// fallback provider — and GET /admin/api/quota must keep working.
func TestSourceExhaustedLimitedNoWindows(t *testing.T) {
	// one Limited account with no windows ⇒ exhausted (M3: the DeepSeek case)
	g := ProviderQuota{Source: "deepseek", Accounts: []QuotaAccount{{Label: "acct-a", Limited: true}}}
	if !sourceExhausted(g) {
		t.Fatal("Limited:true with no windows must gate skip_when_exhausted")
	}
	// same account + one open window elsewhere ⇒ the source can still serve
	g.Accounts = append(g.Accounts, QuotaAccount{Label: "acct-b",
		FiveHour: &QuotaWindow{Used: 1, Cap: 10}})
	if sourceExhausted(g) {
		t.Fatal("an open window must keep the source routable")
	}
}

// M4: the background refresher — not the test calling cachedQuotaReport —
// must populate the exhausted set within one tick of a limited report.
func TestStartQuotaRefresher(t *testing.T) {
	billing := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"credits":{"monthlyCredits":1},"windowLimits":{"limited":true}}`)
	}))
	defer billing.Close()
	billingHits := 0
	resetQuotaCache(t, &billingHits, billing.URL)
	origSet := exhaustedSet
	exhaustedSet = nil
	t.Cleanup(func() { exhaustedSet = origSet })

	cfg := &config.Config{
		Listen: ":0",
		Keys:   []*config.Key{{Key: "ar-agent", Name: "pi", Allow: []string{"*"}}},
		Providers: []*config.Provider{
			{Name: "cmdcode", BaseURL: "https://api.commandcode.ai/provider/v1", Wire: "openai", SkipWhenExhausted: true,
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
	srv := New(p, auth.NewKeyStore(cfg.Keys), st, "", "pw", "test")

	stop := srv.StartQuotaRefresher(20 * time.Millisecond)
	defer stop() // before t.Cleanup restores — no tick can race the unwrites
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if quotaExhausted("cmdcode") {
			return // refresher rebuilt the report and stored the exhausted set
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("exhausted set still empty after 5s — refresher never refreshed")
}

func TestQuotaExhaustedRoutingSkipsToFallback(t *testing.T) {
	// billing stub: limited account → the cmdcode source is exhausted
	billing := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/alpha/billing/credits" {
			t.Errorf("billing path %s", r.URL.Path)
		}
		fmt.Fprint(w, `{"credits":{"monthlyCredits":69.99},"windowLimits":{"limited":true,"fiveHour":{"used":14,"cap":14,"exceeded":true,"resetAt":1789918840825},"weekly":{"used":35,"cap":35,"exceeded":true,"resetAt":1790253079242}}}`)
	}))
	defer billing.Close()
	// the billing fetcher talks to api.commandcode.ai — redirect it
	billingHits := 0
	resetQuotaCache(t, &billingHits, billing.URL)
	origSet := exhaustedSet
	exhaustedSet = nil
	t.Cleanup(func() {
		exhaustedSet = origSet
	})

	// chat upstream for the exhausted provider: any hit here is a routing bug
	var exhaustedHits, fallbackHits atomic.Int32
	exaustUp := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		exhaustedHits.Add(1)
		w.Write([]byte(`{"id":"x","object":"chat.completion","model":"m","choices":[{"index":0,"message":{"role":"assistant","content":"from-exhausted"},"finish_reason":"stop"}],"usage":{}}`))
	}))
	defer exaustUp.Close()
	fallbackUp := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fallbackHits.Add(1)
		w.Write([]byte(`{"id":"x","object":"chat.completion","model":"m","choices":[{"index":0,"message":{"role":"assistant","content":"from-fallback"},"finish_reason":"stop"}],"usage":{}}`))
	}))
	defer fallbackUp.Close()

	cfg := &config.Config{
		Listen: ":0",
		Keys:   []*config.Key{{Key: "ar-agent", Name: "pi", Allow: []string{"*"}}},
		Providers: []*config.Provider{
			{Name: "cmdcode", BaseURL: "https://api.commandcode.ai/provider/v1", Wire: "openai", SkipWhenExhausted: true,
				Auth: config.AuthConf{Type: "static", Keys: []string{"k1-secret"}}},
			{Name: "fallback", BaseURL: fallbackUp.URL, Wire: "openai",
				Auth: config.AuthConf{Type: "static", Keys: []string{"k2-secret"}}},
		},
		Routes: []*config.Route{{Match: "m", Chain: []string{"cmdcode", "fallback"}}},
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
	if p.QuotaExhausted == nil {
		t.Fatal("QuotaExhausted hook not wired despite a quota source being configured")
	}
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)

	// populate the exhausted set through the real report path
	rep := srv.cachedQuotaReport(false)
	if len(rep) != 1 || rep[0].Source != "commandcode" {
		t.Fatalf("report: %+v", rep)
	}
	if !quotaExhausted("cmdcode") {
		t.Fatal("cmdcode source is limited:true — must be marked exhausted")
	}
	if quotaExhausted("fallback") {
		t.Fatal("fallback has no quota source — never exhausted")
	}

	// chat must land on the fallback
	req, _ := http.NewRequest("POST", ts.URL+"/v1/chat/completions",
		strings.NewReader(`{"model":"m","stream":false,"messages":[{"role":"user","content":"hi"}]}`))
	req.Header.Set("Authorization", "Bearer ar-agent")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("chat status %d: %s", resp.StatusCode, b)
	}
	var out struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatalf("bad json: %v", err)
	}
	if got := out.Choices[0].Message.Content; got != "from-fallback" {
		t.Fatalf("served by %q — exhausted provider was dispatched", got)
	}
	if exhaustedHits.Load() != 0 {
		t.Fatalf("exhausted provider got %d upstream calls, want 0", exhaustedHits.Load())
	}
	if fallbackHits.Load() != 1 {
		t.Fatalf("fallback calls: %d, want 1", fallbackHits.Load())
	}

	// the dashboard endpoint still works (same report, same handler)
	req, _ = http.NewRequest("GET", ts.URL+"/admin/api/quota", nil)
	req.SetBasicAuth("x", "secretpw")
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	qb, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 {
		t.Fatalf("quota status %d: %s", resp.StatusCode, qb)
	}
	var q struct {
		Quotas []ProviderQuota `json:"quotas"`
	}
	if err := json.Unmarshal(qb, &q); err != nil {
		t.Fatalf("bad quota json: %v", err)
	}
	if len(q.Quotas) != 1 || q.Quotas[0].Source != "commandcode" || len(q.Quotas[0].Accounts) != 1 || !q.Quotas[0].Accounts[0].Limited {
		t.Fatalf("quota report: %+v", q.Quotas)
	}
}
