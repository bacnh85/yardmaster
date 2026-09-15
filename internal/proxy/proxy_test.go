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

	"agent-router/internal/config"
	"agent-router/internal/provider"
)

// sseBody reads an SSE response into (firstDataTime over start, all data lines).
func readSSE(t *testing.T, resp *http.Response) (ttft time.Duration, lines []string) {
	t.Helper()
	start := time.Now()
	sc := bufio.NewScanner(resp.Body)
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
	ttft, lines := readSSE(t, resp)
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

func TestTerminalErrorNoFailover(t *testing.T) {
	var calls2 atomic.Int32
	up1 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(400)
		w.Write([]byte(`{"error":{"message":"bad request"}}`))
	}))
	defer up1.Close()
	up2 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls2.Add(1)
		w.WriteHeader(200)
	}))
	defer up2.Close()

	cfg := &config.Config{Providers: []*config.Provider{
		{Name: "a", BaseURL: up1.URL, Wire: "openai", Models: []string{"m"}, Auth: config.AuthConf{Keys: []string{"k"}}},
		{Name: "b", BaseURL: up2.URL, Wire: "openai", Models: []string{"m"}, Auth: config.AuthConf{Keys: []string{"k"}}},
	}}
	p := NewProxy(provider.New(cfg), nil, nil)
	ts := httptest.NewServer(http.HandlerFunc(p.ServeChat))
	defer ts.Close()

	resp, _ := http.Post(ts.URL, "application/json", strings.NewReader(`{"model":"m"}`))
	defer resp.Body.Close()
	if resp.StatusCode != 400 {
		t.Fatalf("want 400 passthrough, got %d", resp.StatusCode)
	}
	if calls2.Load() != 0 {
		t.Fatal("should not failover on 400")
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
	ttft, lines := readSSE(t, resp)
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
	_, lines := readSSE(t, resp)
	joined := strings.Join(lines, "\n")
	for _, want := range []string{`"content":"hola"`, `"finish_reason":"stop"`, `"cache_read_tokens":30`} {
		if !strings.Contains(joined, want) {
			t.Fatalf("missing %q in %s", want, joined)
		}
	}
	if lines[len(lines)-1] != "[DONE]" {
		t.Fatalf("last line: %q", lines[len(lines)-1])
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

	inflight, total := p.Stats()
	if total != 1 || inflight != 0 {
		t.Fatalf("stats: inflight=%d total=%d", inflight, total)
	}
	if len(p.Active.List()) != 0 {
		t.Fatal("active should be empty after finish")
	}
}
