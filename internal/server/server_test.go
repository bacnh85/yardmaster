package server

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"agent-router/internal/auth"
	"agent-router/internal/config"
	"agent-router/internal/provider"
	"agent-router/internal/proxy"
	"agent-router/internal/store"
)

func oaiChunk(delta map[string]any, finish any) map[string]any {
	return map[string]any{
		"id": "c", "object": "chat.completion.chunk", "model": "m",
		"choices": []any{map[string]any{"index": 0, "delta": delta, "finish_reason": finish}},
	}
}

func TestServerEndToEnd(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if ah := r.Header.Get("Authorization"); ah != "Bearer sk-up" {
			t.Errorf("upstream auth: %q", ah)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		b, _ := json.Marshal(oaiChunk(map[string]any{"content": "hi"}, nil))
		bu, _ := json.Marshal(map[string]any{"id": "c", "object": "chat.completion.chunk", "model": "m",
			"choices": []any{}, "usage": map[string]any{"prompt_tokens": 10, "completion_tokens": 1}})
		fmt.Fprintf(w, "data: %s\n\ndata: %s\n\ndata: [DONE]\n\n", b, bu)
		w.(http.Flusher).Flush()
	}))
	defer up.Close()

	dbPath := filepath.Join(t.TempDir(), "test.db")
	st, err := store.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	cfg := &config.Config{
		Listen: ":0",
		Costs:  map[string]*config.Cost{"test-model": {Input: 1, Output: 2}},
		Keys:   []*config.Key{{Key: "ar-agent", Name: "pi", Allow: []string{"*"}}},
		Providers: []*config.Provider{
			{Name: "mock", BaseURL: up.URL, Wire: "openai", Models: []string{"test-model"},
				Auth: config.AuthConf{Type: "static", Keys: []string{"sk-up"}}}},
	}
	cfg.Defaults()
	cfg.Validate()
	reg := provider.New(cfg)
	p := proxy.NewProxy(reg, st, cfg.CostFor)
	p.Version = "test"
	srv := New(p, auth.NewKeyStore(cfg.Keys), st, "", "secretpw", "test")

	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	// 1. no key → 401
	resp, _ := http.Post(ts.URL+"/v1/chat/completions", "application/json",
		strings.NewReader(`{"model":"test-model"}`))
	if resp.StatusCode != 401 {
		t.Fatalf("want 401, got %d", resp.StatusCode)
	}
	resp.Body.Close()

	// 2. bad key → 401
	req, _ := http.NewRequest("POST", ts.URL+"/v1/chat/completions", strings.NewReader(`{"model":"test-model","stream":true}`))
	req.Header.Set("Authorization", "Bearer ar-wrong")
	resp, _ = http.DefaultClient.Do(req)
	if resp.StatusCode != 401 {
		t.Fatalf("want 401, got %d", resp.StatusCode)
	}
	resp.Body.Close()

	// 3. good key → stream
	req, _ = http.NewRequest("POST", ts.URL+"/v1/chat/completions",
		strings.NewReader(`{"model":"test-model","stream":true,"messages":[]}`))
	req.Header.Set("Authorization", "Bearer ar-agent")
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != 200 {
		t.Fatalf("status %d", resp.StatusCode)
	}
	body := make([]byte, 4096)
	resp.Body.Read(body)
	resp.Body.Close()
	if !bytes.Contains(body, []byte(`"content":"hi"`)) {
		t.Fatalf("stream body: %s", body)
	}

	// 4. /v1/models
	req, _ = http.NewRequest("GET", ts.URL+"/v1/models", nil)
	req.Header.Set("Authorization", "Bearer ar-agent")
	resp, _ = http.DefaultClient.Do(req)
	var models struct {
		Data []struct{ ID string }
	}
	json.NewDecoder(resp.Body).Decode(&models)
	resp.Body.Close()
	if len(models.Data) != 1 || models.Data[0].ID != "test-model" {
		t.Fatalf("models: %v", models.Data)
	}

	// 5. admin API requires auth
	resp, _ = http.Get(ts.URL + "/admin/api/summary")
	if resp.StatusCode != 401 {
		t.Fatalf("admin want 401, got %d", resp.StatusCode)
	}
	resp.Body.Close()

	// with basic auth
	req, _ = http.NewRequest("GET", ts.URL+"/admin/api/summary?hours=1", nil)
	req.SetBasicAuth("", "secretpw")
	resp, _ = http.DefaultClient.Do(req)
	if resp.StatusCode != 200 {
		t.Fatalf("admin summary: %d", resp.StatusCode)
	}
	var out struct {
		Summary store.Summary `json:"summary"`
	}
	json.NewDecoder(resp.Body).Decode(&out)
	resp.Body.Close()

	// 6. wait for the async writer to flush the request record
	// (401s are rejected before the proxy, models isn't recorded — 1 record total)
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		sum, err := st.SummarySince(time.Hour, "hour")
		if err == nil && sum.Requests >= 1 {
			if sum.TokIn != 10 || sum.TokOut != 1 {
				t.Fatalf("tokens: in=%d out=%d", sum.TokIn, sum.TokOut)
			}
			if sum.CostUSD == 0 {
				t.Fatal("cost not computed")
			}
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatal("records never landed in store")
}
