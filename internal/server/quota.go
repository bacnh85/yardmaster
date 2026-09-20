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
	"strings"
	"sync"
	"time"
)

// QuotaWindow is one rolling usage window. Used/Cap are USD; ResetAt is epoch ms.
type QuotaWindow struct {
	Used     float64 `json:"used"`
	Cap      float64 `json:"cap"`
	Exceeded bool    `json:"exceeded,omitempty"`
	ResetAt  int64   `json:"reset_at,omitempty"`
}

// QuotaAccount is one account (one API key) with its usage windows.
type QuotaAccount struct {
	Label          string       `json:"label"`
	Suffix         string       `json:"suffix"`
	FiveHour       *QuotaWindow `json:"five_hour,omitempty"`
	Weekly         *QuotaWindow `json:"weekly,omitempty"`
	MonthlyCredits *float64     `json:"monthly_credits,omitempty"` // USD remaining
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
	if u, err := url.Parse(baseURL); err == nil && strings.EqualFold(u.Host, "api.commandcode.ai") {
		return "commandcode"
	}
	return ""
}

// billingPath is appended to the provider origin to reach its usage endpoint.
const billingPath = "/alpha/billing/credits"

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

const quotaTTL = 60 * time.Second

var (
	quotaMu       sync.Mutex
	quotaReport   []ProviderQuota
	quotaUntil    time.Time
	quotaInFlight chan struct{} // non-nil while a report build is running
)

// handleQuota serves GET /admin/api/quota — one group per quota source, one
// account row per distinct key. ?refresh=1 bypasses the 60s cache.
//
// Builds run detached from the request context (context.Background) so an
// aborted dashboard request can never write "context canceled" errors into
// the shared 60s cache, and concurrent viewers wait on the single in-flight
// build instead of serializing behind each other's upstream I/O.
func (s *Server) handleQuota(w http.ResponseWriter, r *http.Request) {
	writeJSON := func(v any) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(v)
	}

	for {
		if r.URL.Query().Get("refresh") != "1" {
			quotaMu.Lock()
			rep, fresh := quotaReport, time.Now().Before(quotaUntil)
			quotaMu.Unlock()
			if rep != nil && fresh {
				writeJSON(map[string]any{"quotas": rep})
				return
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

	writeJSON(map[string]any{"quotas": rep})
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
		origin := u.Scheme + "://" + u.Host + billingPath
		for i, key := range p.Auth.Keys {
			if g.fetched[key] {
				continue
			}
			g.fetched[key] = true
			acct, err := fetchCommandCodeQuota(context.Background(), origin, key)
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
		g.pq.FetchedAt = now.UnixMilli()
		report = append(report, *g.pq)
	}
	return report
}
