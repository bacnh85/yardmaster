// Package server wires HTTP handlers: OpenAI + Anthropic endpoints, admin API,
// and the embedded dashboard.
package server

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/bacnh85/yardmaster/internal/auth"
	"github.com/bacnh85/yardmaster/internal/config"
	"github.com/bacnh85/yardmaster/internal/proxy"
	"github.com/bacnh85/yardmaster/internal/store"
	"github.com/bacnh85/yardmaster/internal/translate"
	"github.com/bacnh85/yardmaster/web"
)

type Server struct {
	Proxy      *proxy.Proxy
	Keys       *auth.KeyStore
	Store      *store.Store
	ConfigPath string
	Version    string

	adminMu   sync.Mutex
	adminPass string
	sessions  map[string]time.Time
}

func New(p *proxy.Proxy, keys *auth.KeyStore, st *store.Store, cfgPath, adminPass, version string) *Server {
	return &Server{
		Proxy: p, Keys: keys, Store: st,
		ConfigPath: cfgPath, Version: version,
		adminPass: adminPass, sessions: map[string]time.Time{},
	}
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/chat/completions", s.wrap(s.Proxy.ServeChat))
	mux.HandleFunc("POST /v1/responses", s.wrap(s.Proxy.ServeResponses))
	mux.HandleFunc("POST /v1/messages", s.wrap(s.Proxy.ServeMessages))
	mux.HandleFunc("POST /v1/messages/count_tokens", s.wrap(s.wrapCountTokens))
	mux.HandleFunc("GET /v1/models", s.wrap(s.handleModels))
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("ok"))
	})
	mux.HandleFunc("POST /admin/api/login", s.handleLogin)
	mux.HandleFunc("/admin/api/", s.wrapAdmin(s.handleAdmin))
	mux.HandleFunc("/", s.handleStatic)
	return mux
}

// wrap applies inbound auth + request timing.
func (s *Server) wrap(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		keyName, allow, ok := s.authenticate(r)
		if !ok {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(401)
			json.NewEncoder(w).Encode(map[string]any{"error": map[string]any{
				"message": "invalid or missing API key", "type": "authentication_error"}})
			return
		}
		ctx := r.Context()
		ctx = proxy.WithStartTime(ctx, start)
		ctx = proxy.WithInboundKey(ctx, keyName)
		ctx = proxy.WithInboundAllow(ctx, allow)
		h(w, r.WithContext(ctx))
	}
}

func (s *Server) authenticate(r *http.Request) (name string, allow []string, ok bool) {
	key := ""
	if ah := r.Header.Get("Authorization"); strings.HasPrefix(ah, "Bearer ") {
		key = strings.TrimPrefix(ah, "Bearer ")
	}
	if key == "" {
		key = r.Header.Get("x-api-key")
	}
	if key == "" {
		return "", nil, false
	}
	k, ok := s.Keys.Check(key)
	if !ok {
		return "", nil, false
	}
	return k.Name, k.Allow, true
}

func (s *Server) wrapCountTokens(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(io.LimitReader(r.Body, 64<<20))
	if err != nil {
		http.Error(w, err.Error(), 400)
		return
	}
	var req map[string]any
	if json.Unmarshal(body, &req) != nil {
		http.Error(w, "bad json", 400)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{"input_tokens": translate.CountTokensEstimate(req)})
}

func (s *Server) handleModels(w http.ResponseWriter, r *http.Request) {
	// Field names follow what the pi-router extension's mapModel() reads:
	// context_length / max_output_tokens / capabilities.vision.
	type caps struct {
		Vision bool `json:"vision,omitempty"`
	}
	type model struct {
		ID            string `json:"id"`
		Object        string `json:"object"`
		Owned         string `json:"owned_by"`
		ContextLength int    `json:"context_length,omitempty"`
		MaxOutTokens  int    `json:"max_output_tokens,omitempty"`
		Capabilities  *caps  `json:"capabilities,omitempty"`
	}
	data := make([]model, 0) // never nil — empty registry must marshal as [], not null
	seen := map[string]bool{}
	metas := cachedCatalogMetas(s.Proxy.Reg.Config().Providers)
	for _, m := range s.Proxy.Reg.Models() {
		if seen[m] {
			continue
		}
		seen[m] = true
		entry := model{ID: m, Object: "model", Owned: "yardmaster"}
		if mm, ok := lookupMeta(metas, m); ok && (mm.Context > 0 || mm.Image) {
			entry.ContextLength = mm.Context
			entry.MaxOutTokens = mm.MaxOutput
			entry.Capabilities = &caps{Vision: mm.Image}
		}
		data = append(data, entry)
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{"object": "list", "data": data})
}

// lookupMeta finds catalog metadata for an advertised id, trying the id
// verbatim then with a known provider prefix stripped.
func lookupMeta(metas map[string]ModelMeta, id string) (ModelMeta, bool) {
	if mm, ok := metas[canonModelID(id)]; ok {
		return mm, true
	}
	if i := strings.IndexByte(id, '/'); i > 0 {
		if mm, ok := metas[canonModelID(id[i+1:])]; ok {
			return mm, true
		}
	}
	return ModelMeta{}, false
}

// cachedCatalogMetas merges the providers' in-cache catalogs into one
// canon-id → meta map. Cache-hit only — never triggers an upstream fetch.
func cachedCatalogMetas(providers []*config.Provider) map[string]ModelMeta {
	out := map[string]ModelMeta{}
	for _, p := range providers {
		e, ok := catalogCache.Load(p.BaseURL)
		if !ok {
			continue
		}
		for _, mm := range e.(catalogEntry).models {
			k := canonModelID(mm.ID)
			if _, dup := out[k]; !dup {
				out[k] = mm
			}
		}
	}
	return out
}

// ---- admin ----

func (s *Server) handleLogin(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Password string `json:"password"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&req); err != nil {
		http.Error(w, "bad json", 400)
		return
	}
	if s.adminPass == "" || subtle.ConstantTimeCompare([]byte(req.Password), []byte(s.adminPass)) != 1 {
		http.Error(w, "unauthorized", 401)
		return
	}
	tok := make([]byte, 24)
	if _, err := rand.Read(tok); err != nil {
		http.Error(w, "entropy failure", 500)
		return
	}
	token := hex.EncodeToString(tok)
	s.adminMu.Lock()
	s.sessions[token] = time.Now().Add(7 * 24 * time.Hour)
	s.adminMu.Unlock()
	http.SetCookie(w, &http.Cookie{
		Name: "ar_admin", Value: token, Path: "/", HttpOnly: true,
		SameSite: http.SameSiteLaxMode, MaxAge: 7 * 24 * 3600,
	})
	w.Write([]byte(`{"ok":true}`))
}

func (s *Server) adminAuthed(r *http.Request) bool {
	// AR_ALLOW_ANON_ADMIN=1: demo/screenshot mode — dashboard open without login.
	// Never enable on a routed interface.
	if os.Getenv("AR_ALLOW_ANON_ADMIN") == "1" {
		return true
	}
	if c, err := r.Cookie("ar_admin"); err == nil {
		s.adminMu.Lock()
		exp, ok := s.sessions[c.Value]
		if ok && !time.Now().Before(exp) {
			delete(s.sessions, c.Value)
			ok = false
		}
		s.adminMu.Unlock()
		if ok {
			return true
		}
	}
	_, pw, ok := r.BasicAuth()
	return ok && s.adminPass != "" && subtle.ConstantTimeCompare([]byte(pw), []byte(s.adminPass)) == 1
}

func (s *Server) wrapAdmin(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !s.adminAuthed(r) {
			http.Error(w, "unauthorized", 401)
			return
		}
		h(w, r)
	}
}

func (s *Server) handleAdmin(w http.ResponseWriter, r *http.Request) {
	path := strings.TrimPrefix(r.URL.Path, "/admin/api/")
	q := r.URL.Query()
	writeJSON := func(v any) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(v)
	}
	switch {
	case path == "version" && r.Method == "GET":
		writeJSON(map[string]any{"version": s.Version})
	case path == "summary" && r.Method == "GET":
		hours, _ := strconv.Atoi(q.Get("hours"))
		if hours <= 0 {
			hours = 24
		}
		sum, err := s.Store.SummarySince(time.Duration(hours)*time.Hour, q.Get("bucket"))
		if err != nil {
			http.Error(w, err.Error(), 500)
			return
		}
		inflight, total := s.Proxy.Stats()
		sum.Bucket = q.Get("bucket")
		writeJSON(map[string]any{"summary": sum, "inflight": inflight, "total": total})
	case path == "requests" && r.Method == "GET":
		limit, _ := strconv.Atoi(q.Get("limit"))
		rows, err := s.Store.Recent(limit)
		if err != nil {
			http.Error(w, err.Error(), 500)
			return
		}
		writeJSON(map[string]any{"requests": rows})
	case path == "breakdown" && r.Method == "GET":
		by := q.Get("by")
		if by != "model" && by != "provider" && by != "key_name" {
			http.Error(w, "by must be model|provider|key_name", 400)
			return
		}
		hours, _ := strconv.Atoi(q.Get("hours"))
		if hours <= 0 {
			hours = 24
		}
		rows, err := s.Store.BreakdownBy(by, time.Duration(hours)*time.Hour)
		if err != nil {
			http.Error(w, err.Error(), 500)
			return
		}
		writeJSON(map[string]any{"breakdown": rows})
	case path == "keys" && r.Method == "GET":
		out := make([]map[string]any, 0, len(s.Proxy.Reg.Config().Keys))
		for _, k := range s.Proxy.Reg.Config().Keys {
			out = append(out, map[string]any{
				"name": k.Name, "key_suffix": suffix(k.Key), "allow": k.Allow, "rpm": k.RPM,
			})
		}
		writeJSON(map[string]any{"keys": out})
	case path == "providers" && r.Method == "GET":
		out := make([]map[string]any, 0, len(s.Proxy.Reg.Config().Providers))
		for _, p := range s.Proxy.Reg.Config().Providers {
			conns := make([]map[string]any, 0, len(p.Auth.Keys))
			for i, k := range p.Auth.Keys {
				conns = append(conns, map[string]any{"label": p.Auth.KeyLabel(i), "suffix": suffix(k)})
			}
			out = append(out, map[string]any{
				"name": p.Name, "wire": p.Wire, "base_url": p.BaseURL,
				"models": nonNil(p.Models), "dispatch_interval_ms": p.DispatchIntervalMS,
				"prefix":            p.Prefix,
				"preset":            p.Preset,
				"disabled":          p.Disabled,
				"session":           p.Session,
				"auth_type":         p.Auth.Type,
				"adaptive_thinking": p.AdaptiveThinking, "inject_cache_control": p.InjectCacheControl,
				"connections": conns,
				"accounts":    oauthAccountStates(p, s.Proxy.Pool),
			})
		}
		writeJSON(map[string]any{"providers": out})
	case path == "quota" && r.Method == "GET":
		s.handleQuota(w, r)
	case path == "live" && r.Method == "GET":
		s.serveLive(w, r)
	case path == "keys" && r.Method == "POST":
		var req struct {
			Name  string   `json:"name"`
			Allow []string `json:"allow"`
			RPM   int      `json:"rpm"`
		}
		if json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&req) != nil || req.Name == "" {
			http.Error(w, "name required", 400)
			return
		}
		raw := config.GenKey()
		if !s.mutate(w, func(c *config.Config) error {
			for _, k := range c.Keys {
				if k.Name == req.Name {
					return fmt.Errorf("key %q already exists", req.Name)
				}
			}
			allow := req.Allow
			if len(allow) == 0 {
				allow = []string{"*"}
			}
			c.Keys = append(c.Keys, &config.Key{Key: raw, Name: req.Name, Allow: allow, RPM: req.RPM})
			return nil
		}) {
			return
		}
		// the raw key is shown exactly once, in this response
		writeJSON(map[string]any{"ok": true, "key": raw})
	case strings.HasPrefix(path, "keys/") && r.Method == "DELETE":
		name := strings.TrimPrefix(path, "keys/")
		if !s.mutate(w, func(c *config.Config) error {
			for i, k := range c.Keys {
				if k.Name == name {
					c.Keys = append(c.Keys[:i], c.Keys[i+1:]...)
					return nil
				}
			}
			return fmt.Errorf("no key named %q", name)
		}) {
			return
		}
		writeJSON(map[string]any{"ok": true})
	case path == "playground" && r.Method == "POST":
		var req struct {
			Provider  string `json:"provider"`
			Model     string `json:"model"`
			Prompt    string `json:"prompt"`
			MaxTokens int    `json:"max_tokens"`
		}
		if json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&req) != nil ||
			req.Provider == "" || req.Model == "" || req.Prompt == "" {
			http.Error(w, "provider, model and prompt are required", 400)
			return
		}
		res, err := s.Proxy.Probe(r.Context(), req.Provider, req.Model, req.Prompt, req.MaxTokens)
		if err != nil {
			http.Error(w, err.Error(), 502)
			return
		}
		writeJSON(res)
	case strings.HasPrefix(path, "providers/") && strings.HasSuffix(path, "/models") && r.Method == "GET":
		name := strings.TrimSuffix(strings.TrimPrefix(path, "providers/"), "/models")
		models, err := s.catalog(r.Context(), name)
		if err != nil {
			http.Error(w, err.Error(), 400)
			return
		}
		writeJSON(map[string]any{"models": models})
	case strings.HasPrefix(path, "providers/") && strings.HasSuffix(path, "/keys") && r.Method == "POST":
		name := strings.TrimSuffix(strings.TrimPrefix(path, "providers/"), "/keys")
		var req struct {
			Key   string `json:"key"`
			Label string `json:"label"`
		}
		if json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&req) != nil || req.Key == "" {
			http.Error(w, "key required", 400)
			return
		}
		if !s.mutate(w, func(c *config.Config) error {
			for _, x := range c.Providers {
				if x.Name == name {
					for _, k := range x.Auth.Keys {
						if k == req.Key {
							return fmt.Errorf("key already present")
						}
					}
					x.Auth.Keys = append(x.Auth.Keys, req.Key)
					// keep KeyLabels index-aligned with Keys even when earlier
					// keys were unlabeled: pad, then append this label
					for len(x.Auth.KeyLabels) < len(x.Auth.Keys)-1 {
						x.Auth.KeyLabels = append(x.Auth.KeyLabels, "")
					}
					x.Auth.KeyLabels = append(x.Auth.KeyLabels, req.Label)
					return nil
				}
			}
			return fmt.Errorf("no provider named %q", name)
		}) {
			return
		}
		writeJSON(map[string]any{"ok": true})
	case strings.HasPrefix(path, "providers/") && strings.Contains(path, "/keys/") && r.Method == "DELETE":
		rest := strings.TrimPrefix(path, "providers/")
		slash := strings.Index(rest, "/keys/")
		name, idxStr := rest[:slash], rest[slash+len("/keys/"):]
		// index-addressed (not suffix): two keys may share a 6-char tail and
		// each ✕ must remove the distinct key the user clicked. Order comes
		// from the same GET the UI rendered — admin-only, races acceptable.
		idx, err := strconv.Atoi(idxStr)
		if err != nil || idx < 0 {
			http.Error(w, "key index must be an integer", 400)
			return
		}
		if !s.mutate(w, func(c *config.Config) error {
			for _, x := range c.Providers {
				if x.Name == name {
					if idx >= len(x.Auth.Keys) {
						return fmt.Errorf("key index %d out of range (%d keys on %q)", idx, len(x.Auth.Keys), name)
					}
					x.Auth.Keys = append(x.Auth.Keys[:idx], x.Auth.Keys[idx+1:]...)
					// keep labels index-aligned with the keys they describe
					if idx < len(x.Auth.KeyLabels) {
						x.Auth.KeyLabels = append(x.Auth.KeyLabels[:idx], x.Auth.KeyLabels[idx+1:]...)
					}
					return nil
				}
			}
			return fmt.Errorf("no provider named %q", name)
		}) {
			return
		}
		writeJSON(map[string]any{"ok": true})
	case path == "providers" && r.Method == "POST":
		var f providerForm
		if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&f); err != nil {
			http.Error(w, "bad json", 400)
			return
		}
		p, err := f.provider()
		if err != nil {
			http.Error(w, err.Error(), 400)
			return
		}
		if !s.mutate(w, func(c *config.Config) error {
			for _, x := range c.Providers {
				if x.Name == p.Name {
					return fmt.Errorf("provider %q already exists", p.Name)
				}
			}
			c.Providers = append(c.Providers, p)
			return nil
		}) {
			return
		}
		writeJSON(map[string]any{"ok": true})
	case strings.HasPrefix(path, "providers/") && r.Method == "PUT":
		name := strings.TrimPrefix(path, "providers/")
		var f providerForm
		if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&f); err != nil {
			http.Error(w, "bad json", 400)
			return
		}
		p, err := f.provider()
		if err != nil {
			http.Error(w, err.Error(), 400)
			return
		}
		p.Name = name
		if !s.mutate(w, func(c *config.Config) error {
			for i, x := range c.Providers {
				if x.Name == name {
					// the form covers a subset; preserve advanced fields it can't express
					if f.Keys == nil {
						p.Auth.Keys = x.Auth.Keys
					}
					if f.KeyLabels == nil {
						p.Auth.KeyLabels = x.Auth.KeyLabels // survive key edits unless explicitly sent
					}
					if f.Prefix == nil {
						p.Prefix = x.Prefix // omitted field keeps the stored prefix
					}
					if f.Session == nil {
						p.Session = x.Session // omitted field keeps the stored session headers
					}
					if f.Preset == "" {
						p.Preset = x.Preset
					}
					if f.Disabled == nil {
						p.Disabled = x.Disabled // omitted field keeps the stored state
					} else {
						p.Disabled = *f.Disabled // registry toggle sends it explicitly
					}
					p.Auth.Type = x.Auth.Type // form is static-only; never downgrade oauth
					p.Auth.OAuth = x.Auth.OAuth
					p.ModelMap = x.ModelMap
					p.ExtraHeaders = x.ExtraHeaders
					p.BodyOverrides = x.BodyOverrides
					p.HeadersTimeoutS = x.HeadersTimeoutS
					c.Providers[i] = p
					return nil
				}
			}
			return fmt.Errorf("no provider named %q", name)
		}) {
			return
		}
		writeJSON(map[string]any{"ok": true})
	case strings.HasPrefix(path, "providers/") && r.Method == "DELETE":
		name := strings.TrimPrefix(path, "providers/")
		if !s.mutate(w, func(c *config.Config) error {
			kept := c.Providers[:0]
			for _, x := range c.Providers {
				if x.Name != name {
					kept = append(kept, x)
				}
			}
			if len(kept) == len(c.Providers) {
				return fmt.Errorf("no provider named %q", name)
			}
			c.Providers = kept
			// scrub routes that pointed at the removed provider
			routes := c.Routes[:0]
			for _, rt := range c.Routes {
				chain := rt.Chain[:0]
				for _, n := range rt.Chain {
					if n != name {
						chain = append(chain, n)
					}
				}
				if len(chain) > 0 {
					rt.Chain = chain
					routes = append(routes, rt)
				}
			}
			c.Routes = routes
			return nil
		}) {
			return
		}
		writeJSON(map[string]any{"ok": true})
	case path == "reload" && r.Method == "POST":
		s.Reload()
		writeJSON(map[string]any{"ok": true})
	case path == "config" && r.Method == "GET":
		cfg := s.Proxy.Reg.Config()
		// redacted view: never serialize AuthConf (keys/tokens) to the client
		provs := make([]map[string]any, 0, len(cfg.Providers))
		for _, p := range cfg.Providers {
			provs = append(provs, map[string]any{
				"name": p.Name, "wire": p.Wire, "base_url": p.BaseURL,
				"models": nonNil(p.Models), "auth_type": p.Auth.Type,
				"num_keys": len(p.Auth.Keys), "num_accounts": len(p.Auth.OAuth),
				"session":              p.Session,
				"dispatch_interval_ms": p.DispatchIntervalMS,
				"adaptive_thinking":    p.AdaptiveThinking,
				"inject_cache_control": p.InjectCacheControl,
			})
		}
		writeJSON(map[string]any{
			"listen": cfg.Listen, "db_path": cfg.DBPath,
			"providers": provs, "routes": cfg.Routes,
		})
	default:
		http.NotFound(w, r)
	}
}

func suffix(k string) string {
	if len(k) > 6 {
		return "…" + k[len(k)-6:]
	}
	return k
}

func nonNil(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}

// mutate persists a config change: fresh copy from disk -> fn -> save -> reload.
// Returns false (and writes the HTTP error) on any failure.
func (s *Server) mutate(w http.ResponseWriter, fn func(*config.Config) error) bool {
	if s.ConfigPath == "" {
		http.Error(w, "started without a config file; edits disabled", 400)
		return false
	}
	cfg, err := config.Load(s.ConfigPath)
	if err != nil {
		http.Error(w, err.Error(), 500)
		return false
	}
	if err := fn(cfg); err != nil {
		http.Error(w, err.Error(), 400)
		return false
	}
	if err := cfg.Validate(); err != nil {
		http.Error(w, err.Error(), 400)
		return false
	}
	if err := cfg.Save(s.ConfigPath); err != nil {
		http.Error(w, err.Error(), 500)
		return false
	}
	s.Reload()
	return true
}

// providerForm is the dashboard-editable subset of a provider.
type providerForm struct {
	Name               string   `json:"name"`
	Wire               string   `json:"wire"`
	BaseURL            string   `json:"base_url"`
	Models             []string `json:"models"`
	Keys               []string `json:"keys"`
	KeyLabels          []string `json:"keyLabels"`
	Prefix             *string  `json:"prefix"`  // nil = omitted (keep stored); "" = none; else routing prefix
	Session            *string  `json:"session"` // nil = omitted (keep stored); "" = none; "opencode" = session headers
	Preset             string   `json:"preset"`
	Disabled           *bool    `json:"disabled"` // nil = omitted (keep stored)
	DispatchIntervalMS int      `json:"dispatch_interval_ms"`
	AdaptiveThinking   bool     `json:"adaptive_thinking"`
	InjectCacheControl bool     `json:"inject_cache_control"`
}

func (f providerForm) provider() (*config.Provider, error) {
	if f.Wire != "openai" && f.Wire != "anthropic" && f.Wire != "responses" {
		return nil, fmt.Errorf("wire must be openai, anthropic, or responses")
	}
	u, err := url.Parse(f.BaseURL)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return nil, fmt.Errorf("base_url must be an http(s) URL")
	}
	prefix := ""
	if f.Prefix != nil {
		prefix = strings.ToLower(strings.TrimSpace(*f.Prefix))
		if prefix != "" && !config.ValidPrefix(prefix) {
			return nil, fmt.Errorf("prefix must be 1-12 lowercase letters, digits or hyphens")
		}
	}
	session := ""
	if f.Session != nil {
		session = *f.Session
		if session != "" && session != "opencode" {
			return nil, fmt.Errorf("session must be empty or opencode")
		}
	}
	disabled := false
	if f.Disabled != nil {
		disabled = *f.Disabled
	}
	return &config.Provider{
		Name:               f.Name,
		Prefix:             prefix,
		Wire:               f.Wire,
		BaseURL:            f.BaseURL,
		Auth:               config.AuthConf{Type: "static", Keys: f.Keys, KeyLabels: f.KeyLabels},
		Models:             f.Models,
		Session:            session,
		Preset:             f.Preset,
		Disabled:           disabled,
		DispatchIntervalMS: f.DispatchIntervalMS,
		AdaptiveThinking:   f.AdaptiveThinking,
		InjectCacheControl: f.InjectCacheControl,
	}, nil
}

func oauthAccountStates(p *config.Provider, pool *auth.OAuthPool) []map[string]any {
	states := pool.States(p.Name)
	out := make([]map[string]any, 0, len(p.Auth.OAuth))
	for _, a := range p.Auth.OAuth {
		st := states[a.Name] // zero value when the pool has no live state
		state := "seed"      // configured but never refreshed/used
		switch {
		case a.Disabled:
			state = "disabled"
		case st.Cooling:
			state = "cooldown"
		case st.Fresh:
			state = "ok"
		case st.LastError != "":
			state = "error"
		}
		out = append(out, map[string]any{
			"name": a.Name, "kind": a.Kind, "disabled": a.Disabled,
			"expires_at": a.ExpiresAt, "state": state, "last_error": st.LastError,
		})
	}
	return out
}

// serveLive pushes active requests + rolling counters as SSE.
func (s *Server) serveLive(w http.ResponseWriter, r *http.Request) {
	f, _ := w.(http.Flusher)
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Accel-Buffering", "no")
	tick := time.NewTicker(time.Second)
	defer tick.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case <-tick.C:
			inflight, total := s.Proxy.Stats()
			payload, _ := json.Marshal(map[string]any{
				"active":   s.Proxy.Active.List(),
				"inflight": inflight, "total": total,
			})
			fmt.Fprintf(w, "data: %s\n\n", payload)
			if f != nil {
				f.Flush()
			}
		}
	}
}

// Reload re-reads the config file and hot-swaps registry + keys.
func (s *Server) Reload() {
	cfg, err := config.Load(s.ConfigPath)
	if err != nil {
		fmt.Printf("reload failed: %v\n", err)
		return
	}
	s.Proxy.Reg.Reload(cfg)
	s.Keys.Replace(cfg.Keys)
	s.Proxy.Pool.RegisterConfig(cfg)
	fmt.Printf("config reloaded (%d providers, %d keys)\n", len(cfg.Providers), len(cfg.Keys))
}

// ---- static dashboard ----

func (s *Server) handleStatic(w http.ResponseWriter, r *http.Request) {
	path := strings.TrimPrefix(r.URL.Path, "/")
	if path == "" {
		path = "index.html"
	}
	b, err := web.Dist.ReadFile("dist/" + path)
	if err != nil {
		// SPA fallback
		b, err = web.Dist.ReadFile("dist/index.html")
		if err != nil {
			http.Error(w, "dashboard not built (run: npm --prefix web run build)", 404)
			return
		}
		path = "index.html"
	}
	switch {
	case strings.HasSuffix(path, ".js"):
		w.Header().Set("Content-Type", "text/javascript")
	case strings.HasSuffix(path, ".css"):
		w.Header().Set("Content-Type", "text/css")
	case strings.HasSuffix(path, ".html"):
		w.Header().Set("Content-Type", "text/html")
	case strings.HasSuffix(path, ".svg"):
		w.Header().Set("Content-Type", "image/svg+xml")
	}
	w.Header().Set("Cache-Control", "no-cache")
	w.Write(b)
}
