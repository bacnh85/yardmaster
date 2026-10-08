package proxy

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/bacnh85/yardmaster/internal/config"
	"github.com/bacnh85/yardmaster/internal/provider"
)

// Gateways that hide failures behind HTTP 200 must fail the hop like any
// transport error so the failover chain runs and the breaker learns —
// zai-start's start-plan quota death answered {"code":1005,"msg":"exceed
// quota limit"} with status 200 and starved clients of any error signal.
func TestEnvelope200Failover(t *testing.T) {
	var calls1, calls2 atomic.Int32
	up1 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls1.Add(1)
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"code":1005,"msg":"exceed quota limit","logid":"x"}`))
	}))
	defer up1.Close()
	up2 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls2.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		w.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"ok\"}}]}\n\ndata: [DONE]\n\n"))
	}))
	defer up2.Close()

	cfg := &config.Config{Providers: []*config.Provider{
		{Name: "a", BaseURL: up1.URL, Wire: "openai", Models: []string{"m"}, Auth: config.AuthConf{Keys: []string{"k1"}}},
		{Name: "b", BaseURL: up2.URL, Wire: "openai", Models: []string{"m"}, Auth: config.AuthConf{Keys: []string{"k2"}}},
	}}
	p := NewProxy(provider.New(cfg), nil, nil)
	ts := httptest.NewServer(http.HandlerFunc(p.ServeChat))
	defer ts.Close()

	post := func() string {
		resp, err := http.Post(ts.URL, "application/json",
			strings.NewReader(`{"model":"m","stream":true,"messages":[{"role":"user","content":"hi"}]}`))
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return string(b)
	}
	for i := 0; i < 3; i++ { // 3 envelope strikes trip the breaker
		if out := post(); !strings.Contains(out, "ok") {
			t.Fatalf("post %d: failover body %q, want ok", i, out)
		}
	}
	if out := post(); !strings.Contains(out, "ok") { // breaker cools a → skip
		t.Fatalf("post 4: body %q, want ok", out)
	}
	if calls1.Load() != 3 {
		t.Fatalf("envelope provider hit %d times, want 3 (then breaker-cooled)", calls1.Load())
	}
	if calls2.Load() != 4 {
		t.Fatalf("chain tail calls: %d, want 4", calls2.Load())
	}
}

// Non-stream envelope with no failover target: the error must surface as 502
// with the envelope text instead of a misleading empty 200 success.
func TestEnvelope200NonStreamSurfaces502(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"code":1005,"msg":"exceed quota limit"}`))
	}))
	defer up.Close()

	cfg := &config.Config{Providers: []*config.Provider{
		{Name: "a", BaseURL: up.URL, Wire: "openai", Models: []string{"m"}, Auth: config.AuthConf{Keys: []string{"k"}}},
	}}
	p := NewProxy(provider.New(cfg), nil, nil)
	ts := httptest.NewServer(http.HandlerFunc(p.ServeChat))
	defer ts.Close()

	resp, err := http.Post(ts.URL, "application/json",
		strings.NewReader(`{"model":"m","stream":false,"messages":[{"role":"user","content":"hi"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 502 {
		t.Fatalf("status %d, want 502 (body %s)", resp.StatusCode, b)
	}
	if !strings.Contains(string(b), "exceed quota limit") {
		t.Fatalf("body %s, want envelope msg surfaced", b)
	}
}

// A 200 with an empty body (zai-start's openai-wire death: headers only,
// Content-Length 0) must fail over instead of streaming nothing.
func TestEmpty200Failover(t *testing.T) {
	var calls1, calls2 atomic.Int32
	up1 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls1.Add(1)
		w.Header().Set("Content-Length", "0")
		w.WriteHeader(200)
	}))
	defer up1.Close()
	up2 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls2.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		w.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"ok\"}}]}\n\ndata: [DONE]\n\n"))
	}))
	defer up2.Close()

	cfg := &config.Config{Providers: []*config.Provider{
		{Name: "a", BaseURL: up1.URL, Wire: "openai", Models: []string{"m"}, Auth: config.AuthConf{Keys: []string{"k1"}}},
		{Name: "b", BaseURL: up2.URL, Wire: "openai", Models: []string{"m"}, Auth: config.AuthConf{Keys: []string{"k2"}}},
	}}
	p := NewProxy(provider.New(cfg), nil, nil)
	ts := httptest.NewServer(http.HandlerFunc(p.ServeChat))
	defer ts.Close()

	resp, err := http.Post(ts.URL, "application/json",
		strings.NewReader(`{"model":"m","stream":true,"messages":[{"role":"user","content":"hi"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(b), "ok") {
		t.Fatalf("body %q, want failover to ok", b)
	}
	if calls1.Load() != 1 || calls2.Load() != 1 {
		t.Fatalf("hits: empty=%d ok=%d, want 1/1", calls1.Load(), calls2.Load())
	}
}
