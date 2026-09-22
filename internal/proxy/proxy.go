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
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/bacnh85/yardmaster/internal/auth"
	"github.com/bacnh85/yardmaster/internal/config"
	"github.com/bacnh85/yardmaster/internal/provider"
	"github.com/bacnh85/yardmaster/internal/store"
	"github.com/bacnh85/yardmaster/internal/translate"
	"github.com/bacnh85/yardmaster/internal/zcode"
)

const (
	WireOpenAI    = "openai"
	WireAnthropic = "anthropic"
	WireResponses = "responses" // OpenAI Responses API (Codex backend)
)

type Proxy struct {
	Reg     *provider.Registry
	Client  *http.Client
	Store   *store.Store
	Version string
	Cost    func(model string) config.Cost
	Pool    *auth.OAuthPool
	Cd      *provider.Cooldowns
	DumpDir string // debug: write upstream request bodies here (YARDMASTER_DUMP_DIR)

	Active   Active
	inflight atomic.Int64
	total    atomic.Int64
	dumpSeq  atomic.Int64

	zc     *zcode.Manager // zai zcode_signing parity (lazy — see zcodeManager)
	zcOnce sync.Once
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
		Cd:     provider.NewCooldowns(),
	}
}

// ---- active request registry (live dashboard) ----

type ActiveEntry struct {
	ID       string    `json:"id"`
	Model    string    `json:"model"`
	Provider string    `json:"provider"`
	Key      string    `json:"key"`
	Stream   bool      `json:"stream"`
	Start    time.Time `json:"start"`
	TTFTms   float64   `json:"ttft_ms"`
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

// ServeResponses handles POST /v1/responses (OpenAI Responses wire in).
func (p *Proxy) ServeResponses(w http.ResponseWriter, r *http.Request) {
	p.serve(w, r, WireResponses)
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
	if clientWire == WireResponses && strictJSON && req != nil {
		// normalize the Responses request to the chat hub; the raw body bytes
		// stay original so a Responses-wire upstream gets a byte-exact request
		req = translate.ResponsesReqToChat(req)
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
		limKey := tgt.APIKey
		if tgt.AuthType == "oauth" {
			limKey = "oauth:" + tgt.AcctName
			// quota-cooled oauth accounts are skipped, not burned as a failover attempt
			if p.Pool != nil && p.Pool.Cooling(tgt.Provider.Name, tgt.AcctName) {
				lastErr = fmt.Sprintf("%s: account %s cooling down", tgt.Provider.Name, tgt.AcctName)
				continue
			}
		} else if p.Cd != nil && p.Cd.Cooling(tgt.Provider.Name, limKey) {
			// rate-limited / breaker-tripped static keys are skipped too
			lastErr = fmt.Sprintf("%s: key cooling down", tgt.Provider.Name)
			continue
		}
		if lim := p.Reg.Limiter(tgt.Provider, limKey); lim != nil {
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
			if tgt.AuthType == "oauth" && p.Pool != nil {
				p.Pool.MarkResult(tgt.Provider.Name, tgt.AcctName, resp.StatusCode, string(b))
			}
			if p.Cd != nil {
				if resp.StatusCode == 429 {
					p.Cd.Mark429(tgt.Provider.Name, limKey, resp.Header.Get("Retry-After"))
				} else if resp.StatusCode == 500 || resp.StatusCode == 502 || resp.StatusCode == 503 || resp.StatusCode == 504 || resp.StatusCode == 529 {
					p.Cd.MarkFail(tgt.Provider.Name, limKey)
				}
			}
			continue
		}

		// success — stream or translate
		if tgt.AuthType == "oauth" && p.Pool != nil {
			p.Pool.MarkResult(tgt.Provider.Name, tgt.AcctName, 200, "") // reset cooldown ladder
		}
		if p.Cd != nil {
			p.Cd.Reset(tgt.Provider.Name, limKey)
		}
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
	// DeepSeek-family reasoning models reject assistant turns without
	// reasoning_content while thinking mode is active (see translate.
	// InjectDeepseekReasoningPassback). Applied to every openai-wire request
	// for a matching model so client support (pi extension flag, CLI version)
	// no longer matters.
	injectPassback := pv.Wire == WireOpenAI && isDeepseekFamily(upModel)

	switch {
	case pv.Wire == clientWire || (clientWire == WireResponses && pv.Wire == WireOpenAI):
		// same wire (incl. responses↔responses), or a responses client (req
		// pre-normalized to the chat hub) hitting an openai upstream: patch model
		// id (+ body overrides) via JSON re-encode when needed
		if strictJSON {
			if pv.Wire == WireResponses {
				// responses client: req was normalized to the chat hub — re-patch
				// the ORIGINAL responses body instead so upstream gets native wire
				var orig map[string]any
				json.Unmarshal(rawBody, &orig)
				orig["model"] = upModel
				for k, v := range pv.BodyOverrides {
					if _, exists := orig[k]; !exists {
						orig[k] = v
					}
				}
				bodyOut, _ = json.Marshal(orig)
			} else {
				req["model"] = upModel
				for k, v := range pv.BodyOverrides {
					if _, exists := req[k]; !exists {
						req[k] = v
					}
				}
				if injectPassback {
					translate.InjectDeepseekReasoningPassback(req)
				}
				// same-wire anthropic passthrough (e.g. Claude Code → zai): the
				// cross-wire translate never runs, so inject cache markers here
				if pv.Wire == WireAnthropic && opts.InjectCacheControl {
					translate.InjectCacheControlAnthropic(req)
				}
				bodyOut, _ = json.Marshal(req)
			}
		} else {
			bodyOut = rawBody // ponytail: unparseable body passes untouched; model patch skipped
		}
		if pv.Wire == WireOpenAI {
			url += "/chat/completions"
		} else if pv.Wire == WireResponses {
			url += "/responses"
		} else {
			url = anthropicEndpoint(url)
		}
	case pv.Wire == WireResponses:
		// any client wire -> Responses upstream (Codex backend). Chat wire is
		// the hub: anthropic clients are normalized first, then chat→responses.
		chat := req
		if clientWire == WireAnthropic {
			if !strictJSON {
				return nil, errors.New("unparseable anthropic body for translation")
			}
			chat = translate.AnthropicReqToOpenAI(req, opts)
		}
		if strictJSON {
			chat["model"] = upModel
			for k, v := range pv.BodyOverrides {
				if _, exists := chat[k]; !exists {
					chat[k] = v
				}
			}
		}
		bodyOut, _ = json.Marshal(translate.ChatReqToResponses(chat))
		url += "/responses"
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
		url = anthropicEndpoint(url)
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
		if injectPassback {
			translate.InjectDeepseekReasoningPassback(o)
		}
		bodyOut, _ = json.Marshal(o)
		url += "/chat/completions"
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(bodyOut))
	if err != nil {
		return nil, err
	}
	if p.DumpDir != "" { // debug body capture (cache-hit diagnosis)
		seq := p.dumpSeq.Add(1)
		name := fmt.Sprintf("%s/%03d-%s-%s.json", p.DumpDir, seq, tgt.Provider.Name, strings.ReplaceAll(upModel, "/", "_"))
		os.WriteFile(name, bodyOut, 0o600)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Accept", "text/event-stream, application/json")
	if tgt.AuthType == "oauth" {
		p.setOAuthAuth(httpReq, tgt, findAcct(pv, tgt.AcctName))
	} else {
		SetAuth(httpReq, pv.Wire, tgt.APIKey)
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
		httpReq.Header.Set("x-opencode-client", "yardmaster")
	}
	// helpful attribution headers (zcode signing, below, replaces the UA)
	httpReq.Header.Set("User-Agent", "github.com/bacnh85/yardmaster/"+p.Version)
	if pv.ZcodeSigning {
		zc := p.zcodeManager()
		for k, v := range zc.Identity.Headers() {
			httpReq.Header.Set(k, v) // ZCode identity (incl. its User-Agent) wins
		}
		httpReq.Header.Set("X-Session-Id", p.opencodeSession("zcode:"+pv.Name+":"+tgt.APIKey))
		zc.Sign(ctx, httpReq) // fail-open: unsigned on any ineligible/failed path
	}
	return httpReq, nil
}

// anthropicEndpoint appends /v1/messages, tolerating base URLs that already
// end in /v1 (e.g. https://opencode.ai/zen/v1 → …/zen/v1/messages).
func anthropicEndpoint(base string) string {
	return strings.TrimSuffix(base, "/v1") + "/v1/messages"
}

// opencodeSession returns a stable uuid per provider+key so upstream routing
// sticks (opencode requires the header; clients that send their own win).

// findAcct locates an oauth account config inside a provider.
func findAcct(pv *config.Provider, name string) *config.OAuthAcct {
	for _, a := range pv.Auth.OAuth {
		if a.Name == name {
			return a
		}
	}
	return nil
}

// setOAuthAuth applies the account kind's auth headers (mimicking the CLI
// clients these subscriptions belong to). Applied before provider
// extra_headers so a user override wins.
func (p *Proxy) setOAuthAuth(h *http.Request, tgt *provider.Target, acct *config.OAuthAcct) {
	pv := tgt.Provider
	var token string
	if p.Pool != nil && tgt.AcctName != "" {
		t, err := p.Pool.Token(h.Context(), pv.Name, tgt.AcctName)
		if err != nil {
			token = "" // buildUpstream callers treat empty as a build failure? no —
			// fall through with whatever we have; upstream 401s feed MarkResult
			if acct != nil && acct.AccessTok != "" {
				token = acct.AccessTok
			}
		} else {
			token = t
		}
	}
	if token == "" {
		return
	}
	kind := ""
	if acct != nil {
		kind = acct.Kind
	}
	switch kind {
	case "claude-code":
		// Claude Code subscription: OAuth bearer on the anthropic wire
		h.Header.Set("Authorization", "Bearer "+token)
		h.Header.Set("anthropic-beta", "oauth-2025-04-20")
		h.Header.Set("x-app", "cli")
		h.Header.Set("User-Agent", "claude-cli/2.0.0 (external, cli)")
	case "codex":
		// Codex/ChatGPT subscription: bearer on the responses wire
		h.Header.Set("Authorization", "Bearer "+token)
		if acct != nil && acct.AccountID != "" {
			h.Header.Set("chatgpt-account-id", acct.AccountID)
		}
		h.Header.Set("OpenAI-Beta", "responses=experimental")
		h.Header.Set("originator", "codex_cli_rs")
		h.Header.Set("User-Agent", "codex_cli_rs/0.1.0")
	default:
		SetAuth(h, pv.Wire, token)
	}
}

var opencodeSessions sync.Map

// isDeepseekFamily reports whether the upstream model id belongs to the
// reasoning-model families that require reasoning_content passback in
// thinking mode — the exact set pi's native opencode-go catalog flags with
// compat.requiresReasoningContentOnAssistantMessages (deepseek-v4*, glm-5.1,
// kimi-k2.7-code). Extended if a new family starts enforcing it.
func isDeepseekFamily(model string) bool {
	m := strings.ToLower(model)
	if i := strings.LastIndexByte(m, '/'); i >= 0 {
		m = m[i+1:] // tolerate prefixed ids
	}
	return strings.HasPrefix(m, "deepseek") || m == "glm-5.1" || m == "kimi-k2.7-code"
}

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

// SetAuth applies the wire-appropriate static-key auth headers (x-api-key +
// anthropic-version for the anthropic wire, Bearer otherwise).
func SetAuth(h *http.Request, wire, key string) {
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
func (p *Proxy) zcodeManager() *zcode.Manager {
	p.zcOnce.Do(func() {
		m := zcode.NewManager(zcode.DefaultIdentity(), nil)
		m.Logf = func(msg string) { log.Printf("[zcode] %s", strings.TrimPrefix(msg, "client-signing: ")) }
		p.zc = m
	})
	return p.zc
}

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
	if tgt.Provider.ZcodeSigning {
		p.zcodeManager().NoteStatus(tgt.APIKey, resp.StatusCode) // 401 ladder, scoped to this credential
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
		// full-body usage: the line-based tee can't parse a JSON body without
		// "data:" lines — use the exact-body parser (falls back to estimate)
		return ParseUsageJSON(clientWire, b), sinceT(r, firstTouch)
	}

	// wire translation path
	if tgt.Provider.Wire == WireResponses {
		// normalize the Responses wire to openai chat up front: SSE streams are
		// translated byte-stream-level; non-stream JSON in forwardTranslateFull.
		if stream {
			resp.Body = newRespToChatBody(resp.Body, upModel)
		}
	}
	if stream {
		return p.forwardTranslateStream(w, r, clientWire, tgt, resp, req, upModel, touch, &firstTouch)
	}
	return p.forwardTranslateFull(w, r, clientWire, tgt, resp, req, upModel, touch, &firstTouch)
}

// respToChatBody converts a Responses-API SSE stream into openai
// chat-completions SSE bytes on the fly (no buffering beyond one event).
type respToChatBody struct {
	src       io.ReadCloser
	sc        *bufio.Scanner
	rc        *translate.Resp2ChatStream
	buf       bytes.Buffer
	eventName string
	eof       bool
	scanErr   error
}

func newRespToChatBody(src io.ReadCloser, model string) *respToChatBody {
	b := &respToChatBody{src: src, rc: translate.NewResp2ChatStream(model), sc: bufio.NewScanner(src)}
	b.sc.Buffer(make([]byte, 64*1024), 4<<20)
	return b
}

func (b *respToChatBody) Read(p []byte) (int, error) {
	for b.buf.Len() == 0 {
		if b.eof {
			if b.scanErr != nil {
				return 0, b.scanErr
			}
			return 0, io.EOF
		}
		if !b.sc.Scan() {
			b.eof = true
			if err := b.sc.Err(); err != nil {
				// transport-level break (reset, oversized line): do NOT synthesize
				// a clean [DONE] — surface the error after draining buffered bytes
				b.scanErr = err
				continue
			}
			for _, ch := range b.rc.Done() {
				b.writeChunk(ch)
			}
			b.buf.WriteString("data: [DONE]\n\n")
			continue
		}
		line := strings.TrimSpace(b.sc.Text())
		switch {
		case line == "":
			b.eventName = ""
		case strings.HasPrefix(line, "event:"):
			b.eventName = strings.TrimSpace(strings.TrimPrefix(line, "event:"))
		case strings.HasPrefix(line, "data:"):
			var ev map[string]any
			if json.Unmarshal([]byte(strings.TrimSpace(strings.TrimPrefix(line, "data:"))), &ev) == nil {
				for _, ch := range b.rc.Event(b.eventName, ev) {
					b.writeChunk(ch)
				}
			}
		}
	}
	return b.buf.Read(p)
}

func (b *respToChatBody) writeChunk(ch map[string]any) {
	d, _ := json.Marshal(ch)
	b.buf.WriteString("data: " + string(d) + "\n\n")
}

func (b *respToChatBody) Close() error { return b.src.Close() }

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
	// model may be absent (e.g. matched via a "*" catch-all route) — never
	// type-assert directly or a missing key panics mid-stream
	clientModel, _ := req["model"].(string)
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
		if named := clientWire != WireOpenAI; named {
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

	if clientWire == WireResponses {
		// any non-responses upstream (chat-hub format) → client responses SSE
		tr := translate.NewChat2RespStream(clientModel)
		feedChat := func(chunk map[string]any) bool {
			if u := asUsageMap(chunk["usage"]); u != nil {
				applyOpenAIUsage(&usage, u)
			}
			for _, e := range tr.Chunk(chunk) {
				if writeEvent(e.Name, e.Data) != nil {
					return false
				}
			}
			return true
		}
		if tgt.Provider.Wire == WireAnthropic {
			// anthropic upstream → chat chunks (Anth2OAI) → responses events
			a2o := translate.NewAnth2OAIStream(clientModel)
			eventName := ""
			done := false
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
				stop := eventName == "message_stop"
				for _, chunk := range a2o.Event(eventName, ev) {
					if !feedChat(chunk) {
						return usage, sinceT(r, *firstTouch)
					}
				}
				if stop {
					done = true
					break
				}
			}
			// upstream ended without message_stop: synthesize the tail
			if a2o.SawStart() && !done {
				for _, chunk := range a2o.Event("message_delta", map[string]any{
					"delta": map[string]any{"stop_reason": "end_turn"},
					"usage": map[string]any{"output_tokens": usage.Out},
				}) {
					if !feedChat(chunk) {
						break
					}
				}
				feedChat(a2o.UsageChunk())
			}
			if err := sc.Err(); err != nil {
				log.Printf("stream translate: upstream read error: %v", err)
			}
		} else {
			// upstream openai chat SSE (or responses-normalized) → responses
			for sc.Scan() {
				line := sc.Bytes()
				if !bytes.HasPrefix(line, []byte("data:")) {
					continue
				}
				data := strings.TrimSpace(string(line[5:]))
				if data == "[DONE]" {
					break
				}
				var chunk map[string]any
				if json.Unmarshal([]byte(data), &chunk) != nil {
					continue
				}
				if !feedChat(chunk) {
					return usage, sinceT(r, *firstTouch)
				}
			}
			if err := sc.Err(); err != nil {
				log.Printf("stream translate: upstream read error: %v", err)
			}
		}
		for _, e := range tr.Finish() {
			writeEvent(e.Name, e.Data)
		}
		usage.Estimate = usage.In == 0 && usage.Out == 0
		if usage.Estimate {
			usage.In = usage.estBytes / 4
		}
		return usage, 0
	}

	if clientWire == WireAnthropic {
		// upstream openai → client anthropic
		tr := translate.NewOAI2AnthStream(clientModel)
		done := false
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
				done = true
				break
			}
			var chunk map[string]any
			if json.Unmarshal([]byte(data), &chunk) != nil {
				continue
			}
			if u := asUsageMap(chunk["usage"]); u != nil {
				applyOpenAIUsage(&usage, u)
			}
			for _, ev := range tr.Chunk(chunk) {
				if writeEvent(ev.Name, ev.Data) != nil {
					return usage, 0
				}
			}
		}
		// upstream ended without [DONE] (drop/timeout): still terminate the
		// anthropic stream or the client hangs until its own timeout
		if !done {
			for _, ev := range tr.Finish() {
				if writeEvent(ev.Name, ev.Data) != nil {
					break
				}
			}
		}
	} else if tgt.Provider.Wire == WireResponses {
		// upstream responses (normalized to openai chat chunks by respToChatBody)
		for sc.Scan() {
			line := sc.Bytes()
			if !bytes.HasPrefix(line, []byte("data:")) {
				continue
			}
			data := strings.TrimSpace(string(line[5:]))
			if data == "[DONE]" {
				break
			}
			var chunk map[string]any
			if json.Unmarshal([]byte(data), &chunk) != nil {
				continue
			}
			if u := asUsageMap(chunk["usage"]); u != nil {
				applyOpenAIUsage(&usage, u)
			}
			if writeEvent("", chunk) != nil {
				return usage, sinceT(r, *firstTouch)
			}
		}
		if err := sc.Err(); err != nil {
			log.Printf("stream translate: upstream read error: %v", err)
		}
		if err := sc.Err(); err != nil {
			log.Printf("stream translate: upstream read error: %v", err)
		}
		writeDone(w)
	} else {
		// upstream anthropic → client openai
		tr := translate.NewAnth2OAIStream(clientModel)
		eventName := ""
		done := false
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
						return usage, sinceT(r, *firstTouch)
					}
				}
				writeDone(w)
				done = true
				break
			}
			for _, chunk := range tr.Event(eventName, ev) {
				if writeEvent("", chunk) != nil {
					return usage, sinceT(r, *firstTouch)
				}
			}
		}
		// upstream ended without message_stop: emit finish so client isn't left hanging
		if tr.SawStart() && !done {
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
	upWire := tgt.Provider.Wire
	if upWire == WireResponses {
		// normalize to openai chat completion, then reuse the openai paths
		m = translate.ResponsesRespToChat(m)
		body, _ = json.Marshal(m)
		upWire = WireOpenAI
	}
	if upWire == WireAnthropic {
		// non-stream anthropic body must be normalized too, else it leaks raw
		// to openai/responses clients
		m = translate.AnthropicRespToOpenAI(m)
		body, _ = json.Marshal(m)
		upWire = WireOpenAI
	}
	usage := ParseUsageJSON(upWire, body)
	var out map[string]any
	if clientWire == WireAnthropic {
		out = translate.OpenAIRespToAnthropic(m)
	} else if clientWire == WireResponses {
		out = translate.ChatRespToResponses(m)
	} else {
		out = m
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
			// Z.ai reports cache fields only here, not in message_start
			if v := num(u["cache_read_input_tokens"]); v > 0 {
				usage.CacheR = v
			}
			if v := num(u["cache_creation_input_tokens"]); v > 0 {
				usage.CacheW = v
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
