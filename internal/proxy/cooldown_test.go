package proxy

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/bacnh85/yardmaster/internal/config"
	"github.com/bacnh85/yardmaster/internal/provider"
)

// cooled static targets are skipped without an upstream call; the chain tail
// serves the request.
func TestCooldownSkipsRateLimitedTarget(t *testing.T) {
	var calls1, calls2 atomic.Int32
	up1 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls1.Add(1)
		w.Header().Set("Retry-After", "60")
		w.WriteHeader(429)
	}))
	defer up1.Close()
	up2 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls2.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		w.Write([]byte("data: {\"choices\":[],\"object\":\"chat.completion.chunk\"}\n\ndata: [DONE]\n\n"))
		w.(http.Flusher).Flush()
	}))
	defer up2.Close()

	cfg := &config.Config{Providers: []*config.Provider{
		{Name: "a", BaseURL: up1.URL, Wire: "openai", Models: []string{"m"}, Auth: config.AuthConf{Keys: []string{"k1"}}},
		{Name: "b", BaseURL: up2.URL, Wire: "openai", Models: []string{"m"}, Auth: config.AuthConf{Keys: []string{"k2"}}},
	}}
	p := NewProxy(provider.New(cfg), nil, nil)
	ts := httptest.NewServer(http.HandlerFunc(p.ServeChat))
	defer ts.Close()

	post := func() {
		resp, err := http.Post(ts.URL, "application/json", strings.NewReader(`{"model":"m","stream":true}`))
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != 200 {
			t.Fatalf("status: %d", resp.StatusCode)
		}
	}

	post() // a 429s → b serves; a now cooling for Retry-After 60s
	post() // a must be skipped without an upstream call
	post()

	if calls1.Load() != 1 {
		t.Fatalf("cooled provider hit %d times, want 1", calls1.Load())
	}
	if calls2.Load() != 3 {
		t.Fatalf("chain tail calls: %d, want 3", calls2.Load())
	}
}

// three consecutive 5xx trip the breaker: the 4th request skips the target.
func TestCooldown5xxBreaker(t *testing.T) {
	var calls atomic.Int32
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.WriteHeader(503)
	}))
	defer up.Close()

	cfg := &config.Config{Providers: []*config.Provider{
		{Name: "a", BaseURL: up.URL, Wire: "openai", Models: []string{"m"}, Auth: config.AuthConf{Keys: []string{"k1"}}},
	}}
	p := NewProxy(provider.New(cfg), nil, nil)
	ts := httptest.NewServer(http.HandlerFunc(p.ServeChat))
	defer ts.Close()

	for i := 0; i < 3; i++ { // breaker threshold: each request reaches upstream
		resp, err := http.Post(ts.URL, "application/json", strings.NewReader(`{"model":"m","stream":true}`))
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != 503 {
			t.Fatalf("request %d status: %d", i, resp.StatusCode)
		}
	}
	resp, err := http.Post(ts.URL, "application/json", strings.NewReader(`{"model":"m","stream":true}`))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if calls.Load() != 3 {
		t.Fatalf("breaker did not skip: %d upstream calls", calls.Load())
	}
	if resp.StatusCode != 502 {
		t.Fatalf("all-cooled status: %d", resp.StatusCode)
	}
}
