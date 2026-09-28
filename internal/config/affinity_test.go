package config

import (
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// affinity round-trips from yaml, defaults to disabled with built-in
// ttl/header fallbacks, and validates ttl_s / header sanity. Disabled or
// absent affinity must never make a valid config invalid (zero behavior
// change when the feature is off).
func TestAffinityConfig(t *testing.T) {
	base := `
providers:
  - name: a
    base_url: https://a.example/v1
    wire: openai
    auth: {type: static, keys: [k]}
routes:
  - {match: m, chain: [a]}
`
	t.Run("absent = disabled, defaults via Effective*", func(t *testing.T) {
		var cfg Config
		if err := yaml.Unmarshal([]byte(base), &cfg); err != nil {
			t.Fatal(err)
		}
		if cfg.Routing.Affinity != nil {
			t.Fatalf("absent affinity must stay nil, got %+v", cfg.Routing.Affinity)
		}
		cfg.Defaults()
		if err := cfg.Validate(); err != nil {
			t.Fatal(err)
		}
		if cfg.Routing.Affinity != nil {
			t.Fatalf("Defaults must not invent affinity, got %+v", cfg.Routing.Affinity)
		}
		if cfg.Routing.Affinity.EffectiveTTL() != DefaultAffinityTTLS {
			t.Fatalf("EffectiveTTL = %d, want %d", cfg.Routing.Affinity.EffectiveTTL(), DefaultAffinityTTLS)
		}
		if cfg.Routing.Affinity.EffectiveHeader() != DefaultAffinityHeader {
			t.Fatalf("EffectiveHeader = %q, want %q", cfg.Routing.Affinity.EffectiveHeader(), DefaultAffinityHeader)
		}
	})

	t.Run("yaml round-trip with overrides", func(t *testing.T) {
		var cfg Config
		if err := yaml.Unmarshal([]byte(base+`
routing:
  affinity:
    enabled: true
    ttl_s: 60
    header: x-my-session
`), &cfg); err != nil {
			t.Fatal(err)
		}
		if err := cfg.Validate(); err != nil {
			t.Fatal(err)
		}
		a := cfg.Routing.Affinity
		if a == nil || !a.Enabled || a.TTLS != 60 || a.Header != "x-my-session" {
			t.Fatalf("affinity not parsed: %+v", a)
		}
		if a.EffectiveTTL() != 60 || a.EffectiveHeader() != "x-my-session" {
			t.Fatalf("Effective* must honor explicit config: %+v", a)
		}
	})

	t.Run("ttl_s 0 falls back to default", func(t *testing.T) {
		var cfg Config
		if err := yaml.Unmarshal([]byte(base+`
routing:
  affinity: {enabled: true, ttl_s: 0}
`), &cfg); err != nil {
			t.Fatal(err)
		}
		if err := cfg.Validate(); err != nil {
			t.Fatalf("ttl_s 0 is the unset marker, must validate: %v", err)
		}
		if got := cfg.Routing.Affinity.EffectiveTTL(); got != DefaultAffinityTTLS {
			t.Fatalf("EffectiveTTL = %d, want %d", got, DefaultAffinityTTLS)
		}
	})

	cases := []struct {
		name    string
		yaml    string
		wantErr string
	}{
		{"negative ttl rejected", `routing:
  affinity: {enabled: true, ttl_s: -5}`, "ttl_s must be >= 0"},
		{"negative ttl rejected even when disabled", `routing:
  affinity: {enabled: false, ttl_s: -1}`, "ttl_s must be >= 0"},
		{"whitespace-padded header rejected", `routing:
  affinity: {enabled: true, header: " x-sess"}`, "header must not have leading"},
		{"empty header ok (built-in default)", `routing:
  affinity: {enabled: true, header: ""}`, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var cfg Config
			if err := yaml.Unmarshal([]byte(base+"\n"+tc.yaml), &cfg); err != nil {
				t.Fatal(err)
			}
			err := cfg.Validate()
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("want valid, got: %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("want error %q, got %v", tc.wantErr, err)
			}
		})
	}
}
