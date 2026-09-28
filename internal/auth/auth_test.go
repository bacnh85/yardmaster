package auth

import (
	"errors"
	"sync"
	"testing"

	"github.com/bacnh85/yardmaster/internal/config"
)

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
