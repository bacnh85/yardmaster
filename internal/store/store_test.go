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
