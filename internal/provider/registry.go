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
	Provider      *config.Provider
	APIKey        string
	AuthType      string // static | oauth
	AcctName      string // oauth account name (empty for static)
	ModelOverride string // combo requests: the natural upstream id (empty = use requested)
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

// comboMatches returns the combo for "combo/<name>" requests, nil otherwise.
func (r *Registry) comboMatches(model string) *config.Combo {
	name, ok := strings.CutPrefix(model, "combo/")
	if !ok {
		return nil
	}
	for _, cb := range r.cfg.Combos {
		if cb.Name == name {
			return cb
		}
	}
	return nil
}

// comboClone is a shallow provider copy scoped to one combo member: auth
// narrowed to the member's selected key labels / account names (empty = all).
func comboClone(p *config.Provider, member *config.ComboMember) *config.Provider {
	if len(member.Keys) == 0 {
		return p
	}
	want := map[string]bool{}
	for _, k := range member.Keys {
		want[k] = true
	}
	cp := *p // shallow — downstream only reads
	cp.Auth.Keys = nil
	cp.Auth.KeyLabels = nil
	cp.Auth.KeyDisabled = nil
	for i, k := range p.Auth.Keys {
		if want[p.Auth.KeyLabel(i)] {
			cp.Auth.Keys = append(cp.Auth.Keys, k)
			cp.Auth.KeyLabels = append(cp.Auth.KeyLabels, p.Auth.KeyLabel(i))
			if i < len(p.Auth.KeyDisabled) {
				cp.Auth.KeyDisabled = append(cp.Auth.KeyDisabled, p.Auth.KeyDisabled[i])
			}
		}
	}
	for _, a := range p.Auth.OAuth {
		if want[a.Name] {
			cp.Auth.OAuth = append(cp.Auth.OAuth, a)
		}
	}
	return &cp
}

// comboMember is one resolved member: its provider scoped to the selected
// connections, plus the upstream model id to dispatch under.
type comboMember struct {
	p     *config.Provider
	model string
}

// comboMembers maps combo members to scoped providers: priority order by
// default; weighted-rr rotates the head proportionally (same shape as routes).
func (r *Registry) comboMembers(cb *config.Combo) []comboMember {
	members := cb.Members
	eff := cb.Strategy
	if eff == "" {
		eff = r.cfg.Routing.Strategy
	}
	if eff == "weighted-rr" && len(members) > 1 {
		weights := make([]int, len(members))
		any := false
		for i, m := range members {
			if m.Weight > 0 {
				any = true
			}
			weights[i] = m.Weight
		}
		// blank weight = 1: a literal 0 would starve the member (wrrPick never
		// picks x < 0) — and the UI weight field is optional per row
		if any {
			for i, w := range weights {
				if w == 0 {
					weights[i] = 1
				}
			}
		}
		head := wrrPick(&r.wrr, config.ComboID(cb.Name), weights, len(members))
		rotated := make([]*config.ComboMember, 0, len(members))
		rotated = append(rotated, members[head])
		rotated = append(rotated, members[:head]...)
		rotated = append(rotated, members[head+1:]...)
		members = rotated
	}
	out := make([]comboMember, 0, len(members))
	for _, m := range members {
		for _, p := range r.cfg.Providers {
			if p.Name == m.Provider {
				model := m.Model
				if model == "" {
					model = cb.Model
				}
				out = append(out, comboMember{p: comboClone(p, m), model: model})
				break
			}
		}
	}
	return out
}

// comboTargets expands a combo into ordered dispatch targets: one pass over
// resolved members (curation checked against the MEMBER's upstream id, since
// providers may curate different ids for the same model), each target carrying
// that member's model as the dispatch override.
func (r *Registry) comboTargets(cb *config.Combo) []*Target {
	var targets []*Target
	for _, m := range r.comboMembers(cb) {
		for _, t := range r.buildTargets([]*config.Provider{m.p}, m.model, m.model, false) {
			t.ModelOverride = m.model
			targets = append(targets, t)
		}
	}
	if len(targets) > 8 {
		targets = targets[:8] // same failover fan-out cap as the route path
	}
	return targets
}

// Resolve returns the ordered dispatch targets for a model.
// Combos win first ("combo/<name>" → pooled members), then routes; no route
// → any provider whose Models list contains the model (config order). A
// "prefix/model" request restricts candidates to providers carrying that
// prefix. Providers that can't serve the model (non-empty Models list without
// it) are skipped inside chains.
func (r *Registry) Resolve(model string, allow []string) []*Target {
	r.mu.RLock()
	defer r.mu.RUnlock()
	if !KeyAllowed(allow, model) {
		return nil
	}
	if cb := r.comboMatches(model); cb != nil {
		return r.comboTargets(cb)
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
	targets := r.buildTargets(provs, name, model, prefixed)
	if len(targets) > 8 {
		targets = targets[:8] // ponytail: cap failover fan-out; more is a config smell
	}
	return targets
}

// buildTargets expands scoped providers into ordered dispatch targets (oauth
// accounts or static keys, honoring per-key disable flags and rotation).
// name is the id providers' Models lists are matched against (bare for
// prefixed requests), full the requested id for full-id-curated natural ids.
// Shared by the combo and route/auto paths — cooldowns and limiters key on the
// returned targets, never on this call.
func (r *Registry) buildTargets(provs []*config.Provider, name, full string, prefixed bool) []*Target {
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
		if prefixed && (contains(p.Models, full) || containsMap(p.ModelMap, full)) {
			pname = full
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

// ClassifierCombo reports whether the combo is a decision combo: the declared
// Type wins ("decision" = yes, "chat"/"" = no, validation keeps members in
// line); without a Type, fall back to detecting all-classifier membership
// (legacy/hand-written configs).
func (r *Registry) ClassifierCombo(cb *config.Combo) bool {
	switch cb.Type {
	case "decision":
		return true
	case "chat":
		return false
	}
	if len(cb.Members) == 0 {
		return false
	}
	isClf := map[string]bool{}
	for _, p := range r.cfg.Providers {
		isClf[p.Name] = p.Wire == "classifier"
	}
	for _, m := range cb.Members {
		if !isClf[m.Provider] {
			return false
		}
	}
	return true
}

// Models returns the union of all advertised models (for /v1/models).
func (r *Registry) Models() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	seen := map[string]bool{}
	var out []string
	for _, cb := range r.cfg.Combos {
		if r.ClassifierCombo(cb) {
			continue // decision combos advertise on /v1/systemone/models only
		}
		if id := config.ComboID(cb.Name); !seen[id] {
			seen[id] = true
			out = append(out, id)
		}
	}
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
