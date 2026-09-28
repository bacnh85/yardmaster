// Session affinity: pin a coding-agent session to the provider+connection
// that served its first request (TTL-bound) so upstream prompt caches —
// paid cache_write on every inject_cache_control request — actually hit.
package proxy

import (
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/bacnh85/yardmaster/internal/config"
	"github.com/bacnh85/yardmaster/internal/provider"
)

// affinityMaxEntries bounds the session→binding map. Deliberately a flat cap
// with lazy expiry + random eviction, not an LRU: this tracks coding-agent
// sessions (one row per conversation, not per request) — bounded memory beats
// perfect recency, and a random eviction only degrades cache hit rate, never
// correctness.
const affinityMaxEntries = 10000

// affinitySessionMax caps the inbound session id; absurd ids are ignored
// rather than turning the map key into an unbounded memory sink.
const affinitySessionMax = 256

// AffinityStore maps session id → serving connection. Zero value is usable;
// consult it only when config enables affinity.
type AffinityStore struct {
	mu sync.Mutex
	m  map[string]affinityBinding
	// now fakes the clock in tests; nil = time.Now
	now func() time.Time
}

// affinityBinding is one pinned session. Conn is the stable connection label
// (static key label or oauth account name) — pinning provider alone would
// still forfeit the prompt cache on a key switch. Label, never the secret.
type affinityBinding struct {
	Provider  string
	Conn      string
	ExpiresAt time.Time
}

func (s *AffinityStore) clock() time.Time {
	if s.now != nil {
		return s.now()
	}
	return time.Now()
}

// get returns the live binding for session, if any. Expired entries are
// dropped on read so a stale pin can't outlive its TTL.
func (s *AffinityStore) get(session string) (affinityBinding, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	b, ok := s.m[session]
	if !ok {
		return affinityBinding{}, false
	}
	if !s.clock().Before(b.ExpiresAt) {
		delete(s.m, session)
		return affinityBinding{}, false
	}
	return b, true
}

// set records the serving connection for session. Write-time eviction keeps
// the map bounded: expired entries first (free), then a random victim when
// still full — the cap is only hit when 10k DISTINCT sessions coexist within
// one ttl, where an LRU's recency guarantee buys nothing (see
// affinityMaxEntries).
func (s *AffinityStore) set(session, providerName, conn string, ttl time.Duration) {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.clock()
	if s.m == nil {
		s.m = make(map[string]affinityBinding)
	}
	if len(s.m) >= affinityMaxEntries {
		for k, b := range s.m {
			if !now.Before(b.ExpiresAt) {
				delete(s.m, k)
			}
		}
		if len(s.m) >= affinityMaxEntries {
			for k := range s.m {
				delete(s.m, k)
				break
			}
		}
	}
	s.m[session] = affinityBinding{Provider: providerName, Conn: conn, ExpiresAt: now.Add(ttl)}
}

// clear drops the binding: a dead connection must not be re-pinned.
func (s *AffinityStore) clear(session string) {
	s.mu.Lock()
	delete(s.m, session)
	s.mu.Unlock()
}

// Len reports live (unexpired) entries for the dashboard read model.
func (s *AffinityStore) Len() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.clock()
	n := 0
	for _, b := range s.m {
		if now.Before(b.ExpiresAt) {
			n++
		}
	}
	return n
}

// sessionID extracts the caller's session identity for affinity: first
// non-empty of the configured header, the opencode client session header,
// the anthropic metadata.user_id body field (Claude Code), and the openai
// user body field. wire is the inbound wire (only the anthropic body carries
// metadata); req may be nil for unparseable bodies. Ids are capped at
// affinitySessionMax bytes.
func sessionID(a *config.Affinity, wire string, r *http.Request, req map[string]any) string {
	if a == nil {
		return ""
	}
	for _, h := range []string{a.EffectiveHeader(), "x-opencode-session"} {
		if v := trimSession(r.Header.Get(h)); v != "" {
			return v
		}
	}
	if wire == WireAnthropic {
		if md, ok := req["metadata"].(map[string]any); ok {
			if id, _ := md["user_id"].(string); id != "" {
				if v := trimSession(id); v != "" {
					return v
				}
			}
		}
	}
	if id, _ := req["user"].(string); id != "" {
		if v := trimSession(id); v != "" {
			return v
		}
	}
	return ""
}

// trimSession whitespace-trims and caps a session id at affinitySessionMax
// bytes (truncated — ids that long are machine-generated; a shared prefix
// collision is negligible against the cache win).
func trimSession(v string) string {
	v = strings.TrimSpace(v)
	if len(v) > affinitySessionMax {
		v = v[:affinitySessionMax]
	}
	return v
}

// affinityEnabled reports whether the loaded config turned affinity on.
func (p *Proxy) affinityEnabled() bool {
	cfg := p.Reg.Config()
	return cfg != nil && cfg.Routing.Affinity != nil && cfg.Routing.Affinity.Enabled
}

// affinityLookup returns the session's live binding, if any. Callers gate on
// it before reordering the resolved target list.
func (p *Proxy) affinityLookup(session string) (providerName, conn string, ok bool) {
	if session == "" || !p.affinityEnabled() {
		return "", "", false
	}
	b, live := p.affinity.get(session)
	if !live {
		return "", "", false
	}
	return b.Provider, b.Conn, true
}

// affinityClear drops the session's binding (dead connection).
func (p *Proxy) affinityClear(session string) {
	if session == "" {
		return
	}
	p.affinity.clear(session)
}

// affinityPin records the serving provider+connection for session at the
// first-byte point. No-op when affinity is off or the request carried no
// session id.
func (p *Proxy) affinityPin(session, providerName, conn string) {
	if session == "" || !p.affinityEnabled() {
		return
	}
	ttl := time.Duration(p.Reg.Config().Routing.Affinity.EffectiveTTL()) * time.Second
	p.affinity.set(session, providerName, conn, ttl)
}

// AffinitySummary is the minimal dashboard read model for session affinity.
type AffinitySummary struct {
	Enabled bool `json:"enabled"`
	Entries int  `json:"entries"` // live (unexpired) session pins
}

// AffinityStats reports the current affinity state for the admin routing
// view. Server-side wiring is a follow-up (GET /admin/api/routing does not
// exist yet — only PUT).
func (p *Proxy) AffinityStats() AffinitySummary {
	enabled := p.affinityEnabled()
	if !enabled {
		return AffinitySummary{}
	}
	return AffinitySummary{Enabled: true, Entries: p.affinity.Len()}
}

// reorderForAffinity moves the target matching (providerName, conn) to the
// front, preserving the relative order of the rest — the pin biases, the
// failover chain stays intact. Returns the (possibly unchanged) list and the
// position of the bound connection: 0 when it heads the promoted list, -1
// when it is absent (not routable for this model — no reorder, no pin
// bookkeeping for the request).
func reorderForAffinity(targets []*provider.Target, providerName, conn string) ([]*provider.Target, int) {
	if providerName == "" {
		return targets, -1
	}
	idx := -1
	for i, t := range targets {
		if t.Provider.Name == providerName && connectionLabel(t) == conn {
			idx = i
			break
		}
	}
	if idx < 0 {
		return targets, -1
	}
	if idx == 0 {
		return targets, 0
	}
	out := make([]*provider.Target, 0, len(targets))
	out = append(out, targets[idx])
	out = append(out, targets[:idx]...)
	out = append(out, targets[idx+1:]...)
	return out, 0
}
