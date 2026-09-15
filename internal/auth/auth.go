// Package auth provides the per-key dispatch limiter and inbound API key checks.
package auth

import (
	"context"
	"sync"
	"time"

	"golang.org/x/time/rate"

	"agent-router/internal/config"
)

// Limiter spaces dispatch STARTS at least `interval` apart. Streams themselves
// are never serialized — the lock is held only to compute the next slot
// (same design as pi-model-tools' zai-throttle, but in-process since the
// router owns every dispatch).
type Limiter struct {
	interval time.Duration
	mu       sync.Mutex
	next     time.Time
}

func NewLimiter(interval time.Duration) *Limiter {
	return &Limiter{interval: interval}
}

// Wait blocks until this caller's dispatch slot, or ctx is done.
func (l *Limiter) Wait(ctx context.Context) error {
	l.mu.Lock()
	now := time.Now()
	start := l.next
	if start.Before(now) {
		start = now
	}
	l.next = start.Add(l.interval)
	l.mu.Unlock()

	d := time.Until(start)
	if d <= 0 {
		return nil
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
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
func (ks *KeyStore) Check(value string) (*config.Key, bool) {
	ks.mu.RLock()
	k, ok := ks.keys[value]
	var lim *rate.Limiter
	if ok && k.RPM > 0 {
		lim = ks.limiters[value]
		if lim == nil {
			lim = rate.NewLimiter(rate.Limit(float64(k.RPM)/60.0), k.RPM)
			ks.limiters[value] = lim
		}
	}
	ks.mu.RUnlock()
	if !ok {
		return nil, false
	}
	if lim != nil && !lim.Allow() {
		return nil, false
	}
	return k, true
}
