// Package config loads and validates the yardmaster YAML config.
package config

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"strings"

	"gopkg.in/yaml.v3"
)

type Config struct {
	Listen        string           `yaml:"listen"`
	AdminPassword string           `yaml:"admin_password"`
	DBPath        string           `yaml:"db_path"`
	Providers     []*Provider      `yaml:"providers"`
	Routes        []*Route         `yaml:"routes"`
	Keys          []*Key           `yaml:"keys"`
	Costs         map[string]*Cost `yaml:"costs"` // $ per 1M tokens, overrides defaults
}

type Provider struct {
	Name               string            `yaml:"name"`
	Prefix             string            `yaml:"prefix"` // short routing prefix; models exposed as "prefix/model"
	BaseURL            string            `yaml:"base_url"`
	Wire               string            `yaml:"wire"`    // "openai" | "anthropic"
	Session            string            `yaml:"session"` // "opencode" adds x-opencode-session/client headers
	Preset             string            `yaml:"preset"`  // registry id this provider was created from (e.g. "opencode-go"); "" = custom
	Disabled           bool              `yaml:"disabled"`
	Auth               AuthConf          `yaml:"auth"`
	Models             []string          `yaml:"models"`               // upstream models this provider serves; empty = any
	ModelMap           map[string]string `yaml:"model_map"`            // requested model -> upstream model id
	DispatchIntervalMS int               `yaml:"dispatch_interval_ms"` // per-key min interval between request starts; 0 = unthrottled
	ExtraHeaders       map[string]string `yaml:"extra_headers"`
	BodyOverrides      map[string]any    `yaml:"body_overrides"`
	AdaptiveThinking   bool              `yaml:"adaptive_thinking"`    // zai-style thinking:{type:adaptive}+output_config.effort
	InjectCacheControl bool              `yaml:"inject_cache_control"` // add ephemeral markers when translating to anthropic wire
	HeadersTimeoutS    int               `yaml:"headers_timeout_s"`    // max wait for upstream response headers (default 300)
}

type AuthConf struct {
	Type      string       `yaml:"type"` // static | oauth
	Keys      []string     `yaml:"keys"`
	KeyLabels []string     `yaml:"key_labels"` // optional, index-aligned with Keys
	OAuth     []*OAuthAcct `yaml:"oauth_accounts"`
}

// KeyLabel returns the display label for key i (default "Key N").
func (a *AuthConf) KeyLabel(i int) string {
	if i < len(a.KeyLabels) && a.KeyLabels[i] != "" {
		return a.KeyLabels[i]
	}
	return fmt.Sprintf("Key %d", i+1)
}

type OAuthAcct struct {
	Name          string `yaml:"name"`
	Kind          string `yaml:"kind"` // claude-code | codex | opencode | custom
	RefreshTok    string `yaml:"refresh_token"`
	AccessTok     string `yaml:"access_token"`
	ExpiresAt     int64  `yaml:"expires_at"`     // unix seconds
	AccountID     string `yaml:"account_id"`     // codex: chatgpt-account-id header
	TokenEndpoint string `yaml:"token_endpoint"` // overrides the kind default
	ClientID      string `yaml:"client_id"`      // overrides the kind default
	Disabled      bool   `yaml:"disabled"`
}

type Route struct {
	Match string   `yaml:"match"` // exact model id or "prefix*"
	Chain []string `yaml:"chain"` // provider names, tried in order
}

type Key struct {
	Key   string   `yaml:"key"`
	Name  string   `yaml:"name"`
	Allow []string `yaml:"allow"` // route/model patterns; ["*"] = all
	RPM   int      `yaml:"rpm"`   // inbound requests-per-minute limit; 0 = unlimited
}

// AuthKindDefaults returns the baked-in OAuth token endpoint + client_id for
// known account kinds. ok=false for kinds without verified public defaults
// (e.g. "opencode") — those must set token_endpoint + client_id in config.
func AuthKindDefaults(kind string) (endpoint, clientID string, ok bool) {
	switch kind {
	case "claude-code":
		return "https://platform.claude.com/v1/oauth/token",
			"9d1c250a-e61b-44d9-88ed-5944d1962f5e", true
	case "codex":
		return "https://auth.openai.com/oauth/token",
			"app_EMoamEEZ73f0CkXaXp7hrann", true
	}
	return "", "", false
}

// ValidPrefix reports whether s is an acceptable routing prefix.
func ValidPrefix(s string) bool {
	if len(s) == 0 || len(s) > 12 {
		return false
	}
	for _, c := range s {
		if !(c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '-') {
			return false
		}
	}
	return true
}

// Cost is USD per 1M tokens.
type Cost struct {
	Input      float64 `yaml:"input"`
	Output     float64 `yaml:"output"`
	CacheRead  float64 `yaml:"cache_read"`
	CacheWrite float64 `yaml:"cache_write"`
}

// DefaultCosts are rough published-rate estimates ($/Mtok); config `costs` overrides.
// ponytail: coarse estimates on purpose — override per model in config for exact billing.
var DefaultCosts = map[string]*Cost{
	"deepseek":         {Input: 0.30, Output: 1.20, CacheRead: 0.03},
	"deepseek-flash":   {Input: 0.30, Output: 1.20, CacheRead: 0.03},
	"deepseek-v4-pro":  {Input: 0.60, Output: 2.20, CacheRead: 0.06},
	"glm-5.3":          {Input: 1.00, Output: 3.20, CacheRead: 0.10},
	"glm-5.3-flash":    {Input: 0.30, Output: 1.00, CacheRead: 0.03},
	"glm-5.2":          {Input: 0.60, Output: 2.20, CacheRead: 0.06},
	"claude-sonnet-5":  {Input: 3.00, Output: 15.00, CacheRead: 0.30, CacheWrite: 3.75},
	"claude-opus-5":    {Input: 15.00, Output: 75.00, CacheRead: 1.50, CacheWrite: 18.75},
	"claude-haiku-4.5": {Input: 1.00, Output: 5.00, CacheRead: 0.10},
	"gpt-5.5":          {Input: 1.25, Output: 10.00, CacheRead: 0.12},
	"kimi-k3":          {Input: 0.60, Output: 2.50, CacheRead: 0.06},
	"minimax-m3":       {Input: 0.30, Output: 1.20, CacheRead: 0.03},
	"qwen3.7-max":      {Input: 0.60, Output: 2.40, CacheRead: 0.06},
	"grok-4.5":         {Input: 3.00, Output: 15.00, CacheRead: 0.30},
}

func Load(path string) (*Config, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var c Config
	if err := yaml.Unmarshal(b, &c); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	c.Defaults()
	if err := c.Validate(); err != nil {
		return nil, err
	}
	return &c, nil
}

func (c *Config) Defaults() {
	if c.Listen == "" {
		c.Listen = ":8787"
	}
	if c.DBPath == "" {
		c.DBPath = "yardmaster.db"
	}
	if c.AdminPassword == "" {
		c.AdminPassword = "admin"
	}
}

func (c *Config) Validate() error {
	names := map[string]bool{}
	for _, p := range c.Providers {
		if p.Name == "" {
			return fmt.Errorf("provider missing name")
		}
		if names[p.Name] {
			return fmt.Errorf("duplicate provider %q", p.Name)
		}
		names[p.Name] = true
		// prefixes may be shared across providers (one preset = several wire
		// entries over one gateway, e.g. Zen) — only the format is validated
		if p.Prefix != "" && !ValidPrefix(p.Prefix) {
			return fmt.Errorf("provider %s: prefix must be 1-12 lowercase letters, digits or hyphens", p.Name)
		}
		if p.BaseURL == "" {
			return fmt.Errorf("provider %s: missing base_url", p.Name)
		}
		if p.Wire != "openai" && p.Wire != "anthropic" && p.Wire != "responses" {
			return fmt.Errorf("provider %s: wire must be openai|anthropic|responses", p.Name)
		}
		if p.Auth.Type == "" {
			p.Auth.Type = "static"
		}
		if p.Auth.Type != "static" && p.Auth.Type != "oauth" {
			return fmt.Errorf("provider %s: auth.type must be static|oauth", p.Name)
		}
		if p.HeadersTimeoutS == 0 {
			p.HeadersTimeoutS = 300
		}
		if p.Wire == "responses" && p.Auth.Type == "" {
			p.Auth.Type = "static"
		}
		for i, a := range p.Auth.OAuth {
			if a.Name == "" {
				return fmt.Errorf("provider %s: oauth account %d missing name", p.Name, i)
			}
			if a.RefreshTok == "" && a.AccessTok == "" {
				return fmt.Errorf("provider %s: account %s needs refresh_token (or access_token as seed)", p.Name, a.Name)
			}
			if a.TokenEndpoint == "" || a.ClientID == "" {
				ep, cid, ok := AuthKindDefaults(a.Kind)
				if !ok {
					return fmt.Errorf("provider %s: account %s: unknown kind %q — set token_endpoint + client_id, or use kind claude-code|codex", p.Name, a.Name, a.Kind)
				}
				if a.TokenEndpoint == "" {
					a.TokenEndpoint = ep
				}
				if a.ClientID == "" {
					a.ClientID = cid
				}
			}
		}
	}
	pnames := names
	for _, r := range c.Routes {
		if r.Match == "" {
			return fmt.Errorf("route missing match")
		}
		for _, n := range r.Chain {
			if !pnames[n] {
				return fmt.Errorf("route %q references unknown provider %q", r.Match, n)
			}
		}
	}
	seen := map[string]bool{}
	for _, k := range c.Keys {
		if k.Key == "" {
			return fmt.Errorf("key entry missing key")
		}
		if seen[k.Key] {
			return fmt.Errorf("duplicate key ...%s", tail(k.Key, 4))
		}
		seen[k.Key] = true
		if len(k.Allow) == 0 {
			k.Allow = []string{"*"}
		}
	}
	return nil
}

// CostFor returns the effective cost for a model (exact, then family prefix, then defaults).
func (c *Config) CostFor(model string) Cost {
	if v, ok := c.Costs[model]; ok {
		return *v
	}
	base := model
	if i := strings.IndexByte(base, '/'); i >= 0 {
		base = base[i+1:]
	}
	if v, ok := c.Costs[base]; ok {
		return *v
	}
	if v, ok := DefaultCosts[model]; ok {
		return *v
	}
	if v, ok := DefaultCosts[base]; ok {
		return *v
	}
	for prefix, v := range DefaultCosts {
		if strings.HasPrefix(base, prefix) {
			return *v
		}
	}
	return Cost{}
}

func tail(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[len(s)-n:]
}

// GenKey generates an ar- prefixed API key for CLI agents.
func GenKey() string {
	b := make([]byte, 24)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return "ar-" + hex.EncodeToString(b)
}

// Save writes the config back as YAML (atomic replace). Dashboard edits
// rewrite the file; comments in hand-written YAML are not preserved.
func (c *Config) Save(path string) error {
	b, err := yaml.Marshal(c)
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
