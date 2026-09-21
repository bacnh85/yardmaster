package server

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/bacnh85/yardmaster/internal/auth"
	"github.com/bacnh85/yardmaster/internal/config"
	"github.com/bacnh85/yardmaster/internal/provider"
	"github.com/bacnh85/yardmaster/internal/proxy"
	"github.com/bacnh85/yardmaster/internal/store"

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
				Auth:    config.AuthConf{Type: "static", Keys: []string{"sk-up"}},
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
	// (session is now form-expressible: the PUT must send it explicitly)
	if code, b := admin("PUT", "providers/mock", strings.NewReader(
		`{"wire":"openai","base_url":"`+up.URL+`","models":["test-model"],"keys":["sk-up"],"session":"opencode"}`)); code != 200 {
		t.Fatalf("edit provider: %d %s", code, b)
	}
	if code, b := admin("GET", "providers", nil); code != 200 || !bytes.Contains(b, []byte(`"session":"opencode"`)) {
		t.Fatalf("providers GET missing session prefill: %d %s", code, b)
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
	// regression (review): a PUT that omits "session" must keep the stored
	// session headers — older API clients send the previous payload shape
	if code, b := admin("PUT", "providers/mock", strings.NewReader(
		`{"wire":"openai","base_url":"`+up.URL+`","models":["test-model"],"keys":["sk-up"]}`)); code != 200 {
		t.Fatalf("edit provider (no session): %d %s", code, b)
	}
	b3, err := os.ReadFile(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(b3, []byte("session: opencode")) {
		t.Fatalf("PUT without session cleared it:\n%s", b3)
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

// Regression: editing an oauth provider from the dashboard must not downgrade
// it to static auth — the edit form cannot express auth.type at all.
func TestProviderPutPreservesOAuth(t *testing.T) {
	cfgPath := filepath.Join(t.TempDir(), "config.yaml")
	cfg := &config.Config{
		Listen: ":0",
		Keys:   []*config.Key{{Key: "ar-agent", Name: "pi", Allow: []string{"*"}}},
		Providers: []*config.Provider{
			{Name: "claude-sub", BaseURL: "https://api.anthropic.com", Wire: "anthropic",
				Auth: config.AuthConf{Type: "oauth", OAuth: []*config.OAuthAcct{
					{Name: "claude-main", Kind: "claude-code", RefreshTok: "rt-x"},
				}}},
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
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	p := proxy.NewProxy(provider.New(cfg), st, cfg.CostFor)
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

	// form edit: wire/models/toggles only — no auth fields in the payload
	code, body := admin("PUT", "providers/claude-sub", strings.NewReader(
		`{"name":"claude-sub","wire":"anthropic","base_url":"https://api.anthropic.com","models":["claude-sonnet-5"],"dispatch_interval_ms":0,"adaptive_thinking":false,"inject_cache_control":false}`))
	if code != 200 {
		t.Fatalf("PUT provider: %d %s", code, body)
	}

	loaded, err := config.Load(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	got := loaded.Providers[0]
	if got.Auth.Type != "oauth" {
		t.Fatalf("PUT downgraded auth.type to %q", got.Auth.Type)
	}
	if len(got.Auth.OAuth) != 1 || got.Auth.OAuth[0].Name != "claude-main" {
		t.Fatalf("oauth accounts lost: %+v", got.Auth.OAuth)
	}

	// admin view agrees
	code, body = admin("GET", "providers", nil)
	if code != 200 {
		t.Fatalf("GET providers: %d", code)
	}
	if !strings.Contains(string(body), `"auth_type":"oauth"`) {
		t.Fatalf("admin providers lost oauth type: %s", body)
	}
	if !strings.Contains(string(body), `"name":"claude-main"`) {
		t.Fatalf("admin providers lost account: %s", body)
	}
}

// models.dev enrichment: npm → wire family mapping + cost/context merge.
func TestCatalogEnrichment(t *testing.T) {
	stub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"opencode":{"models":{
			"m-chat":{"reasoning":true,"tool_call":true,"limit":{"context":128000},"cost":{"input":0.3,"output":1.2}},
			"m-claude":{"limit":{"context":1000000},"cost":{"input":2,"output":10,"cache_read":0.2},"provider":{"npm":"@ai-sdk/anthropic"}},
			"m-gpt":{"cost":{"input":1},"provider":{"npm":"@ai-sdk/openai"}},
			"m-gem":{"provider":{"npm":"@ai-sdk/google"}}
		}},
		"xai":{"models":{
			"grok-4.6":{"limit":{"context":200000},"cost":{"input":2,"output":6},"provider":{"npm":"@ai-sdk/openai"}}
		}}}`)
	}))
	defer stub.Close()
	oldURL := modelsDevURL
	modelsDevURL = stub.URL
	resetModelsDevCache()
	t.Cleanup(func() { modelsDevURL = oldURL; resetModelsDevCache() })

	// "m.gpt" exercises the dot↔dash canonical join (upstream may use dots
	// where models.dev uses dashes and vice versa)
	enrichCatalog("https://opencode.ai/zen/v1", ids("zzz-unknown", "m-chat", "m-claude", "m.gpt", "m-gem"))
	enrichCatalog("https://opencode.ai/zen/go/v1", ids("grok-4.6"))
	e, ok := catalogCache.Load("https://opencode.ai/zen/v1")
	if !ok {
		t.Fatal("no cache entry")
	}
	models := e.(catalogEntry).models
	byID := map[string]ModelMeta{}
	for _, mm := range models {
		byID[mm.ID] = mm
	}
	if len(models) != 5 || models[0].ID != "m-chat" {
		t.Fatalf("ids not sorted/complete: %+v", models)
	}
	if byID["m-chat"].Family != "chat" || !byID["m-chat"].Reasoning || !byID["m-chat"].ToolCall ||
		byID["m-chat"].Context != 128000 || byID["m-chat"].Input != 0.3 {
		t.Fatalf("m-chat meta: %+v", byID["m-chat"])
	}
	if byID["m-claude"].Family != "anthropic" || byID["m-claude"].Input != 2 || byID["m-claude"].CacheRead != 0.2 {
		t.Fatalf("m-claude meta: %+v", byID["m-claude"])
	}
	if byID["m.gpt"].Family != "responses" {
		t.Fatalf("m-gpt meta: %+v", byID["m-gpt"])
	}
	if byID["m-gem"].Family != "gemini" {
		t.Fatalf("m-gem meta: %+v", byID["m-gem"])
	}
	if byID["zzz-unknown"].Family != "" || byID["zzz-unknown"].Context != 0 {
		t.Fatalf("unknown meta: %+v", byID["zzz-unknown"])
	}
	// cross-provider fallback: grok-4.6 is filed under xai, not opencode —
	// limits/pricing flow through, family must NOT (wrong wire grouping)
	ge, ok := catalogCache.Load("https://opencode.ai/zen/go/v1")
	if !ok {
		t.Fatal("no go cache entry")
	}
	grok := ge.(catalogEntry).models[0]
	if grok.ID != "grok-4.6" || grok.Context != 200000 || grok.Input != 2 || grok.Family != "" {
		t.Fatalf("grok fallback meta: %+v", grok)
	}
}

// zen/go base URLs must read the opencode-go models.dev entry (Go-specific
// pricing), and cross-provider 0/0 "cost" (token-plan bundles) is unknown
// pricing — never "free".
func TestCatalogOpenCodeGoPricing(t *testing.T) {
	stub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"opencode":{"models":{
			"qwen3.8-max":{"cost":{"input":9,"output":9}}
		}},
		"opencode-go":{"models":{
			"qwen3.8-max":{"cost":{"input":2,"output":6}},
			"tiered":{"cost":{"input":0.3,"output":1.2,"cache_read":0.06,"tiers":[{"input":0.6}],"context_over_200k":{"input":0.6}}}
		}},
		"bundlevendor":{"models":{
			"mystery-model":{"limit":{"context":500000},"cost":{"input":0,"output":0}}
		}}}`)
	}))
	defer stub.Close()
	oldURL := modelsDevURL
	modelsDevURL = stub.URL
	resetModelsDevCache()
	t.Cleanup(func() { modelsDevURL = oldURL; resetModelsDevCache() })

	enrichCatalog("https://opencode.ai/zen/go/v1", ids("qwen3.8-max", "mystery-model", "tiered"))
	e, ok := catalogCache.Load("https://opencode.ai/zen/go/v1")
	if !ok {
		t.Fatal("no cache entry")
	}
	byID := map[string]ModelMeta{}
	for _, mm := range e.(catalogEntry).models {
		byID[mm.ID] = mm
	}
	if q := byID["qwen3.8-max"]; q.Input != 2 || q.Output != 6 || q.Free {
		t.Fatalf("go pricing must come from the opencode-go entry: %+v", q)
	}
	if m := byID["mystery-model"]; m.Free || m.Input != -1 || m.Output != -1 {
		t.Fatalf("flat 0/0 cost must price as unknown, not free: %+v", m)
	}
	// tiered cost maps (arrays/objects alongside the numbers) must still parse
	if t2 := byID["tiered"]; t2.Input != 0.3 || t2.Output != 1.2 || t2.CacheRead != 0.06 {
		t.Fatalf("tiered cost must keep numeric fields: %+v", t2)
	}

	// without /go the plain opencode entry still applies
	enrichCatalog("https://opencode.ai/zen/v1", ids("qwen3.8-max"))
	e2, ok := catalogCache.Load("https://opencode.ai/zen/v1")
	if !ok {
		t.Fatal("no v1 cache entry")
	}
	if q := e2.(catalogEntry).models[0]; q.Input != 9 {
		t.Fatalf("v1 pricing must come from the opencode entry: %+v", q)
	}
}

// catalog warming must not reorder the provider's live config Models slice —
// it is shared with routing and config saves, and enrichCatalog sorts in place.
func TestCatalogDoesNotMutateConfigModels(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "test.db")
	st, err := store.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	cfg := &config.Config{
		Providers: []*config.Provider{{Name: "ocg", BaseURL: "https://127.0.0.1:1", Wire: "openai",
			Models: []string{"zz-model", "aa-model"}, Auth: config.AuthConf{Type: "static", Keys: []string{"sk"}}}},
	}
	cfg.Defaults()
	cfg.Validate()
	reg := provider.New(cfg)
	s := New(proxy.NewProxy(reg, st, cfg.CostFor), auth.NewKeyStore(cfg.Keys), st, "", "pw", "test")

	stub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, `{}`) // models.dev knows nothing → pv.Models fallback path
	}))
	defer stub.Close()
	oldURL := modelsDevURL
	modelsDevURL = stub.URL
	resetModelsDevCache()
	t.Cleanup(func() { modelsDevURL = oldURL; resetModelsDevCache() })

	models, err := s.catalog(context.Background(), "ocg") // upstream :1 is unreachable → fallback
	if err != nil {
		t.Fatal(err)
	}
	if len(models) != 2 || models[0].ID != "aa-model" {
		t.Fatalf("catalog models: %+v", models)
	}
	if got := cfg.Providers[0].Models; got[0] != "zz-model" || got[1] != "aa-model" {
		t.Fatalf("live config Models slice was reordered: %v", got)
	}
}

// an empty catalog (transient upstream failure) must be cached only briefly —
// a 1h TTL would stick the detail page at 0 models with no bypass
func TestCatalogEmptyShortTTL(t *testing.T) {
	stub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, `{}`)
	}))
	defer stub.Close()
	oldURL := modelsDevURL
	modelsDevURL = stub.URL
	resetModelsDevCache()
	t.Cleanup(func() { modelsDevURL = oldURL; resetModelsDevCache() })

	enrichCatalog("https://empty.example/v1", nil)
	e, ok := catalogCache.Load("https://empty.example/v1")
	if !ok {
		t.Fatal("no cache entry")
	}
	if until := e.(catalogEntry).until; until.After(time.Now().Add(3 * time.Minute)) {
		t.Fatalf("empty catalog cached too long: %v", until)
	}
}

// the cross-provider flat index must pick the same winner for duplicate ids on
// every load (provider map iteration order is randomized)
func TestCatalogFlatDeterministic(t *testing.T) {
	stub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"zvendor":{"models":{"dup-model":{"cost":{"input":2,"output":6}}}},
		"avendor":{"models":{"dup-model":{"cost":{"input":1,"output":3}}}}}`)
	}))
	defer stub.Close()
	oldURL := modelsDevURL
	modelsDevURL = stub.URL
	t.Cleanup(func() { modelsDevURL = oldURL; resetModelsDevCache() })

	for i := 0; i < 3; i++ {
		resetModelsDevCache() // force a fresh models.dev load each round
		enrichCatalog("https://dup.example/v1", ids("dup-model"))
		e, _ := catalogCache.Load("https://dup.example/v1")
		if got := e.(catalogEntry).models[0]; got.Input != 1 || got.Output != 3 {
			t.Fatalf("round %d: nondeterministic flat winner: %+v", i, got)
		}
	}
}

func resetModelsDevCache() {
	modelsDevMu.Lock()
	modelsDevData, modelsDevUntil = nil, time.Time{}
	modelsDevFlat = nil
	modelsDevMu.Unlock()
}

// ids builds bare-id ModelMeta slices for enrichCatalog test calls.
func ids(ss ...string) []ModelMeta {
	out := make([]ModelMeta, len(ss))
	for i, s := range ss {
		out[i] = ModelMeta{ID: s}
	}
	return out
}

// Upstream /models metadata (CommandCode-style): supported_endpoints drives
// the wire family and beats an npm-family guess, name/context fill in without
// models.dev, and vendor-namespaced ids price via the bare id under any
// vendor (upstream context stays authoritative).
func TestUpstreamModelsEndpoints(t *testing.T) {
	dev := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"somevendor":{"models":{
			"claude-style":{"cost":{"input":1,"output":2},"provider":{"npm":"@ai-sdk/openai"}},
			"glm-test":{"cost":{"input":0.15,"output":0.5},"limit":{"context":999}}
		}}}`)
	}))
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/models" {
			t.Errorf("unexpected upstream path %q", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"object":"list","data":[
			{"id":"claude-style","name":"Claude Style","context_length":1000000,"supported_endpoints":["/messages"]},
			{"id":"gpt-style","name":"GPT Style","context_length":1050000,"supported_endpoints":["/chat/completions","/responses"]},
			{"id":"z-ai/glm-test","name":"GLM Test","context_length":1000000,"supported_endpoints":["/chat/completions"]},
			{"id":"resp-only","name":"Resp Only","supported_endpoints":["/responses"]},
			{"id":"plain","name":"Plain"}
		]}`)
	}))
	defer up.Close()
	oldURL := modelsDevURL
	modelsDevURL = dev.URL
	resetModelsDevCache()
	t.Cleanup(func() { modelsDevURL = oldURL; resetModelsDevCache() })

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
			{Name: "cmdcode", BaseURL: up.URL, Wire: "openai",
				Auth: config.AuthConf{Type: "static", Keys: []string{"sk-cc"}}},
		},
	}
	cfg.Defaults()
	cfg.Validate()
	p := proxy.NewProxy(provider.New(cfg), st, cfg.CostFor)
	p.Version = "test"
	srv := New(p, auth.NewKeyStore(cfg.Keys), st, "", "pw", "test")

	metas, err := srv.catalog(context.Background(), "cmdcode")
	if err != nil {
		t.Fatal(err)
	}
	byID := map[string]ModelMeta{}
	for _, mm := range metas {
		byID[mm.ID] = mm
	}
	c := byID["claude-style"]
	if c.Family != "anthropic" || c.Name != "Claude Style" || c.Context != 1000000 || c.Input != 1 {
		t.Fatalf("claude-style: endpoint family must beat npm + dev pricing merge: %+v", c)
	}
	if g := byID["gpt-style"]; g.Family != "chat" || g.Context != 1050000 {
		t.Fatalf("gpt-style: %+v", g)
	}
	z := byID["z-ai/glm-test"]
	if z.Family != "chat" || z.Input != 0.15 || z.Name != "GLM Test" || z.Context != 1000000 {
		t.Fatalf("z-ai/glm-test: namespaced bare-id pricing + upstream context: %+v", z)
	}
	if r := byID["resp-only"]; r.Family != "responses" {
		t.Fatalf("resp-only: %+v", r)
	}
	if pl := byID["plain"]; pl.Family != "" || pl.Name != "Plain" {
		t.Fatalf("plain (no endpoints): %+v", pl)
	}
}

// enrichCatalog runs concurrently (admin handler + WarmCatalogs) and must not
// read the models.dev package vars unlocked — run under -race in CI.
func TestCatalogConcurrent(t *testing.T) {
	stub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"opencode":{"models":{"m-chat":{"limit":{"context":128000}}}}}`)
	}))
	defer stub.Close()
	oldURL := modelsDevURL
	modelsDevURL = stub.URL
	resetModelsDevCache()
	t.Cleanup(func() { modelsDevURL = oldURL; resetModelsDevCache() })

	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 25; j++ {
				resetModelsDevCache() // force the fetch+assign path mid-flight
				enrichCatalog("https://opencode.ai/zen/v1", ids("m-chat", "zzz"))
			}
		}()
	}
	wg.Wait()
	// assertion (value without -race): the last write wins with intact data
	e, ok := catalogCache.Load("https://opencode.ai/zen/v1")
	if !ok {
		t.Fatal("no cache entry after concurrent enrich")
	}
	models := e.(catalogEntry).models
	if len(models) != 2 || models[0].ID != "m-chat" || models[0].Context != 128000 || models[1].ID != "zzz" {
		t.Fatalf("concurrent enrich left corrupt cache: %+v", models)
	}
}

// /v1/models must speak the pi-router extension's dialect: prefixed-only ids,
// context_length + capabilities.vision from the cached catalog.
func TestV1ModelsFields(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "test.db")
	st, err := store.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	cfg := &config.Config{
		Listen: ":0",
		Keys:   []*config.Key{{Key: "ar-agent", Name: "pi", Allow: []string{"*"}}},
		Providers: []*config.Provider{{
			Name: "zen", Prefix: "ocg", BaseURL: "https://opencode.ai/zen/v1", Wire: "openai",
			Models: []string{"m-chat", "m-vision"},
			Auth:   config.AuthConf{Type: "static", Keys: []string{"k"}}}},
	}
	cfg.Defaults()
	cfg.Validate()
	reg := provider.New(cfg)
	srv := New(proxy.NewProxy(reg, st, cfg.CostFor), auth.NewKeyStore(cfg.Keys), st, "", "", "test")
	// seed the cache as the warm loop / detail page would
	catalogCache.Store("https://opencode.ai/zen/v1", catalogEntry{models: []ModelMeta{
		{ID: "m-chat", Context: 1000000, MaxOutput: 131072},
		{ID: "m-vision", Context: 200000, MaxOutput: 8192, Image: true},
	}})

	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()
	req, _ := http.NewRequest("GET", ts.URL+"/v1/models", nil)
	req.Header.Set("Authorization", "Bearer ar-agent")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var got struct {
		Data []struct {
			ID            string `json:"id"`
			ContextLength int    `json:"context_length"`
			MaxOutTokens  int    `json:"max_output_tokens"`
			Capabilities  *struct {
				Vision bool `json:"vision"`
			} `json:"capabilities"`
			Pricing map[string]any `json:"pricing"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatal(err)
	}
	byID := map[string]int{}
	for i, m := range got.Data {
		byID[m.ID] = i
	}
	chat, ok := byID["ocg/m-chat"]
	if !ok {
		t.Fatalf("prefixed id missing: %+v", got.Data)
	}
	if got.Data[chat].ContextLength != 1000000 || got.Data[chat].MaxOutTokens != 131072 {
		t.Errorf("ocg/m-chat enrichment: %+v", got.Data[chat])
	}
	if got.Data[chat].Capabilities != nil && got.Data[chat].Capabilities.Vision {
		t.Errorf("non-vision model must not claim vision: %+v", got.Data[chat])
	}
	vis, ok := byID["ocg/m-vision"]
	if !ok || got.Data[vis].Capabilities == nil || !got.Data[vis].Capabilities.Vision {
		t.Errorf("ocg/m-vision must carry capabilities.vision: %+v", got.Data)
	}
	for _, m := range got.Data {
		if m.Pricing != nil {
			t.Errorf("pricing must be gone: %+v", m)
		}
	}
}

// Playground (all three wire families) + per-key connection management.
func TestPlaygroundAndProviderKeys(t *testing.T) {
	// hermetic: never hit models.dev from unit tests (review finding)
	stubDev := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{}`))
	}))
	defer stubDev.Close()
	oldURL := modelsDevURL
	modelsDevURL = stubDev.URL
	resetModelsDevCache()
	t.Cleanup(func() { modelsDevURL = oldURL; resetModelsDevCache() })

	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/models") {
			if r.URL.Path == "/v1/models" { // openai-wire provider: Bearer
				if r.Header.Get("Authorization") != "Bearer sk-chatkey-aaaa" {
					t.Errorf("models auth header: %q", r.Header.Get("Authorization"))
				}
			} else { // anthropic-wire provider: x-api-key + anthropic-version
				if r.Header.Get("x-api-key") != "sk-akey-bbbb" {
					t.Errorf("anthropic models auth header: %q", r.Header.Get("x-api-key"))
				}
				if r.Header.Get("anthropic-version") == "" {
					t.Errorf("anthropic-version header missing on /models fetch")
				}
			}
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprintf(w, `{"data":[{"id":"m-chat"},{"id":"m-claude"},{"id":"m-gpt"}]}`)
			return
		}
		var body map[string]any
		json.NewDecoder(r.Body).Decode(&body)
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/v1/chat/completions":
			if body["stream"] != false {
				t.Errorf("playground must be non-streaming")
			}
			if body["model"] != "m-chat" {
				t.Errorf("model patch: %v", body["model"])
			}
			fmt.Fprintf(w, `{"choices":[{"message":{"role":"assistant","content":"hola-chat"}}],
				"usage":{"prompt_tokens":7,"completion_tokens":3}}`)
		case "/v1/messages":
			if r.Header.Get("x-api-key") != "sk-akey-bbbb" {
				t.Errorf("anthropic auth header: %q", r.Header.Get("x-api-key"))
			}
			fmt.Fprintf(w, `{"type":"message","id":"m1","role":"assistant","content":[{"type":"text","text":"hola-anthropic"}],
				"usage":{"input_tokens":5,"output_tokens":2}}`)
		case "/responses":
			if r.Header.Get("Authorization") != "Bearer sk-rkey-cccc" {
				t.Errorf("responses auth header: %q", r.Header.Get("Authorization"))
			}
			fmt.Fprintf(w, `{"id":"r1","model":"m-gpt","status":"completed",
				"output":[{"type":"message","content":[{"type":"output_text","text":"hola-responses"}]}],
				"usage":{"input_tokens":4,"output_tokens":2}}`)
		default:
			http.NotFound(w, r)
		}
	})
	up := httptest.NewServer(mux)
	defer up.Close()

	cfgPath := filepath.Join(t.TempDir(), "config.yaml")
	cfg := &config.Config{
		Listen: ":0",
		Keys:   []*config.Key{{Key: "ar-agent", Name: "pi", Allow: []string{"*"}}},
		Providers: []*config.Provider{
			{Name: "zen-chat", BaseURL: up.URL + "/v1", Wire: "openai", Models: []string{"m-chat"},
				Auth: config.AuthConf{Type: "static", Keys: []string{"sk-chatkey-aaaa"}}},
			{Name: "zen-anthropic", BaseURL: up.URL, Wire: "anthropic", Models: []string{"m-claude"},
				Auth: config.AuthConf{Type: "static", Keys: []string{"sk-akey-bbbb"}}},
			{Name: "zen-responses", BaseURL: up.URL, Wire: "responses", Models: []string{"m-gpt"},
				Auth: config.AuthConf{Type: "static", Keys: []string{"sk-rkey-cccc"}}},
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
	p := proxy.NewProxy(provider.New(cfg), st, func(string) config.Cost { return config.Cost{Input: 1, Output: 2} })
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
	type probeResp struct {
		Text   string `json:"text"`
		TokIn  int    `json:"tok_in"`
		TokOut int    `json:"tok_out"`
	}
	probe := func(provider, model string) (int, probeResp) {
		body, _ := json.Marshal(map[string]any{"provider": provider, "model": model, "prompt": "hi"})
		code, b := admin("POST", "playground", bytes.NewReader(body))
		var res probeResp
		if code == 200 {
			json.Unmarshal(b, &res)
		}
		return code, res
	}

	if code, res := probe("zen-chat", "m-chat"); code != 200 || res.Text != "hola-chat" ||
		res.TokIn != 7 || res.TokOut != 3 {
		t.Fatalf("chat probe: %d %+v", code, res)
	}
	if code, res := probe("zen-anthropic", "m-claude"); code != 200 || res.Text != "hola-anthropic" ||
		res.TokIn != 5 || res.TokOut != 2 {
		t.Fatalf("anthropic probe: %d %+v", code, res)
	}
	if code, res := probe("zen-responses", "m-gpt"); code != 200 || res.Text != "hola-responses" ||
		res.TokIn != 4 || res.TokOut != 2 {
		t.Fatalf("responses probe: %d %+v", code, res)
	}

	// catalog endpoint passes upstream ids through (enrichment is base-url keyed)
	if code, b := admin("GET", "providers/zen-chat/models", nil); code != 200 ||
		!strings.Contains(string(b), `"id":"m-claude"`) {
		t.Fatalf("catalog: %d %s", code, b)
	}
	if code, b := admin("GET", "providers/zen-anthropic/models", nil); code != 200 ||
		!strings.Contains(string(b), `"id":"m-chat"`) {
		t.Fatalf("anthropic catalog: %d %s", code, b)
	}

	// connections: add a key, list masked, remove by suffix
	if code, b := admin("POST", "providers/zen-chat/keys", strings.NewReader(`{"key":"sk-second-key-9999"}`)); code != 200 {
		t.Fatalf("add key: %d %s", code, b)
	}
	code, b := admin("GET", "providers", nil)
	if code != 200 || !bytes.Contains(b, []byte(`…y-9999`)) || !bytes.Contains(b, []byte(`…y-aaaa`)) {
		t.Fatalf("key_suffixes missing: %d %s", code, b)
	}
	// duplicate-suffix keys: add a key sharing the FIRST key's 6-char tail —
	// a first-suffix-match delete would remove index 0, index delete removes
	// exactly the key the user clicked (reviewer acceptance criterion)
	if code, b := admin("POST", "providers/zen-chat/keys", strings.NewReader(`{"key":"dup2-y-aaaa"}`)); code != 200 {
		t.Fatalf("add dup-suffix key: %d %s", code, b)
	}
	if code, b := admin("DELETE", "providers/zen-chat/keys/2", nil); code != 200 {
		t.Fatalf("remove key idx 2: %d %s", code, b)
	}
	// the GET cannot distinguish identical suffixes — assert on raw config keys
	raw, err := os.ReadFile(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(raw, []byte("sk-chatkey-aaaa")) || !bytes.Contains(raw, []byte("sk-second-key-9999")) {
		t.Fatalf("wrong key removed (config):\n%s", raw)
	}
	if bytes.Contains(raw, []byte("dup2-y-aaaa")) {
		t.Fatalf("clicked key not removed (config):\n%s", raw)
	}
	code, b = admin("GET", "providers", nil)
	if code != 200 {
		t.Fatalf("providers after remove: %d", code)
	}
	if n := bytes.Count(b, []byte(`…y-aaaa`)); n != 1 {
		t.Fatalf("dup-suffix keys: want exactly 1 remaining …y-aaaa, got %d in %s", n, b)
	}
}

// POST /v1/responses: auth-gated like the other wires, routes to the upstream
// with the chat-hub normalization, and returns native Responses output.
func TestResponsesRoute(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req map[string]any
		json.NewDecoder(r.Body).Decode(&req)
		if req["messages"] == nil {
			t.Errorf("upstream want chat req, got %v", req)
		}
		json.NewEncoder(w).Encode(map[string]any{
			"id": "cmpl-x", "object": "chat.completion", "model": "m",
			"choices": []any{map[string]any{"index": 0, "finish_reason": "stop",
				"message": map[string]any{"role": "assistant", "content": "pong"}}},
			"usage": map[string]any{"prompt_tokens": 3, "completion_tokens": 1},
		})
	}))
	defer up.Close()

	st, err := store.Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	cfg := &config.Config{
		Listen: ":0",
		Keys:   []*config.Key{{Key: "ar-agent", Name: "pi", Allow: []string{"*"}}},
		Providers: []*config.Provider{
			{Name: "mock", BaseURL: up.URL, Wire: "openai", Models: []string{"test-model"},
				Auth: config.AuthConf{Type: "static", Keys: []string{"sk-up"}}}},
	}
	cfg.Defaults()
	cfg.Validate()
	p := proxy.NewProxy(provider.New(cfg), st, cfg.CostFor)
	srv := New(p, auth.NewKeyStore(cfg.Keys), st, "", "secretpw", "test")
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	// no key → 401
	resp, _ := http.Post(ts.URL+"/v1/responses", "application/json",
		strings.NewReader(`{"model":"test-model","input":"hi"}`))
	if resp.StatusCode != 401 {
		t.Fatalf("want 401, got %d", resp.StatusCode)
	}
	resp.Body.Close()

	// good key → chat upstream, responses-wire body back
	req, _ := http.NewRequest("POST", ts.URL+"/v1/responses",
		strings.NewReader(`{"model":"test-model","input":"hi"}`))
	req.Header.Set("Authorization", "Bearer ar-agent")
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var m map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&m); err != nil {
		t.Fatal(err)
	}
	if m["object"] != "response" || m["status"] != "completed" {
		t.Fatalf("bad envelope: %v", m)
	}
	out := m["output"].([]any)
	text := out[0].(map[string]any)["content"].([]any)[0].(map[string]any)["text"]
	if text != "pong" {
		t.Fatalf("text: %v", text)
	}
	u := m["usage"].(map[string]any)
	if u["input_tokens"] != 3.0 || u["output_tokens"] != 1.0 {
		t.Fatalf("usage: %v", u)
	}
}
