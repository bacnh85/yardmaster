// Package config loads and validates the agent-router YAML config.
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
	BaseURL            string            `yaml:"base_url"`
	Wire               string            `yaml:"wire"` // "openai" | "anthropic"
	Auth               AuthConf          `yaml:"auth"`
	Models             []string          `yaml:"models"`    // upstream models this provider serves; empty = any
	ModelMap           map[string]string `yaml:"model_map"` // requested model -> upstream model id
	DispatchIntervalMS int               `yaml:"dispatch_interval_ms"` // per-key min interval between request starts; 0 = unthrottled
	ExtraHeaders       map[string]string `yaml:"extra_headers"`
	BodyOverrides      map[string]any    `yaml:"body_overrides"`
	AdaptiveThinking   bool              `yaml:"adaptive_thinking"` // zai-style thinking:{type:adaptive}+output_config.effort
	InjectCacheControl bool              `yaml:"inject_cache_control"` // add ephemeral markers when translating to anthropic wire
	HeadersTimeoutS    int               `yaml:"headers_timeout_s"`    // max wait for upstream response headers (default 300)
}

type AuthConf struct {
	Type  string       `yaml:"type"` // static | oauth
	Keys  []string     `yaml:"keys"`
	OAuth []*OAuthAcct `yaml:"oauth_accounts"`
}

type OAuthAcct struct {
	Name        string `yaml:"name"`
	Kind        string `yaml:"kind"` // claude-code | codex | opencode
	RefreshTok  string `yaml:"refresh_token"`
	AccessTok   string `yaml:"access_token"`
	ExpiresAt   int64  `yaml:"expires_at"` // unix seconds
	Disabled    bool   `yaml:"disabled"`
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
	"deepseek":            {Input: 0.30, Output: 1.20, CacheRead: 0.03},
	"deepseek-flash":      {Input: 0.30, Output: 1.20, CacheRead: 0.03},
	"deepseek-v4-pro":     {Input: 0.60, Output: 2.20, CacheRead: 0.06},
	"glm-5.3":             {Input: 1.00, Output: 3.20, CacheRead: 0.10},
	"glm-5.3-flash":       {Input: 0.30, Output: 1.00, CacheRead: 0.03},
	"glm-5.2":             {Input: 0.60, Output: 2.20, CacheRead: 0.06},
	"claude-sonnet-5":     {Input: 3.00, Output: 15.00, CacheRead: 0.30, CacheWrite: 3.75},
	"claude-opus-5":       {Input: 15.00, Output: 75.00, CacheRead: 1.50, CacheWrite: 18.75},
	"claude-haiku-4.5":    {Input: 1.00, Output: 5.00, CacheRead: 0.10},
	"gpt-5.5":             {Input: 1.25, Output: 10.00, CacheRead: 0.12},
	"kimi-k3":             {Input: 0.60, Output: 2.50, CacheRead: 0.06},
	"minimax-m3":          {Input: 0.30, Output: 1.20, CacheRead: 0.03},
	"qwen3.7-max":         {Input: 0.60, Output: 2.40, CacheRead: 0.06},
	"grok-4.5":            {Input: 3.00, Output: 15.00, CacheRead: 0.30},
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
		c.DBPath = "agent-router.db"
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
		if p.BaseURL == "" {
			return fmt.Errorf("provider %s: missing base_url", p.Name)
		}
		if p.Wire != "openai" && p.Wire != "anthropic" {
			return fmt.Errorf("provider %s: wire must be openai|anthropic", p.Name)
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
