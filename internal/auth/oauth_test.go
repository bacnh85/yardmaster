package auth

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/bacnh85/yardmaster/internal/config"
	"github.com/bacnh85/yardmaster/internal/store"
)

// newTestPool spins a pool against a fake token endpoint; the handler can
// assert request bodies and rotate refresh tokens.
func newTestPool(t *testing.T, handler http.HandlerFunc) (*OAuthPool, *store.Store) {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	st, err := store.Open(t.TempDir() + "/test.db")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	p := NewOAuthPool(st)
	t.Cleanup(p.Close)
	p.Register("prov", &config.OAuthAcct{
		Name: "a1", Kind: "custom", RefreshTok: "rt-original",
		TokenEndpoint: srv.URL + "/token", ClientID: "cid",
		ExpiresAt: time.Now().Add(-time.Hour).Unix(), // already stale
	})
	return p, st
}

func TestOAuthRefreshAndRotation(t *testing.T) {
	var refreshes atomic.Int32
	p, st := newTestPool(t, func(w http.ResponseWriter, r *http.Request) {
		refreshes.Add(1)
		var body map[string]any
		json.NewDecoder(r.Body).Decode(&body)
		if body["grant_type"] != "refresh_token" || body["refresh_token"] == "" || body["client_id"] != "cid" {
			t.Errorf("bad refresh body: %v", body)
		}
		// rotate: every refresh returns a NEW refresh token (one-time use)
		n := refreshes.Load()
		json.NewEncoder(w).Encode(map[string]any{
			"access_token":  "at-" + string(rune('0'+n)),
			"refresh_token": "rt-" + string(rune('0'+n)),
			"expires_in":    3600,
		})
	})

	ctx := context.Background()
	tok, err := p.Token(ctx, "prov", "a1")
	if err != nil || tok != "at-1" {
		t.Fatalf("first token: %q %v", tok, err)
	}
	// fresh → no second HTTP call
	if tok, _ = p.Token(ctx, "prov", "a1"); tok != "at-1" || refreshes.Load() != 1 {
		t.Fatalf("fresh token should not refresh: %q n=%d", tok, refreshes.Load())
	}
	// rotated refresh token must be persisted to the store (survives restart)
	if _, rt, _ := st.LoadOAuth("prov", "a1"); rt != "rt-1" {
		t.Fatalf("rotated refresh token not persisted: %q", rt)
	}
	// new pool over the same store: seed comes from the DB, not config
	p2 := NewOAuthPool(st)
	defer p2.Close()
	p2.Register("prov", &config.OAuthAcct{
		Name: "a1", Kind: "custom", RefreshTok: "rt-original",
		TokenEndpoint: "http://unused", ClientID: "cid",
	})
	if tok, _ := p2.Token(ctx, "prov", "a1"); tok != "at-1" {
		t.Fatalf("stored access token should be reused: %q", tok)
	}
}

func TestOAuthRefreshFailure(t *testing.T) {
	p, _ := newTestPool(t, func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"error":"invalid_grant"}`, 400)
	})
	if _, err := p.Token(context.Background(), "prov", "a1"); err == nil {
		t.Fatal("expected refresh failure")
	}
	st := p.States("prov")["a1"]
	if !st.Cooling {
		t.Fatal("network-level refresh failure should cool the account briefly")
	}
	if st.LastError == "" {
		t.Fatal("last_error should be recorded")
	}
}

func TestOAuthMarkResultCooldown(t *testing.T) {
	// always-fresh token so refresh never fires
	p, _ := newTestPool(t, func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{"access_token": "at", "expires_in": 3600})
	})
	ctx := context.Background()
	if _, err := p.Token(ctx, "prov", "a1"); err != nil {
		t.Fatal(err)
	}
	p.MarkResult("prov", "a1", 200, "")
	if p.Cooling("prov", "a1") {
		t.Fatal("200 must clear cooldown")
	}
	p.MarkResult("prov", "a1", 429, `{"code":1302}`)
	if !p.Cooling("prov", "a1") {
		t.Fatal("429 must cool the account")
	}
	p.MarkResult("prov", "a1", 200, "")
	if p.Cooling("prov", "a1") {
		t.Fatal("success must clear cooldown")
	}
	// 401 invalidates the access token so the next Token() refreshes
	if _, err := p.Token(ctx, "prov", "a1"); err != nil {
		t.Fatal(err)
	}
	p.MarkResult("prov", "a1", 401, "expired")
	if !p.Cooling("prov", "a1") {
		t.Fatal("401 should trigger a short cooldown")
	}
	if tok, _ := p.Token(ctx, "prov", "a1"); tok != "at" {
		t.Fatalf("expected re-refreshed token, got %q", tok)
	}
	// unknown account: no-ops, no panic
	p.MarkResult("prov", "nope", 429, "")
	if p.Cooling("prov", "nope") {
		t.Fatal("unknown account must not report cooling")
	}
}

func TestOAuthNoRefreshToken(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { t.Error("must not call endpoint") }))
	defer srv.Close()
	p := NewOAuthPool(nil)
	defer p.Close()
	p.Register("prov", &config.OAuthAcct{
		Name: "seed-only", Kind: "custom", AccessTok: "seed",
		TokenEndpoint: srv.URL, ClientID: "x",
		ExpiresAt: 0, // unknown expiry
	})
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	// unknown expiry + no refresh: returns the seed within the stale window
	tok, err := p.Token(ctx, "prov", "seed-only")
	if err != nil || tok != "seed" {
		t.Fatalf("seed token should be usable: %q %v", tok, err)
	}
}

// Reload-vs-refresh race: Register() (config reload) mutates account fields
// while refresh() snapshots and uses them. Must be clean under -race.
func TestOAuthRegisterDuringRefreshRace(t *testing.T) {
	var refreshes atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		refreshes.Add(1)
		json.NewEncoder(w).Encode(map[string]any{
			"access_token":  "at-" + fmt.Sprint(refreshes.Load()),
			"refresh_token": "rt-" + fmt.Sprint(refreshes.Load()),
			"expires_in":    1, // force constant refreshing
		})
	}))
	defer srv.Close()
	st, err := store.Open(t.TempDir() + "/test.db")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	p := NewOAuthPool(st)
	defer p.Close()

	stop := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(2)
	go func() { // A: hammer Token
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
				_, _ = p.Token(context.Background(), "prov", "a1")
			}
		}
	}()
	go func() { // B: hammer Register (config reload) with changing tokens
		defer wg.Done()
		i := 0
		for {
			select {
			case <-stop:
				return
			default:
				i++
				p.Register("prov", &config.OAuthAcct{
					Name: "a1", Kind: "custom", RefreshTok: fmt.Sprintf("rt-seed-%d", i),
					TokenEndpoint: srv.URL + "/token", ClientID: "cid",
					ExpiresAt: time.Now().Add(-time.Second).Unix(),
				})
			}
		}
	}()
	time.Sleep(300 * time.Millisecond)
	close(stop)
	wg.Wait()
}
