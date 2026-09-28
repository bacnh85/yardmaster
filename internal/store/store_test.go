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

	sum, err := s2.SummarySince(365*24*time.Hour, "day")
	if err != nil {
		t.Fatal(err)
	}
	// The assertion below is the actual regression guard (it fails if the 30d
	// guard is removed). A wall-clock bound used to sit here; it was dropped
	// because it can no longer catch anything — with percentiles() now using
	// sort.Float64s the restored pass is not the dominant cost (inserting the
	// 93k-row fixture under -race is), so the bound only risked flaking.
	// ponytail: if a slow pass ever needs catching, bound the call, not the test.
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

// SaveCooldown→LoadCooldowns round-trip at unix-second granularity; upsert
// overwrites in place (one row per target); zero-or-past until clears; rows
// survive the reopen a restart performs.
func TestCooldownPersistence(t *testing.T) {
	db := filepath.Join(t.TempDir(), "t.db")
	s, err := Open(db)
	if err != nil {
		t.Fatal(err)
	}
	until := time.Now().Add(5 * time.Minute).Truncate(time.Second) // store keeps whole seconds
	if err := s.SaveCooldown("p", "k1", "429", until); err != nil {
		t.Fatal(err)
	}
	if err := s.SaveCooldown("p2", "k2", "breaker", until.Add(-time.Minute)); err != nil {
		t.Fatal(err)
	}

	got, err := s.LoadCooldowns()
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("want 2 rows, got %d: %+v", len(got), got)
	}
	byKey := map[string]CooldownRow{}
	for _, r := range got {
		byKey[r.Provider+"\x00"+r.Key] = r
	}
	r := byKey["p\x00k1"]
	if r.Reason != "429" || !r.Until.Equal(until) {
		t.Errorf("p/k1: got %+v, want reason 429 until %v", r, until)
	}
	r = byKey["p2\x00k2"]
	if r.Reason != "breaker" || !r.Until.Equal(until.Add(-time.Minute)) {
		t.Errorf("p2/k2: got %+v, want reason breaker until %v", r, until.Add(-time.Minute))
	}

	// upsert: same PK replaces reason+until instead of erroring or doubling
	if err := s.SaveCooldown("p", "k1", "429", until.Add(3*time.Minute)); err != nil {
		t.Fatal(err)
	}
	got, err = s.LoadCooldowns()
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("upsert must keep one row per target, got %d: %+v", len(got), got)
	}
	for _, r := range got {
		if r.Provider == "p" && !r.Until.Equal(until.Add(3*time.Minute)) {
			t.Errorf("upsert until = %v, want %v", r.Until, until.Add(3*time.Minute))
		}
	}

	// zero until clears (Reset), past until clears too
	if err := s.SaveCooldown("p", "k1", "429", time.Time{}); err != nil {
		t.Fatal(err)
	}
	if err := s.SaveCooldown("p2", "k2", "breaker", time.Now().Add(-time.Second)); err != nil {
		t.Fatal(err)
	}
	if got, err := s.LoadCooldowns(); err != nil || len(got) != 0 {
		t.Fatalf("cleared rows: got %d rows (%v, %v), want 0", len(got), got, err)
	}

	// empty-then-cleared state survives the reopen
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s2, err := Open(db)
	if err != nil {
		t.Fatal(err)
	}
	future := time.Now().Add(time.Minute).Truncate(time.Second)
	if err := s2.SaveCooldown("p3", "k3", "429", future); err != nil {
		t.Fatal(err)
	}
	if err := s2.Close(); err != nil {
		t.Fatal(err)
	}
	s3, err := Open(db)
	if err != nil {
		t.Fatal(err)
	}
	defer s3.Close()
	got, err = s3.LoadCooldowns()
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Provider != "p3" || got[0].Key != "k3" || !got[0].Until.Equal(future) {
		t.Fatalf("after reopen: got %+v, want [{p3 k3 429 %v}]", got, future)
	}
}

// MonthSpend sums only the key's rows at/after the cutoff; rows for other keys
// and rows before the cutoff stay out of the sum.
func TestMonthSpend(t *testing.T) {
	db := filepath.Join(t.TempDir(), "t.db")
	s, err := Open(db)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UnixMilli()
	inside := now - 24*time.Hour.Milliseconds()
	before := now - 35*24*time.Hour.Milliseconds()
	s.Submit(&Record{Ts: inside, Key: "alpha", CostUSD: 1.25})
	s.Submit(&Record{Ts: inside, Key: "alpha", CostUSD: 0.75})
	s.Submit(&Record{Ts: before, Key: "alpha", CostUSD: 5}) // outside the window
	s.Submit(&Record{Ts: inside, Key: "beta", CostUSD: 9})  // other key
	if err := s.Close(); err != nil {                       // drain the async batch writer
		t.Fatal(err)
	}

	s2, err := Open(db)
	if err != nil {
		t.Fatal(err)
	}
	defer s2.Close()
	cutoff := now - 30*24*time.Hour.Milliseconds()
	got, err := s2.MonthSpend("alpha", cutoff)
	if err != nil {
		t.Fatal(err)
	}
	if got != 2.0 {
		t.Fatalf("MonthSpend(alpha) = %v, want 2.0 (pre-cutoff and other-key rows excluded)", got)
	}
	if got, err := s2.MonthSpend("ghost", cutoff); err != nil || got != 0 {
		t.Fatalf("MonthSpend(unknown key) = %v, %v; want 0, nil", got, err)
	}
}
