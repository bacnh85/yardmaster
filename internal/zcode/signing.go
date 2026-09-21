// Package zcode implements ZCode desktop-client parity for Z.ai coding-plan
// traffic: identity headers plus per-request Client-Signing V4 (Ed25519
// signatures + 8-bit proof-of-work), so yardmaster traffic is protocol-identical
// to ZCode 3.9+ (including the campaign entitlement that converts ZCode coding
// plan usage at a 0.67 quota coefficient ≈ 1.5x effective quota).
//
// Ported from pi-model-tools/extensions/lib/zcode-signing.ts (MIT, ported from
// TriDefender/zcode-api). Fail-open everywhere, matching the real client: gate
// off/unreachable → unsigned; handshake failure → unsigned; credential without
// an `{apiKeyId}.{apiKeySecret}` two-part form → unsigned; two consecutive 401s
// after signed requests → process-lifetime bypass.
package zcode

import (
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/ed25519"
	"crypto/hkdf"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"sync"
	"time"
)

// The gate answers on zcode.z.ai; the get_sign_key handshake plane is
// host-fixed to api.z.ai (live-verified 2026-09-06: /api/paas/* 404s on
// zcode.z.ai). gateOrigin/handshakeOrigin are vars so tests can retarget them.
var (
	gateOrigin      = "https://zcode.z.ai"
	handshakeOrigin = "https://api.z.ai"

	gatePath         = "/api/v1/agent/configs"
	handshakePath    = "/api/paas/c1f3a7e2/v2/client"
	appID            = "zcode"
	defaultAppVer    = "3.10.2" // matches the ZCode desktop release the port tracked
	kdfSalt          = "WD_CLIENT_SIGN_KDF_SALT"
	kdfInfoHMAC      = "getSignKey_hmac"
	kdfInfoEd25519   = "ed25519_priv"
	handshakeMethod  = "get_sign_key"
	nonceBytes       = 16
	powNonceBytes    = 12
	gateTTLS         = time.Hour
	gateUnavailCool  = 30 * time.Second
	handshakeBackoff = time.Minute
	bypassAfter401s  = 2
)

// zaiOrigins is the credential-egress allowlist: the gate/handshake carry the
// full two-part key and must never be attempted for any other base URL
// (open.bigmodel.cn, corporate proxies, …).
var zaiOrigins = map[string]bool{
	"https://zcode.z.ai": true,
	"https://api.z.ai":   true,
}

// ---- credential ----

// ParseCredential splits a two-part `{apiKeyId}.{apiKeySecret}` key (exactly
// one dot, both halves non-empty). Single-part legacy keys never sign.
func ParseCredential(credential string) (id, secret string, ok bool) {
	dot := strings.Index(credential, ".")
	if dot <= 0 || dot != strings.LastIndex(credential, ".") {
		return "", "", false
	}
	id, secret = credential[:dot], credential[dot+1:]
	if strings.TrimSpace(id) == "" || strings.TrimSpace(secret) == "" {
		return "", "", false
	}
	return id, secret, true
}

// ---- identity headers ----

// Identity is the resolved ZCode desktop identity for this process.
type Identity struct {
	AppVersion  string
	DeviceMid   string
	Timezone    string
	Language    string
	Platform    string // e.g. darwin-arm64
	OsCategory  string // darwin→macos, windows→windows, else linux
	OsVersion   string
	ReleaseChan string
}

// printable gates a header value to printable ASCII, mirroring the client's
// `pio` helper (unprintable values drop the header entirely).
func printable(s string) (string, bool) {
	s = strings.TrimSpace(s)
	if s == "" {
		return "", false
	}
	for _, r := range s {
		if r < 0x20 || r > 0x7e {
			return "", false
		}
	}
	return s, true
}

// DefaultIdentity resolves the process identity: env overrides, then the
// installed-ZCode telemetry id if present, then a persisted random UUID.
func DefaultIdentity() Identity {
	appVer, ok := printable(os.Getenv("ZCODE_IDENTITY_APP_VERSION"))
	if !ok {
		appVer = defaultAppVer
	}
	dev := os.Getenv("ZCODE_IDENTITY_DEVICE_MID")
	if dev == "" {
		dev = deviceMid()
	}
	plat := runtime.GOOS
	arch := runtime.GOARCH
	osVer, _ := printable(os.Getenv("ZCODE_IDENTITY_OS_VERSION"))
	if osVer == "" {
		osVer = "" // the real client sends the OS release; unknown is fine (header dropped)
	}
	lang, _ := printable(os.Getenv("ZCODE_IDENTITY_LANGUAGE"))
	if lang == "" {
		lang, _ = printable(strings.SplitN(os.Getenv("LANG"), ".", 2)[0])
	}
	if lang == "" {
		lang = "en-US"
	}
	tz := time.Local.String()
	release := "production"
	if strings.EqualFold(strings.TrimSpace(os.Getenv("ZCODE_ENV")), "test") {
		release = "test"
	}
	cat := "linux"
	switch plat {
	case "darwin":
		cat = "macos"
	case "windows":
		cat = "windows"
	}
	return Identity{
		AppVersion:  appVer,
		DeviceMid:   dev,
		Timezone:    tz,
		Language:    lang,
		Platform:    plat + "-" + arch,
		OsCategory:  cat,
		OsVersion:   osVer,
		ReleaseChan: release,
	}
}

// deviceMid: ZCode's own telemetry id when installed (~/.zcode), else a stable
// random UUID persisted once (never credential material).
func deviceMid() string {
	if b, err := os.ReadFile(filepath.Join(homeDir(), ".zcode", "v2", "telemetry-state.json")); err == nil {
		var s struct {
			DeviceMid string `json:"deviceMid"`
		}
		if json.Unmarshal(b, &s) == nil && strings.TrimSpace(s.DeviceMid) != "" {
			return strings.TrimSpace(s.DeviceMid)
		}
	}
	cache := filepath.Join(homeDir(), ".yardmaster-zcode-device-mid")
	if b, err := os.ReadFile(cache); err == nil {
		if id := strings.TrimSpace(string(b)); id != "" {
			return id
		}
	}
	id := newUUID()
	_ = os.WriteFile(cache, []byte(id), 0o600)
	return id
}

func homeDir() string {
	h, err := os.UserHomeDir()
	if err != nil {
		return "."
	}
	return h
}

func newUUID() string {
	b := make([]byte, 16)
	rand.Read(b)
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	h := hex.EncodeToString(b)
	return h[0:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:32]
}

// IdentityHeaders builds the ZCode desktop client's exact identity header set.
func (id Identity) Headers() map[string]string {
	h := map[string]string{
		"HTTP-Referer":      gateOrigin,
		"User-Agent":        "ZCode/unknown",
		"X-Title":           "Z Code@electron",
		"X-ZCode-Agent":     "glm",
		"X-Release-Channel": id.ReleaseChan,
		"X-Client-Language": id.Language,
		"X-Client-Timezone": id.Timezone,
		"X-Os-Category":     id.OsCategory,
		"X-Platform":        id.Platform,
		"X-Device-Mid":      id.DeviceMid,
	}
	if v, ok := printable(id.AppVersion); ok {
		h["User-Agent"] = "ZCode/" + v
		h["X-ZCode-App-Version"] = v
	}
	if id.OsVersion != "" {
		h["X-Os-Version"] = id.OsVersion
	}
	return h
}

// ---- crypto core ----

func hkdfBits(secret, info string) []byte {
	out, err := hkdf.Key(sha256.New, []byte(secret), []byte(kdfSalt), info, 32)
	if err != nil {
		return nil
	}
	return out
}

func randomHex(n int) string {
	b := make([]byte, n)
	rand.Read(b)
	return hex.EncodeToString(b)
}

// handshakeSig = base64(HMAC-SHA256(HKDF(secret, "getSignKey_hmac"),
// "get_sign_key\n{id}\n{ts}\n{nonce}"))
func handshakeSig(id, secret, ts, nonce string) string {
	mac := hmac.New(sha256.New, hkdfBits(secret, kdfInfoHMAC))
	mac.Write([]byte(handshakeMethod + "\n" + id + "\n" + ts + "\n" + nonce))
	return base64.StdEncoding.EncodeToString(mac.Sum(nil))
}

// decryptPrivateKey unwraps the handshake's `privateCipher`: AES-256-GCM over
// base64-TEXT of the PKCS8 key, key = HKDF(secret, "ed25519_priv"), 12-byte iv
// prefix, AAD = apiKeyId.
func decryptPrivateKey(id, secret, privateCipher string) (ed25519.PrivateKey, error) {
	raw, err := base64.StdEncoding.DecodeString(privateCipher)
	if err != nil || len(raw) <= 12+16 {
		return nil, fmt.Errorf("privateCipher too short or invalid: %w", err)
	}
	block, err := aes.NewCipher(hkdfBits(secret, kdfInfoEd25519))
	if err != nil {
		return nil, err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	plain, err := gcm.Open(nil, raw[:12], raw[12:], []byte(id))
	if err != nil {
		return nil, fmt.Errorf("decrypt: %w", err)
	}
	// The plaintext is a BASE64 STRING of the PKCS8 key (not raw DER).
	pkcs8Text := strings.TrimSpace(string(plain))
	if !isBase64(pkcs8Text) {
		return nil, errors.New("privateCipher payload is not base64 text")
	}
	pkcs8, err := base64.StdEncoding.DecodeString(pkcs8Text)
	if err != nil {
		return nil, err
	}
	key, err := x509.ParsePKCS8PrivateKey(pkcs8)
	if err != nil {
		return nil, err
	}
	ed, ok := key.(ed25519.PrivateKey)
	if !ok {
		return nil, errors.New("handshake key is not Ed25519")
	}
	return ed, nil
}

var base64Re = regexp.MustCompile(`^[A-Za-z0-9+/]*={0,2}$`)

func isBase64(s string) bool { return base64Re.MatchString(s) }

// ProofOfWork solves the client PoW: seed = hex(SHA256("{id}\n{app}\n{session}\n{ts}"))[:32];
// candidate = 12-byte hex nonce + 8-hex-digit counter such that
// SHA256("{seed}\n{candidate}") has 8 leading zero bits (~256 iterations).
func ProofOfWork(id, sessionID, ts string) (string, error) {
	seedDigest := sha256.Sum256([]byte(id + "\n" + appID + "\n" + sessionID + "\n" + ts))
	seed := hex.EncodeToString(seedDigest[:])[:32]
	nonce := randomHex(powNonceBytes)
	for counter := uint32(0); ; counter++ {
		candidate := fmt.Sprintf("%s%08x", nonce, counter)
		d := sha256.Sum256([]byte(seed + "\n" + candidate))
		if d[0] == 0 { // 8 leading zero bits
			return candidate, nil
		}
	}
}

// ---- manager ----

type state struct {
	gateEnabled    bool
	gateExpiresAt  time.Time
	gateNegUntil   time.Time
	handshakeNegAt time.Time
	privKey        ed25519.PrivateKey
	handshaking    chan struct{} // non-nil while a handshake is in flight
	epoch          int
	bypass         bool
	consec401s     int
	lastSigned     bool   // this state produced the most recent signed request
	cred           string // credential this state belongs to (ladder scoping)
}

// Fetcher performs the manager's HTTP calls (gate + handshake). Injectable for
// tests; nil means the default client.
type Fetcher func(ctx context.Context, req *http.Request) (*http.Response, error)

// Manager holds per-(origin, credential) signing state. Safe for concurrent use.
type Manager struct {
	Identity Identity
	Fetch    Fetcher
	Logf     func(string)

	mu     sync.Mutex
	states map[string]*state
	// signedCred is the credential of the most recent successfully signed
	// request — the 401 ladder applies only to it (never to other keys).
	signedCred string
}

// NewManager returns a manager with the given identity. fetch nil → default.
func NewManager(id Identity, fetch Fetcher) *Manager {
	if fetch == nil {
		client := &http.Client{Timeout: 15 * time.Second}
		fetch = func(ctx context.Context, req *http.Request) (*http.Response, error) {
			return client.Do(req.WithContext(ctx))
		}
	}
	return &Manager{Identity: id, Fetch: fetch, states: map[string]*state{}}
}

// Sign adds V4 signing headers to req in place when every precondition holds.
// Returns true only when the request was actually signed. Never fails the
// request: every ineligible or failed path leaves the headers untouched.
func (m *Manager) Sign(ctx context.Context, req *http.Request) bool {
	if !zaiOrigins[originOf(req.URL)] {
		return false // credential-egress guard: never sign (or probe) for other origins
	}
	if req.Header.Get("X-Client-Sig") != "" {
		return false // already signed upstream
	}
	sessionID := req.Header.Get("X-Session-Id")
	if sessionID == "" {
		m.note("request has no x-session-id — signing skipped")
		return false
	}
	cred := req.Header.Get("X-Api-Key")
	if cred == "" {
		cred = strings.TrimPrefix(req.Header.Get("Authorization"), "Bearer ")
	}
	id, secret, ok := ParseCredential(cred)
	if !ok {
		m.note("credential has no {apiKeyId}.{apiKeySecret} separator — signing skipped")
		return false
	}

	key := originOf(req.URL) + "\n" + cred
	st := m.stateFor(key)
	m.mu.Lock()
	if st.bypass {
		m.mu.Unlock()
		return false
	}
	m.mu.Unlock()

	if !m.gateEnabled(ctx, st, key, cred) {
		return false
	}
	priv, err := m.privateKey(ctx, st, key, id, secret)
	if err != nil {
		m.note("signing handshake failed (" + shorten(err.Error()) + ") — sending unsigned")
		m.resetKey(key)
		m.mu.Lock()
		st.handshakeNegAt = time.Now().Add(handshakeBackoff)
		m.mu.Unlock()
		return false
	}

	ts := fmt.Sprintf("%d", time.Now().UnixMilli())
	nonce := randomHex(nonceBytes)
	pow, err := ProofOfWork(id, sessionID, ts)
	if err != nil {
		m.note("pow failed — sending unsigned")
		return false
	}
	msg := id + "\n" + ts + "\n" + m.Identity.AppVersion + "\n" + sessionID + "\n" + nonce
	sig := base64.StdEncoding.EncodeToString(ed25519.Sign(priv, []byte(msg)))

	h := req.Header
	h.Set("X-Client-Ts", ts)
	h.Set("X-Client-Version", m.Identity.AppVersion)
	h.Set("X-Client-Sig", sig)
	h.Set("X-Session-Id", sessionID)
	h.Set("X-Client-Nonce", nonce)
	h.Set("X-App-Id", appID)
	h.Set("X-Client-Pow", pow)

	m.mu.Lock()
	st.lastSigned = true
	st.cred = cred
	m.signedCred = cred
	m.mu.Unlock()
	return true
}

// NoteStatus feeds the 401 ladder: call once per upstream response with the
// credential that served it. Two consecutive 401s after signed requests
// bypass signing for that credential (process lifetime); any non-401 resets
// the count. Other credentials' ladders are untouched.
func (m *Manager) NoteStatus(credential string, status int) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if credential == "" || credential != m.signedCred {
		return // response for a request we didn't sign (or a different key)
	}
	m.signedCred = ""
	for _, st := range m.states {
		if !st.lastSigned || st.cred != credential {
			continue // a different key's response must never move this ladder
		}
		st.lastSigned = false
		if status == http.StatusUnauthorized {
			st.consec401s++
			st.epoch++
			st.privKey = nil
			if st.consec401s >= bypassAfter401s {
				st.bypass = true
				m.note("client-signing: repeated 401 after signed requests — bypassing signing for this credential")
			}
		} else {
			st.consec401s = 0
		}
	}
}

func (m *Manager) note(msg string) {
	if m.Logf != nil {
		m.Logf("client-signing: " + msg)
	}
}

func (m *Manager) stateFor(key string) *state {
	m.mu.Lock()
	defer m.mu.Unlock()
	st := m.states[key]
	if st == nil {
		st = &state{}
		m.states[key] = st
	}
	return st
}

func (m *Manager) resetKey(key string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if st := m.states[key]; st != nil {
		st.epoch++
		st.privKey = nil
	}
}

func originOf(u *url.URL) string {
	return u.Scheme + "://" + u.Host
}

func shorten(s string) string {
	if len(s) > 80 {
		return s[:80]
	}
	return s
}

// gateEnabled reports whether the server currently requests client signing
// (codingPlanSignature.enable), with a 1h positive TTL and negative cooldowns.
func (m *Manager) gateEnabled(ctx context.Context, st *state, key, cred string) bool {
	m.mu.Lock()
	now := time.Now()
	if now.Before(st.gateExpiresAt) {
		en := st.gateEnabled
		m.mu.Unlock()
		return en
	}
	if now.Before(st.gateNegUntil) {
		m.mu.Unlock()
		return false
	}
	m.mu.Unlock()

	en, unavailable := m.fetchGate(ctx, cred)
	m.mu.Lock()
	defer m.mu.Unlock()
	// re-read: another caller may have refreshed while we probed
	if now.Before(st.gateExpiresAt) {
		return st.gateEnabled
	}
	if en {
		st.gateEnabled = true
		st.gateExpiresAt = now.Add(gateTTLS)
		st.gateNegUntil = time.Time{}
		m.note("client-signing: server enabled codingPlanSignature — signing requests")
		return true
	}
	st.gateEnabled = false
	if unavailable {
		st.gateNegUntil = now.Add(gateUnavailCool)
	} else {
		st.gateNegUntil = time.Time{}
		st.gateExpiresAt = now.Add(gateTTLS) // disabled is a definitive answer; cache 1h
	}
	return false
}

// fetchGate GETs the agent/configs gate with identity headers minus
// X-ZCode-Agent/X-Device-Mid plus x-api-key. Second return = transport failure
// (unavailable, brief cooldown) vs a definitive disabled answer.
func (m *Manager) fetchGate(ctx context.Context, cred string) (enabled, unavailable bool) {
	ih := m.Identity.Headers()
	delete(ih, "X-ZCode-Agent")
	delete(ih, "X-Device-Mid")
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, gateOrigin+gatePath, nil)
	if err != nil {
		return false, true
	}
	for k, v := range ih {
		req.Header.Set(k, v)
	}
	req.Header.Set("x-api-key", cred)
	resp, err := m.Fetch(ctx, req)
	if err != nil {
		return false, true
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return false, true
	}
	var body struct {
		Code int `json:"code"`
		Data struct {
			CodingPlanSignature *struct {
				Enable bool `json:"enable"`
			} `json:"codingPlanSignature"`
		} `json:"data"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&body); err != nil || body.Code != 0 {
		return false, true
	}
	if body.Data.CodingPlanSignature == nil {
		return false, false // key present-but-absent answer: disabled
	}
	return body.Data.CodingPlanSignature.Enable, false
}

// privateKey returns the handshake-issued Ed25519 key, single-flighting the
// handshake per credential (waiters share the in-flight attempt).
func (m *Manager) privateKey(ctx context.Context, st *state, key, id, secret string) (ed25519.PrivateKey, error) {
	m.mu.Lock()
	if st.privKey != nil {
		k := st.privKey
		m.mu.Unlock()
		return k, nil
	}
	if !st.handshakeNegAt.IsZero() && time.Now().Before(st.handshakeNegAt) {
		m.mu.Unlock()
		return nil, errors.New("handshake backoff")
	}
	if ch := st.handshaking; ch != nil {
		m.mu.Unlock()
		<-ch // share the in-flight handshake
		m.mu.Lock()
		defer m.mu.Unlock()
		if st.privKey != nil {
			return st.privKey, nil
		}
		return nil, errors.New("handshake failed")
	}
	st.handshaking = make(chan struct{})
	m.mu.Unlock()

	cipherText, err := m.doHandshake(ctx, id, secret)

	m.mu.Lock()
	defer m.mu.Unlock()
	close(st.handshaking)
	st.handshaking = nil
	if err != nil {
		st.handshakeNegAt = time.Now().Add(handshakeBackoff)
		return nil, err
	}
	priv, err := decryptPrivateKey(id, secret, cipherText)
	if err != nil {
		st.handshakeNegAt = time.Now().Add(handshakeBackoff)
		return nil, err
	}
	st.privKey = priv
	return priv, nil
}

// doHandshake POSTs get_sign_key to the api.z.ai handshake plane and returns
// data.privateCipher.
func (m *Manager) doHandshake(ctx context.Context, id, secret string) (string, error) {
	ts := fmt.Sprintf("%d", time.Now().UnixMilli())
	nonce := randomHex(nonceBytes)
	body, _ := json.Marshal(map[string]string{
		"apiKey": id + "." + secret,
		"nonce":  nonce,
		"sig":    handshakeSig(id, secret, ts, nonce),
		"ts":     ts,
	})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, handshakeOrigin+handshakePath, bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", id+"."+secret)
	req.Header.Set("Content-Type", "application/json")
	resp, err := m.Fetch(ctx, req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return "", fmt.Errorf("handshake_http_%d", resp.StatusCode)
	}
	var env struct {
		Code int    `json:"code"`
		Msg  string `json:"msg"`
		Data struct {
			PrivateCipher string `json:"privateCipher"`
		} `json:"data"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&env); err != nil {
		return "", err
	}
	if env.Code == 500 {
		return "", errors.New("handshake_server_500")
	}
	if env.Code != 200 {
		return "", fmt.Errorf("handshake_rejected: %s", env.Msg)
	}
	if env.Data.PrivateCipher == "" {
		return "", errors.New("handshake_omitted_privateCipher")
	}
	return env.Data.PrivateCipher, nil
}
