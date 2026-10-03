package proxy

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/bacnh85/yardmaster/internal/config"
	"github.com/bacnh85/yardmaster/internal/provider"
)

// TestHeadersTimeoutFailover: a hung first hop must be abandoned after its
// per-provider headers timeout and the request must fail over to the next
// target — not sit until the client's own (Cloudflare 120s) window expires.
func TestHeadersTimeoutFailover(t *testing.T) {
	hung := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body) // read the body so the server notices disconnects
		<-r.Context().Done()        // never send headers
	}))
	defer hung.Close()
	ok := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Write([]byte("data: [DONE]\n\n"))
	}))
	defer ok.Close()

	cfg := &config.Config{Providers: []*config.Provider{
		{Name: "hung", BaseURL: hung.URL, Wire: "openai", Models: []string{"m"},
			Auth: config.AuthConf{Keys: []string{"k"}}, HeadersTimeoutS: 1},
		{Name: "ok", BaseURL: ok.URL, Wire: "openai", Models: []string{"m"},
			Auth: config.AuthConf{Keys: []string{"k"}}},
	}}
	p := NewProxy(provider.New(cfg), nil, nil)
	ts := httptest.NewServer(http.HandlerFunc(p.ServeChat))
	defer ts.Close()

	start := time.Now()
	resp, err := http.Post(ts.URL, "application/json",
		strings.NewReader(`{"model":"m","stream":true,"messages":[{"role":"user","content":"hi"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("status %d, want 200 (failover after headers timeout)", resp.StatusCode)
	}
	if d := time.Since(start); d > 10*time.Second {
		t.Fatalf("failover took %v, want ~1s headers timeout + quick hop", d)
	}
}

// TestHeadersTimeoutClampedToDeadline: even a 300s-configured hop must not
// consume the whole request deadline — the attempt budget is clamped to half
// the remaining deadline so a failover hop can still run inside the window.
// (Deadline-bearing contexts are the CountTokens/health-probe paths; inbound
// streaming requests carry no deadline by design — long streams are legal.)
func TestHeadersTimeoutClampedToDeadline(t *testing.T) {
	hung := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body) // read the body so the server notices disconnects
		<-r.Context().Done()
	}))
	defer hung.Close()

	cfg := &config.Config{Providers: []*config.Provider{
		{Name: "hung", BaseURL: hung.URL, Wire: "openai", Models: []string{"m"},
			Auth: config.AuthConf{Keys: []string{"k"}}, HeadersTimeoutS: 300},
	}}
	p := NewProxy(provider.New(cfg), nil, nil)
	tgt := p.Reg.Resolve("m", []string{"*"})[0]

	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, "POST", hung.URL+"/chat/completions",
		strings.NewReader(`{"model":"m","messages":[]}`))
	start := time.Now()
	_, err := p.do(tgt, req)
	if err == nil {
		t.Fatal("hung upstream returned a response, want headers-timeout error")
	}
	if d := time.Since(start); d > 3*time.Second {
		t.Fatalf("attempt ran %v, want clamped to ~2s (half the 4s deadline)", d)
	}
}
