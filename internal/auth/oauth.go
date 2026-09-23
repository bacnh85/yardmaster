package auth

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/bacnh85/yardmaster/internal/config"
	"github.com/bacnh85/yardmaster/internal/store"
)

// OAuthPool keeps live state (fresh tokens, cooldowns) for every configured
// oauth account, refreshes tokens single-flight per account, persists rotated
// refresh tokens to SQLite, and pre-refreshes tokens nearing expiry.
// Provider-agnostic: token endpoint + client_id come from the account config
// (baked-in defaults exist for the claude-code and codex kinds).
type OAuthPool struct {
	store  *store.Store
	client *http.Client

	mu    sync.Mutex
	accts map[string]*oauthAcct // provider + "/" + account name

	stop chan struct{}
	once sync.Once
}

type oauthAcct struct {
	provider, name     string
	endpoint, clientID string
	accountID          string // codex chatgpt-account-id
	access, refresh    string
	expires            time.Time // zero = unknown; treated as stale after 1h without refresh
	lastRefresh        time.Time
	coolUntil          time.Time
	coolSteps          int
	lastErr            string
	lastStatus         int
	refreshMu          sync.Mutex // serializes the HTTP refresh per account
}

const (
	expiryMargin  = 2 * time.Minute // refresh this long before expiry
	unknownExpiry = time.Hour       // no expires_in from upstream → assume 1h
	staleAfter    = 24 * time.Hour  // unrefreshed seed token older than this is re-verified by use
)

// NewOAuthPool builds an empty pool; call Register per configured account
// (again on every config reload).
func NewOAuthPool(st *store.Store) *OAuthPool {
	p := &OAuthPool{
		store:  st,
		client: &http.Client{Timeout: 30 * time.Second},
		accts:  map[string]*oauthAcct{},
		stop:   make(chan struct{}),
	}
	go p.loop()
	return p
}

// RegisterConfig registers every oauth account in the config (call at startup
// and on every reload).
func (p *OAuthPool) RegisterConfig(cfg *config.Config) {
	if p == nil {
		return
	}
	for _, prov := range cfg.Providers {
		for _, a := range prov.Auth.OAuth {
			p.Register(prov.Name, a)
		}
	}
}

// Register adds/updates an account from config. Existing live state for the
// same provider+name is kept (tokens survive reloads).
func (p *OAuthPool) Register(providerName string, a *config.OAuthAcct) {
	if p == nil || a == nil {
		return
	}
	key := providerName + "/" + a.Name
	p.mu.Lock()
	defer p.mu.Unlock()
	if ex, ok := p.accts[key]; ok {
		ex.endpoint, ex.clientID = a.TokenEndpoint, a.ClientID
		ex.accountID = a.AccountID
		if a.RefreshTok != "" && a.RefreshTok != ex.refresh {
			ex.refresh = a.RefreshTok
			ex.access, ex.expires = "", time.Time{} // new seed → force refresh
		}
		return
	}
	acc := &oauthAcct{
		provider: providerName, name: a.Name,
		endpoint: a.TokenEndpoint, clientID: a.ClientID, accountID: a.AccountID,
		refresh: a.RefreshTok,
	}
	// stored tokens win over the (possibly stale) config seed — they carry the
	// rotated refresh token from the last run
	var expMs int64
	acc.access, acc.refresh, expMs = p.store.LoadOAuth(providerName, a.Name)
	if acc.refresh == "" {
		acc.refresh = a.RefreshTok
	}
	if acc.access == "" {
		acc.access = a.AccessTok
	}
	if expMs > 0 {
		acc.expires = time.UnixMilli(expMs)
	} else if a.ExpiresAt > 0 {
		acc.expires = time.Unix(a.ExpiresAt, 0)
	}
	acc.lastRefresh = time.Now()
	p.accts[key] = acc
}

func (p *OAuthPool) get(providerName, acctName string) *oauthAcct {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.accts[providerName+"/"+acctName]
}

// Token returns a valid access token, refreshing (single-flight) when stale.
func (p *OAuthPool) Token(ctx context.Context, providerName, acctName string) (string, error) {
	a := p.get(providerName, acctName)
	if a == nil {
		return "", fmt.Errorf("oauth account %s/%s not registered", providerName, acctName)
	}
	p.mu.Lock()
	access := a.access
	fresh := access != "" && time.Now().Before(a.expires.Add(-expiryMargin))
	p.mu.Unlock()
	if fresh {
		return access, nil
	}
	return p.refresh(ctx, a)
}

func (p *OAuthPool) refresh(ctx context.Context, a *oauthAcct) (string, error) {
	a.refreshMu.Lock()
	defer a.refreshMu.Unlock()
	// snapshot mutable fields under p.mu — Register() (config reload) may
	// mutate endpoint/clientID/refresh concurrently
	p.mu.Lock()
	access, refreshTok, endpoint, clientID, lastRefresh :=
		a.access, a.refresh, a.endpoint, a.clientID, a.lastRefresh
	fresh := access != "" && time.Now().Before(a.expires.Add(-expiryMargin))
	p.mu.Unlock()
	if fresh {
		return access, nil
	}
	if refreshTok == "" {
		if access != "" && time.Since(lastRefresh) < staleAfter {
			return access, nil // seed token without refresh: use until it demonstrably fails
		}
		p.setErr(a, 0, "no refresh_token")
		return "", fmt.Errorf("oauth account %s/%s: no refresh_token", a.provider, a.name)
	}

	body, _ := json.Marshal(map[string]any{
		"grant_type": "refresh_token", "refresh_token": refreshTok, "client_id": clientID,
	})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := p.client.Do(req)
	if err != nil {
		p.setErr(a, 0, "refresh: "+err.Error())
		return "", fmt.Errorf("oauth refresh %s/%s: %w", a.provider, a.name, err)
	}
	defer resp.Body.Close()
	var tok struct {
		AccessToken      string `json:"access_token"`
		RefreshToken     string `json:"refresh_token"`
		ExpiresIn        int64  `json:"expires_in"`
		Error            string `json:"error"`
		ErrorDescription string `json:"error_description"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&tok); err != nil {
		p.setErr(a, resp.StatusCode, "refresh: bad response ("+err.Error()+")")
		return "", fmt.Errorf("oauth refresh %s/%s: bad response", a.provider, a.name)
	}
	if resp.StatusCode != 200 || tok.AccessToken == "" {
		msg := strings.TrimSpace(tok.Error + " " + tok.ErrorDescription)
		p.setErr(a, resp.StatusCode, "refresh: http "+fmt.Sprint(resp.StatusCode)+" "+msg)
		return "", fmt.Errorf("oauth refresh %s/%s: http %d %s", a.provider, a.name, resp.StatusCode, msg)
	}
	expiry := time.Now().Add(time.Duration(tok.ExpiresIn) * time.Second)
	if tok.ExpiresIn <= 0 {
		expiry = time.Now().Add(unknownExpiry)
	}
	p.mu.Lock()
	a.access = tok.AccessToken
	if tok.RefreshToken != "" { // rotation: upstream hands back a new refresh token
		a.refresh = tok.RefreshToken
	}
	a.expires, a.lastRefresh, a.lastErr = expiry, time.Now(), ""
	access, refresh := a.access, a.refresh
	p.mu.Unlock()
	p.store.SaveOAuth(a.provider, a.name, access, refresh, expiry.UnixMilli())
	return access, nil
}

// setErr records a refresh failure and applies cooldown policy:
// 4xx = grant rejected (dead account, long cooldown), 5xx/network = transient.
func (p *OAuthPool) setErr(a *oauthAcct, status int, msg string) {
	p.mu.Lock()
	a.lastErr = strings.TrimSpace(msg)
	switch {
	case status >= 400 && status < 500:
		a.coolUntil = time.Now().Add(10 * time.Minute)
	case status == 0, status >= 500:
		a.coolUntil = time.Now().Add(30 * time.Second)
	default:
		a.coolUntil = time.Time{}
	}
	p.mu.Unlock()
}

// MarkResult feeds an upstream dispatch outcome back into the pool so cooled
// accounts are skipped by the failover loop.
func (p *OAuthPool) MarkResult(providerName, acctName string, status int, errMsg string) {
	a := p.get(providerName, acctName)
	if a == nil {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	switch status {
	case 200:
		a.lastErr, a.coolSteps = "", 0
		a.coolUntil = time.Time{}
	case 401: // access token rejected → drop it so the next Token() refreshes
		a.access = ""
		a.coolUntil = time.Now().Add(5 * time.Second)
	case 403, 429: // quota/subscription limits
		if status == 429 {
			a.coolSteps++
		}
		d := time.Minute << min(a.coolSteps, 4)
		if d > 10*time.Minute {
			d = 10 * time.Minute
		}
		a.coolUntil = time.Now().Add(d)
		if errMsg != "" {
			a.lastErr = errMsg
			a.lastStatus = status
		}
	}
}

// Cooling reports whether the account is currently in quota cooldown.
func (p *OAuthPool) Cooling(providerName, acctName string) bool {
	if p == nil {
		return false
	}
	a := p.get(providerName, acctName)
	if a == nil {
		return false
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	return time.Now().Before(a.coolUntil)
}

// Until returns when the account's cooldown expires (zero time if not cooling).
func (p *OAuthPool) Until(providerName, acctName string) time.Time {
	if p == nil {
		return time.Time{}
	}
	a := p.get(providerName, acctName)
	if a == nil {
		return time.Time{}
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if !time.Now().Before(a.coolUntil) {
		return time.Time{}
	}
	return a.coolUntil
}

// State is the admin-API view of one account (never includes tokens).
type State struct {
	Name      string `json:"name"`
	Kind      string `json:"kind"`
	Disabled  bool   `json:"disabled"`
	ExpiresAt int64  `json:"expires_at"` // unix seconds of the access token; 0 = unknown
	Fresh     bool   `json:"fresh"`
	Cooling   bool   `json:"cooling"`
	LastError string `json:"last_error,omitempty"`
}

// States snapshots all accounts of one provider.
func (p *OAuthPool) States(providerName string) map[string]State {
	out := map[string]State{}
	if p == nil {
		return out
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, a := range p.accts {
		if a.provider != providerName {
			continue
		}
		out[a.name] = State{
			Name:      a.name,
			ExpiresAt: a.expires.Unix(),
			Fresh:     a.access != "" && time.Now().Before(a.expires),
			Cooling:   time.Now().Before(a.coolUntil),
			LastError: a.lastErr,
		}
	}
	return out
}

// Close stops the background refresher.
func (p *OAuthPool) Close() {
	if p == nil {
		return
	}
	p.once.Do(func() { close(p.stop) })
}

// loop pre-refreshes tokens nearing expiry so requests never pay refresh
// latency. Failures stay recorded in lastErr; the dispatch path retries
// on demand.
func (p *OAuthPool) loop() {
	t := time.NewTicker(time.Minute)
	defer t.Stop()
	for {
		select {
		case <-p.stop:
			return
		case <-t.C:
			p.mu.Lock()
			var due []*oauthAcct
			for _, a := range p.accts {
				if a.refresh != "" && time.Now().After(a.expires.Add(-10*time.Minute)) &&
					time.Now().Before(a.expires.Add(-expiryMargin)) && time.Now().After(a.coolUntil) {
					due = append(due, a)
				}
			}
			p.mu.Unlock()
			for _, a := range due {
				ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
				_, _ = p.refresh(ctx, a)
				cancel()
			}
		}
	}
}
