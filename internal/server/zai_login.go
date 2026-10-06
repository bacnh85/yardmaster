package server

// Z.ai start-plan OAuth CLI login — server-mediated flow mirroring the ZCode
// desktop client 3.12.3 `startOAuthWithPolling` (port of zcode-api
// CliOAuthClient): POST oauth/cli/init → user opens authorize_url (+ the
// desktop interstitial so the flow completes server-side) → poll
// oauth/cli/poll/{flow_id} until status:"ready" → data.token is the plan JWT
// (start-plan credential) and data.zai.access_token the coding-plan API key.
// The JWT has no exp; only a real 401/biz-3012 means re-login.
//
// Flow state lives in memory (flow_id → poll token), same lifecycle as the
// admin login throttle. yardmaster is the poller, so the browser never
// returns to localhost.

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

const zaiLoginOrigin = "https://zcode.z.ai"
const zaiLoginAppVersion = "3.14.0" // zcode-api DEFAULT_APP_VERSION

// test seam: point the OAuth client at a stub (tests, never prod config).
var zaiLoginOriginOverride string

func replaceZaiLoginOrigin(url string) func() {
	prev := zaiLoginOriginOverride
	zaiLoginOriginOverride = url
	return func() { zaiLoginOriginOverride = prev }
}

func zaiLoginBase() string {
	if zaiLoginOriginOverride != "" {
		return zaiLoginOriginOverride
	}
	return zaiLoginOrigin
}

type zaiLoginFlow struct {
	pollToken string
	expiresAt time.Time
}

type zaiLoginStore struct {
	mu    sync.Mutex
	flows map[string]zaiLoginFlow
}

var zaiFlows = &zaiLoginStore{flows: map[string]zaiLoginFlow{}}

func writeJSONBody(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(v)
}

func (s *zaiLoginStore) put(id string, f zaiLoginFlow) {
	s.mu.Lock()
	defer s.mu.Unlock()
	// opportunistic sweep keeps the map bounded without a timer
	now := time.Now()
	for k, v := range s.flows {
		if now.After(v.expiresAt) {
			delete(s.flows, k)
		}
	}
	s.flows[id] = f
}

func (s *zaiLoginStore) pollToken(id string) (string, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	f, ok := s.flows[id]
	if !ok || time.Now().After(f.expiresAt) {
		return "", false
	}
	return f.pollToken, true
}

// zcodeEnvelope is zcode.z.ai's {code, msg, data} wrapper (code 0 = ok).
type zcodeEnvelope struct {
	Code int             `json:"code"`
	Msg  string          `json:"msg"`
	Data json.RawMessage `json:"data"`
}

// interstitialURL mirrors zcode-api applyInterstitial: REPLACE the authorize
// URL's redirect_uri param with the desktop interstitial PAGE — the zcode.z.ai
// page records the authorization server-side (flipping the poll to ready)
// before bouncing the browser to zcode://oauth/callback. Appending a second
// redirect_uri would leave the server-side callback in place and the poll
// would never flip.
func interstitialURL(authorize string) string {
	u, err := url.Parse(authorize)
	if err != nil {
		return authorize
	}
	q := u.Query()
	interstitial := zaiLoginOrigin + "/app/oauth/login?redirect=" +
		url.QueryEscape("zcode://oauth/callback") + "&app_version=" + url.QueryEscape(zaiLoginAppVersion)
	q.Set("redirect_uri", interstitial)
	u.RawQuery = q.Encode()
	return u.String()
}

// POST /admin/api/zai/login — start a login flow, return the authorize URL.
func (s *Server) handleZaiLoginStart(w http.ResponseWriter, r *http.Request) {
	pollToken := make([]byte, 32)
	if _, err := rand.Read(pollToken); err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		zaiLoginBase()+"/api/v1/oauth/cli/init",
		strings.NewReader(`{"provider":"zai"}`))
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+hex.EncodeToString(pollToken))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		http.Error(w, "zai login init: "+err.Error(), 502)
		return
	}
	defer resp.Body.Close()
	var env zcodeEnvelope
	if err := json.NewDecoder(resp.Body).Decode(&env); err != nil || env.Code != 0 {
		msg := env.Msg
		if msg == "" {
			msg = fmt.Sprintf("HTTP %d", resp.StatusCode)
		}
		http.Error(w, "zai login init failed: "+msg, 502)
		return
	}
	var d struct {
		FlowID          string `json:"flow_id"`
		AuthorizeURL    string `json:"authorize_url"`
		ExpiresAt       int64  `json:"expires_at"` // unix seconds
		PollIntervalSec int    `json:"poll_interval_sec"`
	}
	if err := json.Unmarshal(env.Data, &d); err != nil || d.FlowID == "" || d.AuthorizeURL == "" {
		http.Error(w, "zai login init: invalid response data", 502)
		return
	}
	exp := time.Unix(d.ExpiresAt, 0)
	if d.ExpiresAt <= 0 {
		exp = time.Now().Add(10 * time.Minute)
	}
	zaiFlows.put(d.FlowID, zaiLoginFlow{pollToken: hex.EncodeToString(pollToken), expiresAt: exp})
	writeJSONBody(w, map[string]any{
		"flow_id":           d.FlowID,
		"authorize_url":     interstitialURL(d.AuthorizeURL),
		"expires_at":        d.ExpiresAt,
		"poll_interval_sec": d.PollIntervalSec,
	})
}

// GET /admin/api/zai/login/poll?flow_id=… — one poll round.
// 200 {status:"pending"} | 200 {status:"ready", jwt, access_token} |
// 200 {status:"failed", error} | 404 unknown/expired flow.
func (s *Server) handleZaiLoginPoll(w http.ResponseWriter, r *http.Request) {
	flowID := r.URL.Query().Get("flow_id")
	pollToken, ok := zaiFlows.pollToken(flowID)
	if !ok {
		http.Error(w, "unknown or expired login flow", 404)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		zaiLoginBase()+"/api/v1/oauth/cli/poll/"+flowID, nil)
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	req.Header.Set("Authorization", "Bearer "+pollToken)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		writeJSONBody(w, map[string]any{"status": "pending"}) // transient — keep polling
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 && resp.StatusCode != http.StatusRequestTimeout && resp.StatusCode != http.StatusTooManyRequests {
		http.Error(w, fmt.Sprintf("zai login poll failed: HTTP %d", resp.StatusCode), 502)
		return
	}
	var env zcodeEnvelope
	if err := json.NewDecoder(resp.Body).Decode(&env); err != nil {
		writeJSONBody(w, map[string]any{"status": "pending"})
		return
	}
	if env.Code != 0 {
		writeJSONBody(w, map[string]any{"status": "failed", "error": fmt.Sprintf("code=%d msg=%s", env.Code, env.Msg)})
		return
	}
	var d struct {
		Status string `json:"status"`
		Token  string `json:"token"`
		Zai    struct {
			AccessToken string `json:"access_token"`
		} `json:"zai"`
	}
	_ = json.Unmarshal(env.Data, &d)
	switch d.Status {
	case "ready":
		if d.Token == "" {
			writeJSONBody(w, map[string]any{"status": "failed", "error": "ready without plan token"})
			return
		}
		zaiFlows.mu.Lock()
		delete(zaiFlows.flows, flowID)
		zaiFlows.mu.Unlock()
		writeJSONBody(w, map[string]any{"status": "ready", "jwt": d.Token, "access_token": d.Zai.AccessToken})
	case "failed":
		writeJSONBody(w, map[string]any{"status": "failed", "error": "authorization failed or denied"})
	default:
		writeJSONBody(w, map[string]any{"status": "pending"})
	}
}
