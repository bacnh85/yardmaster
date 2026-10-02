package server

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/bacnh85/yardmaster/internal/auth"
	"github.com/bacnh85/yardmaster/internal/config"
	"github.com/bacnh85/yardmaster/internal/provider"
	"github.com/bacnh85/yardmaster/internal/proxy"
	"github.com/bacnh85/yardmaster/internal/store"
)

// retentionCfg spins up a server + admin-test client against a temp config/db.
func retentionCfg(t *testing.T, days int) (*Server, string, func(method, path string, body any) (int, string)) {
	t.Helper()
	cfgPath := filepath.Join(t.TempDir(), "config.yaml")
	cfg := &config.Config{
		Listen: ":0", RetentionDays: days,
		Keys: []*config.Key{{Key: "ar-x", Name: "pi", Allow: []string{"*"}}},
		Providers: []*config.Provider{
			{Name: "p", BaseURL: "https://unused.example/v1", Wire: "openai", Auth: config.AuthConf{Keys: []string{"k"}}},
		},
	}
	b, _ := json.Marshal(cfg)
	if err := os.WriteFile(cfgPath, b, 0o600); err != nil {
		t.Fatal(err)
	}
	cfg.Defaults()
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	p := proxy.NewProxy(provider.New(cfg), st, cfg.CostFor)
	srv := New(p, auth.NewKeyStore(cfg.Keys), st, cfgPath, "pw", "test")
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	admin := func(method, path string, body any) (int, string) {
		var rd io.Reader
		if body != nil {
			rd = bytes.NewReader([]byte(body.(string)))
		}
		req, _ := http.NewRequest(method, ts.URL+"/admin/api/"+path, rd)
		req.SetBasicAuth("", "pw")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(b)
	}
	return srv, cfgPath, admin
}

// DeleteOlderThan removes only rows older than the window and reports the count.
func TestDeleteOlderThan(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	now := time.Now()
	st.Submit(&store.Record{Ts: now.Add(-48 * time.Hour).UnixMilli(), Model: "old"})
	st.Submit(&store.Record{Ts: now.Add(-1 * time.Hour).UnixMilli(), Model: "fresh"})
	// rows flush on the 500ms batch tick — poll until both are visible
	deadline := time.Now().Add(3 * time.Second)
	for {
		rows, err := st.Recent(10, 0)
		if err != nil {
			t.Fatal(err)
		}
		if len(rows) == 2 || time.Now().After(deadline) {
			if len(rows) != 2 {
				t.Fatalf("expected 2 flushed rows, got %d", len(rows))
			}
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	n, err := st.DeleteOlderThan(24 * time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("pruned %d rows, want 1", n)
	}
	rows, _ := st.Recent(10, 0)
	if len(rows) != 1 || rows[0].Model != "fresh" {
		t.Fatalf("fresh row must survive: %+v", rows)
	}
	// 0/negative duration is a no-op
	if n, _ := st.DeleteOlderThan(0); n != 0 {
		t.Fatalf("zero window pruned %d rows", n)
	}
}

// PUT retention persists via mutate and shows up in the next config GET; the
// value survives a reload from disk. Negative days 400s through Validate.
func TestRetentionEndpoint(t *testing.T) {
	_, cfgPath, admin := retentionCfg(t, 0)

	if got := getConfigDays(t, admin); got != 0 {
		t.Fatalf("default retention = %d, want 0", got)
	}
	if code, body := admin("PUT", "retention", `{"days":7}`); code != 200 {
		t.Fatalf("PUT retention: %d %s", code, body)
	}
	if got := getConfigDays(t, admin); got != 7 {
		t.Fatalf("retention after PUT = %d, want 7", got)
	}
	code, body := admin("PUT", "retention", `{"days":-1}`)
	if code != 400 || !strings.Contains(body, "retention_days") {
		t.Fatalf("negative days: %d %s", code, body)
	}
	// persisted to disk: a fresh load still sees 7
	loaded, err := config.Load(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.RetentionDays != 7 {
		t.Fatalf("retention on disk = %d, want 7", loaded.RetentionDays)
	}
}

// getConfigDays reads retention_days from GET config via the admin client.
func getConfigDays(t *testing.T, admin func(string, string, any) (int, string)) int {
	t.Helper()
	code, body := admin("GET", "config", nil)
	if code != 200 {
		t.Fatalf("GET config: %d %s", code, body)
	}
	var dump struct {
		RetentionDays int `json:"retention_days"`
	}
	if err := json.Unmarshal([]byte(body), &dump); err != nil {
		t.Fatal(err)
	}
	return dump.RetentionDays
}

// StartRetentionPruner deletes expired rows when the tick fires with a
// non-zero configured window.
func TestRetentionPruner(t *testing.T) {
	srv, _, _ := retentionCfg(t, 1)
	st := srv.Store
	stop := srv.StartRetentionPruner(20 * time.Millisecond)
	t.Cleanup(stop)

	st.Submit(&store.Record{Ts: time.Now().Add(-48 * time.Hour).UnixMilli(), Model: "old"})
	deadline := time.Now().Add(3 * time.Second)
	for {
		rows, _ := st.Recent(10, 0)
		if len(rows) == 0 || time.Now().After(deadline) {
			if len(rows) != 0 {
				t.Fatalf("old row survived pruning: %+v", rows)
			}
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
}
