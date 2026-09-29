// Package auth provides the per-key dispatch limiter and inbound API key checks.
package auth

import (
	"errors"
	"sync"
	"time"

	"golang.org/x/time/rate"

	"github.com/bacnh85/yardmaster/internal/config"
)

// Check outcomes: unknown keys and RPM exhaustion must be distinguishable —
// both used to collapse to "not ok" and look like a fatal bad-key 401 to
// agents, which then stopped retrying.
var (
	ErrUnknownKey  = errors.New("unknown API key")
	ErrRateLimited = errors.New("key RPM limit exceeded")
)

// Limiter paces dispatch STARTS: burst concurrent fan-out requests dispatch
// immediately, sustained traffic is spaced `interval` apart (token bucket —
// same design as pi-model-tools' zai-throttle for Z.ai 1302 protection, but
// burst-capable for parallel agent fan-out). Streams themselves are never
// serialized. burst < 1 = 1: strict spacing, byte-identical to the original
// hand-rolled slot limiter.
type Limiter struct {
	*rate.Limiter
	interval time.Duration
	burst    int
}

func NewLimiter(interval time.Duration, burst int) *Limiter {
	if burst < 1 {
		burst = 1
	}
	return &Limiter{rate.NewLimiter(rate.Every(interval), burst), interval, burst}
}

// PaceMatches reports whether the limiter's config equals interval+burst, so
// callers can detect a config change and rebuild (hot reload / dashboard edit).
func (l *Limiter) PaceMatches(interval time.Duration, burst int) bool {
	return l.interval == interval && l.burst == burst
}

// ---- inbound API keys ----

// KeyStore validates inbound agent keys and enforces per-key RPM.
type KeyStore struct {
	mu       sync.RWMutex
	keys     map[string]*config.Key
	limiters map[string]*rate.Limiter
}

func NewKeyStore(keys []*config.Key) *KeyStore {
	ks := &KeyStore{keys: map[string]*config.Key{}, limiters: map[string]*rate.Limiter{}}
	ks.Replace(keys)
	return ks
}

func (ks *KeyStore) Replace(keys []*config.Key) {
	ks.mu.Lock()
	defer ks.mu.Unlock()
	ks.keys = map[string]*config.Key{}
	for _, k := range keys {
		ks.keys[k.Key] = k
	}
}

// Check validates a key value and its RPM budget. Returns the key config.
// nil error = allowed; ErrUnknownKey = not a configured key (caller answers
// 401); ErrRateLimited = RPM exhausted (caller answers 429 so agents retry
// instead of treating it as a fatal bad-key auth failure).
// Full lock (not RLock): limiter creation mutates limiters; concurrent map
// writes here would crash the process.
func (ks *KeyStore) Check(value string) (*config.Key, error) {
	ks.mu.Lock()
	defer ks.mu.Unlock()
	k, ok := ks.keys[value]
	if !ok {
		return nil, ErrUnknownKey
	}
	if k.RPM > 0 {
		lim := ks.limiters[value]
		if lim == nil {
			lim = rate.NewLimiter(rate.Limit(float64(k.RPM)/60.0), k.RPM)
			ks.limiters[value] = lim
		}
		if !lim.Allow() {
			// key config still returned so the caller can name the RPM limit
			// in the 429 body.
			return k, ErrRateLimited
		}
	}
	return k, nil
}
