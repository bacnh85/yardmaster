package auth

import (
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/bacnh85/yardmaster/internal/config"
)

// burst=1 must reproduce the original strict-spacing limiter: one dispatch
// immediate, the next held back a full interval.
func TestLimiterStrictSpacing(t *testing.T) {
	l := NewLimiter(80*time.Millisecond, 1)
	start := time.Now()
	if err := l.Wait(t.Context()); err != nil {
		t.Fatalf("first wait: %v", err)
	}
	if err := l.Wait(t.Context()); err != nil {
		t.Fatalf("second wait: %v", err)
	}
	if d := time.Since(start); d < 60*time.Millisecond {
		t.Fatalf("second dispatch after %s, want >= ~interval (strict spacing)", d)
	}
}

// burst=N lets N concurrent fan-out requests dispatch immediately; the
// sustained tail is still spaced.
func TestLimiterBurst(t *testing.T) {
	const burst = 3
	l := NewLimiter(80*time.Millisecond, burst)
	start := time.Now()
	var wg sync.WaitGroup
	errs := make([]error, burst)
	for i := 0; i < burst; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			errs[i] = l.Wait(t.Context())
		}(i)
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Fatalf("wait %d: %v", i, err)
		}
	}
	if d := time.Since(start); d >= 60*time.Millisecond {
		t.Fatalf("burst %d took %s; want all %d immediate", burst, d, burst)
	}
	// sustained tail: the next dispatch past the burst is paced
	if err := l.Wait(t.Context()); err != nil {
		t.Fatalf("post-burst wait: %v", err)
	}
	if d := time.Since(start); d < 60*time.Millisecond {
		t.Fatalf("post-burst dispatch after %s, want >= ~interval (sustained pacing)", d)
	}
}

// Regression: concurrent Check() on a fresh RPM key used to race on the
// limiters map (fatal concurrent map writes). Run with -race.
func TestCheckConcurrentRPM(t *testing.T) {
	ks := NewKeyStore([]*config.Key{
		{Key: "ar-race", Name: "t", Allow: []string{"*"}, RPM: 1000000},
	})
	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			k, err := ks.Check("ar-race")
			if err != nil || k == nil {
				t.Error("Check should succeed")
			}
		}()
	}
	wg.Wait()
}

func TestCheckUnknownKeyAndRPM(t *testing.T) {
	ks := NewKeyStore([]*config.Key{
		{Key: "ar-k", Name: "t", Allow: []string{"*"}, RPM: 1},
	})
	if _, err := ks.Check("nope"); !errors.Is(err, ErrUnknownKey) {
		t.Fatalf("unknown key: err = %v, want ErrUnknownKey", err)
	}
	// RPM=1 → second immediate call in the same window is rejected
	if _, err := ks.Check("ar-k"); err != nil {
		t.Fatalf("first call should pass: %v", err)
	}
	k, err := ks.Check("ar-k")
	if !errors.Is(err, ErrRateLimited) {
		t.Fatalf("second call: err = %v, want ErrRateLimited", err)
	}
	// key config rides along so the caller can name the limit in the 429 body
	if k == nil || k.RPM != 1 {
		t.Fatalf("rate-limited Check returned key %+v, want config with RPM 1", k)
	}
}
