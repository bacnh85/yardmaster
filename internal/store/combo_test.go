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
