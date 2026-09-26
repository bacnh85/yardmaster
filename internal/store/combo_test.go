package store

import (
	"path/filepath"
	"testing"
	"time"
)

// ComboUsageSince groups combo traffic by (model, provider): only combo-
// prefixed models appear, one row per serving provider, with failover counts
// from the attempts column and per-group TTFT p50.
func TestComboUsageSince(t *testing.T) {
	db := filepath.Join(t.TempDir(), "t.db")
	s, err := Open(db)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UnixMilli()
	rows := []*Record{
		{Ts: now, Model: "combo/x", Provider: "a", Status: 200, TTFTms: 100, Attempts: 1},
		{Ts: now, Model: "combo/x", Provider: "b", Status: 200, TTFTms: 300, Attempts: 2}, // failed over once
		{Ts: now, Model: "combo/x", Provider: "b", Status: 500, Attempts: 3},
		{Ts: now, Model: "plain", Provider: "a", Status: 200, Attempts: 1}, // not a combo
	}
	for _, r := range rows {
		s.Submit(r)
	}
	if err := s.Close(); err != nil { // drain the async batch writer
		t.Fatal(err)
	}
	s2, err := Open(db)
	if err != nil {
		t.Fatal(err)
	}
	defer s2.Close()
	out, err := s2.ComboUsageSince(time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if len(out) != 2 {
		t.Fatalf("want 2 (combo,provider) rows, got %d: %+v", len(out), out)
	}
	byProv := map[string]ComboUsageRow{}
	for _, r := range out {
		if r.ComboID != "combo/x" {
			t.Fatalf("non-combo row leaked in: %+v", r)
		}
		byProv[r.Provider] = r
	}
	a, okA := byProv["a"]
	b, okB := byProv["b"]
	if !okA || !okB {
		t.Fatalf("missing member rows: %+v", out)
	}
	if a.Requests != 1 || a.Failovers != 0 || a.TTFTp50 == nil || *a.TTFTp50 != 100 {
		t.Fatalf("member a: %+v", a)
	}
	if b.Requests != 2 || b.Errors != 1 || b.Failovers != 2 { // both b rows failed over (attempts 2, 3)
		t.Fatalf("member b: %+v", b)
	}
}

// Past 30d the TTFT percentile pass is skipped (it scans and sorts every ttft
// row in the window, and this endpoint is polled) — same contract as an empty
// window, so the counters stay and ttft_p50_ms goes nil. 30d itself is
// inclusive on both this and SummarySince. Removing the guard fails this test.
func TestComboUsageLongWindowSkipsTTFT(t *testing.T) {
	db := filepath.Join(t.TempDir(), "t.db")
	s, err := Open(db)
	if err != nil {
		t.Fatal(err)
	}
	s.Submit(&Record{Ts: time.Now().UnixMilli(), Model: "combo/x", Provider: "a", Status: 200, TTFTms: 100, Attempts: 1})
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s2, err := Open(db)
	if err != nil {
		t.Fatal(err)
	}
	defer s2.Close()

	short, err := s2.ComboUsageSince(24 * time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if len(short) != 1 || short[0].TTFTp50 == nil {
		t.Fatalf("24h window must still collect ttft: %+v", short)
	}

	// boundary: 720h == 30d exactly, and the Combos tab has a 30d button —
	// flipping the guard's > to >= would blank ttft there silently
	edge, err := s2.ComboUsageSince(30 * 24 * time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if len(edge) != 1 || edge[0].TTFTp50 == nil {
		t.Fatalf("30d is inclusive, must still collect ttft: %+v", edge)
	}

	long, err := s2.ComboUsageSince(2160 * time.Hour) // 90d
	if err != nil {
		t.Fatal(err)
	}
	if len(long) != 1 {
		t.Fatalf("want 1 row, got %+v", long)
	}
	if long[0].Requests != 1 || long[0].TTFTp50 != nil {
		t.Fatalf("90d must keep counters but drop ttft: %+v", long[0])
	}
}
