package provider

import (
	"testing"
	"time"
)

// hookSpy records persist-hook invocations; cd runs on a fixed clock so
// expected windows compare exactly.
type hookSpy struct {
	cd    *Cooldowns
	now   time.Time
	calls []hookCall
}

type hookCall struct {
	provider, key, reason string
	until                 time.Time
}

func newSpyCooldowns(base time.Time) *hookSpy {
	s := &hookSpy{cd: NewCooldowns(), now: base}
	s.cd.now = func() time.Time { return s.now }
	s.cd.SetPersistHook(func(provider, key, reason string, until time.Time) {
		s.calls = append(s.calls, hookCall{provider, key, reason, until})
	})
	return s
}

// Mark429 must persist on set, on extension, and never on a hint that would
// shorten a live window (the in-memory rule the hook mirrors).
func TestPersistHookMark429(t *testing.T) {
	base := time.Unix(1700000000, 0)
	s := newSpyCooldowns(base)

	s.cd.Mark429("p", "k", "120")
	if len(s.calls) != 1 {
		t.Fatalf("set: %d hook calls, want 1: %+v", len(s.calls), s.calls)
	}
	want := hookCall{"p", "k", "429", base.Add(120 * time.Second)}
	if s.calls[0] != want {
		t.Errorf("set: got %+v, want %+v", s.calls[0], want)
	}

	// shorter hint mutates nothing → no persistence
	s.cd.Mark429("p", "k", "30")
	if len(s.calls) != 1 {
		t.Fatalf("shorter hint must not fire the hook: %+v", s.calls)
	}

	// extension fires with the new window
	s.cd.Mark429("p", "k", "300") // capped at max, not beyond
	if len(s.calls) != 2 {
		t.Fatalf("extension: %d hook calls, want 2: %+v", len(s.calls), s.calls)
	}
	want.until = base.Add(max429Cooldown)
	if s.calls[1] != want {
		t.Errorf("extension: got %+v, want %+v", s.calls[1], want)
	}
}

// The breaker persists on trip (and on window extension mid-storm), not on
// ordinary counted fails.
func TestPersistHookBreakerTrip(t *testing.T) {
	base := time.Unix(1700000000, 0)
	s := newSpyCooldowns(base)

	s.cd.MarkFail("p", "k")
	s.cd.MarkFail("p", "k")
	if len(s.calls) != 0 {
		t.Fatalf("counted fails must not persist: %+v", s.calls)
	}

	s.cd.MarkFail("p", "k") // third inside the window trips
	if len(s.calls) != 1 {
		t.Fatalf("trip: %d hook calls, want 1: %+v", len(s.calls), s.calls)
	}
	want := hookCall{"p", "k", "breaker", base.Add(breakerCooldown)}
	if s.calls[0] != want {
		t.Errorf("trip: got %+v, want %+v", s.calls[0], want)
	}

	// a later storm extending the window persists the new expiry
	s.now = base.Add(10 * time.Second)
	s.cd.MarkFail("p", "k")
	s.cd.MarkFail("p", "k")
	s.cd.MarkFail("p", "k")
	if len(s.calls) != 2 {
		t.Fatalf("extended trip: %d hook calls, want 2: %+v", len(s.calls), s.calls)
	}
	want.until = base.Add(10*time.Second + breakerCooldown)
	if s.calls[1] != want {
		t.Errorf("extended trip: got %+v, want %+v", s.calls[1], want)
	}
}

// Reset persists a cleared window (zero until) so the store deletes its row;
// Restore must not write back what it just read.
func TestPersistHookResetAndRestoreSilent(t *testing.T) {
	base := time.Unix(1700000000, 0)
	s := newSpyCooldowns(base)

	s.cd.Reset("p", "never-marked") // no fire: no in-memory entry → guard suppresses the hook
	if len(s.calls) != 0 {
		t.Fatalf("reset of never-marked key fired the hook: %+v", s.calls)
	}
	s.cd.Mark429("p", "marked", "60")
	s.cd.Reset("p", "marked") // marked key HAS an entry → fire (delete) still happens
	if len(s.calls) != 2 || s.calls[1] != (hookCall{"p", "marked", "reset", time.Time{}}) {
		t.Fatalf("reset: got %+v, want 2 calls ending in {p marked reset zero}", s.calls)
	}

	s.cd.Restore("p", "k", "429", base.Add(time.Minute))
	nBefore := len(s.calls)
	s.cd.Restore("p", "k", "429", base.Add(time.Minute))
	if len(s.calls) != nBefore {
		t.Fatalf("restore must not fire the hook: %d → %d: %+v", nBefore, len(s.calls), s.calls)
	}
}

// Restore replays a persisted window as if cooled; lapsed entries are a
// no-op — downtime must not cool a healthy key.
func TestRestore(t *testing.T) {
	base := time.Unix(1700000000, 0)
	cases := []struct {
		name    string
		until   time.Time
		cooling bool
	}{
		{"live window", base.Add(time.Minute), true},
		{"expired while down", base.Add(-time.Second), false},
		{"zero until", time.Time{}, false},
	}
	for _, tc := range cases {
		cd := NewCooldowns()
		cd.now = func() time.Time { return base }
		cd.Restore("p", "k", "429", tc.until)
		if got := cd.Cooling("p", "k"); got != tc.cooling {
			t.Errorf("%s: Cooling = %v, want %v", tc.name, got, tc.cooling)
		}
		if want := tc.until; tc.cooling && !cd.Until("p", "k").Equal(want) {
			t.Errorf("%s: Until = %v, want %v", tc.name, cd.Until("p", "k"), want)
		}
	}
}
