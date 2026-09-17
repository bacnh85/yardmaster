// Package provider holds the provider registry and model→provider routing.
package provider

import (
	"strings"
	"sync"
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

func keyAllowed(patterns []string, model string) bool {
	for _, p := range patterns {
		if matchRoute(p, model) {
			return true
		}
	}
	return false
}

// Resolve returns the ordered dispatch targets for a model.
// First matching route wins; no route → any provider whose Models list
// contains the model (config order). Providers that can't serve the model
// (non-empty Models list without it) are skipped inside chains.
func (r *Registry) Resolve(model string, allow []string) []*Target {
	r.mu.RLock()
	defer r.mu.RUnlock()
	if !keyAllowed(allow, model) {
		return nil
	}
	var provs []*config.Provider
	matched := false
	for _, rt := range r.cfg.Routes {
		if matchRoute(rt.Match, model) {
			for _, name := range rt.Chain {
				for _, p := range r.cfg.Providers {
					if p.Name == name {
						provs = append(provs, p)
						break
					}
				}
			}
			matched = true
			break
		}
	}
	if !matched {
		for _, p := range r.cfg.Providers {
			if len(p.Models) == 0 || contains(p.Models, model) {
				provs = append(provs, p)
			}
		}
	}
	var targets []*Target
	for _, p := range provs {
		if len(p.Models) > 0 && !contains(p.Models, model) && !containsMap(p.ModelMap, model) {
			continue
		}
		switch p.Auth.Type {
		case "oauth":
			for _, a := range p.Auth.OAuth {
				if !a.Disabled {
					targets = append(targets, &Target{Provider: p, AuthType: "oauth", AcctName: a.Name})
				}
			}
		default: // static
			for _, k := range p.Auth.Keys {
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

// UpstreamModel maps a requested model to the provider's upstream id.
func UpstreamModel(p *config.Provider, model string) string {
	if p.ModelMap != nil {
		if v, ok := p.ModelMap[model]; ok {
			return v
		}
	}
	return model
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
		for _, m := range p.Models {
			if !seen[m] {
				seen[m] = true
				out = append(out, m)
			}
		}
	}
	return out
}
