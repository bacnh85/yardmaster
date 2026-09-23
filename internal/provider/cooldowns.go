package provider

import (
	"net/http"
	"strconv"
	"sync"
	"time"
)

// Cooldowns is per-target (provider+key) failure memory: a 429 cools the key
// for Retry-After seconds, repeated 5xx trips a small breaker. Cooling targets
// are skipped at dispatch. Static keys only — oauth keeps its pool ladder.
type Cooldowns struct {
	mu  sync.Mutex
	m   map[string]*cooldownEntry
	now func() time.Time // overridable in tests
}

type cooldownEntry struct {
	until time.Time // cooling while now < until
	fails int       // 5xx count inside the breaker window
	since time.Time // breaker window start
}

const (
	default429Cooldown = 30 * time.Second
	max429Cooldown     = 5 * time.Minute
	breakerThreshold   = 3 // ponytail: fixed knobs; config when someone needs one
	breakerWindow      = 60 * time.Second
	breakerCooldown    = 30 * time.Second
)

func NewCooldowns() *Cooldowns {
	return &Cooldowns{m: map[string]*cooldownEntry{}, now: time.Now}
}

// Mark429 cools the target for the upstream Retry-After hint (seconds or
// HTTP-date), or the default when absent/unparseable. A new hint extends an
// active cooldown but never shortens it (in-flight requests race: a headerless
// 429 landing late must not cut a live 300s window to 30s).
func (c *Cooldowns) Mark429(provider, key, retryAfter string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	d := default429Cooldown
	if ra, ok := parseRetryAfter(retryAfter, c.now()); ok {
		if ra > max429Cooldown { // ponytail: cap misbehaving upstreams
			ra = max429Cooldown
		}
		d = ra
	}
	e := c.entry(provider, key)
	if t := c.now().Add(d); e.until.IsZero() || t.After(e.until) {
		e.until = t
	}
}

// MarkFail records a 5xx; breakerThreshold within breakerWindow trips a cooldown.
func (c *Cooldowns) MarkFail(provider, key string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	now := c.now()
	e := c.entry(provider, key)
	if e.since.IsZero() || now.Sub(e.since) > breakerWindow {
		e.since, e.fails = now, 0
	}
	e.fails++
	if e.fails >= breakerThreshold {
		if t := now.Add(breakerCooldown); t.After(e.until) { // never shorten a live 429 window
			e.until = t
		}
		e.since, e.fails = time.Time{}, 0
	}
}

func (c *Cooldowns) Cooling(provider, key string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.m[provider+"\x00"+key]
	return ok && c.now().Before(e.until)
}

// Until returns when the cooldown expires (zero time if not cooling).
func (c *Cooldowns) Until(provider, key string) time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.m[provider+"\x00"+key]
	if !ok || !c.now().Before(e.until) {
		return time.Time{}
	}
	return e.until
}

func (c *Cooldowns) Reset(provider, key string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.m, provider+"\x00"+key)
}

// CooldownEntry is one active cooldown (for the dashboard).
type CooldownEntry struct {
	Provider string    `json:"provider"`
	Key      string    `json:"key"`
	Until    time.Time `json:"until"`
}

func (c *Cooldowns) Snapshot() []CooldownEntry {
	c.mu.Lock()
	defer c.mu.Unlock()
	now := c.now()
	var out []CooldownEntry
	for k, e := range c.m {
		if now.Before(e.until) {
			p, key, _ := splitNul(k)
			out = append(out, CooldownEntry{Provider: p, Key: keySuffix(key), Until: e.until})
		}
	}
	return out
}

// keySuffix redacts: dashboard key convention is …+last 6 chars.
func keySuffix(k string) string {
	if k == "" {
		return ""
	}
	if len(k) <= 6 {
		return "…" + k
	}
	return "…" + k[len(k)-6:]
}

func (c *Cooldowns) entry(provider, key string) *cooldownEntry {
	k := provider + "\x00" + key
	e, ok := c.m[k]
	if !ok {
		e = &cooldownEntry{}
		c.m[k] = e
	}
	return e
}

// parseRetryAfter understands the seconds form and HTTP-date form.
func parseRetryAfter(v string, now time.Time) (time.Duration, bool) {
	if v == "" {
		return 0, false
	}
	if s, err := strconv.Atoi(v); err == nil {
		if s < 0 {
			return 0, false
		}
		return time.Duration(s) * time.Second, true
	}
	if t, err := http.ParseTime(v); err == nil {
		if d := t.Sub(now); d > 0 {
			return d, true
		}
	}
	return 0, false
}

func splitNul(k string) (provider, key string, ok bool) {
	for i := 0; i < len(k); i++ {
		if k[i] == 0 {
			return k[:i], k[i+1:], true
		}
	}
	return k, "", false
}
