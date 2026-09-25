// Package provider holds the provider registry and model→provider routing.
package provider

import (
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/bacnh85/yardmaster/internal/auth"
	"github.com/bacnh85/yardmaster/internal/config"
)

// Target is one concrete dispatch destination: a provider with a specific key.
type Target struct {
	Provider *config.Provider
	APIKey   string
	AuthType string // static | oauth
	AcctName string // oauth account name (empty for static)
}

// Registry resolves models to ordered dispatch targets and owns per-key limiters.
type Registry struct {
	mu       sync.RWMutex
	cfg      *config.Config
	limiters sync.Map // "provider:key" -> *auth.Limiter
	wrr      sync.Map // route match -> *uint64 (weighted-rr pick counter)
	rr       sync.Map // provider name -> *uint64 (round_robin key counter)
}

func New(cfg *config.Config) *Registry {
	return &Registry{cfg: cfg}
}

// Reload swaps the config atomically (hot reload).
func (r *Registry) Reload(cfg *config.Config) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.cfg = cfg
}

func (r *Registry) Config() *config.Config {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.cfg
}

// matchRoute: "exact" or "prefix*".
func matchRoute(pattern, model string) bool {
	if strings.HasSuffix(pattern, "*") {
		return strings.HasPrefix(model, strings.TrimSuffix(pattern, "*"))
	}
	return pattern == model
}

// KeyAllowed reports whether any pattern admits the model id — the same
// matcher Registry.Resolve applies to a key's Allow list (exact or "prefix*").
func KeyAllowed(patterns []string, model string) bool {
	for _, p := range patterns {
		if matchRoute(p, model) {
			return true
		}
	}
	return false
}

// SplitPrefix splits "prefix/model" into its provider prefix and the bare
// upstream id. matched=false when the first segment is not a known prefix —
// natural ids containing "/" (openrouter-style) pass through untouched.
func SplitPrefix(cfg *config.Config, model string) (prefix, bare string, matched bool) {
	i := strings.IndexByte(model, '/')
	if i <= 0 || i == len(model)-1 {
		return "", model, false
	}
	p := model[:i]
	for _, x := range cfg.Providers {
		if x.Prefix == p {
			return p, model[i+1:], true
		}
	}
	return "", model, false
}

// Resolve returns the ordered dispatch targets for a model.
// First matching route wins; no route → any provider whose Models list
// contains the model (config order). A "prefix/model" request restricts
// candidates to providers carrying that prefix. Providers that can't serve
// the model (non-empty Models list without it) are skipped inside chains.
func (r *Registry) Resolve(model string, allow []string) []*Target {
	r.mu.RLock()
	defer r.mu.RUnlock()
	if !KeyAllowed(allow, model) {
		return nil
	}
	prefix, bare, prefixed := SplitPrefix(r.cfg, model)
	var provs []*config.Provider
	matched := false
	for _, rt := range r.cfg.Routes {
		if matchRoute(rt.Match, model) {
			var chainPos []int
			for i, name := range rt.Chain {
				for _, p := range r.cfg.Providers {
					if p.Name == name {
						provs = append(provs, p)
						chainPos = append(chainPos, i)
						break
					}
				}
			}
			// weighted-rr: pick the chain head proportionally, keep the rest as
			// fallbacks in chain order (OpenRouter's LB-then-fallback shape)
			eff := rt.Strategy // empty inherits the global routing default
			if eff == "" {
				eff = r.cfg.Routing.Strategy
			}
			if eff == "weighted-rr" && len(provs) > 1 {
				head := wrrPick(&r.wrr, rt.Match, rt.Weights, len(rt.Chain))
				for i, cp := range chainPos {
					if cp != head {
						continue
					}
					re := make([]*config.Provider, 0, len(provs))
					re = append(re, provs[i])
					re = append(re, provs[:i]...)
					re = append(re, provs[i+1:]...)
					provs = re
					break
				}
			}
			matched = true
			break
		}
	}
	if !matched {
		if prefixed {
			// prefixed requests must hit a curated model — an empty Models
			// list (wildcard) inside the group would silently wrong-wire it.
			// A provider whose prefix equals a vendor namespace of its own
			// natural ids (prefix "openrouter", curated "openrouter/auto")
			// also serves the FULL id bare: SplitPrefix already stripped the
			// segment, so match it back against the full model id too.
			for _, p := range r.cfg.Providers {
				if p.Prefix == prefix && (contains(p.Models, bare) || containsMap(p.ModelMap, bare) || contains(p.Models, model) || containsMap(p.ModelMap, model)) {
					provs = append(provs, p)
				}
			}
		} else {
			for _, p := range r.cfg.Providers {
				if len(p.Models) == 0 || contains(p.Models, model) {
					provs = append(provs, p)
				}
			}
		}
	}
	name := model // id this provider's Models list is checked against
	if prefixed {
		name = bare
	}
	var targets []*Target
	for _, p := range provs {
		if p.Disabled {
			continue
		}
		// full-id-curated natural id (prefix == vendor namespace): this
		// provider matched on the full model id, not the stripped bare —
		// decided per provider so a disabled full-id sibling can't flip the
		// match name for everyone sharing the prefix
		pname := name
		if prefixed && (contains(p.Models, model) || containsMap(p.ModelMap, model)) {
			pname = model
		}
		if len(p.Models) > 0 && !contains(p.Models, pname) && !containsMap(p.ModelMap, pname) {
			continue
		}
		switch p.Auth.Type {
		case "oauth":
			var accts []*config.OAuthAcct
			for _, a := range p.Auth.OAuth {
				if !a.Disabled {
					accts = append(accts, a)
				}
			}
			if effectiveRotation(p, r.cfg.Routing.Rotation) == "round_robin" {
				accts = rotate(accts, nextCount(&r.rr, p.Name))
			}
			for _, a := range accts {
				targets = append(targets, &Target{Provider: p, AuthType: "oauth", AcctName: a.Name})
			}
		default: // static
			// per-connection disable: a key flagged key_disabled never dispatches
			// (flags are index-aligned with the STORED keys — filter in one pass,
			// never mutate in place or the indexes shift out of alignment)
			keys := make([]string, 0, len(p.Auth.Keys))
			for i, k := range p.Auth.Keys {
				if i < len(p.Auth.KeyDisabled) && p.Auth.KeyDisabled[i] {
					continue
				}
				keys = append(keys, k)
			}
			if effectiveRotation(p, r.cfg.Routing.Rotation) == "round_robin" {
				keys = rotate(keys, nextCount(&r.rr, p.Name))
			}
			for _, k := range keys {
				targets = append(targets, &Target{Provider: p, APIKey: k, AuthType: "static"})
			}
		}
	}
	if len(targets) > 8 {
		targets = targets[:8] // ponytail: cap failover fan-out; more is a config smell
	}
	return targets
}

func contains(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

func containsMap(m map[string]string, s string) bool {
	if m == nil {
		return false
	}
	_, ok := m[s]
	return ok
}

// UpstreamModel maps a requested model to the provider's upstream id:
// strips the provider prefix, then applies model_map.
func UpstreamModel(p *config.Provider, model string) string {
	if p.Prefix != "" {
		if bare, ok := strings.CutPrefix(model, p.Prefix+"/"); ok {
			// a bare natural id whose full form is itself curated (e.g.
			// "openrouter/auto" under prefix "openrouter") must go upstream
			// whole — strip only when the full id isn't itself a served model
			if !contains(p.Models, model) && !containsMap(p.ModelMap, model) {
				model = bare
			}
		}
	}
	if p.ModelMap != nil {
		if v, ok := p.ModelMap[model]; ok {
			return v
		}
	}
	return model
}

// nextCount returns the request index (0-based) for key — shared per-route /
// per-provider selection counters.
func nextCount(m *sync.Map, key string) uint64 {
	var c *uint64
	if v, ok := m.Load(key); ok {
		c = v.(*uint64)
	} else {
		c = new(uint64)
		actual, _ := m.LoadOrStore(key, c)
		c = actual.(*uint64)
	}
	return atomic.AddUint64(c, 1) - 1
}

// wrrPick maps the route's request counter through cumulative weights:
// weights [3,1] over 2 chain entries → heads 0,0,0,1,0,0,0,1,… proportionally
// over time. Missing/short weights default to an equal spread over n entries.
func wrrPick(m *sync.Map, route string, weights []int, n int) int {
	if len(weights) != n {
		equal := make([]int, n)
		for i := range equal {
			equal[i] = 1
		}
		weights = equal
	}
	total := 0
	for _, w := range weights {
		total += w
	}
	if total <= 0 {
		return 0
	}
	x := nextCount(m, route) % uint64(total)
	for i, w := range weights {
		if x < uint64(w) {
			return i
		}
		x -= uint64(w)
	}
	return 0
}

// rotate shifts s left by n; s with fewer than 2 entries is unchanged.
func rotate[T any](s []T, n uint64) []T {
	if len(s) < 2 {
		return s
	}
	off := int(n % uint64(len(s)))
	out := make([]T, 0, len(s))
	out = append(out, s[off:]...)
	return append(out, s[:off]...)
}

// Limiter returns the per-provider-key dispatch limiter (nil = unthrottled).
func (r *Registry) Limiter(p *config.Provider, apiKey string) *auth.Limiter {
	if p.DispatchIntervalMS <= 0 {
		return nil
	}
	key := p.Name + ":" + apiKey
	if l, ok := r.limiters.Load(key); ok {
		return l.(*auth.Limiter)
	}
	l := auth.NewLimiter(time.Duration(p.DispatchIntervalMS) * time.Millisecond)
	actual, _ := r.limiters.LoadOrStore(key, l)
	return actual.(*auth.Limiter)
}

// Models returns the union of all advertised models (for /v1/models).
func (r *Registry) Models() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	seen := map[string]bool{}
	var out []string
	for _, rt := range r.cfg.Routes {
		if !seen[rt.Match] && rt.Match != "*" {
			seen[rt.Match] = true
			alias := strings.TrimSuffix(strings.TrimSuffix(rt.Match, "*"), "-")
			if alias != "" {
				out = append(out, alias)
			}
		}
	}
	for _, p := range r.cfg.Providers {
		if p.Disabled || p.Wire == "classifier" {
			continue // disabled providers and decision models advertise nothing
		}
		for _, m := range p.Models {
			for _, id := range advertised(p.Prefix, m) {
				if !seen[id] {
					seen[id] = true
					out = append(out, id)
				}
			}
		}
	}
	return out
}

// advertised lists the ids a provider exposes for one curated model. Prefixed
// providers expose only "prefix/model" — the bare id is ambiguous once two
// providers carry the same model (pi shows the upstream prefix instead).
func advertised(prefix, m string) []string {
	if prefix == "" {
		return []string{m}
	}
	return []string{prefix + "/" + m}
}

// effectiveRotation resolves the provider's key-rotation setting: an explicit
// provider rotation wins, then the global routing default, then "first".
func effectiveRotation(p *config.Provider, global string) string {
	if p.Rotation != "" {
		return p.Rotation
	}
	if global != "" {
		return global
	}
	return "first"
}
