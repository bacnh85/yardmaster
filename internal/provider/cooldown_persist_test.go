package provider

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/bacnh85/yardmaster/internal/store"
)

// End to end: mutations on a store-wired Cooldowns must survive a restart —
// a fresh Cooldowns attached to the same DB still sees the key cooling, a
// Reset on the live side clears the persisted row, and a key whose window
// lapsed while the process was down comes back not cooling.
func TestAttachCooldownStoreRoundTrip(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	cd := NewCooldowns()
	AttachCooldownStore(cd, st)
	cd.Mark429("p", "k429", "120")
	for i := 0; i < 3; i++ {
		cd.MarkFail("p", "kbreaker")
	}

	reloaded := NewCooldowns()
	AttachCooldownStore(reloaded, st)
	if !reloaded.Cooling("p", "k429") {
		t.Fatal("429 window lost across reload")
	}
	if !reloaded.Cooling("p", "kbreaker") {
		t.Fatal("breaker window lost across reload")
	}
	if u := reloaded.Until("p", "k429"); !u.After(time.Now()) {
		t.Fatalf("restored window in the past: %v", u)
	}

	cd.Reset("p", "k429") // success clears on the live side
	reloaded = NewCooldowns()
	AttachCooldownStore(reloaded, st)
	if reloaded.Cooling("p", "k429") {
		t.Fatal("reset key still cooling after reload — DELETE missing")
	}
	if !reloaded.Cooling("p", "kbreaker") {
		t.Fatal("unrelated window lost by the reset")
	}
}

// Reset must not DELETE a disk row this process never marked: the proxy
// Resets after every success, and an unconditional fire would put a SQLite
// write on the hot path even when nothing was cooling.
func TestResetSkipsForeignRow(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	// Foreign row: written directly to the store, never marked on any Cooldowns.
	until := time.Now().Add(time.Hour)
	if err := st.SaveCooldown("p", "foreign-key", "429", until); err != nil {
		t.Fatal(err)
	}

	// Fresh Cooldowns with ONLY the save hook wired — no restore, so the
	// foreign key has no in-memory entry.
	cd := NewCooldowns()
	cd.SetPersistHook(func(provider, key, reason string, u time.Time) {
		if err := st.SaveCooldown(provider, key, reason, u); err != nil {
			t.Errorf("persist: %v", err)
		}
	})
	cd.Reset("p", "foreign-key") // guard must suppress the fire → no DELETE

	rows, err := st.LoadCooldowns()
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range rows {
		if r.Provider == "p" && r.Key == "foreign-key" {
			return // guard held: row survived
		}
	}
	t.Fatal("foreign row deleted by Reset — guard not applied")
}

// Attach must tolerate a nil store/Cooldowns (deployment without persistence).
func TestAttachCooldownStoreNil(t *testing.T) {
	AttachCooldownStore(NewCooldowns(), nil)
	AttachCooldownStore(nil, &store.Store{})
}
