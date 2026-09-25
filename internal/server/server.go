// Package server wires HTTP handlers: OpenAI + Anthropic endpoints, admin API,
// and the embedded dashboard.
package server

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
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
	"github.com/bacnh85/yardmaster/internal/provider"
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
	// System One decision wire: /v1/systemone canonical (TypeSafe-SDK and
	// OpenRouter-SystemOne compatible via TYPESAFE_BASE_URL=https://<host>/v1);
	// /v1/decisions + /v1/classifier are aliases of the same handler.
	mux.HandleFunc("POST /v1/systemone", s.wrap(s.Proxy.ServeClassifier))
	mux.HandleFunc("POST /v1/decisions", s.wrap(s.Proxy.ServeClassifier))
	mux.HandleFunc("POST /v1/classifier", s.wrap(s.Proxy.ServeClassifier))
	mux.HandleFunc("GET /v1/models", s.wrap(s.handleModels))
	mux.HandleFunc("GET /v1/systemone/models", s.wrap(s.handleSystemoneModels))
	mux.HandleFunc("GET /v1/usage", s.wrap(s.handleUsage))
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
		k, ok := s.authenticate(r)
		if !ok {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(401)
			json.NewEncoder(w).Encode(map[string]any{"error": map[string]any{
				"message": "invalid or missing API key", "type": "authentication_error"}})
			return
		}
		ctx := r.Context()
		ctx = proxy.WithStartTime(ctx, start)
		ctx = proxy.WithInboundKey(ctx, k.Name)
		ctx = proxy.WithInboundAllow(ctx, k.Allow)
		ctx = context.WithValue(ctx, inboundKeyCtx{}, k) // full key config for e.g. GET /v1/usage
		h(w, r.WithContext(ctx))
	}
}

type inboundKeyCtx struct{}

// inboundKey returns the authenticated *config.Key from wrap's context.
func inboundKey(r *http.Request) *config.Key {
	k, _ := r.Context().Value(inboundKeyCtx{}).(*config.Key)
	return k
}

func (s *Server) authenticate(r *http.Request) (*config.Key, bool) {
	key := ""
	if ah := r.Header.Get("Authorization"); strings.HasPrefix(ah, "Bearer ") {
		key = strings.TrimPrefix(ah, "Bearer ")
	}
	if key == "" {
		key = r.Header.Get("x-api-key")
	}
	if key == "" {
		return nil, false
	}
	return s.Keys.Check(key)
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

// handleSystemoneModels lists decision-model ids (classifier-wire providers)
// as /v1/systemone resolves them. /v1/models deliberately excludes them —
// chat advertisement must stay clean; this is the discovery path for System
// One clients (pi-classifier model picker).
func (s *Server) handleSystemoneModels(w http.ResponseWriter, r *http.Request) {
	type model struct {
		ID     string `json:"id"`
		Object string `json:"object"`
		Owned  string `json:"owned_by"`
		Family string `json:"family"`
	}
	data := make([]model, 0) // never nil — empty registry must marshal as [], not null
	seen := map[string]bool{}
	for _, p := range s.Proxy.Reg.Config().Providers {
		if p.Disabled || p.Wire != proxy.WireClassifier {
			continue
		}
		for _, m := range p.Models {
			// inlined prefix rule — advertised() is unexported in internal/provider
			id := m
			if p.Prefix != "" {
				id = p.Prefix + "/" + m
			}
			if seen[id] {
				continue
			}
			seen[id] = true
			data = append(data, model{ID: id, Object: "model", Owned: p.Name, Family: "classifier"})
		}
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
// canon-id → meta map, plus each provider's curated ids the catalog omits
// (manually added models) enriched from the ALREADY-LOADED models.dev snapshot.
// Cache-hit only on both counts — this runs on the agent-facing /v1/models
// path, so it must never fetch upstream OR models.dev (modelsDevCached, not
// modelsDevSnapshot: the latter would retry a 15s fetch per request when
// models.dev is down, serialized behind modelsDevMu).
func cachedCatalogMetas(providers []*config.Provider) map[string]ModelMeta {
	out := map[string]ModelMeta{}
	dev, devFlat := modelsDevCached()
	for _, p := range providers {
		e, ok := catalogCache.Load(p.BaseURL)
		var cached []ModelMeta
		if ok {
			cached = e.(catalogEntry).models
			for _, mm := range cached {
				k := canonModelID(mm.ID)
				if _, dup := out[k]; !dup {
					out[k] = mm
				}
			}
		}
		// curated ids the catalog omits — same enrichment the detail page gets,
		// so /v1/models advertises context/capabilities for manual models too
		for _, mm := range curatedMetas(p, cached, dev, devFlat) {
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
		s.cacheSaved(sum, time.Duration(hours)*time.Hour)
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
	case path == "timeseries" && r.Method == "GET":
		by := q.Get("by")
		if by != "model" {
			http.Error(w, "by must be model", 400)
		} else if hours, _ := strconv.Atoi(q.Get("hours")); hours <= 0 {
			http.Error(w, "hours must be > 0", 400)
		} else if rows, err := s.Store.TimeSeriesByModel(time.Duration(hours)*time.Hour, q.Get("bucket")); err != nil {
			http.Error(w, err.Error(), 500)
		} else {
			// ponytail: payload is buckets×models — small, no pagination
			writeJSON(map[string]any{"series": rows})
		}
	case path == "keys" && r.Method == "GET":
		lastUsed, err := s.Store.KeyLastUsed() // nil store → empty map
		if err != nil {
			http.Error(w, err.Error(), 500)
			return
		}
		out := make([]map[string]any, 0, len(s.Proxy.Reg.Config().Keys))
		for _, k := range s.Proxy.Reg.Config().Keys {
			out = append(out, map[string]any{
				"name": k.Name, "key_suffix": suffix(k.Key), "allow": k.Allow, "rpm": k.RPM,
				"usage": k.Usage, // nil = allowed (default ON)
				"id":    keyID(k.Key),
				// unix ms, same unit as last_used so the UI renders both with one formatter
				"created_at": k.CreatedAt,
				"last_used":  lastUsed[k.Name],
			})
		}
		writeJSON(map[string]any{"keys": out})
	case path == "providers" && r.Method == "GET":
		out := make([]map[string]any, 0, len(s.Proxy.Reg.Config().Providers))
		for _, p := range s.Proxy.Reg.Config().Providers {
			conns := make([]map[string]any, 0, len(p.Auth.Keys))
			for i, k := range p.Auth.Keys {
				conns = append(conns, map[string]any{"label": p.Auth.KeyLabel(i), "suffix": suffix(k),
					"disabled": i < len(p.Auth.KeyDisabled) && p.Auth.KeyDisabled[i]})
			}
			out = append(out, map[string]any{
				"name": p.Name, "wire": p.Wire, "base_url": p.BaseURL,
				"models": nonNil(p.Models), "dispatch_interval_ms": p.DispatchIntervalMS,
				"prefix":            p.Prefix,
				"preset":            p.Preset,
				"disabled":          p.Disabled,
				"session":           p.Session,
				"rotation":          p.Rotation,
				"subscription":      p.Subscription,
				"auth_type":         p.Auth.Type,
				"adaptive_thinking": p.AdaptiveThinking, "inject_cache_control": p.InjectCacheControl,
				"zcode_signing": p.ZcodeSigning,
				"extra_headers": p.ExtraHeaders, "body_overrides": p.BodyOverrides,
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
			Usage *bool    `json:"usage"`
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
			c.Keys = append(c.Keys, &config.Key{
				Key: raw, Name: req.Name, Allow: allow, RPM: req.RPM, Usage: req.Usage,
				CreatedAt: time.Now().UnixMilli(), // unix ms, same unit as request ts
			})
			return nil
		}) {
			return
		}
		// the raw key is shown exactly once, in this response
		writeJSON(map[string]any{"ok": true, "key": raw})
	case strings.HasPrefix(path, "keys/") && r.Method == "PUT":
		// edit a key: {name?, allow?, rpm?, usage?, key?} — nil pointers keep
		// stored values; a non-empty key rotates the secret in place
		name := strings.TrimPrefix(path, "keys/")
		var req struct {
			Name  *string   `json:"name"`
			Allow *[]string `json:"allow"`
			RPM   *int      `json:"rpm"`
			Usage *bool     `json:"usage"`
			Key   *string   `json:"key"`
		}
		if json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&req) != nil {
			http.Error(w, "bad json", 400)
			return
		}
		if !s.mutate(w, func(c *config.Config) error {
			for _, k := range c.Keys {
				if k.Name != name {
					continue
				}
				if req.Name != nil && *req.Name != "" && *req.Name != name {
					for _, other := range c.Keys {
						if other.Name == *req.Name {
							return fmt.Errorf("key %q already exists", *req.Name)
						}
					}
					k.Name = *req.Name
				}
				if req.Allow != nil {
					k.Allow = *req.Allow
					if len(k.Allow) == 0 {
						k.Allow = []string{"*"}
					}
				}
				if req.RPM != nil {
					k.RPM = *req.RPM
				}
				if req.Usage != nil {
					k.Usage = req.Usage
				}
				if req.Key != nil && *req.Key != "" {
					k.Key = *req.Key // rotation; empty string keeps the stored secret
					k.CreatedAt = time.Now().UnixMilli() // the new credential starts its own clock
				}
				return nil
			}
			return fmt.Errorf("no key named %q", name)
		}) {
			return
		}
		// history follows the key: without this, renaming blanks "last used"
		// and splits Usage into two buckets. Best-effort — a DB hiccup must
		// not fail an otherwise-saved config edit.
		if req.Name != nil && *req.Name != "" && *req.Name != name {
			if err := s.Store.RenameKey(name, *req.Name); err != nil {
				fmt.Printf("keys: re-attribute history %q → %q: %v\n", name, *req.Name, err)
			}
		}
		writeJSON(map[string]any{"ok": true})
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
			KeyIndex  int    `json:"key_index"`
			Messages  []struct {
				Role    string `json:"role"`
				Content string `json:"content"`
			} `json:"messages"`
		}
		if json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&req) != nil ||
			req.Provider == "" || req.Model == "" || (req.Prompt == "" && len(req.Messages) == 0) {
			http.Error(w, "provider, model and prompt (or messages) are required", 400)
			return
		}
		// 400 (not Probe's 502) when the key index doesn't exist — the client
		// picked the key, so it's a bad request, not an upstream failure.
		// Omitted key_index (0) hits the first key like before; negative → first.
		if pv := providerByName(s.Proxy.Reg.Config().Providers, req.Provider); pv != nil {
			if pv.Wire == proxy.WireClassifier {
				http.Error(w, "classifier models are decision models — they don't answer chat probes", 400)
				return
			}
			if len(pv.Auth.Keys) > 0 && req.KeyIndex >= len(pv.Auth.Keys) {
				http.Error(w, "key index out of range", 400)
				return
			}
		}
		msgs := make([]proxy.ChatMessage, len(req.Messages))
		for i, m := range req.Messages {
			msgs[i] = proxy.ChatMessage{Role: m.Role, Content: m.Content}
		}
		if len(msgs) == 0 {
			msgs = []proxy.ChatMessage{{Role: "user", Content: req.Prompt}}
		}
		res, err := s.Proxy.Probe(r.Context(), req.Provider, req.Model, msgs, req.MaxTokens, req.KeyIndex)
		if err != nil {
			http.Error(w, err.Error(), 502)
			return
		}
		writeJSON(res)
	case path == "catalog" && r.Method == "GET":
		// aggregate model catalog across every enabled provider, deduped by
		// canonical id; exposed=1 keeps only models some provider serves
		writeJSON(map[string]any{"models": s.catalogRows(r.Context(), q.Get("exposed") == "1")})
	case strings.HasPrefix(path, "providers/") && strings.HasSuffix(path, "/models") && r.Method == "GET":
		name := strings.TrimSuffix(strings.TrimPrefix(path, "providers/"), "/models")
		// ?refresh=1 busts the base_url catalog cache — the dashboard refresh
		// button must fetch upstream, not re-serve the 1h-cached entry
		models, err := s.catalog(r.Context(), name, q.Get("refresh") == "1")
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
		invalidateQuotaReport()
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
					// ...and the per-connection disabled flags
					if idx < len(x.Auth.KeyDisabled) {
						x.Auth.KeyDisabled = append(x.Auth.KeyDisabled[:idx], x.Auth.KeyDisabled[idx+1:]...)
					}
					return nil
				}
			}
			return fmt.Errorf("no provider named %q", name)
		}) {
			return
		}
		invalidateQuotaReport()
		writeJSON(map[string]any{"ok": true})
	case strings.HasPrefix(path, "providers/") && strings.Contains(path, "/keys/") && r.Method == "PUT":
		// edit one stored connection: {label?, key?} — nil/empty keeps. Index-
		// addressed like DELETE (duplicate suffixes must target the right key).
		rest := strings.TrimPrefix(path, "providers/")
		slash := strings.Index(rest, "/keys/")
		name, idxStr := rest[:slash], rest[slash+len("/keys/"):]
		idx, err := strconv.Atoi(idxStr)
		if err != nil || idx < 0 {
			http.Error(w, "key index must be an integer", 400)
			return
		}
		var req struct {
			Key      *string `json:"key"`
			Label    *string `json:"label"`
			Disabled *bool   `json:"disabled"` // nil = keep stored per-connection enable state
		}
		if json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&req) != nil {
			http.Error(w, "bad json", 400)
			return
		}
		if !s.mutate(w, func(c *config.Config) error {
			for _, x := range c.Providers {
				if x.Name == name {
					if idx >= len(x.Auth.Keys) {
						return fmt.Errorf("key index %d out of range (%d keys on %q)", idx, len(x.Auth.Keys), name)
					}
					if req.Key != nil && *req.Key != "" {
						for i, k := range x.Auth.Keys {
							if k == *req.Key && i != idx {
								return fmt.Errorf("key already present")
							}
						}
						x.Auth.Keys[idx] = *req.Key
					}
					if req.Label != nil {
						for len(x.Auth.KeyLabels) < idx+1 {
							x.Auth.KeyLabels = append(x.Auth.KeyLabels, "")
						}
						x.Auth.KeyLabels[idx] = *req.Label
					}
					if req.Disabled != nil {
						for len(x.Auth.KeyDisabled) < idx+1 {
							x.Auth.KeyDisabled = append(x.Auth.KeyDisabled, false)
						}
						x.Auth.KeyDisabled[idx] = *req.Disabled
					}
					return nil
				}
			}
			return fmt.Errorf("no provider named %q", name)
		}) {
			return
		}
		invalidateQuotaReport()
		writeJSON(map[string]any{"ok": true})
	case strings.HasPrefix(path, "providers/") && strings.HasSuffix(path, "/subscription") && r.Method == "PUT":
		// change the plan tier of one provider entry: {plan} — "" clears.
		// Dedicated endpoint so the dashboard never has to round-trip the full
		// providerForm (which would risk clobbering advanced fields).
		name := strings.TrimSuffix(strings.TrimPrefix(path, "providers/"), "/subscription")
		var req struct {
			Plan string `json:"plan"`
		}
		if json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&req) != nil {
			http.Error(w, "bad json", 400)
			return
		}
		plan := strings.ToLower(strings.TrimSpace(req.Plan))
		if !config.ValidSubscription(plan) {
			http.Error(w, "plan must be empty, goat, pro, max, or free", 400)
			return
		}
		if !s.mutate(w, func(c *config.Config) error {
			for _, x := range c.Providers {
				if x.Name == name {
					x.Subscription = plan
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
			if f.KeyDisabled == nil {
				p.Auth.KeyDisabled = x.Auth.KeyDisabled // omitted (model toggles etc.) keeps per-connection flags
			}
					if f.Prefix == nil {
						p.Prefix = x.Prefix // omitted field keeps the stored prefix
					}
					if f.Session == nil {
						p.Session = x.Session // omitted field keeps the stored session headers
					}
					if f.Rotation == nil {
						p.Rotation = x.Rotation // omitted field keeps the stored rotation
					}
					if f.Subscription == nil {
						p.Subscription = x.Subscription // omitted field keeps the stored subscription
					}
					if f.Preset == "" {
						p.Preset = x.Preset
					}
					if f.Disabled == nil {
						p.Disabled = x.Disabled // omitted field keeps the stored state
					} else {
						p.Disabled = *f.Disabled // registry toggle sends it explicitly
					}
					if f.ExtraHeaders != nil {
						p.ExtraHeaders = f.ExtraHeaders // form can express these now
					} else {
						p.ExtraHeaders = x.ExtraHeaders // omitted keeps stored
					}
					if f.BodyOverrides != nil {
						p.BodyOverrides = f.BodyOverrides
					} else {
						p.BodyOverrides = x.BodyOverrides
					}
					if f.DispatchIntervalMS != nil {
						p.DispatchIntervalMS = *f.DispatchIntervalMS
					} else {
						p.DispatchIntervalMS = x.DispatchIntervalMS
					}
					if f.AdaptiveThinking != nil {
						p.AdaptiveThinking = *f.AdaptiveThinking
					} else {
						p.AdaptiveThinking = x.AdaptiveThinking
					}
					if f.InjectCacheControl != nil {
						p.InjectCacheControl = *f.InjectCacheControl
					} else {
						p.InjectCacheControl = x.InjectCacheControl
					}
					if f.ZcodeSigning != nil {
						p.ZcodeSigning = *f.ZcodeSigning
					} else {
						p.ZcodeSigning = x.ZcodeSigning
					}
					p.Auth.Type = x.Auth.Type // form is static-only; never downgrade oauth
					p.Auth.OAuth = x.Auth.OAuth
					p.ModelMap = x.ModelMap
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
			invalidateQuotaReport()
			// scrub routes that pointed at the removed provider — weights stay
			// index-aligned with the chain so a weighted-rr route survives
			routes := c.Routes[:0]
			for _, rt := range c.Routes {
				chain := rt.Chain[:0]
				weights := rt.Weights[:0]
				for i, n := range rt.Chain {
					if n != name {
						chain = append(chain, n)
						if i < len(rt.Weights) {
							weights = append(weights, rt.Weights[i])
						}
					}
				}
				if len(chain) > 0 {
					rt.Chain = chain
					if len(rt.Weights) > 0 {
						rt.Weights = weights
					}
					routes = append(routes, rt)
				}
			}
			c.Routes = routes
			// scrub combo members on the removed provider; combos are single-model
			// pools, so a member gone = combo gone when it was the last one
			combos := c.Combos[:0]
			for _, cb := range c.Combos {
				members := cb.Members[:0]
				for _, m := range cb.Members {
					if m.Provider != name {
						members = append(members, m)
					}
				}
				if len(members) > 0 {
					cb.Members = members
					combos = append(combos, cb)
				}
			}
			c.Combos = combos
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
				"zcode_signing":        p.ZcodeSigning,
			})
		}
		writeJSON(map[string]any{
			"listen": cfg.Listen, "db_path": cfg.DBPath,
			"providers": provs, "routes": cfg.Routes,
		})
	case path == "routes" && r.Method == "PUT":
		// replaces the whole routes list: [{match, chain, strategy, weights}]
		var routes []*config.Route
		if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&routes); err != nil {
			http.Error(w, "bad json", 400)
			return
		}
		if !s.mutate(w, func(c *config.Config) error {
			c.Routes = routes
			return nil
		}) {
			return
		}
		writeJSON(map[string]any{"ok": true})
	case path == "combos" && r.Method == "GET":
		out := make([]*config.Combo, 0, len(s.Proxy.Reg.Config().Combos))
		out = append(out, s.Proxy.Reg.Config().Combos...)
		writeJSON(map[string]any{"combos": out})
	case path == "combos" && r.Method == "PUT":
		// replaces the whole combos list: [{name, model, strategy, members}]
		var combos []*config.Combo
		if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&combos); err != nil {
			http.Error(w, "bad json", 400)
			return
		}
		if !s.mutate(w, func(c *config.Config) error {
			c.Combos = combos
			return nil
		}) {
			return
		}
		writeJSON(map[string]any{"ok": true})
	case path == "combos/usage" && r.Method == "GET":
		hours, _ := strconv.Atoi(q.Get("hours"))
		if hours <= 0 {
			hours = 24
		}
		rows, err := s.Store.ComboUsageSince(time.Duration(hours) * time.Hour)
		if err != nil {
			http.Error(w, err.Error(), 500)
			return
		}
		writeJSON(map[string]any{"usage": rows})
	case path == "cooldowns" && r.Method == "GET":
		var out []provider.CooldownEntry
		if s.Proxy.Cd != nil {
			out = s.Proxy.Cd.Snapshot()
		}
		if out == nil {
			out = []provider.CooldownEntry{}
		}
		writeJSON(out)
	case path == "routing" && r.Method == "PUT":
		// global routing defaults: {strategy, rotation}; "" = built-in default
		var in config.Routing
		if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&in); err != nil {
			http.Error(w, "bad json", 400)
			return
		}
		if !s.mutate(w, func(c *config.Config) error {
			c.Routing = in
			return nil
		}) {
			return
		}
		writeJSON(map[string]any{"ok": true})
	default:
		http.NotFound(w, r)
	}
}

// cacheSaved fills sum.CacheSavedUSD: cached input priced at the full input
// rate minus what the cache actually charged, per model, using the same
// config.Cost the proxy bills with. Estimate by definition — models without a
// price entry contribute nothing.
func (s *Server) cacheSaved(sum *store.Summary, d time.Duration) {
	if s.Proxy == nil || s.Proxy.Cost == nil {
		return
	}
	// light token-only query — BreakdownBy would run the O(n²) TTFT percentile
	// pass on every polled summary
	rows, err := s.Store.CacheTokensByModel(d)
	if err != nil {
		return
	}
	var saved float64
	for _, b := range rows {
		c := s.Proxy.Cost(b.Model)
		saved += float64(b.CacheRead)/1e6*max(0, c.Input-c.CacheRead) +
			float64(b.CacheWrite)/1e6*max(0, c.Input-c.CacheWrite)
	}
	sum.CacheSavedUSD = saved
}

func suffix(k string) string {
	if len(k) > 6 {
		return "…" + k[len(k)-6:]
	}
	return k
}

// keyID is a stable 32-hex reference handle for an inbound key, shown in the
// dashboard (never the key itself). Derived, not stored: rename keeps it, key
// rotation changes it — it identifies the credential instance.
func keyID(k string) string {
	h := sha256.Sum256([]byte(k))
	return hex.EncodeToString(h[:16])
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
	Name               string            `json:"name"`
	Wire               string            `json:"wire"`
	BaseURL            string            `json:"base_url"`
	Models             []string          `json:"models"`
	Keys               []string          `json:"keys"`
	KeyLabels          []string          `json:"keyLabels"`
	KeyDisabled        []bool            `json:"keyDisabled"` // index-aligned with Keys; nil = keep stored
	Prefix             *string           `json:"prefix"`   // nil = omitted (keep stored); "" = none; else routing prefix
	Session            *string           `json:"session"`  // nil = omitted (keep stored); "" = none; "opencode" = session headers
	Rotation           *string           `json:"rotation"` // nil = omitted (keep stored); first | round_robin
	Subscription       *string           `json:"subscription"` // nil = omitted (keep stored); "" = none; goat|pro|max|free
	Preset             string            `json:"preset"`
	Disabled           *bool             `json:"disabled"`             // nil = omitted (keep stored)
	DispatchIntervalMS *int              `json:"dispatch_interval_ms"` // nil = omitted (keep stored)
	AdaptiveThinking   *bool             `json:"adaptive_thinking"`    // nil = omitted (keep stored)
	InjectCacheControl *bool             `json:"inject_cache_control"` // nil = omitted (keep stored)
	ZcodeSigning       *bool             `json:"zcode_signing"`        // nil = omitted (keep stored)
	ExtraHeaders       map[string]string `json:"extra_headers"`        // nil = omitted (keep stored on update)
	BodyOverrides      map[string]any    `json:"body_overrides"`       // nil = omitted (keep stored on update)
}

func derefBool(p *bool) bool { return p != nil && *p }
func derefInt(p *int) int {
	if p == nil {
		return 0
	}
	return *p
}

func (f providerForm) provider() (*config.Provider, error) {
	if f.Wire != "openai" && f.Wire != "anthropic" && f.Wire != "responses" && f.Wire != "classifier" {
		return nil, fmt.Errorf("wire must be openai, anthropic, responses, or classifier")
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
	rotation := ""
	if f.Rotation != nil {
		rotation = *f.Rotation
		if rotation != "" && rotation != "first" && rotation != "round_robin" {
			return nil, fmt.Errorf("rotation must be first or round_robin")
		}
	}
	subscription := ""
	if f.Subscription != nil {
		subscription = strings.ToLower(strings.TrimSpace(*f.Subscription))
		if !config.ValidSubscription(subscription) {
			return nil, fmt.Errorf("subscription must be empty, goat, pro, max, or free")
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
		Auth:               config.AuthConf{Type: "static", Keys: f.Keys, KeyLabels: f.KeyLabels, KeyDisabled: f.KeyDisabled},
		Models:             f.Models,
		Session:            session,
		Preset:             f.Preset,
		Rotation:           rotation,
		Subscription:       subscription,
		Disabled:           disabled,
		DispatchIntervalMS: derefInt(f.DispatchIntervalMS),
		AdaptiveThinking:   derefBool(f.AdaptiveThinking),
		InjectCacheControl: derefBool(f.InjectCacheControl),
		ZcodeSigning:       derefBool(f.ZcodeSigning),
		ExtraHeaders:       f.ExtraHeaders,
		BodyOverrides:      f.BodyOverrides,
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
