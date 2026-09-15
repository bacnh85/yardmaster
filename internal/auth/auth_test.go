package auth

import (
	"sync"
	"testing"

	"agent-router/internal/config"
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
			if _, ok := ks.Check("ar-race"); !ok {
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
	if _, ok := ks.Check("nope"); ok {
		t.Fatal("unknown key accepted")
	}
	// RPM=1 → second immediate call in the same window is rejected
	if _, ok := ks.Check("ar-k"); !ok {
		t.Fatal("first call should pass")
	}
	if _, ok := ks.Check("ar-k"); ok {
		t.Fatal("second call should be rate limited")
	}
}
