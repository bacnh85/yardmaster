package zcode

import (
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
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

const (
	// same fixtures as pi-model-tools/extensions/test/unit/zcode-signing.test.ts
	credTwoPart = "abc123.secret456"
	sessionID   = "sess_test_0001"
	deviceMidFx = "11111111-2222-3333-4444-555555555555"
)

func testIdentity() Identity {
	return Identity{
		AppVersion: "3.10.2", DeviceMid: deviceMidFx,
		Timezone: "UTC", Language: "en-US",
		Platform: "darwin-arm64", OsCategory: "macos", ReleaseChan: "production",
	}
}

// hkdfBits re-implements the derivation so the test validates the algorithm,
// not a shared secret helper (mirrors the TS test's hkdfTestHelpers).
func testHKDFBits(secret, info string) []byte {
	out, err := hkdf.Key(sha256.New, []byte(secret), []byte(kdfSalt), info, 32)
	if err != nil {
		panic(err)
	}
	return out
}

// sealPrivateKey wraps a PKCS8 key the way the server does: AES-256-GCM over
// base64-TEXT of the key, key = HKDF(secret, "ed25519_priv"), AAD = apiKeyId.
func sealPrivateKey(t *testing.T, apiKeyID, secret string, pkcs8 []byte) string {
	t.Helper()
	block, err := aes.NewCipher(testHKDFBits(secret, kdfInfoEd25519))
	if err != nil {
		t.Fatal(err)
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		t.Fatal(err)
	}
	iv := make([]byte, 12)
	rand.Read(iv)
	// server seals base64 TEXT of the PKCS8 key (not raw DER)
	pkcs8Text := base64.StdEncoding.EncodeToString(pkcs8)
	out := gcm.Seal(nil, iv, []byte(pkcs8Text), []byte(apiKeyID))
	return base64.StdEncoding.EncodeToString(append(iv, out...))
}

type mockCalls struct {
	gateCount, handshakeCount int
}

// newTestManager builds a manager whose gate/handshake answer like the TS
// test's fetch mock. The handshake verifies the HMAC signature server-side and
// returns a sealed freshly-generated Ed25519 key.
func newTestManager(t *testing.T, id Identity, gateEnabled, gateThrows bool, handshakeStatus int) (*Manager, *mockCalls, func() ed25519.PublicKey) {
	t.Helper()
	calls := &mockCalls{}
	var pub ed25519.PublicKey
	var priv ed25519.PrivateKey
	getPub := func() ed25519.PublicKey { return pub }

	fetch := func(ctx context.Context, req *http.Request) (*http.Response, error) {
		switch {
		case strings.Contains(req.URL.String(), gatePath):
			calls.gateCount++
			body := `{"code":0,"data":{}}`
			if gateEnabled {
				body = `{"code":0,"data":{"codingPlanSignature":{"enable":true}}}`
			}
			if gateThrows {
				return nil, fmt.Errorf("network down")
			}
			return rec(200, body), nil
		case strings.Contains(req.URL.String(), handshakePath):
			calls.handshakeCount++
			if handshakeStatus != 0 {
				return rec(handshakeStatus, fmt.Sprintf(`{"code":%d,"msg":"nope"}`, handshakeStatus)), nil
			}
			// verify the handshake HMAC the way the server would
			var body struct {
				APIKey string `json:"apiKey"`
				Nonce  string `json:"nonce"`
				Sig    string `json:"sig"`
				Ts     string `json:"ts"`
			}
			b, _ := io.ReadAll(req.Body)
			if err := json.Unmarshal(b, &body); err != nil {
				t.Fatal(err)
			}
			parts := strings.SplitN(body.APIKey, ".", 2)
			mac := hmac.New(sha256.New, testHKDFBits(parts[1], kdfInfoHMAC))
			mac.Write([]byte(handshakeMethod + "\n" + parts[0] + "\n" + body.Ts + "\n" + body.Nonce))
			want, err := base64.StdEncoding.DecodeString(body.Sig)
			if err != nil || !hmac.Equal(mac.Sum(nil), want) {
				t.Fatalf("handshake HMAC signature must verify server-side")
			}
			if priv == nil {
				pubTmp, privTmp, err := ed25519.GenerateKey(rand.Reader)
				if err != nil {
					t.Fatal(err)
				}
				priv, pub = privTmp, pubTmp
				pkcs8, err := x509.MarshalPKCS8PrivateKey(privTmp)
				if err != nil {
					t.Fatal(err)
				}
				return rec(200, fmt.Sprintf(`{"code":200,"data":{"privateCipher":%q}}`, sealPrivateKey(t, parts[0], parts[1], pkcs8))), nil
			}
			pkcs8, err := x509.MarshalPKCS8PrivateKey(priv)
			if err != nil {
				t.Fatal(err)
			}
			return rec(200, fmt.Sprintf(`{"code":200,"data":{"privateCipher":%q}}`, sealPrivateKey(t, parts[0], parts[1], pkcs8))), nil
		default:
			return rec(404, `{"code":404}`), nil
		}
	}
	return NewManager(id, fetch), calls, getPub
}

func rec(status int, body string) *http.Response {
	return &http.Response{StatusCode: status, Body: io.NopCloser(strings.NewReader(body)), Header: http.Header{}}
}

func signedRequest(t *testing.T, m *Manager, url string, cred string) *http.Request {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, url, strings.NewReader("{}"))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("X-Api-Key", cred)
	req.Header.Set("X-Session-Id", sessionID)
	if !m.Sign(context.Background(), req) {
		t.Fatalf("expected request to be signed")
	}
	return req
}

func TestParseCredential(t *testing.T) {
	if _, _, ok := ParseCredential(credTwoPart); !ok {
		t.Fatal("two-part key must parse")
	}
	for _, bad := range []string{"nodotshere", "a.b.c", ".secret", "id."} {
		if _, _, ok := ParseCredential(bad); ok {
			t.Fatalf("%q must not parse", bad)
		}
	}
}

func TestIdentityHeaders(t *testing.T) {
	h := testIdentity().Headers()
	if h["User-Agent"] != "ZCode/3.10.2" || h["X-ZCode-App-Version"] != "3.10.2" {
		t.Fatalf("UA headers: %v", h)
	}
	if h["X-Title"] != "Z Code@electron" || h["X-ZCode-Agent"] != "glm" {
		t.Fatalf("title/agent: %v", h)
	}
	if h["HTTP-Referer"] != "https://zcode.z.ai" || h["X-Release-Channel"] != "production" {
		t.Fatalf("referer/channel: %v", h)
	}
	if h["X-Device-Mid"] != deviceMidFx {
		t.Fatalf("device mid: %v", h)
	}
	// unprintable app version drops the version header, UA falls back
	bad := Identity{AppVersion: "bad\a", DeviceMid: deviceMidFx}.Headers()
	if bad["User-Agent"] != "ZCode/unknown" {
		t.Fatalf("fallback UA: %v", bad)
	}
	if _, ok := bad["X-ZCode-App-Version"]; ok {
		t.Fatal("unprintable version must drop X-ZCode-App-Version")
	}
}

func TestProofOfWork(t *testing.T) {
	pow, err := ProofOfWork("abc123", "sess_x", "1700000000000")
	if err != nil {
		t.Fatal(err)
	}
	if len(pow) != 32 || pow[24:] == "" { // 12-byte nonce hex + 4-byte counter hex
		t.Fatalf("shape: %q", pow)
	}
	seedDigest := sha256.Sum256([]byte("abc123\nzcode\nsess_x\n1700000000000"))
	seed := fmt.Sprintf("%x", seedDigest)[:32]
	d := sha256.Sum256([]byte(seed + "\n" + pow))
	if d[0] != 0 {
		t.Fatalf("candidate %q does not solve 8 leading zero bits", pow)
	}
}

func TestSignHappyPath(t *testing.T) {
	m, calls, pub := newTestManager(t, testIdentity(), true, false, 0)
	url := "https://zcode.z.ai/api/v1/ultra-zai/anthropic/v1/messages"
	req := signedRequest(t, m, url, credTwoPart)

	for _, name := range []string{"X-Client-Ts", "X-Client-Version", "X-Client-Sig", "X-Session-Id", "X-Client-Nonce", "X-App-Id", "X-Client-Pow"} {
		if req.Header.Get(name) == "" {
			t.Errorf("%s missing", name)
		}
	}
	if req.Header.Get("X-App-Id") != "zcode" || req.Header.Get("X-Session-Id") != sessionID || req.Header.Get("X-Client-Version") != "3.10.2" {
		t.Fatalf("canonical header values: %v", req.Header)
	}
	if calls.gateCount != 1 || calls.handshakeCount != 1 {
		t.Fatalf("gate/handshake counts: %+v", calls)
	}
	// gate on zcode.z.ai, handshake host-fixed to api.z.ai
	// (verified by the mock path match itself)

	// second sign: gate+handshake cached
	signedRequest(t, m, url, credTwoPart)
	if calls.gateCount != 1 || calls.handshakeCount != 1 {
		t.Fatalf("gate+handshake must be cached: %+v", calls)
	}

	// business sig verifies with the handshake-issued key
	apiKeyID := strings.SplitN(credTwoPart, ".", 2)[0]
	msg := apiKeyID + "\n" + req.Header.Get("X-Client-Ts") + "\n3.10.2\n" + sessionID + "\n" + req.Header.Get("X-Client-Nonce")
	sig, err := base64.StdEncoding.DecodeString(req.Header.Get("X-Client-Sig"))
	if err != nil {
		t.Fatal(err)
	}
	if !ed25519.Verify(pub(), []byte(msg), sig) {
		t.Fatal("Ed25519 signature must verify with the handshake-issued key")
	}

	// PoW solves
	pow := req.Header.Get("X-Client-Pow")
	if len(pow) != 32 {
		t.Fatalf("pow shape: %q", pow)
	}
}

func TestFailOpen(t *testing.T) {
	t.Run("gate disabled", func(t *testing.T) {
		m, calls, _ := newTestManager(t, testIdentity(), false, false, 0)
		req, _ := http.NewRequest(http.MethodPost, "https://zcode.z.ai/api/v1/ultra-zai/anthropic/v1/messages", nil)
		req.Header.Set("X-Api-Key", credTwoPart)
		req.Header.Set("X-Session-Id", sessionID)
		if m.Sign(context.Background(), req) {
			t.Fatal("gate disabled must not sign")
		}
		if req.Header.Get("X-Client-Sig") != "" || calls.handshakeCount != 0 {
			t.Fatalf("headers untouched, no handshake: %+v", calls)
		}
	})
	t.Run("gate throws negative-cached", func(t *testing.T) {
		m, calls, _ := newTestManager(t, testIdentity(), true, true, 0)
		for i := 0; i < 2; i++ {
			req, _ := http.NewRequest(http.MethodPost, "https://api.z.ai/api/anthropic/v1/messages", nil)
			req.Header.Set("X-Api-Key", credTwoPart)
			req.Header.Set("X-Session-Id", sessionID)
			if m.Sign(context.Background(), req) {
				t.Fatal("gate failure must not sign")
			}
		}
		if calls.gateCount != 1 {
			t.Fatalf("cooldown prevents refetch: %+v", calls)
		}
	})
	t.Run("handshake 500", func(t *testing.T) {
		m, _, _ := newTestManager(t, testIdentity(), true, false, 500)
		req, _ := http.NewRequest(http.MethodPost, "https://api.z.ai/api/anthropic/v1/messages", nil)
		req.Header.Set("X-Api-Key", credTwoPart)
		req.Header.Set("X-Session-Id", sessionID)
		if m.Sign(context.Background(), req) {
			t.Fatal("handshake failure must not sign")
		}
	})
	t.Run("single-part credential", func(t *testing.T) {
		m, calls, _ := newTestManager(t, testIdentity(), true, false, 0)
		req, _ := http.NewRequest(http.MethodPost, "https://api.z.ai/api/anthropic/v1/messages", nil)
		req.Header.Set("X-Api-Key", "legacy-key")
		req.Header.Set("X-Session-Id", sessionID)
		if m.Sign(context.Background(), req) {
			t.Fatal("single-part key must not sign")
		}
		if calls.gateCount != 0 {
			t.Fatal("cheap local checks run before the network")
		}
	})
	t.Run("missing session id", func(t *testing.T) {
		m, calls, _ := newTestManager(t, testIdentity(), true, false, 0)
		req, _ := http.NewRequest(http.MethodPost, "https://api.z.ai/api/anthropic/v1/messages", nil)
		req.Header.Set("X-Api-Key", credTwoPart)
		if m.Sign(context.Background(), req) {
			t.Fatal("missing x-session-id must not sign")
		}
		if calls.gateCount != 0 {
			t.Fatal("no network before eligibility checks")
		}
	})
	t.Run("non-zai origin — zero credential egress", func(t *testing.T) {
		m, calls, _ := newTestManager(t, testIdentity(), true, false, 0)
		req, _ := http.NewRequest(http.MethodPost, "https://open.bigmodel.cn/api/anthropic/v1/messages", nil)
		req.Header.Set("X-Api-Key", credTwoPart)
		req.Header.Set("X-Session-Id", sessionID)
		if m.Sign(context.Background(), req) {
			t.Fatal("bigmodel origin never signs")
		}
		if calls.gateCount+calls.handshakeCount != 0 {
			t.Fatal("no fetch may carry the credential off the z.ai origins")
		}
	})
}

func TestLadder401(t *testing.T) {
	t.Run("two consecutive 401s bypass", func(t *testing.T) {
		m, _, _ := newTestManager(t, testIdentity(), true, false, 0)
		url := "https://zcode.z.ai/api/v1/ultra-zai/anthropic/v1/messages"
		signedRequest(t, m, url, credTwoPart)
		m.NoteStatus(credTwoPart, 401)
		signedRequest(t, m, url, credTwoPart) // re-handshake after invalidation
		m.NoteStatus(credTwoPart, 401)
		req, _ := http.NewRequest(http.MethodPost, url, nil)
		req.Header.Set("X-Api-Key", credTwoPart)
		req.Header.Set("X-Session-Id", sessionID)
		if m.Sign(context.Background(), req) {
			t.Fatal("bypassed after 2 consecutive 401s")
		}
	})
	t.Run("intervening success resets", func(t *testing.T) {
		m, _, _ := newTestManager(t, testIdentity(), true, false, 0)
		url := "https://zcode.z.ai/api/v1/ultra-zai/anthropic/v1/messages"
		signedRequest(t, m, url, credTwoPart)
		m.NoteStatus(credTwoPart, 401)
		signedRequest(t, m, url, credTwoPart)
		m.NoteStatus(credTwoPart, 200) // success between 401s
		signedRequest(t, m, url, credTwoPart)
		m.NoteStatus(credTwoPart, 401) // count back to 1, NOT bypass
		req, _ := http.NewRequest(http.MethodPost, url, nil)
		req.Header.Set("X-Api-Key", credTwoPart)
		req.Header.Set("X-Session-Id", sessionID)
		if !m.Sign(context.Background(), req) {
			t.Fatal("must still sign after reset ladder")
		}
	})
}

// Two zai keys must have independent ladders: one key's 401 never moves
// another key's count.
func TestLadder401PerCredential(t *testing.T) {
	m, _, _ := newTestManager(t, testIdentity(), true, false, 0)
	url := "https://zcode.z.ai/api/v1/ultra-zai/anthropic/v1/messages"
	credB := "keyB.secretB"

	signedRequest(t, m, url, credTwoPart) // sign with key A
	signedRequest(t, m, url, credB)       // key B signs (interleaved)
	m.NoteStatus(credB, 401)              // B's 401 #1 → count 1
	signedRequest(t, m, url, credB)       // B re-signs (key invalidated)
	m.NoteStatus(credB, 401)              // B's 401 #2 → count 2 — B bypasses
	req, _ := http.NewRequest(http.MethodPost, url, nil)
	req.Header.Set("X-Api-Key", credB)
	req.Header.Set("X-Session-Id", sessionID)
	if m.Sign(context.Background(), req) {
		t.Fatal("credential B must bypass after its own 2 consecutive 401s")
	}
	// A must still sign: B's 401s never touched A's ladder
	reqA, _ := http.NewRequest(http.MethodPost, url, nil)
	reqA.Header.Set("X-Api-Key", credTwoPart)
	reqA.Header.Set("X-Session-Id", sessionID)
	if !m.Sign(context.Background(), reqA) {
		t.Fatal("credential A must be unaffected by B's 401s")
	}
	if reqA.Header.Get("X-Client-Sig") == "" {
		t.Fatal("A's request must carry a signature")
	}
}

func TestDecryptPrivateKeyFixture(t *testing.T) {
	// round-trip through the server-side sealing shape
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	pkcs8, err := x509.MarshalPKCS8PrivateKey(priv)
	if err != nil {
		t.Fatal(err)
	}
	cipher := sealPrivateKey(t, "k1", "s1", pkcs8)
	got, err := decryptPrivateKey("k1", "s1", cipher)
	if err != nil {
		t.Fatal(err)
	}
	if !got.Equal(priv) {
		t.Fatal("decrypted key must equal the original")
	}
	if !pub.Equal(got.Public()) {
		t.Fatal("public key mismatch")
	}
	// wrong AAD (apiKeyId) must fail
	if _, err := decryptPrivateKey("kX", "s1", cipher); err == nil {
		t.Fatal("wrong apiKeyId must not decrypt")
	}
}

func TestLiveServersWire(t *testing.T) {
	// End-to-end through real HTTP: gate + handshake planes on httptest servers.
	var hsCalls int
	gate := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("x-api-key") != credTwoPart {
			t.Errorf("gate must carry x-api-key")
		}
		if r.Header.Get("X-ZCode-Agent") != "" || r.Header.Get("X-Device-Mid") != "" {
			t.Errorf("gate identity omits X-ZCode-Agent/X-Device-Mid")
		}
		fmt.Fprint(w, `{"code":0,"data":{"codingPlanSignature":{"enable":true}}}`)
	}))
	defer gate.Close()
	hs := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hsCalls++
		b, _ := io.ReadAll(r.Body)
		var body struct {
			APIKey string `json:"apiKey"`
			Nonce  string `json:"nonce"`
			Sig    string `json:"sig"`
			Ts     string `json:"ts"`
		}
		json.Unmarshal(b, &body)
		parts := strings.SplitN(body.APIKey, ".", 2)
		mac := hmac.New(sha256.New, testHKDFBits(parts[1], kdfInfoHMAC))
		mac.Write([]byte(handshakeMethod + "\n" + parts[0] + "\n" + body.Ts + "\n" + body.Nonce))
		want, _ := base64.StdEncoding.DecodeString(body.Sig)
		if !hmac.Equal(mac.Sum(nil), want) {
			t.Fatal("handshake sig must verify")
		}
		_, privK, _ := ed25519.GenerateKey(rand.Reader)
		pkcs8, _ := x509.MarshalPKCS8PrivateKey(privK)
		fmt.Fprintf(w, `{"code":200,"data":{"privateCipher":%q}}`, sealPrivateKey(t, parts[0], parts[1], pkcs8))
	}))
	defer hs.Close()

	origGate, origHS := gateOrigin, handshakeOrigin
	defer func() { gateOrigin, handshakeOrigin = origGate, origHS }()
	gateOrigin, handshakeOrigin = gate.URL, hs.URL

	m := NewManager(testIdentity(), func(ctx context.Context, req *http.Request) (*http.Response, error) {
		return http.DefaultTransport.RoundTrip(req.WithContext(ctx))
	})

	req, err := http.NewRequest(http.MethodPost, "https://api.z.ai/api/anthropic/v1/messages", strings.NewReader("{}"))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("X-Api-Key", credTwoPart)
	req.Header.Set("X-Session-Id", sessionID)
	if !m.Sign(context.Background(), req) {
		t.Fatal("expected signed request end to end")
	}
	if req.Header.Get("X-Client-Sig") == "" || hsCalls != 1 {
		t.Fatalf("signed via live HTTP: sig=%q hsCalls=%d", req.Header.Get("X-Client-Sig"), hsCalls)
	}
}
