package store

import (
	"path/filepath"
	"testing"
)

func TestKeyLastUsed(t *testing.T) {
	path := filepath.Join(t.TempDir(), "t.db")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	// Close drains the Submit channel and flushes — see run()'s done branch.
	for _, r := range []*Record{
		{Ts: 1000, Key: "alpha", Model: "m", Provider: "p", Status: 200},
		{Ts: 3000, Key: "alpha", Model: "m", Provider: "p", Status: 200},
		{Ts: 2000, Key: "beta", Model: "m", Provider: "p", Status: 200},
		{Ts: 4000, Key: "", Model: "m", Provider: "p", Status: 200}, // keyless request — omitted
	} {
		s.Submit(r)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s2, err := Open(path) // reopen: read from disk, not memory
	if err != nil {
		t.Fatal(err)
	}
	defer s2.Close()

	got, err := s2.KeyLastUsed()
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got["alpha"] != 3000 || got["beta"] != 2000 {
		t.Fatalf("KeyLastUsed = %v, want {alpha:3000, beta:2000}", got)
	}
	if _, ok := got[""]; ok {
		t.Error("empty key_name must not appear")
	}
}

func TestRenameKeyReattributesHistory(t *testing.T) {
	path := filepath.Join(t.TempDir(), "t.db")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range []*Record{
		{Ts: 1000, Key: "old", Model: "m", Provider: "p", Status: 200},
		{Ts: 2000, Key: "other", Model: "m", Provider: "p", Status: 200},
	} {
		s.Submit(r)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s2, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s2.Close()

	if err := s2.RenameKey("old", "new"); err != nil {
		t.Fatal(err)
	}
	got, err := s2.KeyLastUsed()
	if err != nil {
		t.Fatal(err)
	}
	if got["new"] != 1000 || got["other"] != 2000 {
		t.Fatalf("after rename = %v, want {new:1000, other:2000}", got)
	}
	if _, stale := got["old"]; stale {
		t.Error("old name still present after rename")
	}
	// no-op guards
	if err := s2.RenameKey("", "x"); err != nil {
		t.Error(err)
	}
	if err := s2.RenameKey("new", "new"); err != nil {
		t.Error(err)
	}
}
