package store

import (
	"path/filepath"
	"testing"
	"time"
)

// Close must persist records accepted before Close was called.
func TestCloseDrains(t *testing.T) {
	db := filepath.Join(t.TempDir(), "t.db")
	s, err := Open(db)
	if err != nil {
		t.Fatal(err)
	}
	const n = 25
	for i := 0; i < n; i++ {
		s.Submit(&Record{Ts: time.Now().UnixMilli(), Key: "k", Model: "m", Provider: "p",
			Status: 200, TokIn: 10, TokOut: 1})
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	s2, err := Open(db)
	if err != nil {
		t.Fatal(err)
	}
	defer s2.Close()
	rows, err := s2.Recent(n)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != n {
		t.Fatalf("lost records: want %d, got %d", n, len(rows))
	}
}

// Breakdown must carry cache_write so the UI's totalInput (tok_in+cache_read+
// cache_write) reconciles with the Summary card for anthropic cache-write traffic.
func TestBreakdownCarriesCacheWrite(t *testing.T) {
	db := filepath.Join(t.TempDir(), "t.db")
	s, err := Open(db)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UnixMilli()
	s.Submit(&Record{Ts: now, Key: "k", Model: "m1", Provider: "p", Status: 200,
		TokIn: 30, TokOut: 1, CacheRead: 30, CacheWrt: 30})
	s.Submit(&Record{Ts: now, Key: "k", Model: "m2", Provider: "p", Status: 200,
		TokIn: 100, TokOut: 1, CacheRead: 0, CacheWrt: 0})
	if err := s.Close(); err != nil { // drain the async batch writer
		t.Fatal(err)
	}

	s2, err := Open(db)
	if err != nil {
		t.Fatal(err)
	}
	defer s2.Close()
	rows, err := s2.BreakdownBy("model", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	var sumTokIn, sumCR, sumCW int64
	for _, b := range rows {
		sumTokIn += b.TokIn
		sumCR += b.CacheRd
		sumCW += b.CacheWrt
	}
	if sumTokIn != 130 || sumCR != 30 || sumCW != 30 {
		t.Fatalf("breakdown sums in=%d cacheR=%d cacheW=%d, want 130/30/30", sumTokIn, sumCR, sumCW)
	}
	// same invariant the Usage card computes: total = in + read + write
	sum, err := s2.SummarySince(time.Hour, "hour")
	if err != nil {
		t.Fatal(err)
	}
	if sum.TokIn+sum.CacheRead+sum.CacheWrite != sumTokIn+sumCR+sumCW {
		t.Fatalf("card total %d != breakdown total %d — tables would under-count",
			sum.TokIn+sum.CacheRead+sum.CacheWrite, sumTokIn+sumCR+sumCW)
	}
}
