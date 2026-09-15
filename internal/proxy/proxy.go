// Package proxy implements the streaming request pipeline: auth context,
// model routing with ordered failover, passthrough or wire translation,
// usage capture, and stats.
package proxy

import (
	"bufio"
	"bytes"
	"context"
	cryptorand "crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"math/rand"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"agent-router/internal/config"
	"agent-router/internal/provider"
	"agent-router/internal/store"
	"agent-router/internal/translate"
)

const (
	WireOpenAI    = "openai"
	WireAnthropic = "anthropic"
)

type Proxy struct {
	Reg      *provider.Registry
	Client   *http.Client
	Store    *store.Store
	Version  string
	Cost     func(model string) config.Cost

	Active   Active
	inflight atomic.Int64
	total    atomic.Int64
}

// NewProxy builds the proxy with a tuned shared transport.
func NewProxy(reg *provider.Registry, st *store.Store, costFn func(string) config.Cost) *Proxy {
	t := http.DefaultTransport.(*http.Transport).Clone()
	t.MaxIdleConnsPerHost = 64
	t.IdleConnTimeout = 90 * time.Second
	t.ForceAttemptHTTP2 = true
	return &Proxy{
		Reg:    reg,
		Client: &http.Client{Transport: t},
		Store:  st,
		Cost:   costFn,
	}
}

// ---- active request registry (live dashboard) ----

type ActiveEntry struct {
	ID      string    `json:"id"`
	Model   string    `json:"model"`
	Provider string   `json:"provider"`
	Key     string    `json:"key"`
	Stream  bool      `json:"stream"`
	Start   time.Time `json:"start"`
	TTFTms  float64   `json:"ttft_ms"`
}

type Active struct {
	mu sync.Mutex
	m  map[string]*ActiveEntry
}

func (a *Active) Set(e *ActiveEntry) {
	a.mu.Lock()
	if a.m == nil {
		a.m = map[string]*ActiveEntry{}
	}
	a.m[e.ID] = e
	a.mu.Unlock()
}
func (a *Active) TTFT(id string, ms float64) {
	a.mu.Lock()
	if e := a.m[id]; e != nil {
		e.TTFTms = ms
	}
	a.mu.Unlock()
}
func (a *Active) Done(id string) {
	a.mu.Lock()
	delete(a.m, id)
	a.mu.Unlock()
}
func (a *Active) List() []*ActiveEntry {
	a.mu.Lock()
	defer a.mu.Unlock()
	out := make([]*ActiveEntry, 0, len(a.m))
	for _, e := range a.m {
		out = append(out, e)
	}
	return out
}

func (p *Proxy) Stats() (inflight, total int64) {
	return p.inflight.Load(), p.total.Load()
}

// ---- request handling ----

type ctxKey int

const inboundKeyCtx ctxKey = 1

// WithInboundKey stores the authenticated inbound key name in ctx.
func WithInboundKey(ctx context.Context, name string) context.Context {
	return context.WithValue(ctx, inboundKeyCtx, name)
}

func InboundKey(ctx context.Context) string {
	v, _ := ctx.Value(inboundKeyCtx).(string)
	return v
}

// ServeChat handles POST /v1/chat/completions (openai wire in).
func (p *Proxy) ServeChat(w http.ResponseWriter, r *http.Request) {
	p.serve(w, r, WireOpenAI)
}

// ServeMessages handles POST /v1/messages (anthropic wire in).
func (p *Proxy) ServeMessages(w http.ResponseWriter, r *http.Request) {
	p.serve(w, r, WireAnthropic)
}

func (p *Proxy) serve(w http.ResponseWriter, r *http.Request, clientWire string) {
	start := time.Now()
	p.total.Add(1)
	p.inflight.Add(1)
	defer p.inflight.Add(-1)

	body, err := io.ReadAll(io.LimitReader(r.Body, 64<<20))
	if err != nil {
		http.Error(w, "read body: "+err.Error(), 400)
		return
	}
	var req map[string]any
	strictJSON := true
	if err := json.Unmarshal(body, &req); err != nil {
		strictJSON = false
	}
	model := ""
	stream := false
	if req != nil {
		model, _ = req["model"].(string)
		stream, _ = req["stream"].(bool)
	}
	rec := &store.Record{
		Ts:     start.UnixMilli(),
		Key:    InboundKey(r.Context()),
		Model:  model,
		Stream: stream,
	}
	defer func() {
		rec.DurMs = float64(time.Since(start).Milliseconds())
		p.Store.Submit(rec)
	}()

	targets := p.Reg.Resolve(model, allowedModelsFor(r))
	if len(targets) == 0 {
		p.writeError(w, clientWire, 404, "unknown model: "+model)
		rec.Status = 404
		rec.Err = "unknown model"
		return
	}

	activeID := fmt.Sprintf("%d-%d", start.UnixNano(), rand.Int63n(1e9))
	var usage Usage
	var ttft time.Duration
	forwarded := false
	lastErr := ""
	var lastStatus int
	var lastErrBody []byte

	for attempt, tgt := range targets {
		if attempt > 0 {
			// small backoff before failover
			b := time.Duration(100*attempt) * time.Millisecond
			if b > 500*time.Millisecond {
				b = 500 * time.Millisecond
			}
			select {
			case <-time.After(b):
			case <-r.Context().Done():
				rec.Status = 499
				return
			}
		}

		// dispatch throttle (e.g. Z.ai 1302 protection)
		if lim := p.Reg.Limiter(tgt.Provider, tgt.APIKey); lim != nil {
			if err := lim.Wait(r.Context()); err != nil {
				rec.Status = 499
				return
			}
		}

		upModel := provider.UpstreamModel(tgt.Provider, model)
		httpReq, err := p.buildUpstream(r.Context(), tgt, clientWire, upModel, req, body, strictJSON, r.Header.Get("x-opencode-session"))
		if err != nil {
			lastErr = err.Error()
			continue
		}

		resp, err := p.do(tgt, httpReq)
		if err != nil {
			lastErr = err.Error()
			if r.Context().Err() != nil {
				rec.Status = 499
				return
			}
			continue
		}

		// retryable/failing upstream status → next target (nothing forwarded yet).
		// Broad policy on purpose: coding agents send valid requests, and provider-
		// specific 400/401/403s (unknown model id, bad key) should fall through the
		// chain. The last upstream error is forwarded if nothing serves the request.
		if resp.StatusCode >= 400 {
			b, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
			resp.Body.Close()
			lastStatus, lastErrBody, lastErr = resp.StatusCode, b, fmt.Sprintf("%s: http %d", tgt.Provider.Name, resp.StatusCode)
			continue
		}

		// success — stream or translate
		rec.Provider = tgt.Provider.Name
		rec.Attempts = attempt + 1
		p.Active.Set(&ActiveEntry{
			ID: activeID, Model: model, Provider: tgt.Provider.Name,
			Key: rec.Key, Stream: stream, Start: start,
		})
		usage, ttft = p.forward(w, r, clientWire, tgt, resp, req, upModel, stream, activeID)
		p.Active.Done(activeID)

		rec.Status = 200
		rec.TTFTms = float64(ttft.Milliseconds())
		rec.TokIn, rec.TokOut, rec.CacheRead, rec.CacheWrt = usage.In, usage.Out, usage.CacheR, usage.CacheW
		if p.Cost != nil {
			c := p.Cost(model)
			rec.CostUSD = float64(usage.In)/1e6*c.Input + float64(usage.Out)/1e6*c.Output +
				float64(usage.CacheR)/1e6*c.CacheRead + float64(usage.CacheW)/1e6*c.CacheWrite
		}
		forwarded = true
		break
	}

	if !forwarded {
		if rec.Status == 0 {
			if lastStatus > 0 {
				// every target failed — surface the last real upstream error
				rec.Status = lastStatus
				rec.Err = lastErr
				rec.Provider = strings.SplitN(lastErr, ":", 2)[0]
				var out []byte
				if clientWire == WireAnthropic {
					out = translate.ErrToAnthropic(lastErrBody, lastStatus)
				} else {
					out = translate.ErrToOpenAI(lastErrBody, lastStatus)
				}
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(lastStatus)
				w.Write(out)
				return
			}
			rec.Status = 502
			rec.Err = lastErr
			p.writeError(w, clientWire, 502, "all providers failed: "+lastErr)
		}
	}
}

func allowedModelsFor(r *http.Request) []string {
	type keyChecker interface{ Allow() []string }
	// inbound key allow patterns are injected by the server middleware
	if v := r.Context().Value(inboundAllowCtx); v != nil {
		if a, ok := v.([]string); ok {
			return a
		}
	}
	return []string{"*"}
}

const inboundAllowCtx ctxKey = 2

// WithInboundAllow stores the inbound key's allowed model patterns.
func WithInboundAllow(ctx context.Context, allow []string) context.Context {
	return context.WithValue(ctx, inboundAllowCtx, allow)
}

func retryableStatus(code int) bool {
	switch code {
	case 408, 409, 425, 429, 500, 502, 503, 504, 529:
		return true
	}
	return false
}

// buildUpstream constructs the upstream request. For openai upstreams the
// client body may need translation; for anthropic upstreams likewise.
func (p *Proxy) buildUpstream(ctx context.Context, tgt *provider.Target, clientWire, upModel string, req map[string]any, rawBody []byte, strictJSON bool, clientSession string) (*http.Request, error) {
	pv := tgt.Provider
	url := strings.TrimRight(pv.BaseURL, "/")
	var bodyOut []byte
	opts := translate.Options{
		InjectCacheControl: pv.InjectCacheControl,
		AdaptiveThinking:   pv.AdaptiveThinking,
	}

	switch {
	case pv.Wire == clientWire:
		// same wire: patch model id (+ body overrides) via JSON re-encode when needed
		if strictJSON {
			req["model"] = upModel
			for k, v := range pv.BodyOverrides {
				if _, exists := req[k]; !exists {
					req[k] = v
				}
			}
			bodyOut, _ = json.Marshal(req)
		} else {
			bodyOut = rawBody // ponytail: unparseable body passes untouched; model patch skipped
		}
		if pv.Wire == WireOpenAI {
			url += "/chat/completions"
		} else {
			url += "/v1/messages"
			// anthropic base URLs may already end in /v1 or /api/anthropic etc:
			// convention: base_url includes everything up to (not including) the endpoint path.
		}
	case pv.Wire == WireAnthropic:
		// openai client -> anthropic upstream
		if !strictJSON {
			return nil, errors.New("unparseable openai body for translation")
		}
		req["model"] = upModel
		a := translate.OpenAIReqToAnthropic(req, opts)
		for k, v := range pv.BodyOverrides {
			if _, exists := a[k]; !exists {
				a[k] = v
			}
		}
		bodyOut, _ = json.Marshal(a)
		url += "/v1/messages"
	default: // anthropic client -> openai upstream
		if !strictJSON {
			return nil, errors.New("unparseable anthropic body for translation")
		}
		req["model"] = upModel
		o := translate.AnthropicReqToOpenAI(req, opts)
		for k, v := range pv.BodyOverrides {
			if _, exists := o[k]; !exists {
				o[k] = v
			}
		}
		bodyOut, _ = json.Marshal(o)
		url += "/chat/completions"
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(bodyOut))
	if err != nil {
		return nil, err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Accept", "text/event-stream, application/json")
	if tgt.AuthType == "oauth" {
		token := tgt.APIKey
		if token == "" {
			token = oauthToken(tgt)
		}
		setAuth(httpReq, pv.Wire, token)
	} else {
		setAuth(httpReq, pv.Wire, tgt.APIKey)
	}
	for k, v := range pv.ExtraHeaders {
		httpReq.Header.Set(k, v)
	}
	if pv.Session == "opencode" {
		sid := clientSession
		if sid == "" {
			sid = p.opencodeSession(pv.Name + ":" + tgt.APIKey)
		}
		httpReq.Header.Set("x-opencode-session", sid)
		httpReq.Header.Set("x-opencode-client", "agent-router")
	}
	// helpful attribution headers
	httpReq.Header.Set("User-Agent", "agent-router/"+p.Version)
	return httpReq, nil
}

// opencodeSession returns a stable uuid per provider+key so upstream routing
// sticks (opencode requires the header; clients that send their own win).

func oauthToken(tgt *provider.Target) string {
	// Phase 4 hook: OAuth token providers plug in here.
	return ""
}

var opencodeSessions sync.Map

func (p *Proxy) opencodeSession(k string) string {
	if v, ok := opencodeSessions.Load(k); ok {
		return v.(string)
	}
	b := make([]byte, 16)
	cryptorand.Read(b)
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	v := fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
	actual, _ := opencodeSessions.LoadOrStore(k, v)
	return actual.(string)
}

func setAuth(h *http.Request, wire, key string) {
	if key == "" {
		return
	}
	if wire == WireAnthropic {
		h.Header.Set("x-api-key", key)
		h.Header.Set("anthropic-version", "2023-06-01")
	} else {
		h.Header.Set("Authorization", "Bearer "+key)
	}
}

var hopHeaders = []string{
	"Connection", "Proxy-Connection", "Keep-Alive", "Proxy-Authenticate",
	"Proxy-Authorization", "Te", "Trailer", "Transfer-Encoding", "Upgrade",
	"Content-Length",
}

// do sends the request with a headers timeout; streaming bodies are not bounded.
func (p *Proxy) do(tgt *provider.Target, req *http.Request) (*http.Response, error) {
	ctx := req.Context()
	ctx, cancel := context.WithCancel(ctx)
	req = req.WithContext(ctx)
	ht := tgt.Provider.HeadersTimeoutS
	if ht <= 0 {
		ht = 300
	}
	t := time.AfterFunc(time.Duration(ht)*time.Second, cancel)
	resp, err := p.Client.Do(req)
	t.Stop()
	if err != nil {
		cancel()
		return nil, err
	}
	// keep cancel alive for the body pump: store in resp via Unwrap? Simplest:
	// attach to resp.Body via a wrapper that cancels on Close.
	resp.Body = &cancelBody{ReadCloser: resp.Body, cancel: cancel}
	return resp, nil
}

type cancelBody struct {
	io.ReadCloser
	cancel context.CancelFunc
	once   sync.Once
}

func (b *cancelBody) Close() error {
	err := b.ReadCloser.Close()
	b.once.Do(b.cancel)
	return err
}

// forward writes the upstream response to the client. Returns usage + ttft.
func (p *Proxy) forward(w http.ResponseWriter, r *http.Request, clientWire string, tgt *provider.Target, resp *http.Response, req map[string]any, upModel string, stream bool, activeID string) (Usage, time.Duration) {
	defer resp.Body.Close()
	var firstTouch time.Time
	touch := func() {
		if firstTouch.IsZero() {
			firstTouch = time.Now()
			p.Active.TTFT(activeID, float64(firstTouch.Sub(startTime(r)).Milliseconds()))
		}
	}

	// upstream wire == client wire → byte-exact passthrough (fast path)
	if tgt.Provider.Wire == clientWire {
		copyHeaders(w, resp)
		w.WriteHeader(resp.StatusCode)
		tee := NewUsageTee(clientWire)
		if stream {
			flusher, _ := w.(http.Flusher)
			buf := make([]byte, 32*1024)
			for {
				n, err := resp.Body.Read(buf)
				if n > 0 {
					touch()
					tee.Write(buf[:n])
					if _, werr := w.Write(buf[:n]); werr != nil {
						return tee.Usage().Estimated(), sinceT(r, firstTouch) // client gone → ctx cancels upstream
					}
					if flusher != nil {
						flusher.Flush()
					}
				}
				if err != nil {
					break
				}
			}
			return tee.Usage().Estimated(), sinceT(r, firstTouch)
		}
		b, err := io.ReadAll(resp.Body)
		if err != nil {
			return Usage{Estimate: true}, sinceT(r, firstTouch)
		}
		touch()
		tee.Write(b)
		w.Write(b)
		return tee.Usage(), sinceT(r, firstTouch)
	}

	// wire translation path
	if stream {
		return p.forwardTranslateStream(w, r, clientWire, tgt, resp, req, upModel, touch, &firstTouch)
	}
	return p.forwardTranslateFull(w, r, clientWire, tgt, resp, req, upModel, touch, &firstTouch)
}

func startTime(r *http.Request) time.Time {
	if v := r.Context().Value(startCtxKey); v != nil {
		if t, ok := v.(time.Time); ok {
			return t
		}
	}
	return time.Now()
}

func sinceT(r *http.Request, t time.Time) time.Duration {
	if t.IsZero() {
		return 0
	}
	return t.Sub(startTime(r))
}

const startCtxKey ctxKey = 3

// WithStartTime records when the request entered the server.
func WithStartTime(ctx context.Context, t time.Time) context.Context {
	return context.WithValue(ctx, startCtxKey, t)
}

// forwardTranslateStream: upstream streams in provider wire, client expects the other wire.
func (p *Proxy) forwardTranslateStream(w http.ResponseWriter, r *http.Request, clientWire string, tgt *provider.Target, resp *http.Response, req map[string]any, upModel string, touch func(), firstTouch *time.Time) (Usage, time.Duration) {
	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-cache")
	h.Set("X-Accel-Buffering", "no")
	w.WriteHeader(200)
	flusher, _ := w.(http.Flusher)

	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 64*1024), 4<<20)

	var usage Usage
	writeEvent := func(event string, data map[string]any) error {
		touch()
		var b []byte
		var err error
		if clientWire == WireAnthropic {
			name := event
			if name == "" {
				name, _ = data["type"].(string)
			}
			b, err = json.Marshal(data)
			if err != nil {
				return err
			}
			if name != "" {
				_, err = fmt.Fprintf(w, "event: %s\ndata: %s\n\n", name, b)
			} else {
				_, err = fmt.Fprintf(w, "data: %s\n\n", b)
			}
		} else {
			b, err = json.Marshal(data)
			if err != nil {
				return err
			}
			_, err = fmt.Fprintf(w, "data: %s\n\n", b)
		}
		if err == nil && flusher != nil {
			flusher.Flush()
		}
		return err
	}

	if clientWire == WireAnthropic {
		// upstream openai → client anthropic
		tr := translate.NewOAI2AnthStream(req["model"].(string))
		for sc.Scan() {
			line := sc.Bytes()
			if !bytes.HasPrefix(line, []byte("data:")) {
				continue
			}
			data := strings.TrimSpace(string(line[5:]))
			if data == "[DONE]" {
				for _, ev := range tr.Finish() {
					if writeEvent(ev.Name, ev.Data) != nil {
						return usage, 0
					}
				}
				break
			}
			var chunk map[string]any
			if json.Unmarshal([]byte(data), &chunk) != nil {
				continue
			}
			if u := asUsageMap(chunk["usage"]); u != nil {
				usage.In = num(u["prompt_tokens"])
				usage.Out = num(u["completion_tokens"])
				usage.CacheR = num(u["cache_read_tokens"])
				usage.CacheW = num(u["cache_write_tokens"])
			}
			for _, ev := range tr.Chunk(chunk) {
				if writeEvent(ev.Name, ev.Data) != nil {
					return usage, 0
				}
			}
		}
	} else {
		// upstream anthropic → client openai
		tr := translate.NewAnth2OAIStream(req["model"].(string))
		eventName := ""
		for sc.Scan() {
			line := strings.TrimSpace(sc.Text()) // trim \r
			if line == "" {
				eventName = ""
				continue
			}
			if strings.HasPrefix(line, "event:") {
				eventName = strings.TrimSpace(strings.TrimPrefix(line, "event:"))
				continue
			}
			if !strings.HasPrefix(line, "data:") {
				continue
			}
			data := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
			var ev map[string]any
			if json.Unmarshal([]byte(data), &ev) != nil {
				continue
			}
			p.captureAnthropicUsage(&usage, eventName, ev)
			if eventName == "message_stop" {
				for _, chunk := range tr.Event(eventName, ev) {
					if writeEvent("", chunk) != nil {
						return usage, 0
					}
				}
				writeDone(w)
				break
			}
			for _, chunk := range tr.Event(eventName, ev) {
				if writeEvent("", chunk) != nil {
					return usage, 0
				}
			}
		}
		// upstream ended without message_stop: emit finish so client isn't left hanging
		if tr.SawStart() {
			for _, chunk := range tr.Event("message_delta", map[string]any{
				"delta": map[string]any{"stop_reason": "end_turn"},
				"usage": map[string]any{"output_tokens": usage.Out},
			}) {
				if writeEvent("", chunk) != nil {
					break
				}
			}
			writeEvent("", tr.UsageChunk())
			writeDone(w)
		}
	}
	usage.Estimate = usage.In == 0 && usage.Out == 0
	if usage.Estimate {
		usage.In = usage.estBytes / 4
	}
	return usage, 0
}

func writeDone(w http.ResponseWriter) {
	if f, _ := w.(http.Flusher); f != nil {
		fmt.Fprint(w, "data: [DONE]\n\n")
		f.Flush()
	}
}

// forwardTranslateFull handles non-streaming translation.
func (p *Proxy) forwardTranslateFull(w http.ResponseWriter, r *http.Request, clientWire string, tgt *provider.Target, resp *http.Response, req map[string]any, upModel string, touch func(), firstTouch *time.Time) (Usage, time.Duration) {
	body, err := io.ReadAll(io.LimitReader(resp.Body, 64<<20))
	touch()
	if err != nil {
		p.writeError(w, clientWire, 502, "upstream read: "+err.Error())
		return Usage{Estimate: true}, sinceT(r, *firstTouch)
	}
	if resp.StatusCode >= 400 {
		p.forwardError(w, nil, clientWire, tgt.Provider.Wire, resp)
		return ParseUsageJSON(tgt.Provider.Wire, body), 0
	}
	var m map[string]any
	if json.Unmarshal(body, &m) != nil {
		p.writeError(w, clientWire, 502, "upstream returned non-JSON body")
		return Usage{Estimate: true}, 0
	}
	usage := ParseUsageJSON(tgt.Provider.Wire, body)
	var out map[string]any
	if clientWire == WireAnthropic {
		out = translate.OpenAIRespToAnthropic(m)
	} else {
		out = translate.AnthropicRespToOpenAI(m)
	}
	b, _ := json.Marshal(out)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(200)
	w.Write(b)
	return usage, 0
}

// forwardError relays an upstream >=400 response in the client's wire shape.
func (p *Proxy) forwardError(w http.ResponseWriter, r *http.Request, clientWire, upstreamWire string, resp *http.Response) {
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	var out []byte
	if clientWire == WireAnthropic {
		out = translate.ErrToAnthropic(body, resp.StatusCode)
	} else {
		out = translate.ErrToOpenAI(body, resp.StatusCode)
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(resp.StatusCode)
	w.Write(out)
}

func (p *Proxy) writeError(w http.ResponseWriter, wire string, status int, msg string) {
	var b []byte
	if wire == WireAnthropic {
		b, _ = json.Marshal(map[string]any{"type": "error",
			"error": map[string]any{"type": "api_error", "message": msg}})
	} else {
		b, _ = json.Marshal(map[string]any{"error": map[string]any{
			"message": msg, "type": "api_error", "code": status}})
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	w.Write(b)
}

func (p *Proxy) captureAnthropicUsage(usage *Usage, event string, ev map[string]any) {
	switch event {
	case "message_start":
		if msg := asUsageMap(ev["message"]); msg != nil {
			if u := asUsageMap(msg["usage"]); u != nil {
				usage.In = num(u["input_tokens"])
				usage.CacheR = num(u["cache_read_input_tokens"])
				usage.CacheW = num(u["cache_creation_input_tokens"])
			}
		}
	case "message_delta":
		if u := asUsageMap(ev["usage"]); u != nil {
			if v := num(u["output_tokens"]); v > 0 {
				usage.Out = v
			}
			if v := num(u["input_tokens"]); v > 0 {
				usage.In = v
			}
		}
	}
}

func asUsageMap(v any) map[string]any {
	m, _ := v.(map[string]any)
	return m
}

func copyHeaders(w http.ResponseWriter, resp *http.Response) {
	h := w.Header()
	for k, vv := range resp.Header {
		skip := false
		for _, hp := range hopHeaders {
			if strings.EqualFold(k, hp) {
				skip = true
				break
			}
		}
		if !skip {
			for _, v := range vv {
				h.Add(k, v)
			}
		}
	}
}

func init() {
	log.SetFlags(log.LstdFlags | log.Lmicroseconds)
}
