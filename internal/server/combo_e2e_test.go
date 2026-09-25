package server

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
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

// End-to-end through the real serve path: combo dispatch fails over A→B
// combo dispatch fails over A→B with the member model override, /v1/models
// advertises the combo id, and ComboUsageSince logs the served member.
func TestComboE2E(t *testing.T) {
	var gotModel string
	upA := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(503)
	}))
	defer upA.Close()
	upB := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct{ Model string `json:"model"` }
		json.NewDecoder(r.Body).Decode(&req)
		gotModel = req.Model
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"id":"1","object":"chat.completion","created":1,"model":"x","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":5,"completion_tokens":2,"total_tokens":7}}`)
	}))
	defer upB.Close()

	cfg := &config.Config{
		Listen: ":0",
		Keys:   []*config.Key{{Key: "ar-test", Name: "pi", Allow: []string{"*"}}},
		Providers: []*config.Provider{
			{Name: "ocg", BaseURL: upA.URL, Wire: "openai", Models: []string{"deepseek-v4-flash"}, Auth: config.AuthConf{Keys: []string{"oc1"}}},
			{Name: "cmd", BaseURL: upB.URL, Wire: "openai", Models: []string{"deepseek/deepseek-v4.1-flash"}, Auth: config.AuthConf{Keys: []string{"cc1"}}},
		},
		Combos: []*config.Combo{{
			Name: "deepseek-v4.1-flash", Model: "deepseek-v4.1-flash",
			Members: []*config.ComboMember{
				{Provider: "ocg", Model: "deepseek-v4-flash"},
				{Provider: "cmd", Model: "deepseek/deepseek-v4.1-flash"},
			},
		}},
	}
	st, err := store.Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	p := proxy.NewProxy(provider.New(cfg), st, cfg.CostFor)
	p.Version = "e2e"
	srv := New(p, auth.NewKeyStore(cfg.Keys), st, "", "pw", "e2e")
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()
	client := &http.Client{Timeout: 10 * time.Second}

	// 1. /v1/models advertises the combo
	req0, _ := http.NewRequest("GET", ts.URL+"/v1/models", nil)
	req0.Header.Set("Authorization", "Bearer ar-test")
	resp, err := client.Do(req0)
	if err != nil || resp.StatusCode != 200 {
		t.Fatalf("models: %v %v", err, resp)
	}
	buf := make([]byte, 1<<16)
	n, _ := resp.Body.Read(buf)
	resp.Body.Close()
	if !strings.Contains(string(buf[:n]), "combo/deepseek-v4.1-flash") {
		t.Fatalf("combo id missing from /v1/models: %s", buf[:n])
	}

	// 2. chat completion through the combo: A 503s, B serves with member id
	req, _ := http.NewRequest("POST", ts.URL+"/v1/chat/completions", strings.NewReader(
		`{"model":"combo/deepseek-v4.1-flash","messages":[{"role":"user","content":"hi"}]}`))
	req.Header.Set("Authorization", "Bearer ar-test")
	resp, err = client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	n, _ = resp.Body.Read(buf)
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("combo chat: %d %s", resp.StatusCode, buf[:n])
	}
	if gotModel != "deepseek/deepseek-v4.1-flash" {
		t.Fatalf("upstream B got model %q, want the member's upstream id", gotModel)
	}

	// 3. analytics: one combo request logged, served by cmd
	time.Sleep(1200 * time.Millisecond) // async batch writer flush
	rows, err := st.ComboUsageSince(time.Hour)
	if err != nil || len(rows) != 1 {
		t.Fatalf("combo usage: %d rows, err %v", len(rows), err)
	}
	r := rows[0]
	if r.ComboID != "combo/deepseek-v4.1-flash" || r.Provider != "cmd" || r.Requests != 1 || r.Failovers != 1 { // attempt 2 = one skipped member try
		t.Fatalf("unexpected usage row: %+v", r)
	}
}
