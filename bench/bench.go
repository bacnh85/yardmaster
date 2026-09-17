// Package bench is the Phase 0 perf harness: synthetic SSE upstream, N
// concurrent streams, measures added TTFT (proxied vs direct) and stalls.
package bench

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/bacnh85/yardmaster/internal/config"
	"github.com/bacnh85/yardmaster/internal/provider"
	"github.com/bacnh85/yardmaster/internal/proxy"
)

type Options struct {
	Streams int   // concurrent streams
	Rate    int   // tokens/sec per stream
	Toks    int   // tokens per stream
	TTFTms  int   // simulated upstream time-to-first-byte
	Verbose bool
}

type Stats struct {
	P50 float64
	P95 float64
	Max float64
}

type Result struct {
	Direct      Stats // ms
	Proxied     Stats // ms
	OverheadP50 float64
	OverheadP95 float64
	Stalls      int
	Streams     int
	Wall        time.Duration
	TotalTokens int
}

type sample struct {
	ttftMs  float64
	stalled bool
}

// Run executes the harness and returns measurements.
func Run(opts Options) (*Result, error) {
	if opts.Streams <= 0 {
		opts.Streams = 100
	}
	if opts.Rate <= 0 {
		opts.Rate = 300
	}
	if opts.Toks <= 0 {
		opts.Toks = 300
	}
	if opts.TTFTms <= 0 {
		opts.TTFTms = 120
	}

	mock := httptest.NewServer(mockSSE(opts))
	defer mock.Close()

	cfg := &config.Config{
		Providers: []*config.Provider{{
			Name: "mock", BaseURL: mock.URL, Wire: "openai", Models: []string{"bench-model"},
			Auth: config.AuthConf{Type: "static", Keys: []string{"sk-bench"}},
		}},
	}
	p := proxy.NewProxy(provider.New(cfg), nil, nil)
	via := httptest.NewServer(http.HandlerFunc(p.ServeChat))
	defer via.Close()

	t0 := time.Now()
	direct := runPhase(opts, mock.URL, "")
	proxied, totalTokens := runPhaseVia(opts, via.URL)
	res := &Result{
		Direct: statsOf(direct), Proxied: statsOf(proxied),
		OverheadP50: statsOf(proxied).P50 - statsOf(direct).P50,
		OverheadP95: statsOf(proxied).P95 - statsOf(direct).P95,
		Streams:     opts.Streams, Wall: time.Since(t0), TotalTokens: totalTokens,
	}
	for _, s := range proxied {
		if s.stalled {
			res.Stalls++
		}
	}
	return res, nil
}

// mockSSE generates a synthetic openai SSE stream at opts.Rate tok/s.
func mockSSE(opts Options) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		flusher, _ := w.(http.Flusher)
		chunk := func(delta string) {
			c := map[string]any{
				"id": "bench", "object": "chat.completion.chunk", "model": "bench-model",
				"choices": []any{map[string]any{"index": 0, "delta": map[string]any{"content": delta}, "finish_reason": nil}},
			}
			b, _ := json.Marshal(c)
			fmt.Fprintf(w, "data: %s\n\n", b)
			flusher.Flush()
		}
		time.Sleep(time.Duration(opts.TTFTms) * time.Millisecond)
		chunk("")
		interval := time.Second / time.Duration(opts.Rate)
		for i := 0; i < opts.Toks; i++ {
			chunk("tok ")
			time.Sleep(interval)
		}
		final := map[string]any{
			"id": "bench", "object": "chat.completion.chunk", "model": "bench-model",
			"choices": []any{},
			"usage":   map[string]any{"prompt_tokens": 10, "completion_tokens": opts.Toks},
		}
		b, _ := json.Marshal(final)
		fmt.Fprintf(w, "data: %s\n\n", b)
		fmt.Fprint(w, "data: [DONE]\n\n")
		flusher.Flush()
	}
}

func runPhase(opts Options, url, suffix string) []sample {
	samples := make([]sample, opts.Streams)
	var wg sync.WaitGroup
	for i := 0; i < opts.Streams; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			samples[i] = oneStream(url+suffix, opts.Toks)
		}(i)
	}
	wg.Wait()
	return samples
}

func runPhaseVia(opts Options, url string) ([]sample, int) {
	samples := make([]sample, opts.Streams)
	var wg sync.WaitGroup
	var mu sync.Mutex
	totalTokens := 0
	for i := 0; i < opts.Streams; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			body, _ := json.Marshal(map[string]any{
				"model": "bench-model", "stream": true, "messages": []any{
					map[string]any{"role": "user", "content": "bench"}},
			})
			req, _ := http.NewRequest("POST", url, bytes.NewReader(body))
			req.Header.Set("Content-Type", "application/json")
			resp, err := benchClient.Do(req)
			if err != nil {
				samples[i] = sample{ttftMs: -1}
				return
			}
			defer resp.Body.Close()
			start := time.Now()
			var last time.Time
			st := sample{}
			n := 0
			sc := bufio.NewScanner(resp.Body)
			for sc.Scan() {
				line := sc.Text()
				if !strings.HasPrefix(line, "data:") {
					continue
				}
				now := time.Now()
				if st.ttftMs == 0 {
					st.ttftMs = float64(now.Sub(start).Milliseconds())
				} else if now.Sub(last) > 250*time.Millisecond {
					st.stalled = true
				}
				last = now
				n++
			}
			mu.Lock()
			totalTokens += n
			mu.Unlock()
			samples[i] = st
		}(i)
	}
	wg.Wait()
	return samples, totalTokens
}

var benchClient = &http.Client{Transport: &http.Transport{
	MaxIdleConns:        512,
	MaxIdleConnsPerHost: 512,
	IdleConnTimeout:     90 * time.Second,
}}

// oneStream measures TTFT + stalls against the raw mock upstream.
func oneStream(url string, expectChunks int) sample {
	body := strings.NewReader(`{"model":"bench-model","stream":true}`)
	req, _ := http.NewRequest("POST", url, body)
	req.Header.Set("Content-Type", "application/json")
	resp, err := benchClient.Do(req)
	if err != nil {
		return sample{ttftMs: -1}
	}
	defer resp.Body.Close()
	start := time.Now()
	var last time.Time
	st := sample{}
	sc := bufio.NewScanner(resp.Body)
	for sc.Scan() {
		line := sc.Text()
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		now := time.Now()
		if st.ttftMs == 0 {
			st.ttftMs = float64(now.Sub(start).Milliseconds())
		} else if now.Sub(last) > 250*time.Millisecond {
			st.stalled = true
		}
		last = now
	}
	return st
}

func statsOf(ss []sample) Stats {
	var ttfts []float64
	for _, s := range ss {
		if s.ttftMs >= 0 {
			ttfts = append(ttfts, s.ttftMs)
		}
	}
	sort.Float64s(ttfts)
	if len(ttfts) == 0 {
		return Stats{}
	}
	pct := func(p float64) float64 { return ttfts[int(p*float64(len(ttfts)-1))] }
	return Stats{P50: pct(0.5), P95: pct(0.95), Max: ttfts[len(ttfts)-1]}
}

// Print renders the result table + Phase 0 gate check.
func (r *Result) Print(w io.Writer) {
	fmt.Fprintf(w, "streams=%d  wall=%s  total-chunks=%d\n", r.Streams, r.Wall.Round(time.Millisecond), r.TotalTokens)
	fmt.Fprintf(w, "TTFT direct   p50=%.1fms p95=%.1fms max=%.1fms\n", r.Direct.P50, r.Direct.P95, r.Direct.Max)
	fmt.Fprintf(w, "TTFT proxied  p50=%.1fms p95=%.1fms max=%.1fms\n", r.Proxied.P50, r.Proxied.P95, r.Proxied.Max)
	fmt.Fprintf(w, "overhead      p50=%.1fms p95=%.1fms\n", r.OverheadP50, r.OverheadP95)
	fmt.Fprintf(w, "stalls        %d\n", r.Stalls)
	gate := r.OverheadP95 < 5 && r.Stalls == 0
	status := "PASS"
	if !gate {
		status = "FAIL"
	}
	fmt.Fprintf(w, "phase-0 gate (overhead p95 < 5ms, 0 stalls): %s\n", status)
}
