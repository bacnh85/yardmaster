package server

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"agent-router/internal/auth"
	"agent-router/internal/config"
	"agent-router/internal/provider"
	"agent-router/internal/proxy"
	"agent-router/internal/store"

	"gopkg.in/yaml.v3"
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

	// 2b. count_tokens requires auth too
	resp, _ = http.Post(ts.URL+"/v1/messages/count_tokens", "application/json",
		strings.NewReader(`{"model":"test-model","messages":[]}`))
	if resp.StatusCode != 401 {
		t.Fatalf("count_tokens want 401, got %d", resp.StatusCode)
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

	// 5b. /admin/api/config must not leak provider credentials
	req, _ = http.NewRequest("GET", ts.URL+"/admin/api/config", nil)
	req.SetBasicAuth("", "secretpw")
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	cfgBody, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if bytes.Contains(cfgBody, []byte("sk-up")) {
		t.Fatalf("config leaks upstream key: %s", cfgBody)
	}

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

// Nil Go slices marshal to JSON null; the dashboard does .map/.length on these
// arrays, so a null here blanks the whole tab (providers crash, 2026-09-16).
func TestAdminListEndpointsNeverReturnNullArrays(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "test.db")
	st, err := store.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	cfg := &config.Config{
		Listen: ":0",
		Keys:   []*config.Key{{Key: "ar-agent", Name: "pi", Allow: []string{"*"}}},
		Providers: []*config.Provider{
			// no Models, no OAuth accounts — both fields would be nil
			{Name: "empty", BaseURL: "http://127.0.0.1:1", Wire: "openai",
				Auth: config.AuthConf{Type: "static", Keys: []string{"sk-up"}}},
		},
	}
	cfg.Defaults()
	cfg.Validate()
	reg := provider.New(cfg)
	p := proxy.NewProxy(reg, st, cfg.CostFor)
	p.Version = "test"
	srv := New(p, auth.NewKeyStore(cfg.Keys), st, "", "secretpw", "test")
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	for _, path := range []string{"providers", "keys", "requests?limit=10", "summary?hours=1&bucket=hour", "breakdown?by=model"} {
		req, _ := http.NewRequest("GET", ts.URL+"/admin/api/"+path, nil)
		req.SetBasicAuth("", "secretpw")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		for _, field := range []string{`"models":null`, `"accounts":null`, `"keys":null`, `"requests":null`, `"series":null`, `"breakdown":null`} {
			if bytes.Contains(body, []byte(field)) {
				t.Errorf("%s: %s leaked into response", path, field)
			}
		}
	}

	// /v1/models has the same nil-slice hazard: an empty registry (only the
	// Models-less provider above) must emit {"data":[]} — OpenAI SDKs call
	// resp.data.map and crash on null.
	req, _ := http.NewRequest("GET", ts.URL+"/v1/models", nil)
	req.Header.Set("Authorization", "Bearer ar-agent")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	var body []byte
	body, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	if bytes.Contains(body, []byte(`"data":null`)) {
		t.Errorf("/v1/models: \"data\":null leaked into response: %s", body)
	}
}

// An unset/empty admin password must fail closed — subtle.ConstantTimeCompare
// on two empty slices returns 1, which would open the admin API to anyone.
func TestEmptyAdminPasswordFailsClosed(t *testing.T) {
	srv := New(nil, nil, nil, "", "", "test")
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	req, _ := http.NewRequest("GET", ts.URL+"/admin/api/summary", nil)
	req.SetBasicAuth("", "")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 401 {
		t.Fatalf("summary want 401, got %d", resp.StatusCode)
	}

	resp, err = http.Post(ts.URL+"/admin/api/login", "application/json", strings.NewReader(`{"password":""}`))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 401 {
		t.Fatalf("login want 401, got %d", resp.StatusCode)
	}
}

func TestConfigCrudEndpoints(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		b, _ := json.Marshal(oaiChunk(map[string]any{"content": "hi"}, nil))
		fmt.Fprintf(w, "data: %s\n\ndata: [DONE]\n\n", b)
		w.(http.Flusher).Flush()
	}))
	defer up.Close()

	cfgPath := filepath.Join(t.TempDir(), "config.yaml")
	cfg := &config.Config{
		Listen: ":0",
		Keys:   []*config.Key{{Key: "ar-agent", Name: "pi", Allow: []string{"*"}}},
		Providers: []*config.Provider{
			{Name: "mock", BaseURL: up.URL, Wire: "openai", Models: []string{"test-model"},
				Auth: config.AuthConf{Type: "static", Keys: []string{"sk-up"}},
				Session: "opencode", ExtraHeaders: map[string]string{"x-a": "b"}},
		},
	}
	b, _ := yaml.Marshal(cfg)
	if err := os.WriteFile(cfgPath, b, 0o600); err != nil {
		t.Fatal(err)
	}

	st, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	cfg.Defaults()
	cfg.Validate()
	p := proxy.NewProxy(provider.New(cfg), st, cfg.CostFor)
	p.Version = "test"
	srv := New(p, auth.NewKeyStore(cfg.Keys), st, cfgPath, "secretpw", "test")
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()
	admin := func(method, path string, body io.Reader) (int, []byte) {
		req, _ := http.NewRequest(method, ts.URL+"/admin/api/"+path, body)
		req.SetBasicAuth("", "secretpw")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, b
	}
	chat := func(model, key string) int {
		req, _ := http.NewRequest("POST", ts.URL+"/v1/chat/completions",
			strings.NewReader(`{"model":"`+model+`","stream":true,"messages":[]}`))
		req.Header.Set("Authorization", "Bearer "+key)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp.StatusCode
	}

	// keys: add -> usable immediately; delete -> rejected at once
	code, body := admin("POST", "keys", strings.NewReader(`{"name":"k2"}`))
	if code != 200 {
		t.Fatalf("add key: %d %s", code, body)
	}
	var added struct{ Key string }
	json.Unmarshal(body, &added)
	if !strings.HasPrefix(added.Key, "ar-") {
		t.Fatalf("generated key: %q", added.Key)
	}
	if code := chat("test-model", added.Key); code != 200 {
		t.Fatalf("new key rejected: %d", code)
	}
	if code, _ := admin("DELETE", "keys/pi", nil); code != 200 {
		t.Fatalf("delete key: %d", code)
	}
	if code := chat("test-model", "ar-agent"); code != 401 {
		t.Fatalf("deleted key still works: %d", code)
	}

	// regression: names with URL metacharacters must round-trip. Create accepts
	// any non-empty name; the dashboard encodes the name in the DELETE path —
	// if it ever sends a raw '?', the server truncates the name, delete returns
	// 400 "no key named", and the key is stuck in config.
	if code, b := admin("POST", "keys", strings.NewReader(`{"name":"a?b"}`)); code != 200 {
		t.Fatalf("add key a?b: %d %s", code, b)
	}
	if code, b := admin("DELETE", "keys/a%3Fb", nil); code != 200 {
		t.Fatalf("delete key a?b (encoded): %d %s", code, b)
	}
	if code, _ := admin("DELETE", "keys/a%3Fb", nil); code != 400 {
		t.Fatalf("key a?b should already be gone, got: %d", code)
	}

	// providers: add -> routable via models list; validate; delete
	// edit preserves advanced fields the form can't express
	if code, b := admin("PUT", "providers/mock", strings.NewReader(
		`{"wire":"openai","base_url":"`+up.URL+`","models":["test-model"],"keys":["sk-up"]}`)); code != 200 {
		t.Fatalf("edit provider: %d %s", code, b)
	}
	b2, err := os.ReadFile(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	for _, keep := range []string{"session: opencode", "x-a: b"} {
		if !bytes.Contains(b2, []byte(keep)) {
			t.Fatalf("edit lost %q:\n%s", keep, b2)
		}
	}
	form := `{"name":"mock2","wire":"openai","base_url":"` + up.URL + `","models":["m2"],"keys":["sk-up"]}`
	if code, b := admin("POST", "providers", strings.NewReader(form)); code != 200 {
		t.Fatalf("add provider: %d %s", code, b)
	}
	if code := chat("m2", added.Key); code != 200 {
		t.Fatalf("new provider not routable: %d", code)
	}
	if code, b := admin("PUT", "providers/mock2", strings.NewReader(`{"wire":"grpc","base_url":"`+up.URL+`"}`)); code != 400 {
		t.Fatalf("bad wire accepted: %d %s", code, b)
	}
	if code, b := admin("POST", "providers", strings.NewReader(form)); code != 400 {
		t.Fatalf("duplicate provider accepted: %d %s", code, b)
	}
	if code, _ := admin("DELETE", "providers/mock2", nil); code != 200 {
		t.Fatalf("delete provider: %d", code)
	}
	if code := chat("m2", added.Key); code == 200 {
		t.Fatal("provider still routable after delete")
	}

	// file on disk reflects the mutations
	b, err = os.ReadFile(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	for _, bad := range []string{"mock2", "name: pi", "ar-agent"} {
		if bytes.Contains(b, []byte(bad)) {
			t.Fatalf("config file still contains %q:\n%s", bad, b)
		}
	}
	if !bytes.Contains(b, []byte("name: mock")) {
		t.Fatalf("mock provider missing from file:\n%s", b)
	}
}
