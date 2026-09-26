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
	// Cache-tab fields: exactly one of the two rows hit (cache_read > 0)
	if sum.CachedRequests != 1 {
		t.Fatalf("summary cached_requests = %d, want 1", sum.CachedRequests)
	}
	for _, b := range rows {
		want := int64(1)
		if b.Name == "m2" {
			want = 0
		}
		if b.CachedReqs != want {
			t.Fatalf("breakdown %s cached_requests = %d, want %d", b.Name, b.CachedReqs, want)
		}
	}
	// series carries cache_read for the cached-vs-fresh chart
	var seriesCR int64
	for _, p := range sum.Series {
		seriesCR += p.CacheRd
	}
	if seriesCR != 30 {
		t.Fatalf("series cache_read = %d, want 30", seriesCR)
	}
}

// TimeSeriesByModel groups by (bucket, model); tok_in stays cache-exclusive
// (matches SeriesPoint) so charts never double-count cached input.
func TestTimeSeriesByModel(t *testing.T) {
	db := filepath.Join(t.TempDir(), "t.db")
	s, err := Open(db)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UnixMilli()
	hour := int64(3600000)
	b0 := now / hour * hour // current bucket start
	s.Submit(&Record{Ts: b0 + 1000, Key: "k", Model: "m1", Provider: "p", Status: 200,
		TokIn: 30, TokOut: 4, CacheRead: 70, CostUSD: 0.5})
	s.Submit(&Record{Ts: b0 + 2000, Key: "k", Model: "m1", Provider: "p", Status: 200,
		TokIn: 10, TokOut: 2, CacheRead: 5})
	s.Submit(&Record{Ts: b0 + 3000, Key: "k", Model: "m2", Provider: "p", Status: 200,
		TokIn: 100, TokOut: 1, CostUSD: 2.0})
	s.Submit(&Record{Ts: b0 - hour + 1000, Key: "k", Model: "m1", Provider: "p", Status: 200,
		TokIn: 7, TokOut: 1}) // previous bucket
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s2, err := Open(db)
	if err != nil {
		t.Fatal(err)
	}
	defer s2.Close()

	rows, err := s2.TimeSeriesByModel(24*time.Hour, "hour")
	if err != nil {
		t.Fatal(err)
	}
	type key struct {
		ts int64
		m  string
	}
	got := map[key]ModelSeriesPoint{}
	var tsOrder []int64
	for _, r := range rows {
		k := key{r.Bucket, r.Model}
		if _, seen := got[k]; !seen {
			tsOrder = append(tsOrder, r.Bucket)
		}
		got[k] = r
	}
	if len(got) != 3 {
		t.Fatalf("want 3 (bucket,model) groups, got %d: %+v", len(got), rows)
	}
	if len(tsOrder) < 2 || tsOrder[0] >= tsOrder[1] {
		t.Fatalf("rows not ordered by bucket: %v", tsOrder)
	}
	r := got[key{b0, "m1"}]
	if r.TokIn != 40 || r.TokOut != 6 {
		t.Errorf("m1 current bucket: want tok_in 40 (cache-exclusive), tok_out 6; got %+v", r)
	}
	if got[key{b0, "m2"}].Cost != 2.0 {
		t.Errorf("m2 cost: want 2.0; got %+v", got[key{b0, "m2"}])
	}
	if got[key{b0 - hour, "m1"}].TokIn != 7 {
		t.Errorf("m1 previous bucket: want tok_in 7; got %+v", got[key{b0 - hour, "m1"}])
	}
}

// The percentile pass scans and sorts every ttft row in the window;
// SummarySince must skip collection entirely for long windows (the 12-month
// Usage heatmap fetch hits hours=8760 on every mount).
func TestSummarySinceLongRangeSkipsPercentiles(t *testing.T) {
	db := filepath.Join(t.TempDir(), "t.db")
	s, err := Open(db)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UnixMilli()
	hourMs := int64(time.Hour / time.Millisecond)
	// 90k rows beyond the 30d cutoff + 3k inside it: loading and sorting the
	// long window would take minutes; the short window (3k rows)
	// must still produce real percentiles. Rows go in via one direct tx —
	// Submit's 4096-slot channel drops under a bulk producer, and pacing it
	// would cost minutes of sleep.
	insert := func(n int, ts func(i int) int64) {
		tx, err := s.db.Begin()
		if err != nil {
			t.Fatal(err)
		}
		stmt, err := tx.Prepare(`INSERT INTO requests
			(ts,key_name,model,provider,status,stream,ttft_ms,dur_ms,tok_in,tok_out,cache_read,cache_write,cost_usd,err,attempts)
			VALUES (?,?,?,?,?,0,?,0,?,0,0,0,0,'',1)`)
		if err != nil {
			t.Fatal(err)
		}
		for i := 0; i < n; i++ {
			if _, err := stmt.Exec(ts(i), "k", "m", "p", 200, float64(i%500+1), 1); err != nil {
				t.Fatal(err)
			}
		}
		if err := stmt.Close(); err != nil {
			t.Fatal(err)
		}
		if err := tx.Commit(); err != nil {
			t.Fatal(err)
		}
	}
	insert(90000, func(i int) int64 { return now - 90*24*hourMs + int64(i)*int64(30*time.Second/time.Millisecond) })
	insert(3000, func(i int) int64 { return now - int64(i)*int64(time.Second/time.Millisecond) })
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s2, err := Open(db)
	if err != nil {
		t.Fatal(err)
	}
	defer s2.Close()

	// scale proof: every submitted row must have persisted (drops → fix the
	// fixture, not the assertion)
	all, err := s2.SummarySince(365*24*time.Hour, "day")
	if err != nil {
		t.Fatal(err)
	}
	if all.Requests != 93000 {
		t.Fatalf("fixture dropped rows: want 93000 persisted, got %d (inserts outran the writer)", all.Requests)
	}

	start := time.Now()
	sum, err := s2.SummarySince(365*24*time.Hour, "day")
	if err != nil {
		t.Fatal(err)
	}
	// ponytail: wall-clock bound only catches the regression it guards, a
	// restored full-window percentile pass on 93k rows; 5s flaked in CI
	// (-race + 2-core runner) — 30s still fails loud if collection comes back
	if time.Since(start) > 30*time.Second {
		t.Fatalf("year-range summary too slow: %v (ttft skip regressed?)", time.Since(start))
	}
	if sum.TTFTp50 != nil {
		t.Errorf("TTFTp50 must be nil beyond the 30d cutoff, got %v", *sum.TTFTp50)
	}

	// short window keeps the LatencyTab contract: real percentiles
	recent, err := s2.SummarySince(time.Hour, "minute")
	if err != nil {
		t.Fatal(err)
	}
	if recent.TTFTp50 == nil || *recent.TTFTp50 <= 0 {
		t.Errorf("short window must compute percentiles, got %v", recent.TTFTp50)
	}
}
