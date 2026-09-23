package server

// GET /v1/usage — upstream provider usage for the calling API key, as JSON.
// General (provider-agnostic) surface for clients like pi-sub: pass the model
// id's routing prefix as ?provider= to scope the report to that upstream
// (zai, command-code, deepseek); omit it for a worst-case aggregate. Gated by
// the key's usage permission (config Key.Usage, default ON).

import (
	"encoding/json"
	"math"
	"net/http"
	"sort"
	"strings"

	"github.com/bacnh85/yardmaster/internal/config"
	"github.com/bacnh85/yardmaster/internal/provider"
)

type usageWindow struct {
	RemainingPct int   `json:"remaining_pct"` // 0-100
	ResetAt      int64 `json:"reset_at,omitempty"`
}

type usageCredits struct {
	Currency string  `json:"currency"`
	Balance  float64 `json:"balance"`
}

type usageReport struct {
	Provider string                  `json:"provider"`          // routing prefix/slug; "" = aggregate
	Windows  map[string]*usageWindow `json:"windows,omitempty"` // "session" | "weekly"
	// Credits are deterministic, never config-order-arbitrary: scoped reports
	// use the named source's lowest-balance account; aggregates use the source
	// that contributed the worst session window, else the single source with a
	// balance, else nothing (multiple mixed balances are omitted, not guessed).
	Credits   *usageCredits `json:"credits,omitempty"`
	Providers []string      `json:"providers"` // slugs of all reportable upstreams (allow-scoped)
}

// slugFor maps a quota source to its public routing prefix.
func slugFor(source string) string {
	if source == "commandcode" {
		return "command-code"
	}
	return source // zai, deepseek
}

// sourceForSlug maps a ?provider= slug back to a quota source ("" = unknown).
func sourceForSlug(slug string) string {
	switch slug {
	case "zai":
		return "zai"
	case "cmd", "command-code", "commandcode":
		return "commandcode"
	case "ds", "deepseek":
		return "deepseek"
	case "ocg", "opencode-go", "opencode":
		return "opencode"
	case "ol", "ollama":
		return "ollama"
	}
	return ""
}

// windowUsedPct normalizes a QuotaWindow to its used percentage (0-100).
func windowUsedPct(w *QuotaWindow) int {
	var pct float64
	if w.Unit == "pct" {
		pct = w.Used
	} else if w.Cap > 0 {
		pct = 100 * w.Used / w.Cap
	}
	if pct < 0 {
		pct = 0
	}
	if pct > 100 {
		pct = 100
	}
	return int(math.Round(pct))
}

func toUsageWindow(w *QuotaWindow) *usageWindow {
	return &usageWindow{RemainingPct: 100 - windowUsedPct(w), ResetAt: w.ResetAt}
}

// sourceUsage projects one quota source: worst account per window (highest
// used), lowest-balance account for credits (worst case, matching the windows
// rule; accounts within a source virtually never mix currencies).
func sourceUsage(pq *ProviderQuota, providers []string) usageReport {
	out := usageReport{Provider: slugFor(pq.Source), Providers: providers}
	var session, weekly, monthly *QuotaWindow
	for i := range pq.Accounts {
		a := pq.Accounts[i]
		if a.FiveHour != nil && (session == nil || windowUsedPct(a.FiveHour) > windowUsedPct(session)) {
			session = a.FiveHour
		}
		if a.Weekly != nil && (weekly == nil || windowUsedPct(a.Weekly) > windowUsedPct(weekly)) {
			weekly = a.Weekly
		}
		if a.Monthly != nil && (monthly == nil || windowUsedPct(a.Monthly) > windowUsedPct(monthly)) {
			monthly = a.Monthly
		}
		if a.MonthlyCredits != nil && (out.Credits == nil || *a.MonthlyCredits < out.Credits.Balance) {
			cur := a.Currency
			if cur == "" {
				cur = "USD"
			}
			out.Credits = &usageCredits{Currency: cur, Balance: *a.MonthlyCredits}
		}
	}
	if session != nil || weekly != nil || monthly != nil {
		out.Windows = map[string]*usageWindow{}
		if session != nil {
			out.Windows["session"] = toUsageWindow(session)
		}
		if weekly != nil {
			out.Windows["weekly"] = toUsageWindow(weekly)
		}
		if monthly != nil {
			out.Windows["monthly"] = toUsageWindow(monthly)
		}
	}
	return out
}

// aggregateUsage: worst remaining window per type across all sources. Credits
// are deterministic: the source that contributed the worst session window wins;
// when that source has none but exactly one source carries a balance, that one
// is used; mixed/multiple balances are omitted rather than guessed.
func aggregateUsage(rep []ProviderQuota, providers []string) usageReport {
	out := usageReport{Providers: providers}
	// slug order makes ties independent of provider config order
	srcs := make([]ProviderQuota, len(rep))
	copy(srcs, rep)
	sort.Slice(srcs, func(i, j int) bool { return slugFor(srcs[i].Source) < slugFor(srcs[j].Source) })

	var worstSession *usageWindow
	var worstSessionCredits *usageCredits
	var creditsSources []*usageCredits
	for i := range srcs {
		su := sourceUsage(&srcs[i], providers)
		for _, kind := range []string{"session", "weekly", "monthly"} {
			w := su.Windows[kind]
			if w == nil {
				continue
			}
			if out.Windows == nil {
				out.Windows = map[string]*usageWindow{}
			}
			if cur := out.Windows[kind]; cur == nil || w.RemainingPct < cur.RemainingPct {
				out.Windows[kind] = w
			}
		}
		if su.Credits != nil {
			creditsSources = append(creditsSources, su.Credits)
		}
		if w := su.Windows["session"]; w != nil && (worstSession == nil || w.RemainingPct < worstSession.RemainingPct) {
			worstSession = w
			worstSessionCredits = su.Credits
		}
	}
	switch {
	case worstSessionCredits != nil:
		out.Credits = worstSessionCredits
	case len(creditsSources) == 1:
		out.Credits = creditsSources[0]
	}
	return out
}

// providerReachable reports whether the key could route ANY request to p —
// the same matching semantics as Registry.Resolve: routes first (the first
// route whose Match pattern can hit a model the key is allowed must name p in
// its chain), then bare or prefixed forms of p's curated models. A provider
// without a model list catches every bare id, so it is reachable only by
// patterns that can name a bare id — "*" or anything not fully claimed by a
// known prefix ("zai/*" forces all matches into zai's namespace; "cm*" does
// not). Route checks are additive-only: they can make an upstream visible,
// never hide one the legacy model rules already admit (Resolve applies the
// same provs filter either way, so anything routable through a route is also
// routable when the provider list it feeds contains p by direct rules).
func (s *Server) providerReachable(cfg *config.Config, p *config.Provider, allow []string) bool {
	if p.Disabled {
		return false
	}
	// Routes are matched BEFORE prefix resolution and their chain replaces the
	// provider selection wholesale (the wildcard-curation filter still applies
	// per provider). If the first matching route can admit any allowed model
	// and names p, the key can genuinely hit p through that chain.
	for _, rt := range cfg.Routes {
		if !routeAdmitsAllow(rt.Match, allow) {
			continue
		}
		for _, name := range rt.Chain {
			if name == p.Name {
				return true
			}
		}
	}
	if len(p.Models) == 0 {
		for _, pat := range allow {
			if patternEscapesPrefixes(cfg, pat) {
				return true
			}
		}
		return false
	}
	check := func(bare string) bool {
		return provider.KeyAllowed(allow, bare) || (p.Prefix != "" && provider.KeyAllowed(allow, p.Prefix+"/"+bare))
	}
	for _, m := range p.Models {
		if check(m) {
			return true
		}
	}
	for bare := range p.ModelMap {
		if check(bare) {
			return true
		}
	}
	return false
}

// routeAdmitsAllow mirrors Resolve's model filter for one route: the route
// fires when ANY model id it can match is admitted by the key's allow list.
// Match patterns are exact ids or suffix-"*" prefixes (matchRoute), so the
// set overlap test is: prefix candidates by pattern length (a longer pattern
// cannot match inside a shorter one's extent), then prefix-match either way.
func routeAdmitsAllow(match string, allow []string) bool {
	for _, pat := range allow {
		a, b := match, pat
		if len(b) > len(a) {
			a, b = b, a
		}
		if strings.HasPrefix(a, b) {
			return true
		}
	}
	return false
}

// patternEscapesPrefixes reports whether pat can match a bare (non-prefixed)
// model id — the only ids a wildcard provider can catch.
func patternEscapesPrefixes(cfg *config.Config, pat string) bool {
	if !strings.HasSuffix(pat, "*") {
		_, _, prefixed := provider.SplitPrefix(cfg, pat)
		return !prefixed
	}
	pre := strings.TrimSuffix(pat, "*")
	if pre == "" {
		return true
	}
	seg := pre
	if i := strings.IndexByte(seg, '/'); i >= 0 {
		seg = seg[:i]
	}
	for _, x := range cfg.Providers {
		if x.Prefix == seg && strings.HasSuffix(pre, "/") {
			return false // "zai/*": every match is a prefixed zai id
		}
	}
	return true // "cm*" matches bare "cmfoo" too
}

// scopedQuotaReport narrows the cached report to quota sources the key can
// actually route to (config Key.Allow) — restricted keys must not read the
// balances or windows of upstreams they can never hit.
func (s *Server) scopedQuotaReport(allow []string) []ProviderQuota {
	rep := s.cachedQuotaReport(false)
	if len(allow) == 0 {
		allow = []string{"*"}
	}
	cfg := s.Proxy.Reg.Config()
	provs := map[string]*config.Provider{}
	for _, p := range cfg.Providers {
		provs[p.Name] = p
	}
	out := make([]ProviderQuota, 0, len(rep))
	for i := range rep {
		for _, name := range rep[i].Providers {
			if p := provs[name]; p != nil && s.providerReachable(cfg, p, allow) {
				out = append(out, rep[i])
				break
			}
		}
	}
	return out
}

func (s *Server) handleUsage(w http.ResponseWriter, r *http.Request) {
	k := inboundKey(r)
	if !k.UsageAllowed() {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusForbidden)
		json.NewEncoder(w).Encode(map[string]string{"error": "usage permission revoked for this key"})
		return
	}
	rep := s.scopedQuotaReport(k.Allow)
	providers := make([]string, 0, len(rep))
	for i := range rep {
		providers = append(providers, slugFor(rep[i].Source))
	}

	var out usageReport
	if slug := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("provider"))); slug != "" {
		src := sourceForSlug(slug)
		var pq *ProviderQuota
		for i := range rep {
			if rep[i].Source == src {
				pq = &rep[i]
				break
			}
		}
		if pq == nil {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusNotFound)
			json.NewEncoder(w).Encode(map[string]string{"error": "unknown provider: " + slug})
			return
		}
		out = sourceUsage(pq, providers)
	} else {
		out = aggregateUsage(rep, providers)
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(out)
}
