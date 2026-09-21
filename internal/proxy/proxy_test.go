package proxy

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/bacnh85/yardmaster/internal/config"
	"github.com/bacnh85/yardmaster/internal/provider"
)

// sseBody reads an SSE response into (firstDataTime over start, all data lines).
func readSSE(t *testing.T, body io.Reader) (ttft time.Duration, lines []string) {
	t.Helper()
	start := time.Now()
	sc := bufio.NewScanner(body)
	for sc.Scan() {
		line := sc.Text()
		if strings.HasPrefix(line, "data:") {
			payload := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
			if ttft == 0 {
				ttft = time.Since(start)
			}
			lines = append(lines, payload)
		}
	}
	return ttft, lines
}

func oaiChunk(delta map[string]any, finish any) map[string]any {
	return map[string]any{
		"id": "c", "object": "chat.completion.chunk", "model": "m",
		"choices": []any{map[string]any{"index": 0, "delta": delta, "finish_reason": finish}},
	}
}

func TestPassthroughIdentity(t *testing.T) {
	var want bytes.Buffer
	for i := 0; i < 20; i++ {
		b, _ := json.Marshal(oaiChunk(map[string]any{"content": fmt.Sprintf("tok%d ", i)}, nil))
		want.WriteString("data: " + string(b) + "\n\n")
	}
	b, _ := json.Marshal(map[string]any{"id": "c", "object": "chat.completion.chunk", "model": "m",
		"choices": []any{}, "usage": map[string]any{"prompt_tokens": 7, "completion_tokens": 20}})
	want.WriteString("data: " + string(b) + "\n\ndata: [DONE]\n\n")

	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.(http.Flusher).Flush()
		for _, line := range strings.SplitAfter(want.String(), "\n\n") {
			if line != "" {
				fmt.Fprint(w, line)
				w.(http.Flusher).Flush()
				time.Sleep(time.Millisecond)
			}
		}
	}))
	defer up.Close()

	cfg := &config.Config{Providers: []*config.Provider{
		{Name: "p1", BaseURL: up.URL, Wire: "openai", Models: []string{"bench-model"},
			Auth: config.AuthConf{Type: "static", Keys: []string{"sk"}}}}}
	p := NewProxy(provider.New(cfg), nil, nil)
	ts := httptest.NewServer(http.HandlerFunc(p.ServeChat))
	defer ts.Close()

	body := `{"model":"bench-model","stream":true,"messages":[{"role":"user","content":"x"}]}`
	resp, err := http.Post(ts.URL, "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	got, _ := io.ReadAll(resp.Body)
	if string(got) != want.String() {
		t.Fatalf("passthrough not byte-identical:\n got: %q\nwant: %q", string(got), want.String())
	}
}

func TestFailoverOn429(t *testing.T) {
	var calls1, calls2 atomic.Int32
	up1 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls1.Add(1)
		w.WriteHeader(429)
		w.Write([]byte(`{"error":{"message":"rate limited","code":1302}}`))
	}))
	defer up1.Close()
	up2 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls2.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		b, _ := json.Marshal(oaiChunk(map[string]any{"content": "ok"}, nil))
		fmt.Fprintf(w, "data: %s\n\ndata: [DONE]\n\n", b)
		w.(http.Flusher).Flush()
	}))
	defer up2.Close()

	cfg := &config.Config{Providers: []*config.Provider{
		{Name: "a", BaseURL: up1.URL, Wire: "openai", Models: []string{"m"}, Auth: config.AuthConf{Keys: []string{"k1", "k2"}}},
		{Name: "b", BaseURL: up2.URL, Wire: "openai", Models: []string{"m"}, Auth: config.AuthConf{Keys: []string{"k3"}}},
	}}
	p := NewProxy(provider.New(cfg), nil, nil)
	ts := httptest.NewServer(http.HandlerFunc(p.ServeChat))
	defer ts.Close()

	resp, err := http.Post(ts.URL, "application/json",
		strings.NewReader(`{"model":"m","stream":true}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("status: %d", resp.StatusCode)
	}
	ttft, lines := readSSE(t, resp.Body)
	_ = ttft
	if len(lines) < 2 || lines[len(lines)-1] != "[DONE]" {
		t.Fatalf("lines: %v", lines)
	}
	if calls1.Load() != 2 { // both keys of provider a tried
		t.Fatalf("provider a calls: %d", calls1.Load())
	}
	if calls2.Load() != 1 {
		t.Fatalf("provider b calls: %d", calls2.Load())
	}
}

func TestProviderErrorFailsOverAndLastErrorSurfaces(t *testing.T) {
	var calls1, calls2, calls3 atomic.Int32
	up1 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls1.Add(1)
		w.WriteHeader(400)
		w.Write([]byte(`{"error":{"message":"unsupported_model","code":"unsupported_model"}}`))
	}))
	defer up1.Close()
	up2 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls2.Add(1)
		w.WriteHeader(200) // serves fine
		w.Write([]byte(`{"id":"x","object":"chat.completion","model":"m","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{}}`))
	}))
	defer up2.Close()
	up3 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls3.Add(1)
		w.WriteHeader(400)
		w.Write([]byte(`{"error":{"message":"bad"}}`))
	}))
	defer up3.Close()

	cfg := &config.Config{Providers: []*config.Provider{
		{Name: "a", BaseURL: up1.URL, Wire: "openai", Models: []string{"m"}, Auth: config.AuthConf{Keys: []string{"k"}}},
		{Name: "b", BaseURL: up2.URL, Wire: "openai", Models: []string{"m"}, Auth: config.AuthConf{Keys: []string{"k"}}},
		{Name: "c", BaseURL: up3.URL, Wire: "openai", Models: []string{"m"}, Auth: config.AuthConf{Keys: []string{"k"}}},
	}}
	p := NewProxy(provider.New(cfg), nil, nil)
	ts := httptest.NewServer(http.HandlerFunc(p.ServeChat))
	defer ts.Close()

	// provider a 400s → b serves → client 200
	resp, err := http.Post(ts.URL, "application/json", strings.NewReader(`{"model":"m"}`))
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 || !strings.Contains(string(body), "ok") {
		t.Fatalf("want 200 from failover, got %d %s", resp.StatusCode, body)
	}
	if calls1.Load() != 1 || calls2.Load() != 1 || calls3.Load() != 0 {
		t.Fatalf("calls: a=%d b=%d c=%d", calls1.Load(), calls2.Load(), calls3.Load())
	}

	// all fail → client gets the LAST upstream error status + body
	cfg2 := &config.Config{Providers: []*config.Provider{
		{Name: "a", BaseURL: up1.URL, Wire: "openai", Models: []string{"m"}, Auth: config.AuthConf{Keys: []string{"k"}}},
		{Name: "c", BaseURL: up3.URL, Wire: "openai", Models: []string{"m"}, Auth: config.AuthConf{Keys: []string{"k"}}},
	}}
	p2 := NewProxy(provider.New(cfg2), nil, nil)
	ts2 := httptest.NewServer(http.HandlerFunc(p2.ServeChat))
	defer ts2.Close()
	resp2, err := http.Post(ts2.URL, "application/json", strings.NewReader(`{"model":"m"}`))
	if err != nil {
		t.Fatal(err)
	}
	body2, _ := io.ReadAll(resp2.Body)
	resp2.Body.Close()
	if resp2.StatusCode != 400 || !strings.Contains(string(body2), "bad") {
		t.Fatalf("want last upstream error forwarded, got %d %s", resp2.StatusCode, body2)
	}
}

func TestDisconnectCancelsUpstream(t *testing.T) {
	canceled := make(chan struct{})
	var once sync.Once
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		f := w.(http.Flusher)
		for i := 0; i < 1000; i++ {
			select {
			case <-r.Context().Done():
				once.Do(func() { close(canceled) })
				return
			default:
			}
			b, _ := json.Marshal(oaiChunk(map[string]any{"content": "x"}, nil))
			fmt.Fprintf(w, "data: %s\n\n", b)
			f.Flush()
			time.Sleep(10 * time.Millisecond)
		}
	}))
	defer up.Close()

	cfg := &config.Config{Providers: []*config.Provider{
		{Name: "a", BaseURL: up.URL, Wire: "openai", Models: []string{"m"}, Auth: config.AuthConf{Keys: []string{"k"}}}}}
	p := NewProxy(provider.New(cfg), nil, nil)
	ts := httptest.NewServer(http.HandlerFunc(p.ServeChat))
	defer ts.Close()

	ctx, cancel := context.WithCancel(context.Background())
	req, _ := http.NewRequestWithContext(ctx, "POST", ts.URL,
		strings.NewReader(`{"model":"m","stream":true}`))
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	// read a few chunks then disconnect
	buf := make([]byte, 4096)
	resp.Body.Read(buf)
	resp.Body.Read(buf)
	t0 := time.Now()
	cancel()
	select {
	case <-canceled:
		if d := time.Since(t0); d > 200*time.Millisecond {
			t.Fatalf("cancel propagation slow: %s", d)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("upstream never saw cancel")
	}
	resp.Body.Close()
}

func TestDispatchThrottleSpacing(t *testing.T) {
	var dispatches []time.Time
	var mu sync.Mutex
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		dispatches = append(dispatches, time.Now())
		mu.Unlock()
		w.Write([]byte(`{"id":"x","object":"chat.completion","model":"m","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1}}`))
	}))
	defer up.Close()

	cfg := &config.Config{Providers: []*config.Provider{
		{Name: "zai", BaseURL: up.URL, Wire: "openai", Models: []string{"m"},
			DispatchIntervalMS: 300, Auth: config.AuthConf{Keys: []string{"k"}}}}}
	p := NewProxy(provider.New(cfg), nil, nil)
	ts := httptest.NewServer(http.HandlerFunc(p.ServeChat))
	defer ts.Close()

	// fire 3 concurrent requests
	var wg sync.WaitGroup
	for i := 0; i < 3; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			http.Post(ts.URL, "application/json", strings.NewReader(`{"model":"m"}`))
		}()
	}
	wg.Wait()

	mu.Lock()
	defer mu.Unlock()
	if len(dispatches) != 3 {
		t.Fatalf("dispatches: %d", len(dispatches))
	}
	for i := 1; i < len(dispatches); i++ {
		gap := dispatches[i].Sub(dispatches[i-1])
		if gap < 250*time.Millisecond {
			t.Fatalf("dispatch %d gap %s < 250ms (throttle failed)", i, gap)
		}
	}
}

// TestAnthropicPassthroughCacheInjection: same-wire anthropic client → zai-style
// anthropic upstream with inject_cache_control must reach the upstream WITH
// ephemeral markers — the cross-wire translate never runs on this path, so
// before the fix the subscription multiplier silently didn't apply.
func TestAnthropicPassthroughCacheInjection(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/messages" {
			t.Errorf("path: %s", r.URL.Path)
		}
		var req map[string]any
		json.NewDecoder(r.Body).Decode(&req)
		sys, ok := req["system"].([]any)
		if !ok {
			t.Fatalf("system must be blocks: %v", req["system"])
		}
		blk := sys[0].(map[string]any)
		if _, ok := blk["cache_control"]; !ok {
			t.Fatalf("cache_control missing on passthrough: %v", blk)
		}
		if req["speed"] != "fast" {
			t.Fatalf("body override on passthrough: %v", req["speed"])
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"id":"m","type":"message","role":"assistant","model":"glm-5.3","content":[{"type":"text","text":"pong"}],"stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":1}}`)
	}))
	defer up.Close()

	cfg := &config.Config{Providers: []*config.Provider{
		{Name: "zai", BaseURL: up.URL, Wire: "anthropic", Models: []string{"glm-5.3"},
			InjectCacheControl: true,
			BodyOverrides:      map[string]any{"speed": "fast"},
			Auth:               config.AuthConf{Keys: []string{"k"}}}}}
	p := NewProxy(provider.New(cfg), nil, nil)
	ts := httptest.NewServer(http.HandlerFunc(p.ServeMessages))
	defer ts.Close()

	resp, err := http.Post(ts.URL, "application/json", strings.NewReader(
		`{"model":"glm-5.3","max_tokens":64,"system":[{"type":"text","text":"be terse"}],"messages":[{"role":"user","content":[{"type":"text","text":"hi"}]}]}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("status %d: %s", resp.StatusCode, b)
	}
}

// TestAnthropicClientToOpenAIUpstream: full cross-wire stream.
func TestAnthropicClientToOpenAIUpstream(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/chat/completions" {
			t.Errorf("upstream path: %s", r.URL.Path)
		}
		var req map[string]any
		json.NewDecoder(r.Body).Decode(&req)
		if req["model"] != "deepseek-flash" {
			t.Errorf("model: %v", req["model"])
		}
		w.Header().Set("Content-Type", "text/event-stream")
		f := w.(http.Flusher)
		c1, _ := json.Marshal(oaiChunk(map[string]any{"role": "assistant", "content": ""}, nil))
		c2, _ := json.Marshal(oaiChunk(map[string]any{"content": "hello"}, nil))
		c3, _ := json.Marshal(oaiChunk(map[string]any{}, "stop"))
		cu, _ := json.Marshal(map[string]any{"id": "c", "object": "chat.completion.chunk", "model": "m",
			"choices": []any{}, "usage": map[string]any{"prompt_tokens": 11, "completion_tokens": 2}})
		for _, c := range []string{string(c1), string(c2), string(c3), string(cu)} {
			fmt.Fprintf(w, "data: %s\n\n", c)
			f.Flush()
		}
		fmt.Fprint(w, "data: [DONE]\n\n")
		f.Flush()
	}))
	defer up.Close()

	cfg := &config.Config{Providers: []*config.Provider{
		{Name: "ds", BaseURL: up.URL, Wire: "openai", Models: []string{"claude-x"},
			ModelMap: map[string]string{"claude-x": "deepseek-flash"},
			Auth:     config.AuthConf{Keys: []string{"k"}}}}}
	p := NewProxy(provider.New(cfg), nil, nil)
	ts := httptest.NewServer(http.HandlerFunc(p.ServeMessages))
	defer ts.Close()

	resp, err := http.Post(ts.URL, "application/json", strings.NewReader(
		`{"model":"claude-x","stream":true,"max_tokens":100,"system":"be nice","messages":[{"role":"user","content":"hi"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if ct := resp.Header.Get("Content-Type"); ct != "text/event-stream" {
		t.Fatalf("content-type: %s", ct)
	}
	ttft, lines := readSSE(t, resp.Body)
	if ttft <= 0 || len(lines) < 3 {
		t.Fatalf("lines: %v", lines)
	}
	joined := strings.Join(lines, "\n")
	for _, want := range []string{`"type":"message_start"`, `"text":"hello"`, `"stop_reason":"end_turn"`, `"type":"message_stop"`} {
		if !strings.Contains(joined, want) {
			t.Fatalf("missing %q in %s", want, joined)
		}
	}
	// usage folded into message_delta
	if !strings.Contains(joined, `"output_tokens":2`) {
		t.Fatalf("usage missing: %s", joined)
	}
}

// Disabled providers are skipped at dispatch (registry toggle).
func TestDisabledProviderSkipped(t *testing.T) {
	cfg := &config.Config{Providers: []*config.Provider{
		{Name: "off", BaseURL: "http://127.0.0.1:1", Wire: "openai", Disabled: true,
			Models: []string{"m"}, Auth: config.AuthConf{Keys: []string{"k"}}}}}
	p := NewProxy(provider.New(cfg), nil, nil)
	ts := httptest.NewServer(http.HandlerFunc(p.ServeChat))
	defer ts.Close()

	resp, err := http.Post(ts.URL, "application/json", strings.NewReader(`{"model":"m","messages":[]}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 404 {
		t.Fatalf("disabled provider served the request: %d", resp.StatusCode)
	}
}

// TestOpenAIClientToAnthropicUpstream: zai-style upstream.
func TestOpenAIClientToAnthropicUpstream(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/messages" {
			t.Errorf("path: %s", r.URL.Path)
		}
		if r.Header.Get("x-api-key") != "zai-key" {
			t.Errorf("auth header: %s", r.Header.Get("x-api-key"))
		}
		var req map[string]any
		json.NewDecoder(r.Body).Decode(&req)
		if req["thinking"] == nil {
			t.Errorf("expected thinking (adaptive), got none")
		}
		if req["speed"] != "fast" {
			t.Errorf("body override speed missing: %v", req["speed"])
		}
		w.Header().Set("Content-Type", "text/event-stream")
		f := w.(http.Flusher)
		ev := func(name, data string) {
			fmt.Fprintf(w, "event: %s\ndata: %s\n\n", name, data)
			f.Flush()
		}
		ev("message_start", `{"type":"message_start","message":{"id":"m","role":"assistant","usage":{"input_tokens":50,"cache_read_input_tokens":30}}}`)
		ev("content_block_start", `{"type":"content_block_start","index":0,"content_block":{"type":"text"}}`)
		ev("content_block_delta", `{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"hola"}}`)
		ev("content_block_stop", `{"type":"content_block_stop","index":0}`)
		ev("message_delta", `{"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":4}}`)
		ev("message_stop", `{"type":"message_stop"}`)
	}))
	defer up.Close()

	cfg := &config.Config{Providers: []*config.Provider{
		{Name: "zai", BaseURL: up.URL, Wire: "anthropic", Models: []string{"glm-x"},
			AdaptiveThinking: true, InjectCacheControl: true,
			BodyOverrides: map[string]any{"speed": "fast"},
			Auth:          config.AuthConf{Keys: []string{"zai-key"}}}}}
	p := NewProxy(provider.New(cfg), nil, nil)
	ts := httptest.NewServer(http.HandlerFunc(p.ServeChat))
	defer ts.Close()

	resp, err := http.Post(ts.URL, "application/json", strings.NewReader(
		`{"model":"glm-x","stream":true,"reasoning_effort":"high","messages":[{"role":"user","content":"hi"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	_, lines := readSSE(t, bytes.NewReader(raw))
	joined := strings.Join(lines, "\n")
	for _, want := range []string{`"content":"hola"`, `"finish_reason":"stop"`, `"cache_read_tokens":30`} {
		if !strings.Contains(joined, want) {
			t.Fatalf("missing %q in %s", want, joined)
		}
	}
	if lines[len(lines)-1] != "[DONE]" {
		t.Fatalf("last line: %q", lines[len(lines)-1])
	}
	// regression: exactly one finish_reason chunk and one [DONE] per stream
	if n := strings.Count(joined, `"finish_reason":"`); n != 1 {
		t.Fatalf("finish_reason emitted %d times (want 1): %s", n, joined)
	}
	if n := strings.Count(joined, `"stop_reason"`); n > 0 {
		t.Fatalf("anthropic stop_reason leaked into openai stream: %s", joined)
	}
}

// Zen-style anthropic base URL already ends in /v1 — must not double it.
func TestAnthropicBaseURLWithV1Suffix(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/messages" {
			t.Errorf("path: %s", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"id": "m", "role": "assistant",
			"content":     []any{map[string]any{"type": "text", "text": "hola"}},
			"stop_reason": "end_turn",
			"usage":       map[string]any{"input_tokens": 3, "output_tokens": 2},
		})
	}))
	defer up.Close()

	cfg := &config.Config{Providers: []*config.Provider{
		{Name: "zen", BaseURL: up.URL + "/v1", Wire: "anthropic", Models: []string{"claude-haiku-4.5"},
			Auth: config.AuthConf{Keys: []string{"zen-key"}}}}}
	p := NewProxy(provider.New(cfg), nil, nil)
	ts := httptest.NewServer(http.HandlerFunc(p.ServeChat))
	defer ts.Close()

	resp, err := http.Post(ts.URL, "application/json", strings.NewReader(
		`{"model":"claude-haiku-4.5","messages":[{"role":"user","content":"hi"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("want 200, got %d", resp.StatusCode)
	}
}

// TestUsageRecordedThroughStore verifies stats plumbing end-to-end.
func TestUsageRecordedThroughStore(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		b, _ := json.Marshal(oaiChunk(map[string]any{"content": "x"}, nil))
		fmt.Fprintf(w, "data: %s\n\n", b)
		bu, _ := json.Marshal(map[string]any{"id": "c", "object": "chat.completion.chunk", "model": "m",
			"choices": []any{}, "usage": map[string]any{"prompt_tokens": 100, "completion_tokens": 5}})
		fmt.Fprintf(w, "data: %s\n\ndata: [DONE]\n\n", bu)
		w.(http.Flusher).Flush()
	}))
	defer up.Close()

	cfg := &config.Config{Providers: []*config.Provider{
		{Name: "a", BaseURL: up.URL, Wire: "openai", Models: []string{"m"}, Auth: config.AuthConf{Keys: []string{"k"}}}}}
	p := NewProxy(provider.New(cfg), nil, func(string) config.Cost { return config.Cost{Input: 1, Output: 2} })
	ts := httptest.NewServer(http.HandlerFunc(p.ServeChat))
	defer ts.Close()
	http.Post(ts.URL, "application/json", strings.NewReader(`{"model":"m","stream":true}`))

	// inflight decrement races the client-side response completion; poll for it
	var inflight, total int64
	for deadline := time.Now().Add(5 * time.Second); ; {
		inflight, total = p.Stats()
		if total == 1 && inflight == 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("stats: inflight=%d total=%d", inflight, total)
		}
		time.Sleep(5 * time.Millisecond)
	}
	if len(p.Active.List()) != 0 {
		t.Fatal("active should be empty after finish")
	}
}
