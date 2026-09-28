package proxy

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/bacnh85/yardmaster/internal/config"
	"github.com/bacnh85/yardmaster/internal/provider"
)

// affUp is one upstream recording the auth key each request arrived with.
// All keys of a provider share one base_url, so per-key dispatch is observed
// here (the Bearer header is the key itself). failKey 500s only that key so
// failover to the sibling key still serves.
type affUp struct {
	srv *httptest.Server
	mu  sync.Mutex
	// keyHits counts requests per Bearer key; failKey is the key that gets
	// a 500 ("" = none).
	keyHits map[string]int
	failKey string
}

func newAffUp(t *testing.T) *affUp {
	u := &affUp{keyHits: map[string]int{}}
	u.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		u.mu.Lock()
		k := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		u.keyHits[k]++
		fail := k == u.failKey
		u.mu.Unlock()
		if fail {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(500)
			w.Write([]byte(`{"error":"upstream down"}`))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"id":"x","object":"chat.completion","model":"m","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1}}`))
	}))
	t.Cleanup(u.srv.Close)
	return u
}

func (u *affUp) hits(key string) int {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.keyHits[key]
}

func (u *affUp) failKeyNext(key string) {
	u.mu.Lock()
	u.failKey = key
	u.mu.Unlock()
}

// affHarness wires one two-key round_robin provider over one upstream. With
// rotation round_robin the starting key alternates per request, so a
// same-session request pair only lands on the same key twice if the
// affinity pin reordered the targets.
type affHarness struct {
	p  *Proxy
	ts *httptest.Server
	up *affUp
}

func newAffHarness(t *testing.T, aff *config.Affinity, extra func(*config.Config)) *affHarness {
	t.Helper()
	up := newAffUp(t)
	cfg := &config.Config{Providers: []*config.Provider{
		{Name: "p1", BaseURL: up.srv.URL, Wire: "openai", Models: []string{"m"}, Rotation: "round_robin",
			Auth: config.AuthConf{Type: "static", Keys: []string{"sk-a", "sk-b"}}},
	}}
	if aff != nil {
		cfg.Routing.Affinity = aff
	}
	if extra != nil {
		extra(cfg)
	}
	h := &affHarness{p: NewProxy(provider.New(cfg), nil, nil), up: up}
	h.ts = httptest.NewServer(http.HandlerFunc(h.p.ServeChat))
	t.Cleanup(h.ts.Close)
	return h
}

// chat sends one non-streaming chat completion; extra headers via set.
func (h *affHarness) chat(t *testing.T, session string, set func(*http.Request)) (int, string) {
	t.Helper()
	req, _ := http.NewRequest("POST", h.ts.URL, strings.NewReader(`{"model":"m","stream":false,"messages":[{"role":"user","content":"hi"}]}`))
	req.Header.Set("Content-Type", "application/json")
	if session != "" {
		req.Header.Set("x-session-id", session)
	}
	if set != nil {
		set(req)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

// (a) same session → same key despite round_robin rotation which would
// otherwise alternate sk-a, sk-b…
func TestAffinitySameSessionSameKey(t *testing.T) {
	h := newAffHarness(t, &config.Affinity{Enabled: true}, nil)
	for i := 0; i < 3; i++ {
		code, body := h.chat(t, "sess-1", nil)
		if code != 200 {
			t.Fatalf("req %d: status %d: %s", i, code, body)
		}
	}
	if got := h.up.hits("sk-a") + h.up.hits("sk-b"); got != 3 {
		t.Fatalf("total hits = %d, want 3", got)
	}
	if h.up.hits("sk-b") != 0 {
		t.Fatalf("session stuck to sk-a but sk-b got %d hits — pin not applied", h.up.hits("sk-b"))
	}
	// a different session starts on the OTHER key: rotation advanced for it
	// (sess-1's pin must not suppress rotation for other sessions)
	if code, _ := h.chat(t, "sess-2", nil); code != 200 {
		t.Fatal("second session request failed")
	}
	if h.up.hits("sk-b") != 1 {
		t.Fatalf("sk-b hits = %d, want 1 (rotation must proceed for other sessions)", h.up.hits("sk-b"))
	}
	if h.up.hits("sk-a") != 3 {
		t.Fatalf("sk-a hits = %d, want 3", h.up.hits("sk-a"))
	}
	// one more sess-2 request so the rotation counter points at sk-b — then
	// sess-1 must STILL land on sk-a (the pin, not rotation luck)
	if code, _ := h.chat(t, "sess-2", nil); code != 200 {
		t.Fatal("third session request failed")
	}
	if h.up.hits("sk-b") != 2 {
		t.Fatalf("sk-b hits = %d, want 2", h.up.hits("sk-b"))
	}
	if code, _ := h.chat(t, "sess-1", nil); code != 200 {
		t.Fatal("pinned follow-up failed")
	}
	if h.up.hits("sk-a") != 4 || h.up.hits("sk-b") != 2 {
		t.Fatalf("pin lost while rotation pointed at sk-b: sk-a=%d sk-b=%d", h.up.hits("sk-a"), h.up.hits("sk-b"))
	}
}

// (b) different session → different key: covered inside (a); the standalone
// pin below asserts the store shape the routing relies on.
func TestAffinityBindingShape(t *testing.T) {
	h := newAffHarness(t, &config.Affinity{Enabled: true}, nil)
	if _, _, ok := h.p.affinityLookup("none"); ok {
		t.Fatal("unknown session must not bind")
	}
	h.chat(t, "sess-1", nil)
	providerName, conn, ok := h.p.affinityLookup("sess-1")
	if !ok || providerName != "p1" || conn != "Key 1" {
		t.Fatalf("binding = (%q, %q, %v), want (p1, Key 1, true)", providerName, conn, ok)
	}
	if got := h.p.AffinityStats(); got.Enabled != true || got.Entries != 1 {
		t.Fatalf("stats = %+v, want enabled with 1 entry", got)
	}
}

// (c) bound target 500s → failover serves from the other key AND the third
// request does not pin to the dead one.
func TestAffinityDeadBindingCleared(t *testing.T) {
	h := newAffHarness(t, &config.Affinity{Enabled: true}, nil)
	if code, _ := h.chat(t, "sess-1", nil); code != 200 {
		t.Fatal("initial request failed")
	}
	if _, _, ok := h.p.affinityLookup("sess-1"); !ok {
		t.Fatal("binding missing after success")
	}
	h.up.failKeyNext("sk-a")
	// pinned key 500s → failover to the other key; the REQUEST still succeeds
	code, body := h.chat(t, "sess-1", nil)
	if code != 200 {
		t.Fatalf("failover request: status %d: %s", code, body)
	}
	if h.up.hits("sk-b") != 1 {
		t.Fatalf("sk-b hits = %d, want 1 (failover served)", h.up.hits("sk-b"))
	}
	// the dead connection must never be re-pinned: the binding is either
	// cleared or re-bound to the connection that actually served (Key 2)
	_, conn, live := h.p.affinityLookup("sess-1")
	if live && conn != "Key 2" {
		t.Fatalf("binding after failover = %q, want Key 2 or cleared — dead Key 1 must not be re-pinned", conn)
	}
	// next request rides the surviving connection, not the dead one
	h.up.failKeyNext("")
	if code, _ := h.chat(t, "sess-1", nil); code != 200 {
		t.Fatal("post-failover request failed")
	}
	if h.up.hits("sk-a") != 2 {
		t.Fatalf("sk-a hits = %d, want 2 (initial + the failed hop) — dead key must not be re-pinned", h.up.hits("sk-a"))
	}
	if h.up.hits("sk-b") != 2 {
		t.Fatalf("sk-b hits = %d, want 2 (session moved to the serving key)", h.up.hits("sk-b"))
	}
	_, conn, _ = h.p.affinityLookup("sess-1")
	if conn != "Key 2" {
		t.Fatalf("bound to %q, want Key 2 (the serving connection)", conn)
	}
}

// (d) binding expires after ttl (tiny ttl + sleep, no clock injection needed
// through the public surface).
func TestAffinityTTLExpiry(t *testing.T) {
	h := newAffHarness(t, &config.Affinity{Enabled: true, TTLS: 1}, nil)
	if code, _ := h.chat(t, "sess-1", nil); code != 200 {
		t.Fatal("initial request failed")
	}
	if _, _, ok := h.p.affinityLookup("sess-1"); !ok {
		t.Fatal("binding missing")
	}
	time.Sleep(1100 * time.Millisecond)
	if _, _, ok := h.p.affinityLookup("sess-1"); ok {
		t.Fatal("binding must expire after ttl")
	}
	if got := h.p.AffinityStats(); got.Entries != 0 {
		t.Fatalf("stats entries = %d, want 0 after expiry", got.Entries)
	}
}

// (e) disabled → no stickiness: round_robin alternates as before.
func TestAffinityDisabledNoStickiness(t *testing.T) {
	h := newAffHarness(t, nil, nil)
	for i := 0; i < 4; i++ {
		if code, _ := h.chat(t, "sess-1", nil); code != 200 {
			t.Fatalf("req %d failed", i)
		}
	}
	if h.up.hits("sk-a") != 2 || h.up.hits("sk-b") != 2 {
		t.Fatalf("disabled affinity must not stick: sk-a=%d sk-b=%d, want 2/2",
			h.up.hits("sk-a"), h.up.hits("sk-b"))
	}
	if h.p.AffinityStats().Enabled {
		t.Fatal("stats must report disabled")
	}
}

// (f) extraction: anthropic metadata.user_id + openai user field + opencode
// header; configured header wins over the body fields.
func TestAffinityExtraction(t *testing.T) {
	h := newAffHarness(t, &config.Affinity{Enabled: true}, nil)

	// anthropic metadata.user_id on the openai wire is ignored; send via
	// ServeMessages to exercise the anthropic branch
	msgsTS := httptest.NewServer(http.HandlerFunc(h.p.ServeMessages))
	defer msgsTS.Close()
	anth := func(userID string) {
		t.Helper()
		body := `{"model":"m","stream":false,"metadata":{"user_id":"` + userID + `"}}`
		resp, err := http.Post(msgsTS.URL, "application/json", strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if code := resp.StatusCode; code != 200 {
			t.Fatalf("anthropic request: status %d", code)
		}
	}
	anth("claude-code-user-1")
	if _, _, ok := h.p.affinityLookup("claude-code-user-1"); !ok {
		t.Fatal("metadata.user_id not extracted")
	}

	// openai body user field (ServeChat)
	body := `{"model":"m","stream":false,"user":"openai-user-2"}`
	resp, err := http.Post(h.ts.URL, "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("openai user request: status %d", resp.StatusCode)
	}
	if _, _, ok := h.p.affinityLookup("openai-user-2"); !ok {
		t.Fatal("body user field not extracted")
	}

	// opencode client header consulted without a configured header hit
	req2, _ := http.NewRequest("POST", h.ts.URL, strings.NewReader(`{"model":"m","stream":false}`))
	req2.Header.Set("x-opencode-session", "oc-sess-3")
	resp3, err := http.DefaultClient.Do(req2)
	if err != nil {
		t.Fatal(err)
	}
	resp3.Body.Close()
	if _, _, ok := h.p.affinityLookup("oc-sess-3"); !ok {
		t.Fatal("x-opencode-session not extracted")
	}

	// configured header wins over body fields — one request carrying both
	// must bind ONLY the header id
	req4, _ := http.NewRequest("POST", h.ts.URL, strings.NewReader(`{"model":"m","stream":false,"user":"openai-user-shadow"}`))
	req4.Header.Set("x-session-id", "hdr-wins")
	resp4, err := http.DefaultClient.Do(req4)
	if err != nil {
		t.Fatal(err)
	}
	resp4.Body.Close()
	if _, _, ok := h.p.affinityLookup("hdr-wins"); !ok {
		t.Fatal("configured header not extracted")
	}
	if _, _, ok := h.p.affinityLookup("openai-user-shadow"); ok {
		t.Fatal("body user must be shadowed by the configured header on the same request")
	}
}

// oversized session ids are truncated to the 256-byte cap (still bind — the
// cap bounds map-key memory), per the spec.
func TestAffinitySessionCap(t *testing.T) {
	h := newAffHarness(t, &config.Affinity{Enabled: true}, nil)
	big := strings.Repeat("x", 400)
	if code, _ := h.chat(t, big, nil); code != 200 {
		t.Fatal("oversized-session request must still serve")
	}
	if _, _, ok := h.p.affinityLookup(big[:256]); !ok {
		t.Fatal("session id must be truncated to the 256-byte cap and bound")
	}
	if got := h.p.AffinityStats().Entries; got != 1 {
		t.Fatalf("entries = %d, want 1", got)
	}
}

// affinity reordering must not break plain failover order: the pinned
// provider stays the head but the rest of the chain keeps its order.
func TestAffinityReorderPreservesChainOrder(t *testing.T) {
	one := &provider.Target{AuthType: "static", APIKey: "a"}
	one.Provider = &config.Provider{Name: "one", Auth: config.AuthConf{Keys: []string{"a"}}}
	two := &provider.Target{AuthType: "static", APIKey: "b"}
	two.Provider = &config.Provider{Name: "two", Auth: config.AuthConf{Keys: []string{"b"}}}
	three := &provider.Target{AuthType: "static", APIKey: "c"}
	three.Provider = &config.Provider{Name: "three", Auth: config.AuthConf{Keys: []string{"c"}}}

	// connectionLabel renders "Key N", so match on that (the pin stores the
	// same label)
	got, pos := reorderForAffinity([]*provider.Target{one, two, three}, "three", "Key 1")
	if pos != 0 {
		t.Fatalf("pos = %d, want 0 (bound target promoted)", pos)
	}
	if got[0] != three || got[1] != one || got[2] != two {
		t.Fatalf("reorder = %v %v %v, want three,one,two", got[0].Provider.Name, got[1].Provider.Name, got[2].Provider.Name)
	}
	// no match → unchanged, pos -1 (bound connection not routable)
	if got, pos := reorderForAffinity([]*provider.Target{one, two}, "four", "x"); pos != -1 || got[0] != one || got[1] != two {
		t.Fatalf("no-match reorder must be a no-op with pos -1, got pos %d", pos)
	}
	if got, pos := reorderForAffinity([]*provider.Target{one, two}, "one", "Key 1"); pos != 0 || got[0] != one {
		t.Fatal("already-first must be a no-op with pos 0")
	}
}

// (g) transport-level death of the pinned connection (server closed — dial
// refused, not an HTTP status) must clear the pin exactly like a >=400 does;
// otherwise every same-session request re-attempts the dead head first.
func TestAffinityTransportDeathClearsPin(t *testing.T) {
	upA := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"id":"x","object":"chat.completion","model":"m","choices":[{"index":0,"message":{"role":"assistant","content":"from-a"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1}}`))
	}))
	defer upA.Close()
	upB := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"id":"x","object":"chat.completion","model":"m","choices":[{"index":0,"message":{"role":"assistant","content":"from-b"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1}}`))
	}))
	defer upB.Close()

	cfg := &config.Config{Providers: []*config.Provider{
		{Name: "a", BaseURL: upA.URL, Wire: "openai", Models: []string{"m"},
			Auth: config.AuthConf{Keys: []string{"ka"}}},
		{Name: "b", BaseURL: upB.URL, Wire: "openai", Models: []string{"m"},
			Auth: config.AuthConf{Keys: []string{"kb"}}},
	}}
	cfg.Routing.Affinity = &config.Affinity{Enabled: true}
	p := NewProxy(provider.New(cfg), nil, nil)
	ts := httptest.NewServer(http.HandlerFunc(p.ServeChat))
	defer ts.Close()

	chat := func() string {
		t.Helper()
		req, _ := http.NewRequest("POST", ts.URL, strings.NewReader(`{"model":"m","stream":false,"messages":[{"role":"user","content":"hi"}]}`))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("x-session-id", "sess-dead")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return string(b)
	}

	if body := chat(); !strings.Contains(body, "from-a") {
		t.Fatalf("first request served by unexpectedly: %s", body)
	}
	if _, conn, ok := p.affinityLookup("sess-dead"); !ok {
		t.Fatal("binding missing after first success")
	} else if conn != "Key 1" {
		t.Fatalf("bound conn = %q, want Key 1 (provider a)", conn)
	}

	// kill A at the transport level; the pinned target now dial-refuses
	upA.Close()
	if body := chat(); !strings.Contains(body, "from-b") {
		t.Fatalf("failover request: %s", body)
	}
	// the pin must have been cleared on the transport error, then re-bound to
	// the connection that actually served (provider b)
	providerName, _, ok := p.affinityLookup("sess-dead")
	if !ok {
		return // cleared without re-pin is also acceptable
	}
	if providerName == "a" {
		t.Fatal("dead pinned connection (provider a) survived the transport error")
	}
}
