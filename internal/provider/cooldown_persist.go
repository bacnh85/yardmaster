package provider

import (
	"log"
	"time"

	"github.com/bacnh85/yardmaster/internal/store"
)

// AttachCooldownStore mirrors static-key cooldowns to SQLite so a restart
// doesn't forget live 429/breaker windows (oauth tokens already survive via
// the same DB), and replays persisted windows into cd at boot. Best-effort:
// save/load errors are logged once and never fatal — a DB hiccup must not
// break dispatch, and in-memory state stays the source of truth at runtime.
// store does not import this package, so the edge is acyclic.
func AttachCooldownStore(cd *Cooldowns, st *store.Store) {
	if cd == nil || st == nil {
		return
	}
	var warned bool
	fail := func(what string, err error) {
		if !warned { // a wedged DB would otherwise log per request
			warned = true
			log.Printf("provider: cooldown persist: %s: %v (further errors suppressed)", what, err)
		}
	}
	cd.SetPersistHook(func(provider, key, reason string, until time.Time) {
		if err := st.SaveCooldown(provider, key, reason, until); err != nil {
			fail("save", err)
		}
	})
	rows, err := st.LoadCooldowns()
	if err != nil {
		fail("load", err)
		return
	}
	for _, r := range rows { // Restore skips entries that lapsed while down
		cd.Restore(r.Provider, r.Key, r.Reason, r.Until)
	}
}
