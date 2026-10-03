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
	"net/url"
	"os"
	"strconv"
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
	WireOpenAI     = "openai"
	WireAnthropic  = "anthropic"
	WireResponses  = "responses"  // OpenAI Responses API (Codex backend)
	WireClassifier = "classifier" // System One decision models (TypeSafe Jev)
	WireGemini     = "gemini"     // Gemini generateContent (generativelanguage v1beta)
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

	// QuotaExhausted reports whether a provider's billing windows are fully
	// exhausted (server-computed from live quota reports). Nil = disabled —
	// routing never consults quota state.
	QuotaExhausted func(provider string) bool

	Active   Active
	inflight atomic.Int64
	total    atomic.Int64
	dumpSeq  atomic.Int64

	// affinity holds session→connection pins (prompt-cache routing); only
	// consulted when config.Routing.Affinity.Enabled
	affinity AffinityStore

	zc     *zcode.Manager // zai zcode_signing parity (lazy — see zcodeManager)
	zcOnce sync.Once

	// copilot dispatch-time token exchange cache (provider/account → token);
	// refreshed via p.Client when within copilotTokenMargin of expiry
	copilotMu   sync.Mutex
	copilotToks map[string]copilotTok

	// per-provider proxied dispatch clients (proxy_url), keyed by provider
	// name; built lazily so config reload swaps proxies without a restart
	proxyClients sync.Map // provider name → *http.Client
}

// clientFor returns the http.Client for dispatching to pv's upstream: the
// shared p.Client when the provider has no proxy_url, else a per-provider
// client whose transport is p.Client's CLONED with Proxy set (lazy, cached by
// name+proxy_url so a config reload that changes the proxy takes effect — a
// transport must not be shared between a proxied and a direct dial pool).
// Scope: upstream dispatch only (buildUpstream senders:
// chat/messages/responses forwarding and CountTokens); oauth pool refresh,
// quota windows, and catalog fetches deliberately stay on p.Client — they are
// control-plane traffic, not provider request egress.
func (p *Proxy) clientFor(pv *config.Provider) *http.Client {
	if pv == nil || pv.ProxyURL == "" {
		return p.Client
	}
	key := pv.Name + "\x00" + pv.ProxyURL
	if c, ok := p.proxyClients.Load(key); ok {
		return c.(*http.Client)
	}
	base := p.Client.Transport
	if base == nil {
		base = http.DefaultTransport
	}
	t := base.(*http.Transport).Clone()
	u, err := url.Parse(pv.ProxyURL)
	if err != nil {
		// Validate rejects unparseable proxy_url; keep dispatch alive anyway
		// by degrading to the shared client rather than panicking
		return p.Client
	}
	t.Proxy = http.ProxyURL(u)
	c := &http.Client{Transport: t}
	actual, _ := p.proxyClients.LoadOrStore(key, c)
	return actual.(*http.Client)
}

// NewProxy builds the proxy with a tuned shared transport.
func NewProxy(reg *provider.Registry, st *store.Store, costFn func(string) config.Cost) *Proxy {
	t := http.DefaultTransport.(*http.Transport).Clone()
	t.MaxIdleConnsPerHost = 64
	t.IdleConnTimeout = 90 * time.Second
	t.ForceAttemptHTTP2 = true
	p := &Proxy{
		Reg:    reg,
		Client: &http.Client{Transport: t},
		Store:  st,
		Cost:   costFn,
		Cd:     provider.NewCooldowns(),
	}
	// Static-key cooldowns survive restarts when a store is attached
	// (429 Retry-After windows, breaker trips). Best-effort: a missing or
	// wedged DB degrades to in-memory cooldowns, never fails dispatch.
	if st != nil {
		provider.AttachCooldownStore(p.Cd, st)
	}
	return p
}

// ---- active request registry (live dashboard) ----

type ActiveEntry struct {
	ID       string  `json:"id"`
	Model    string  `json:"model"`
	Provider string  `json:"provider"`
	Key      string  `json:"key"`
	Stream   bool    `json:"stream"`
	Start    int64   `json:"start"` // unix millis
	TTFTms   float64 `json:"ttft_ms"`
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

// ServeGemini handles POST /v1beta/models/{model}:generateContent and
// :streamGenerateContent (Gemini wire in). Streaming is the alt=sse query
// param; the model id rides the URL path, not the body.
func (p *Proxy) ServeGemini(w http.ResponseWriter, r *http.Request) {
	if geminiPathModel(r.URL.Path) == "" {
		p.writeError(w, WireGemini, 404, "gemini path must be /v1beta/models/{model}:generateContent[:streamGenerateContent?alt=sse]")
		return
	}
	p.serve(w, r, WireGemini)
}

// geminiPathModel extracts the model id from /v1beta/models/{model}[:method].
// URL-unescaped already by ServeMux; an empty segment or a method other than
// the two generateContent variants → "" (a :countTokens body must not be
// dispatched as a chat request).
func geminiPathModel(path string) string {
	rest, ok := strings.CutPrefix(path, "/v1beta/models/")
	if !ok || rest == "" {
		return ""
	}
	i := strings.IndexByte(rest, ':')
	if i <= 0 {
		return ""
	}
	switch rest[i+1:] {
	case "generateContent", "streamGenerateContent":
		return rest[:i]
	}
	return ""
}

// ServeClassifier handles POST /v1/systemone (System One decisions in —
// {model, state, questions}; non-streaming JSON in/out). /v1/decisions and
// /v1/classifier are aliases routed to the same handler.
func (p *Proxy) ServeClassifier(w http.ResponseWriter, r *http.Request) {
	p.serve(w, r, WireClassifier)
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
	if clientWire == WireGemini {
		// gemini-native client: the model rides the URL path and streaming is
		// the alt=sse query variant — neither is a body field
		model = geminiPathModel(r.URL.Path)
		stream = r.URL.Query().Get("alt") == "sse"
	} else if req != nil {
		model, _ = req["model"].(string)
		stream, _ = req["stream"].(bool)
	}
	if clientWire == WireClassifier {
		stream = false // decision models are request/response only
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

	if clientWire == WireGemini {
		// normalize to the chat hub with model+stream injected; the raw body
		// stays original so a same-wire gemini upstream still gets the
		// client's byte-exact request
		if !strictJSON {
			p.writeError(w, WireGemini, 400, "unparseable gemini request body")
			rec.Status = 400
			rec.Err = "bad gemini request"
			return
		}
		chat, err := translate.GeminiReqToChat(req)
		if err != nil {
			p.writeError(w, WireGemini, 400, err.Error())
			rec.Status = 400
			rec.Err = "bad gemini request"
			return
		}
		chat["model"] = model // rides the URL path on the gemini wire
		if stream {
			chat["stream"] = true
		}
		req = chat
	}

	// session affinity: extraction happens at the entry (before resolve) so a
	// live pin can reorder the resolved target list before any attempt is
	// made. Off, or no session id on the request, changes nothing — plain
	// routing.
	session := sessionID(p.Reg.Config().Routing.Affinity, clientWire, r, req)

	targets := p.Reg.Resolve(model, allowedModelsFor(r))
	if len(targets) == 0 {
		p.writeError(w, clientWire, 404, "unknown model: "+model)
		rec.Status = 404
		rec.Err = "unknown model"
		return
	}

	pinnedConn := ""
	if providerName, conn, ok := p.affinityLookup(session); ok {
		var pos int
		targets, pos = reorderForAffinity(targets, providerName, conn)
		if pos >= 0 {
			pinnedConn = conn // bound connection is live in this chain
		}
	}

	activeID := fmt.Sprintf("%d-%d", start.UnixNano(), rand.Int63n(1e9))
	var usage Usage
	var ttft time.Duration
	var queueDur time.Duration // dispatch-throttle wait (serving attempt)
	forwarded := false
	lastErr := ""
	var lastStatus int
	var lastErrBody []byte
	lastTgt := (*provider.Target)(nil) // target whose error the relay surfaces
	lastUpModel := ""
	lastAttempt := 0
	var cooledUntil time.Time // latest cooldown expiry among skipped targets
	var quotaSkipped *provider.Target

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

		// quota-exhausted provider: skip without burning an upstream attempt
		// (same shape as a cooling target), but remember the first such target —
		// stale quota data must never hard-block availability, so if every
		// target is quota-skipped the first one is attempted anyway (fail open).
		if tgt.Provider.SkipWhenExhausted && p.QuotaExhausted != nil && p.QuotaExhausted(tgt.Provider.Name) {
			if quotaSkipped == nil {
				quotaSkipped = tgt
			}
			lastErr = fmt.Sprintf("%s: quota exhausted", tgt.Provider.Name)
			continue
		}

		// dispatch throttle (e.g. Z.ai 1302 protection)
		limKey := tgt.APIKey
		if tgt.AuthType == "oauth" {
			limKey = "oauth:" + tgt.AcctName
			// quota-cooled oauth accounts are skipped, not burned as a failover attempt
			if p.Pool != nil && p.Pool.Cooling(tgt.Provider.Name, tgt.AcctName) {
				lastErr = fmt.Sprintf("%s: account %s cooling down", tgt.Provider.Name, tgt.AcctName)
				if u := p.Pool.Until(tgt.Provider.Name, tgt.AcctName); u.After(cooledUntil) {
					cooledUntil = u
				}
				continue
			}
		} else if p.Cd != nil && p.Cd.Cooling(tgt.Provider.Name, limKey) {
			// rate-limited / breaker-tripped static keys are skipped too
			lastErr = fmt.Sprintf("%s: key cooling down", tgt.Provider.Name)
			if u := p.Cd.Until(tgt.Provider.Name, limKey); u.After(cooledUntil) {
				cooledUntil = u
			}
			continue
		}
		if lim := p.Reg.Limiter(tgt.Provider, limKey); lim != nil {
			w0 := time.Now()
			if err := lim.Wait(r.Context()); err != nil {
				rec.Status = 499
				return
			}
			queueDur = time.Since(w0)
		}

		reqModel := model // combo requests dispatch under the natural model id
		if tgt.ModelOverride != "" {
			reqModel = tgt.ModelOverride
		}
		upModel := provider.UpstreamModel(tgt.Provider, reqModel)
		httpReq, err := p.buildUpstream(r.Context(), tgt, clientWire, upModel, req, body, strictJSON, r.Header.Get("x-opencode-session"))
		if err != nil {
			lastErr = err.Error()
			continue
		}

		resp, err := p.do(tgt, httpReq)
		if err != nil {
			// transport-level death of the PINNED connection proves it dead
			// just like an HTTP failure does — clear the pin so the session
			// re-binds on the next served request (only the pinned target's
			// own failure clears; a later-chain failure says nothing)
			if attempt == 0 && pinnedConn != "" && connectionLabel(tgt) == pinnedConn {
				p.affinityClear(session)
			}
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
			// the PINNED connection just proved dead — clear the pin so the
			// session re-binds to whatever serves next (a dead connection
			// must not be re-pinned). Only the pinned target's own failure
			// clears: a later-chain failure says nothing about the pin.
			if attempt == 0 && pinnedConn != "" && connectionLabel(tgt) == pinnedConn {
				p.affinityClear(session)
			}
			b, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
			resp.Body.Close()
			lastStatus, lastErrBody, lastErr = resp.StatusCode, b, fmt.Sprintf("%s: http %d", tgt.Provider.Name, resp.StatusCode)
			lastTgt, lastUpModel, lastAttempt = tgt, upModel, attempt+1
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
			Key: rec.Key, Stream: stream, Start: start.UnixMilli(),
		})
		usage, ttft = p.forward(w, r, clientWire, tgt, resp, req, upModel, reqModel, stream, activeID, attempt+1)
		p.Active.Done(activeID)

		// response completed (forward returned) — pin the session to the
		// serving connection so the next request of the session reorders here
		// and its upstream prompt cache hits. Concurrent same-session requests
		// during a long stream intentionally miss the pin and ride rotation.
		p.affinityPin(session, tgt.Provider.Name, connectionLabel(tgt))

		rec.Status = 200
		rec.TTFTms = float64(ttft.Milliseconds())
		rec.QueueMs = float64(queueDur.Milliseconds())
		rec.TokIn, rec.TokOut, rec.CacheRead, rec.CacheWrt = usage.In, usage.Out, usage.CacheR, usage.CacheW
		if p.Cost != nil {
			c := p.Cost(reqModel) // combo dispatch: price the natural model, not combo/<name>
			rec.CostUSD = float64(usage.In)/1e6*c.Input + float64(usage.Out)/1e6*c.Output +
				float64(usage.CacheR)/1e6*c.CacheRead + float64(usage.CacheW)/1e6*c.CacheWrite
		}
		forwarded = true
		break
	}

	if !forwarded && rec.Status == 0 && quotaSkipped != nil {
		// Every target was quota-skipped: stale billing data must never
		// hard-block availability — fall open by attempting the first
		// quota-skipped target like a plain failover hop. The hop mirrors the
		// main loop: dispatch throttle, combo model override, natural-model
		// pricing.
		fmt.Printf("all targets quota-exhausted, attempting %s anyway (fail-open)\n", quotaSkipped.Provider.Name)
		qLimKey := quotaSkipped.APIKey
		if quotaSkipped.AuthType == "oauth" {
			qLimKey = "oauth:" + quotaSkipped.AcctName
		}
		if lim := p.Reg.Limiter(quotaSkipped.Provider, qLimKey); lim != nil {
			w0 := time.Now()
			if err := lim.Wait(r.Context()); err != nil {
				rec.Status = 499
				return
			}
			queueDur = time.Since(w0)
		}
		qReqModel := model
		if quotaSkipped.ModelOverride != "" {
			qReqModel = quotaSkipped.ModelOverride
		}
		qUpModel := provider.UpstreamModel(quotaSkipped.Provider, qReqModel)
		httpReq, err := p.buildUpstream(r.Context(), quotaSkipped, clientWire, qUpModel, req, body, strictJSON, r.Header.Get("x-opencode-session"))
		if err == nil {
			resp, err := p.do(quotaSkipped, httpReq)
			if err == nil && resp.StatusCode < 400 {
				if quotaSkipped.AuthType == "oauth" && p.Pool != nil {
					p.Pool.MarkResult(quotaSkipped.Provider.Name, quotaSkipped.AcctName, 200, "") // reset cooldown ladder
				}
				if p.Cd != nil {
					p.Cd.Reset(quotaSkipped.Provider.Name, quotaSkipped.APIKey)
				}
				rec.Provider = quotaSkipped.Provider.Name
				rec.Attempts = 1
				p.Active.Set(&ActiveEntry{
					ID: activeID, Model: model, Provider: quotaSkipped.Provider.Name,
					Key: rec.Key, Stream: stream, Start: start.UnixMilli(),
				})
				usage, ttft = p.forward(w, r, clientWire, quotaSkipped, resp, req, qUpModel, qReqModel, stream, activeID, 1)
				p.Active.Done(activeID)
				p.affinityPin(session, quotaSkipped.Provider.Name, connectionLabel(quotaSkipped))
				rec.Status = 200
				rec.TTFTms = float64(ttft.Milliseconds())
				rec.QueueMs = float64(queueDur.Milliseconds())
				rec.TokIn, rec.TokOut, rec.CacheRead, rec.CacheWrt = usage.In, usage.Out, usage.CacheR, usage.CacheW
				if p.Cost != nil {
					c := p.Cost(qReqModel)
					rec.CostUSD = float64(usage.In)/1e6*c.Input + float64(usage.Out)/1e6*c.Output +
						float64(usage.CacheR)/1e6*c.CacheRead + float64(usage.CacheW)/1e6*c.CacheWrite
				}
				forwarded = true
			} else if err == nil {
				// failed open and the upstream said no — same accounting as
				// an in-loop hop so the cooldown ladder learns about it
				if pinnedConn != "" && connectionLabel(quotaSkipped) == pinnedConn {
					p.affinityClear(session) // dead pin must not survive
				}
				b, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
				resp.Body.Close()
				lastStatus, lastErrBody, lastErr = resp.StatusCode, b, fmt.Sprintf("%s: http %d", quotaSkipped.Provider.Name, resp.StatusCode)
				lastTgt, lastUpModel, lastAttempt = quotaSkipped, qUpModel, 1
				if quotaSkipped.AuthType == "oauth" && p.Pool != nil {
					p.Pool.MarkResult(quotaSkipped.Provider.Name, quotaSkipped.AcctName, resp.StatusCode, string(b))
				}
				if p.Cd != nil {
					if resp.StatusCode == 429 {
						p.Cd.Mark429(quotaSkipped.Provider.Name, quotaSkipped.APIKey, resp.Header.Get("Retry-After"))
					} else if resp.StatusCode == 500 || resp.StatusCode == 502 || resp.StatusCode == 503 || resp.StatusCode == 504 || resp.StatusCode == 529 {
						p.Cd.MarkFail(quotaSkipped.Provider.Name, quotaSkipped.APIKey)
					}
				}
			} else {
				lastErr = err.Error()
			}
		} else {
			lastErr = err.Error()
		}
	}

	if !forwarded {
		if rec.Status == 0 {
			if lastStatus > 0 {
				// every target failed — surface the last real upstream error
				rec.Status = lastStatus
				rec.Err = lastErr
				rec.Provider = strings.SplitN(lastErr, ":", 2)[0]
				if lastTgt != nil {
					p.setDecisionHeaders(w, lastTgt, lastUpModel, lastAttempt)
				}
				var out []byte
				if clientWire == WireAnthropic {
					out = translate.ErrToAnthropic(lastErrBody, lastStatus)
				} else if clientWire == WireGemini {
					out = translate.ErrToGemini(lastErrBody, lastStatus)
				} else {
					out = translate.ErrToOpenAI(lastErrBody, lastStatus)
				}
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(lastStatus)
				w.Write(out)
				return
			}
			if !cooledUntil.IsZero() {
				// every target was skipped due to cooldown: the router itself is
				// the rate limiter — answer 429 + Retry-After so clients back off
				// instead of hammering (502 invites immediate retries).
				retry := int(time.Until(cooledUntil).Seconds()) + 1
				if retry < 1 {
					retry = 1
				}
				rec.Status = 429
				rec.Err = lastErr
				p.write429(w, clientWire, retry, "all targets cooling down: "+lastErr)
				return
			}
			rec.Status = 502
			rec.Err = lastErr
			p.writeError(w, clientWire, 502, "all providers failed: "+lastErr)
		}
	}
}

func allowedModelsFor(r *http.Request) []string {
	return allowedModelsForContext(r.Context())
}

// allowedModelsForContext reads the inbound key's allowed model patterns
// (injected by the server middleware) off any context.
func allowedModelsForContext(ctx context.Context) []string {
	// inbound key allow patterns are injected by the server middleware
	if v := ctx.Value(inboundAllowCtx); v != nil {
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

// applyBodyOverrides merges provider body_overrides into the upstream body.
// Shallow top-level merge: mode "" or "fill" only sets keys absent from the
// client body (back-compat); "override" replaces client-sent values outright
// (e.g. forcing reasoning_effort regardless of client preference).
func applyBodyOverrides(pv *config.Provider, body map[string]any) {
	override := pv.BodyOverridesMode == "override"
	for k, v := range pv.BodyOverrides {
		if _, exists := body[k]; exists && !override {
			continue
		}
		body[k] = v
	}
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
	case pv.Wire == clientWire || (clientWire == WireResponses && pv.Wire == WireOpenAI) ||
		(clientWire == WireGemini && pv.Wire == WireOpenAI):
		// same wire (incl. responses↔responses, gemini↔gemini), or a client
		// whose req was normalized to the chat hub (responses / gemini) hitting
		// an openai upstream: patch model id (+ body overrides) via JSON
		// re-encode when needed
		if strictJSON {
			if pv.Wire == WireResponses {
				// responses client: req was normalized to the chat hub — re-patch
				// the ORIGINAL responses body instead so upstream gets native wire
				var orig map[string]any
				json.Unmarshal(rawBody, &orig)
				orig["model"] = upModel
				applyBodyOverrides(pv, orig)
				bodyOut, _ = json.Marshal(orig)
			} else if pv.Wire == WireGemini {
				// gemini client: same re-patch, but the gemini body carries no
				// model (it rides the URL path) — only overrides apply
				var orig map[string]any
				json.Unmarshal(rawBody, &orig)
				applyBodyOverrides(pv, orig)
				bodyOut, _ = json.Marshal(orig)
			} else {
				req["model"] = upModel
				applyBodyOverrides(pv, req)
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
		} else if pv.Wire == WireClassifier {
			// TypeSafe System One surface: api.typesafe.ai/v1 and
			// openrouter.ai/api/v1 both append /systemone (verified live)
			url += "/systemone"
		} else if pv.Wire == WireGemini {
			// stream flag lives on the hub body (serve() sets it from the
			// alt=sse query param for gemini clients); the gemini body itself
			// carries none
			url = geminiEndpoint(url, upModel, req["stream"] == true)
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
			applyBodyOverrides(pv, chat)
		}
		bodyOut, _ = json.Marshal(translate.ChatReqToResponses(chat))
		url += "/responses"
	case pv.Wire == WireGemini:
		// any client wire -> Gemini upstream. Chat wire is the hub: every
		// other client is normalized first, then chat→gemini. The model rides
		// the URL path (the gemini body carries none), streaming is the
		// alt=sse variant of the same endpoint.
		chat := req
		if clientWire == WireAnthropic {
			if !strictJSON {
				return nil, errors.New("unparseable anthropic body for translation")
			}
			chat = translate.AnthropicReqToOpenAI(req, opts)
		} else if clientWire == WireGemini {
			// gemini native client: req was normalized to the chat hub in
			// serve() — reuse it
			if !strictJSON {
				return nil, errors.New("unparseable gemini body for translation")
			}
		} else if clientWire != WireOpenAI && clientWire != WireResponses {
			// responses clients arrive pre-normalized to the chat hub (same
			// shape as the responses-upstream case) — anything else is a bug
			return nil, fmt.Errorf("gemini upstream cannot serve %s clients", clientWire)
		}
		if strictJSON {
			chat["model"] = upModel
			applyBodyOverrides(pv, chat)
		}
		gem, _ := translate.ChatReqToGemini(chat)
		bodyOut, _ = json.Marshal(gem)
		// every client normalization carries "stream" on the hub body — the
		// gemini wire expresses it as the URL variant instead
		upStream, _ := chat["stream"].(bool)
		url = geminiEndpoint(url, upModel, upStream)
	case pv.Wire == WireAnthropic:
		// openai client -> anthropic upstream
		if !strictJSON {
			return nil, errors.New("unparseable openai body for translation")
		}
		req["model"] = upModel
		a := translate.OpenAIReqToAnthropic(req, opts)
		applyBodyOverrides(pv, a)
		bodyOut, _ = json.Marshal(a)
		url = anthropicEndpoint(url)
	default: // anthropic client -> openai upstream
		if !strictJSON {
			return nil, errors.New("unparseable anthropic body for translation")
		}
		if clientWire != WireAnthropic && clientWire != WireClassifier {
			// classifier clients are typed systemone bodies, not chat-hub —
			// guard the default against silently mis-shaping new client wires
			return nil, fmt.Errorf("no translation path for %s client -> %s upstream", clientWire, pv.Wire)
		}
		req["model"] = upModel
		o := translate.AnthropicReqToOpenAI(req, opts)
		applyBodyOverrides(pv, o)
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
		if err := p.setOAuthAuth(httpReq, tgt, findAcct(pv, tgt.AcctName)); err != nil {
			return nil, err
		}
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
	if tgt.AuthType == "oauth" {
		// the Copilot gateway requires the plugin identity (incl. its own
		// User-Agent) — reasserted over the attribution UA above
		if acct := findAcct(pv, tgt.AcctName); acct != nil && acct.Kind == "copilot" {
			copilotIdentity(httpReq)
		}
	}
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

// geminiEndpoint composes {base}/v1beta/models/{model}:generateContent —
// :streamGenerateContent?alt=sse when streaming. The model rides the path
// (the gemini body carries none); URL path escaping is up to net/url.
func geminiEndpoint(base, model string, stream bool) string {
	method := ":generateContent"
	if stream {
		method = ":streamGenerateContent?alt=sse"
	}
	return strings.TrimSuffix(base, "/") + "/v1beta/models/" + model + method
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
// extra_headers so a user override wins. A returned error fails this target's
// build: the dispatch loop records it and fails over to the next target.
func (p *Proxy) setOAuthAuth(h *http.Request, tgt *provider.Target, acct *config.OAuthAcct) error {
	pv := tgt.Provider
	if acct != nil && acct.Kind == "copilot" {
		return p.setCopilotAuth(h, tgt, acct)
	}
	var token string
	if p.Pool != nil && tgt.AcctName != "" {
		t, err := p.Pool.Token(h.Context(), pv.Name, tgt.AcctName)
		if err != nil {
			token = "" // fall through with whatever we have; upstream 401s feed MarkResult
			if acct != nil && acct.AccessTok != "" {
				token = acct.AccessTok
			}
		} else {
			token = t
		}
	}
	if token == "" {
		return nil
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
	case "gemini-cli":
		// Gemini CLI subscription: Bearer access token (Google OAuth), not the
		// x-goog-api-key static-key convention. NOTE: only useful against
		// gateways that accept Google OAuth bearers on the v1beta surface —
		// generativelanguage serving needs an API key today (the Code Assist
		// endpoint this kind refreshes against is a different API shape).
		h.Header.Set("Authorization", "Bearer "+token)
	default:
		// qwen and every other kind: plain bearer on the provider's wire
		// (qwen needs no extra identity headers)
		SetAuth(h, pv.Wire, token)
	}
	return nil
}

// Copilot subscription auth: the config's refresh_token holds the long-lived
// GitHub access token (the pool deliberately never refreshes this kind);
// each dispatch exchanges it for a short-lived Copilot token and speaks with
// the IDE plugin's identity headers.
const (
	copilotTokenURL    = "https://api.github.com/copilot_internal/v2/token"
	copilotTokenUserAg = "GitHubCopilotChat/0.38.0"
	copilotTokenMargin = 5 * time.Minute // re-exchange within 5 min of expiry
	copilotUnknownTTL  = time.Hour       // response without expires_at
)

// copilotIdentity sets the headers the Copilot gateway checks on every
// request (verified against the Copilot Chat plugin's traffic). The token
// exchange above intentionally sends its own older UA (0.26.7, verified
// working against api.github.com) — plugin and gateway tolerate the skew.
func copilotIdentity(h *http.Request) {
	h.Header.Set("copilot-integration-id", "vscode-chat")
	h.Header.Set("editor-version", "vscode/1.110.0")
	h.Header.Set("editor-plugin-version", "copilot-chat/0.38.0")
	h.Header.Set("User-Agent", copilotTokenUserAg)
	h.Header.Set("openai-intent", "conversation-panel")
	h.Header.Set("X-GitHub-Api-Version", "2025-04-01")
	h.Header.Set("x-vscode-user-agent-library-version", "electron-fetch")
	h.Header.Set("X-Initiator", "user")
}

func (p *Proxy) setCopilotAuth(h *http.Request, tgt *provider.Target, acct *config.OAuthAcct) error {
	if acct == nil || acct.RefreshTok == "" {
		return fmt.Errorf("copilot account %s: refresh_token must hold the GitHub access token", tgt.AcctName)
	}
	tok, err := p.copilotToken(h.Context(), tgt.Provider.Name, acct.Name, acct.RefreshTok)
	if err != nil {
		return err // this target fails; failover proceeds without a panic
	}
	h.Header.Set("Authorization", "Bearer "+tok)
	copilotIdentity(h)
	return nil
}

type copilotTok struct {
	token string
	exp   time.Time
}

// copilotToken returns the cached short-lived Copilot token for the account,
// exchanging the long-lived GitHub token when missing or within
// copilotTokenMargin of expiry. Cache is per provider+account; a failed
// exchange is an error, never a crash. Uses p.Client (not clientFor) so tests
// can redirect and control-plane traffic bypasses provider proxy_url.
func (p *Proxy) copilotToken(ctx context.Context, providerName, acctName, ghToken string) (string, error) {
	key := providerName + "/" + acctName
	p.copilotMu.Lock()
	if t, ok := p.copilotToks[key]; ok && t.token != "" && time.Now().Before(t.exp.Add(-copilotTokenMargin)) {
		p.copilotMu.Unlock()
		return t.token, nil
	}
	p.copilotMu.Unlock()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, copilotTokenURL, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "Bearer "+ghToken)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	req.Header.Set("User-Agent", "GitHubCopilotChat/0.26.7")
	resp, err := p.Client.Do(req)
	if err != nil {
		return "", fmt.Errorf("copilot token exchange: %w", err)
	}
	defer resp.Body.Close()
	var out struct {
		Token     string `json:"token"`
		ExpiresAt int64  `json:"expires_at"` // unix seconds
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return "", fmt.Errorf("copilot token exchange: http %d: bad response body", resp.StatusCode)
	}
	if resp.StatusCode != 200 || out.Token == "" {
		return "", fmt.Errorf("copilot token exchange: http %d", resp.StatusCode)
	}
	exp := time.Unix(out.ExpiresAt, 0)
	if out.ExpiresAt <= 0 {
		exp = time.Now().Add(copilotUnknownTTL)
	}
	p.copilotMu.Lock()
	if p.copilotToks == nil {
		p.copilotToks = map[string]copilotTok{}
	}
	p.copilotToks[key] = copilotTok{token: out.Token, exp: exp}
	p.copilotMu.Unlock()
	return out.Token, nil
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
// anthropic-version for the anthropic wire, x-goog-api-key for the gemini
// wire, Bearer otherwise).
func SetAuth(h *http.Request, wire, key string) {
	if key == "" {
		return
	}
	if wire == WireAnthropic {
		h.Header.Set("x-api-key", key)
		h.Header.Set("anthropic-version", "2023-06-01")
	} else if wire == WireGemini {
		h.Header.Set("x-goog-api-key", key) // Gemini API convention
	} else {
		h.Header.Set("Authorization", "Bearer "+key)
	}
}

// CountTokens resolves the exact token count for an anthropic-shaped body by
// asking the first same-wire (anthropic) target's /v1/messages/count_tokens —
// the chars/4 estimate undercounts badly and Claude Code trims context off
// this number. ok=false (no anthropic target, non-2xx, bad JSON, transport
// error) means the caller falls back to the estimate. count_tokens is
// advisory: cooled targets are skipped but failures never trip cooldown
// breakers, and nothing here touches stats.
func (p *Proxy) CountTokens(ctx context.Context, body []byte) (int, bool) {
	var req map[string]any
	if json.Unmarshal(body, &req) != nil || req["model"] == nil || req["messages"] == nil {
		return 0, false
	}
	model, _ := req["model"].(string)
	if model == "" {
		return 0, false
	}
	targets := p.Reg.Resolve(model, allowedModelsForContext(ctx))
	// 10s budget when the caller has no deadline: count_tokens must never be
	// the slow path that holds a client's context-trim loop hostage.
	deadline, ok := ctx.Deadline()
	if !ok || time.Until(deadline) > 10*time.Second {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, 10*time.Second)
		defer cancel()
	}
	for _, tgt := range targets {
		if tgt.Provider.Wire != WireAnthropic {
			continue // only a same-wire anthropic upstream can count natively
		}
		if p.Cd != nil && p.Cd.Cooling(tgt.Provider.Name, tgt.APIKey) {
			continue // skip cooled keys — but never MARK, this path is advisory
		}
		reqModel := model
		if tgt.ModelOverride != "" {
			reqModel = tgt.ModelOverride
		}
		upModel := provider.UpstreamModel(tgt.Provider, reqModel)
		// buildUpstream would POST /v1/messages; reuse its model-mapping +
		// auth/headers work, then retarget the URL at count_tokens.
		httpReq, err := p.buildUpstream(ctx, tgt, WireAnthropic, upModel, req, body, true, "")
		if err != nil {
			continue
		}
		httpReq.URL.Path = strings.TrimSuffix(httpReq.URL.Path, "/v1/messages") + "/v1/messages/count_tokens"
		resp, err := p.clientFor(tgt.Provider).Do(httpReq)
		if err != nil {
			continue // silent fallback — count_tokens is best-effort
		}
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		resp.Body.Close()
		if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			continue
		}
		var out struct {
			InputTokens int `json:"input_tokens"`
		}
		if json.Unmarshal(b, &out) != nil {
			continue
		}
		return out.InputTokens, true
	}
	return 0, false
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
		ht = 60
	}
	// Never let one attempt consume the whole client window: behind
	// Cloudflare (proxy-read-timeout 120s) a single 300s-configured hop
	// would 524 before the failover chain gets a chance. Clamp each attempt
	// to half the window so at least one failover hop can still run.
	if abs, ok := ctx.Deadline(); ok {
		if budget := time.Until(abs); budget > 0 {
			cap := budget / 2
			if cap > 0 && time.Duration(ht)*time.Second > cap {
				ht = int(cap.Seconds())
				if ht < 1 {
					ht = 1
				}
			}
		}
	}
	t := time.AfterFunc(time.Duration(ht)*time.Second, cancel)
	resp, err := p.clientFor(tgt.Provider).Do(req)
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
// reqModel is the natural model id cost accounting prices (combo dispatch
// differs from upModel); attempt is the 1-based dispatch hop that served.
func (p *Proxy) forward(w http.ResponseWriter, r *http.Request, clientWire string, tgt *provider.Target, resp *http.Response, req map[string]any, upModel, reqModel string, stream bool, activeID string, attempt int) (Usage, time.Duration) {
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
		// after copyHeaders so an upstream cannot smuggle duplicates; still
		// before WriteHeader/flush — routing transparency precedes any byte
		p.setDecisionHeaders(w, tgt, upModel, attempt)
		if !stream {
			// non-stream: buffer the (bounded) body so cost headers ride the
			// same response — usage is known before any byte is written
			b, err := io.ReadAll(resp.Body)
			if err != nil {
				p.writeError(w, clientWire, 502, "upstream read: "+err.Error())
				return Usage{Estimate: true}, sinceT(r, firstTouch)
			}
			touch()
			u := ParseUsageJSON(clientWire, b)
			p.setNonStreamUsageHeaders(w, reqModel, u)
			w.WriteHeader(resp.StatusCode)
			w.Write(b)
			return u, sinceT(r, firstTouch)
		}
		w.WriteHeader(resp.StatusCode)
		tee := NewUsageTee(clientWire)
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

	// wire translation path: normalize Responses/Gemini upstreams to openai
	// chat up front — SSE streams are translated byte-stream-level
	// (respToChatBody / geminiToChatBody); non-stream JSON in
	// forwardTranslateFull.
	if stream && tgt.Provider.Wire == WireResponses {
		resp.Body = newRespToChatBody(resp.Body, upModel)
	}
	if stream && tgt.Provider.Wire == WireGemini {
		resp.Body = newGeminiToChatBody(resp.Body, upModel)
	}
	if stream {
		return p.forwardTranslateStream(w, r, clientWire, tgt, resp, req, upModel, reqModel, touch, &firstTouch, attempt)
	}
	return p.forwardTranslateFull(w, r, clientWire, tgt, resp, req, upModel, reqModel, touch, &firstTouch, attempt)
}

// X-Yardmaster-* decision headers: make failover observable to agents/CLIs
// (the dashboard already shows provider/attempts — the client wire did not).
// Set on every path that produces a response, before any WriteHeader/flush.
const (
	hdrProvider = "X-Yardmaster-Provider"
	hdrModel    = "X-Yardmaster-Model"
	hdrKey      = "X-Yardmaster-Key"
	hdrAttempts = "X-Yardmaster-Attempts"
	hdrCost     = "X-Yardmaster-Cost-Usd"
	hdrCache    = "X-Yardmaster-Cache-Read"
)

// setDecisionHeaders writes the always-available decision headers. Connection
// label only — the secret (static key or oauth token) never leaves the box.
func (p *Proxy) setDecisionHeaders(w http.ResponseWriter, tgt *provider.Target, upModel string, attempt int) {
	h := w.Header()
	h.Set(hdrProvider, tgt.Provider.Name)
	h.Set(hdrModel, upModel)
	h.Set(hdrKey, connectionLabel(tgt))
	h.Set(hdrAttempts, strconv.Itoa(attempt))
}

// setCostHeaders adds the usage-derived pair. Non-streaming only: usage is
// fully known before the body is written. Streaming responses are already
// committed by the time usage streams in, so they get the decision headers
// only (the usage API / dashboard remain the streaming source of truth).
func setCostHeaders(h http.Header, costUSD float64, cacheRead int) {
	h.Set(hdrCost, strconv.FormatFloat(costUSD, 'f', 4, 64))
	h.Set(hdrCache, strconv.Itoa(cacheRead))
}

// setNonStreamUsageHeaders writes cost/cache headers for a completed
// non-streaming response, pricing the natural model like store accounting.
func (p *Proxy) setNonStreamUsageHeaders(w http.ResponseWriter, reqModel string, u Usage) {
	var cost float64
	if p.Cost != nil {
		c := p.Cost(reqModel)
		cost = float64(u.In)/1e6*c.Input + float64(u.Out)/1e6*c.Output +
			float64(u.CacheR)/1e6*c.CacheRead + float64(u.CacheW)/1e6*c.CacheWrite
	}
	setCostHeaders(w.Header(), cost, u.CacheR)
}

// connectionLabel returns the display label for the connection that served:
// a static key's label (AuthConf.KeyLabel) or the oauth account name — never
// the secret itself.
func connectionLabel(tgt *provider.Target) string {
	if tgt.AuthType == "oauth" {
		return tgt.AcctName
	}
	pv := tgt.Provider
	for i, k := range pv.Auth.Keys {
		if k == tgt.APIKey {
			return pv.Auth.KeyLabel(i)
		}
	}
	// combo-scoped clone (keys filtered to member pins): labels travel with
	// the clone, so a miss here means a mutated/foreign target — "unknown"
	// beats guessing a wrong label or leaking the secret
	return "unknown"
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

// geminiToChatBody converts a Gemini alt=sse stream into openai
// chat-completions SSE bytes on the fly (same shape as respToChatBody). No
// [DONE] sentinel exists on the gemini wire — the translator's Done() emits
// the terminal usage chunk at EOF instead.
type geminiToChatBody struct {
	src     io.ReadCloser
	sc      *bufio.Scanner
	gc      *translate.Gemini2ChatStream
	buf     bytes.Buffer
	eof     bool
	scanErr error
}

func newGeminiToChatBody(src io.ReadCloser, model string) *geminiToChatBody {
	b := &geminiToChatBody{src: src, gc: translate.NewGemini2ChatStream(model), sc: bufio.NewScanner(src)}
	b.sc.Buffer(make([]byte, 64*1024), 4<<20)
	return b
}

func (b *geminiToChatBody) Read(p []byte) (int, error) {
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
				// transport-level break: surface the error after draining
				// buffered bytes — no synthetic usage chunk on a broken stream
				b.scanErr = err
				continue
			}
			for _, ch := range b.gc.Done() {
				b.writeChunk(ch)
			}
			continue
		}
		line := strings.TrimSpace(b.sc.Text())
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		var ev map[string]any
		if json.Unmarshal([]byte(strings.TrimSpace(strings.TrimPrefix(line, "data:"))), &ev) == nil {
			for _, ch := range b.gc.Chunk(ev) {
				b.writeChunk(ch)
			}
		}
	}
	return b.buf.Read(p)
}

func (b *geminiToChatBody) writeChunk(ch map[string]any) {
	d, _ := json.Marshal(ch)
	b.buf.WriteString("data: " + string(d) + "\n\n")
}

func (b *geminiToChatBody) Close() error { return b.src.Close() }

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
func (p *Proxy) forwardTranslateStream(w http.ResponseWriter, r *http.Request, clientWire string, tgt *provider.Target, resp *http.Response, req map[string]any, upModel, reqModel string, touch func(), firstTouch *time.Time, attempt int) (Usage, time.Duration) {
	// model may be absent (e.g. matched via a "*" catch-all route) — never
	// type-assert directly or a missing key panics mid-stream
	clientModel, _ := req["model"].(string)
	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-cache")
	h.Set("X-Accel-Buffering", "no")
	// before WriteHeader/flush — routing transparency precedes any byte
	p.setDecisionHeaders(w, tgt, upModel, attempt)
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
	} else if clientWire == WireGemini {
		// any non-gemini upstream (chat-hub format) → gemini alt=sse data.
		// Tool deltas accumulate and flush as whole functionCall parts on
		// Finish — chat's incremental args have no mid-stream gemini carrier.
		tr := translate.NewChat2GemStream(clientModel)
		feedChat := func(chunk map[string]any) bool {
			if u := asUsageMap(chunk["usage"]); u != nil {
				applyOpenAIUsage(&usage, u)
			}
			for _, d := range tr.Chunk(chunk) {
				if writeEvent("", d) != nil {
					return false
				}
			}
			return true
		}
		if tgt.Provider.Wire == WireAnthropic {
			// anthropic upstream → chat chunks (Anth2OAI) → gemini data
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
			// upstream openai chat SSE (or gemini/responses-normalized) →
			// gemini data
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
		for _, d := range tr.Finish() {
			writeEvent("", d)
		}
		usage.Estimate = usage.In == 0 && usage.Out == 0
		if usage.Estimate {
			usage.In = usage.estBytes / 4
		}
		return usage, 0
	} else if tgt.Provider.Wire == WireResponses || tgt.Provider.Wire == WireGemini {
		// upstream responses/gemini (normalized to openai chat chunks by
		// respToChatBody / geminiToChatBody)
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
func (p *Proxy) forwardTranslateFull(w http.ResponseWriter, r *http.Request, clientWire string, tgt *provider.Target, resp *http.Response, req map[string]any, upModel, reqModel string, touch func(), firstTouch *time.Time, attempt int) (Usage, time.Duration) {
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
	if upWire == WireGemini {
		// normalize to openai chat completion, then reuse the openai paths
		m = translate.GeminiRespToChat(m, upModel)
		body, _ = json.Marshal(m)
		upWire = WireOpenAI
	}
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
	} else if clientWire == WireGemini {
		out = translate.ChatRespToGemini(m)
	} else {
		out = m
	}
	b, _ := json.Marshal(out)
	// decision headers before any WriteHeader; cost/cache ride the same block
	// since usage is fully known on this buffered path
	p.setDecisionHeaders(w, tgt, upModel, attempt)
	p.setNonStreamUsageHeaders(w, reqModel, usage)
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
	} else if clientWire == WireGemini {
		out = translate.ErrToGemini(body, resp.StatusCode)
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
	} else if wire == WireGemini {
		b, _ = json.Marshal(map[string]any{"error": map[string]any{
			"code": status, "message": msg, "status": translate.GeminiStatusForHTTP(status)}})
	} else {
		b, _ = json.Marshal(map[string]any{"error": map[string]any{
			"message": msg, "type": "api_error", "code": status}})
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	w.Write(b)
}

// write429 emits a rate-limit error with Retry-After, anthropic/openai dialect.
func (p *Proxy) write429(w http.ResponseWriter, wire string, retryAfterS int, msg string) {
	w.Header().Set("Retry-After", strconv.Itoa(retryAfterS))
	var b []byte
	if wire == WireAnthropic {
		b, _ = json.Marshal(map[string]any{"type": "error",
			"error": map[string]any{"type": "rate_limit_error", "message": msg}})
	} else if wire == WireGemini {
		b, _ = json.Marshal(map[string]any{"error": map[string]any{
			"code": 429, "message": msg, "status": "RESOURCE_EXHAUSTED"}})
	} else {
		b, _ = json.Marshal(map[string]any{"error": map[string]any{
			"message": msg, "type": "rate_limit_error", "code": 429}})
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(429)
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
