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
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"agent-router/internal/auth"
	"agent-router/internal/config"
	"agent-router/internal/proxy"
	"agent-router/internal/store"
	"agent-router/internal/translate"
	"agent-router/web"
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
	type model struct {
		ID     string `json:"id"`
		Object string `json:"object"`
		Owned  string `json:"owned_by"`
	}
	var data []model
	seen := map[string]bool{}
	for _, m := range s.Proxy.Reg.Models() {
		if !seen[m] {
			seen[m] = true
			data = append(data, model{ID: m, Object: "model", Owned: "agent-router"})
		}
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{"object": "list", "data": data})
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
			out = append(out, map[string]any{
				"name": p.Name, "wire": p.Wire, "base_url": p.BaseURL,
				"models": nonNil(p.Models), "dispatch_interval_ms": p.DispatchIntervalMS,
				"auth_type": p.Auth.Type,
				"accounts":  oauthAccountStates(p),
			})
		}
		writeJSON(map[string]any{"providers": out})
	case path == "live" && r.Method == "GET":
		s.serveLive(w, r)
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
				"num_keys":    len(p.Auth.Keys), "num_accounts": len(p.Auth.OAuth),
				"session":     p.Session,
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

func oauthAccountStates(p *config.Provider) []map[string]any {
	out := make([]map[string]any, 0, len(p.Auth.OAuth))
	for _, a := range p.Auth.OAuth {
		out = append(out, map[string]any{
			"name": a.Name, "kind": a.Kind, "disabled": a.Disabled,
			"expires_at": a.ExpiresAt,
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
