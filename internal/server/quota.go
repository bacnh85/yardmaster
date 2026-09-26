package server

// Provider subscription quota: live usage windows (5-hour / weekly) plus the
// monthly credit balance, fetched per account (API key) from each provider's
// billing endpoint. Served on GET /admin/api/quota for the dashboard; raw keys
// never appear in the response.

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
)

// QuotaWindow is one rolling usage window. Used/Cap are USD; ResetAt is epoch ms.
// Unit "pct" marks a percent-of-quota window (z.ai coding-plan credits):
// Used carries the raw percentage, Cap is 100.
type QuotaWindow struct {
	Used     float64 `json:"used"`
	Cap      float64 `json:"cap"`
	Unit     string  `json:"unit,omitempty"` // "" = USD; "pct" = percent of window quota
	Exceeded bool    `json:"exceeded,omitempty"`
	ResetAt  int64   `json:"reset_at,omitempty"`
}

// QuotaAccount is one account (one API key) with its usage windows.
type QuotaAccount struct {
	Label          string       `json:"label"`
	Suffix         string       `json:"suffix"`
	FiveHour       *QuotaWindow `json:"five_hour,omitempty"`
	Weekly         *QuotaWindow `json:"weekly,omitempty"`
	Monthly        *QuotaWindow `json:"monthly,omitempty"`         // explicit percent window (OpenCode Go); cc/DeepSeek derive the column instead
	MonthlyCredits *float64     `json:"monthly_credits,omitempty"` // USD remaining, or balance per Currency
	Currency       string       `json:"currency,omitempty"`        // balance currency when not USD (e.g. DeepSeek CNY accounts)
	MonthlyTotal   float64      `json:"monthly_total,omitempty"`   // plan allowance; 0 = unknown plan
	Limited        bool         `json:"limited,omitempty"`
	Err            string       `json:"err,omitempty"`
}

// ProviderQuota is the usage report for one quota source: every config
// provider served by it (e.g. cmdcode + cmdcode-claude) and each distinct
// account with live windows.
type ProviderQuota struct {
	Source    string         `json:"source"`
	Providers []string       `json:"providers"`
	Accounts  []QuotaAccount `json:"accounts"`
	FetchedAt int64          `json:"fetched_at"` // unix ms
}

// quotaSource reports which billing API a provider uses ("" = none). Matched
// by upstream host so custom providers pointing at the same gateway count too.
func quotaSource(baseURL string) string {
	u, err := url.Parse(baseURL)
	if err != nil {
		return ""
	}
	switch strings.ToLower(u.Host) {
	case "api.commandcode.ai":
		return "commandcode"
	case "api.deepseek.com":
		return "deepseek"
	case "api.z.ai", "zcode.z.ai":
		return "zai"
	case "opencode.ai":
		if strings.Contains(strings.ToLower(u.Path), "/go/") {
			return "opencode" // zen/go/v1 = Go subscription; plain zen/v1 (credits) has no usage API
		}
	case "openrouter.ai":
		return "openrouter"
	case "ollama.com":
		return "ollama"
	}
	return ""
}

// billingPaths map a quota source to its usage endpoint, appended to the
// provider origin.
var billingPaths = map[string]string{
	"commandcode": "/alpha/billing/credits",
	"deepseek":    "/user/balance",
	"zai":         "/api/monitor/usage/quota/limit",
	"opencode":    "/zen/go/v1/usage",
	"openrouter":  "/api/v1/credits",
	"ollama":      "/api/usage",
}

// quotaClient fetches usage windows; var so tests can redirect upstream.
var quotaClient = &http.Client{Timeout: 7 * time.Second}

// ccCredits mirrors Command Code's /alpha/billing/credits response
// (shape live-verified 2026-09-20; same endpoint pi-sub reads).
type ccCredits struct {
	Credits struct {
		MonthlyCredits float64 `json:"monthlyCredits"`
	} `json:"credits"`
	WindowLimits struct {
		Limited  bool      `json:"limited"`
		FiveHour *ccWindow `json:"fiveHour"`
		Weekly   *ccWindow `json:"weekly"`
	} `json:"windowLimits"`
}

type ccWindow struct {
	Used     float64 `json:"used"`
	Cap      float64 `json:"cap"`
	Exceeded bool    `json:"exceeded"`
	ResetAt  int64   `json:"resetAt"` // epoch ms
}

func toQuotaWindow(w *ccWindow) *QuotaWindow {
	if w == nil || w.Cap <= 0 {
		return nil
	}
	return &QuotaWindow{Used: w.Used, Cap: w.Cap, Exceeded: w.Exceeded, ResetAt: w.ResetAt}
}

// ccPlanMonthly maps a plan's (5h cap, weekly cap) to its monthly credit
// allowance, from the documented limits table
// (https://commandcode.ai/docs/resources/usage-limits). The caps returned by
// the API identify the plan; no billing-cycle reset date is exposed by the API
// (monthly renews on the account's billing date, Studio session auth only).
var ccPlanMonthly = map[[2]float64]float64{
	{3, 6}:    10,  // Go
	{14, 35}:  70,  // GOAT
	{16, 40}:  80,  // Pro
	{45, 90}:  150, // Max 10×
	{90, 180}: 300, // Max 20×
	{12, 24}:  40,  // Team Pro
}

// ccMonthlyTotal returns the plan's monthly credit allowance identified by its
// window caps (0 = unknown caps — leave the total unset).
func ccMonthlyTotal(fiveHourCap, weeklyCap float64) float64 {
	return ccPlanMonthly[[2]float64{fiveHourCap, weeklyCap}]
}

// fetchCommandCodeQuota reads live usage windows for one provider key.
func fetchCommandCodeQuota(ctx context.Context, quotaURL, key string) (*QuotaAccount, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, quotaURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Authorization", "Bearer "+key)
	resp, err := quotaClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 256))
		return nil, fmt.Errorf("HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(b)))
	}
	var body ccCredits
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&body); err != nil {
		return nil, err
	}
	mc := body.Credits.MonthlyCredits
	acct := &QuotaAccount{
		FiveHour:       toQuotaWindow(body.WindowLimits.FiveHour),
		Weekly:         toQuotaWindow(body.WindowLimits.Weekly),
		MonthlyCredits: &mc,
		Limited:        body.WindowLimits.Limited,
	}
	if acct.FiveHour != nil && acct.Weekly != nil {
		acct.MonthlyTotal = ccMonthlyTotal(acct.FiveHour.Cap, acct.Weekly.Cap)
	}
	return acct, nil
}

// fetchDeepSeekQuota reads the prepaid balance for one DeepSeek API key via
// GET /user/balance (https://api-docs.deepseek.com/api/get-user-balance/).
// DeepSeek exposes no usage windows — only the remaining balance (string
// amounts, USD or CNY).
type dsBalance struct {
	IsAvailable  bool `json:"is_available"`
	BalanceInfos []struct {
		Currency     string `json:"currency"`
		TotalBalance string `json:"total_balance"`
	} `json:"balance_infos"`
}

func fetchDeepSeekQuota(ctx context.Context, quotaURL, key string) (*QuotaAccount, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, quotaURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Authorization", "Bearer "+key)
	resp, err := quotaClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 256))
		return nil, fmt.Errorf("HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(b)))
	}
	var body dsBalance
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&body); err != nil {
		return nil, err
	}
	if len(body.BalanceInfos) == 0 {
		return nil, fmt.Errorf("no balance entries")
	}
	info := body.BalanceInfos[0]
	for _, b := range body.BalanceInfos {
		if b.Currency == "USD" {
			info = b
			break
		}
	}
	total, err := strconv.ParseFloat(strings.TrimSpace(info.TotalBalance), 64)
	if err != nil {
		return nil, fmt.Errorf("balance %q: %w", info.TotalBalance, err)
	}
	return &QuotaAccount{
		MonthlyCredits: &total,
		Currency:       info.Currency,
		Limited:        !body.IsAvailable,
	}, nil
}

// zaiQuota mirrors GET https://api.z.ai/api/monitor/usage/quota/limit
// (undocumented; live-verified 2026-09-21 against a lite-plan account:
// `data.limits[]` with TOKENS_LIMIT unit=3 (5h) + TIME_LIMIT unit=5 (MCP
// monthly); AgentDeck #348 also observed a weekly window (unit=6) and wrote
// the array as `windows` — both spellings are accepted here). Unknown/absent
// fields stay unknown — never fabricated; PAYG keys have no windows at all.
type zaiQuotaResp struct {
	Data struct {
		Level   string      `json:"level"` // max | pro | lite
		Limits  []zaiWindow `json:"limits"`
		Windows []zaiWindow `json:"windows"` // alternate spelling seen in the wild
	} `json:"data"`
}

type zaiWindow struct {
	Type       string          `json:"type"`
	Unit       int             `json:"unit"`
	Number     int             `json:"number"`
	Percentage float64         `json:"percentage"`
	NextReset  int64           `json:"nextResetTime"` // epoch ms
	Usage      json.RawMessage `json:"usage"`         // object {percentage} on some windows, bare number on others
}

// usagePct extracts the usage percentage from either shape.
func (w zaiWindow) usagePct() float64 {
	var obj struct {
		Percentage float64 `json:"percentage"`
	}
	if err := json.Unmarshal(w.Usage, &obj); err == nil && obj.Percentage != 0 {
		return obj.Percentage
	}
	var num float64
	if err := json.Unmarshal(w.Usage, &num); err == nil {
		return num
	}
	return 0
}

func (r zaiQuotaResp) windows() []zaiWindow {
	if len(r.Data.Limits) > 0 {
		return r.Data.Limits
	}
	return r.Data.Windows
}

// fetchZaiQuota reads the coding-plan usage windows for one z.ai key. The
// monitor endpoint is served on api.z.ai only (ultra-route providers included).
func fetchZaiQuota(ctx context.Context, quotaURL, key string) (*QuotaAccount, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, quotaURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Authorization", key) // raw key; Bearer also accepted upstream
	resp, err := quotaClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 256))
		return nil, fmt.Errorf("HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(b)))
	}
	var body zaiQuotaResp
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&body); err != nil {
		return nil, err
	}
	acct := &QuotaAccount{}
	for _, w := range body.windows() {
		pct := w.Percentage
		if pct == 0 {
			pct = w.usagePct()
		}
		if pct <= 0 && w.NextReset == 0 {
			continue // no real window content
		}
		win := &QuotaWindow{Used: pct, Cap: 100, Unit: "pct", Exceeded: pct >= 100, ResetAt: w.NextReset}
		switch w.Unit {
		case 3:
			if acct.FiveHour == nil {
				acct.FiveHour = win
			}
		case 6:
			if acct.Weekly == nil {
				acct.Weekly = win
			}
		} // unit 5 (MCP monthly) intentionally ignored
	}
	return acct, nil // PAYG key: no windows — empty account, never a fake 0%
}

// ocUsage mirrors GET https://opencode.ai/zen/go/v1/usage (live-verified
// 2026-09-22 with a Go API key; no workspace id or session header needed).
// Limits are per-model monthly dollar amounts (5h = 20%, weekly = 50%,
// monthly = 100% of it — https://opencode.ai/docs/go/#usage-limits); the API
// blends them into one percent per window. resetsAt is RFC3339.
type ocUsage struct {
	Usage struct {
		Rolling ocWindow `json:"rolling"`
		Weekly  ocWindow `json:"weekly"`
		Monthly ocWindow `json:"monthly"`
	} `json:"usage"`
}

type ocWindow struct {
	Status   string  `json:"status"`
	Percent  float64 `json:"percent"`
	ResetsAt string  `json:"resetsAt"`
}

// ocPctWindow converts one usage window. Exceedance is percent-based — the
// status enum is undocumented (only "ok" observed), so it is never guessed.
// A window the API did not return (no percent, no reset) maps to nil —
// same invariant as the other fetchers: never a fabricated 0%.
func ocPctWindow(w ocWindow) *QuotaWindow {
	if w.Percent == 0 && w.ResetsAt == "" {
		return nil
	}
	reset := int64(0)
	if t, err := time.Parse(time.RFC3339, w.ResetsAt); err == nil {
		reset = t.UnixMilli()
	}
	return &QuotaWindow{Used: w.Percent, Cap: 100, Unit: "pct", Exceeded: w.Percent >= 100, ResetAt: reset}
}

// fetchOpencodeQuota reads the Go subscription usage windows for one key.
func fetchOpencodeQuota(ctx context.Context, quotaURL, key string) (*QuotaAccount, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, quotaURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Authorization", "Bearer "+key)
	resp, err := quotaClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 256))
		return nil, fmt.Errorf("HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(b)))
	}
	var body ocUsage
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&body); err != nil {
		return nil, err
	}
	return &QuotaAccount{
		FiveHour: ocPctWindow(body.Usage.Rolling),
		Weekly:   ocPctWindow(body.Usage.Weekly),
		Monthly:  ocPctWindow(body.Usage.Monthly),
	}, nil
}

// fetchOpenRouterQuota reads the prepaid credit balance for one OpenRouter
// API key via GET /api/v1/credits (documented shape; unauthenticated calls
// 401, so the parser is exercised only with a real key). total_credits is
// the lifetime amount purchased; remaining = total_credits - total_usage.
// No total_credits (or a zero total) means nothing usable to display — the
// account is returned empty rather than fabricated as 0.
type orCredits struct {
	Data struct {
		TotalCredits float64 `json:"total_credits"`
		TotalUsage   float64 `json:"total_usage"`
	} `json:"data"`
}

func fetchOpenRouterQuota(ctx context.Context, quotaURL, key string) (*QuotaAccount, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, quotaURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Authorization", "Bearer "+key)
	resp, err := quotaClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 256))
		return nil, fmt.Errorf("HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(b)))
	}
	var body orCredits
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&body); err != nil {
		return nil, err
	}
	acct := &QuotaAccount{}
	if body.Data.TotalCredits > 0 {
		remain := body.Data.TotalCredits - body.Data.TotalUsage
		if remain < 0 {
			remain = 0 // exhausted prepaid account: $0 left, not a negative claim
		}
		acct.MonthlyCredits = &remain
		acct.MonthlyTotal = body.Data.TotalCredits
	}
	return acct, nil
}

// olUsage mirrors GET https://ollama.com/api/usage (live-verified 2026-09-23
// with a Free-tier key: limits={monthly:{usage:0.67,models:[...]}};
// monopi PR #379 fixtures show session/weekly on other tiers). *.usage is a
// consumed FRACTION (0..1) — the dashboard renders it as a pct window.
// Malformed windows (bare numbers, non-numeric string usages) are dropped
// per-window, never a payload failure or a fabricated 0%. No reset
// timestamps and no plan allowance are exposed — never fabricated.
type olUsage struct {
	Activity struct {
		Cost flexString `json:"cost"`
	} `json:"activity"`
	Limits map[string]json.RawMessage `json:"limits"`
}

// flexFloat accepts a JSON number or numeric string; anything else errors and
// drops just that window (monopi's fixtures show "not-a-number" in the wild).
// ok distinguishes "decoded" from "absent/malformed" so a genuine 0 usage
// window renders as 0% instead of being dropped as no-data.
type flexFloat struct {
	v  float64
	ok bool
}

func (f *flexFloat) UnmarshalJSON(b []byte) error {
	s := strings.Trim(string(b), `"`)
	v, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return fmt.Errorf("not a number: %s", s)
	}
	f.v, f.ok = v, true
	return nil
}

// fetchOllamaQuota reads Ollama Cloud usage windows for one key: session
// (5h) → FiveHour, weekly (7d) → Weekly, both as pct-of-allowance (OpenCode
// convention). A missing or malformed window stays nil — never a fake 0%.
func fetchOllamaQuota(ctx context.Context, quotaURL, key string) (*QuotaAccount, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, quotaURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Authorization", "Bearer "+key)
	resp, err := quotaClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 256))
		return nil, fmt.Errorf("HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(b)))
	}
	var body olUsage
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&body); err != nil {
		return nil, fmt.Errorf("decode usage: %w", err)
	}
	olPct := func(raw json.RawMessage) *QuotaWindow {
		var w struct {
			Usage flexFloat `json:"usage"`
		}
		// ok=false covers a malformed/absent usage (and literal null); a genuine
		// {usage: 0} fresh window must render as 0%, never as no-data
		if json.Unmarshal(raw, &w) != nil || !w.Usage.ok {
			return nil
		}
		pct := float64(w.Usage.v) * 100
		return &QuotaWindow{Used: pct, Cap: 100, Unit: "pct", Exceeded: pct >= 100}
	}
	return &QuotaAccount{
		FiveHour: olPct(body.Limits["session"]),
		Weekly:   olPct(body.Limits["weekly"]),
		Monthly:  olPct(body.Limits["monthly"]), // the only window on Free tier
	}, nil
}

const quotaTTL = 60 * time.Second

var (
	quotaMu       sync.Mutex
	quotaReport   []ProviderQuota
	quotaUntil    time.Time
	quotaInFlight chan struct{} // non-nil while a report build is running
)

// handleQuota serves GET /admin/api/quota — one group per quota source, one
// account row per distinct key. ?refresh=1 bypasses the 60s cache.
func (s *Server) handleQuota(w http.ResponseWriter, r *http.Request) {
	rep := s.cachedQuotaReport(r.URL.Query().Get("refresh") == "1")
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{"quotas": rep})
}

// invalidateQuotaReport drops the shared 60s quota cache after a key mutation
// (add/rotate/delete) — without it a deleted connection's usage row lingers up
// to a minute. A build already in flight may re-store pre-delete data once:
// worst case one extra 60s window.
func invalidateQuotaReport() {
	quotaMu.Lock()
	quotaReport, quotaUntil = nil, time.Time{}
	quotaMu.Unlock()
}

// cachedQuotaReport returns the shared 60s-cached upstream quota report,
// rebuilding it (detached from any request context — an aborted caller must
// never write "context canceled" into the shared cache) when stale. Concurrent
// callers wait on the single in-flight build instead of serializing upstream I/O.
func (s *Server) cachedQuotaReport(refresh bool) []ProviderQuota {
	for {
		if !refresh {
			quotaMu.Lock()
			rep, fresh := quotaReport, time.Now().Before(quotaUntil)
			quotaMu.Unlock()
			if rep != nil && fresh {
				return rep
			}
		}
		quotaMu.Lock()
		if quotaInFlight == nil {
			break // no build running — we are the builder
		}
		ch := quotaInFlight
		quotaMu.Unlock()
		<-ch // a build is already running: wait, then re-check the cache
	}

	inFlight := make(chan struct{})
	quotaInFlight = inFlight
	quotaMu.Unlock()

	// never leave waiters stuck if the build panics: net/http recovers the
	// goroutine, but waiters block on <-inFlight forever unless it's closed
	defer func() {
		quotaMu.Lock()
		if quotaInFlight == inFlight {
			quotaInFlight = nil
			close(inFlight)
		}
		quotaMu.Unlock()
	}()

	rep := s.buildQuotaReport()

	quotaMu.Lock()
	quotaReport = rep
	quotaUntil = time.Now().Add(quotaTTL)
	quotaMu.Unlock()
	return rep
}

// buildQuotaReport fetches every quota source's accounts. Called WITHOUT the
// lock held and detached from any request context (the 7s client timeout
// bounds each fetch).
func (s *Server) buildQuotaReport() []ProviderQuota {
	type groupT struct {
		pq      *ProviderQuota
		fetched map[string]bool // key text → already fetched (wire entries share accounts)
	}
	groups := map[string]*groupT{}
	var order []string
	for _, p := range s.Proxy.Reg.Config().Providers {
		src := quotaSource(p.BaseURL)
		if src == "" || p.Disabled || len(p.Auth.Keys) == 0 {
			continue
		}
		g := groups[src]
		if g == nil {
			g = &groupT{pq: &ProviderQuota{Source: src, Providers: []string{}, Accounts: []QuotaAccount{}}, fetched: map[string]bool{}}
			groups[src] = g
			order = append(order, src)
		}
		g.pq.Providers = append(g.pq.Providers, p.Name)
		u, err := url.Parse(p.BaseURL)
		if err != nil || u.Host == "" {
			continue
		}
		origin := u.Scheme + "://" + u.Host + billingPaths[src]
		if src == "zai" {
			origin = "https://api.z.ai" + billingPaths["zai"] // monitor endpoint lives on api.z.ai, even for ultra-route providers
		}
		for i, key := range p.Auth.Keys {
			if i < len(p.Auth.KeyDisabled) && p.Auth.KeyDisabled[i] {
				continue // disabled connection: dispatches no traffic → no usage row
			}
			if g.fetched[key] {
				continue
			}
			g.fetched[key] = true
			var acct *QuotaAccount
			var err error
			switch src {
			case "deepseek":
				acct, err = fetchDeepSeekQuota(context.Background(), origin, key)
			case "zai":
				acct, err = fetchZaiQuota(context.Background(), origin, key)
			case "opencode":
				acct, err = fetchOpencodeQuota(context.Background(), origin, key)
			case "openrouter":
				acct, err = fetchOpenRouterQuota(context.Background(), origin, key)
			case "ollama":
				acct, err = fetchOllamaQuota(context.Background(), origin, key)
			default:
				acct, err = fetchCommandCodeQuota(context.Background(), origin, key)
			}
			if acct == nil {
				acct = &QuotaAccount{}
			}
			acct.Label = p.Auth.KeyLabel(i)
			acct.Suffix = suffix(key)
			if err != nil {
				acct.Err = err.Error()
			}
			g.pq.Accounts = append(g.pq.Accounts, *acct)
		}
	}

	now := time.Now()
	report := make([]ProviderQuota, 0, len(order))
	for _, src := range order {
		g := groups[src]
		if len(g.pq.Accounts) == 0 {
			continue // every connection disabled — nothing to show
		}
		g.pq.FetchedAt = now.UnixMilli()
		report = append(report, *g.pq)
	}
	return report
}
