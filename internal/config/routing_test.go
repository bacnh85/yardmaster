package config

import (
	"strings"
	"testing"
)

func routingCfg(mod func(*Config)) *Config {
	c := &Config{
		Providers: []*Provider{
			{Name: "a", Wire: "openai", BaseURL: "https://a", Auth: AuthConf{Keys: []string{"k"}}},
			{Name: "b", Wire: "openai", BaseURL: "https://b", Auth: AuthConf{Keys: []string{"k"}}},
		},
		Routes: []*Route{{Match: "m", Chain: []string{"a", "b"}}},
	}
	if mod != nil {
		mod(c)
	}
	return c
}

func TestRoutingValidation(t *testing.T) {
	cases := []struct {
		name    string
		mod     func(*Config)
		wantErr string // "" = valid
	}{
		{"default priority ok", nil, ""},
		{"empty chain rejected", func(c *Config) {
			c.Routes[0].Chain = nil
		}, "needs at least one provider"},
		{"weights on priority rejected", func(c *Config) {
			c.Routes[0].Weights = []int{3, 1}
		}, "weights require strategy weighted-rr"},
		{"inherit weights with global weighted-rr ok", func(c *Config) {
			c.Routing = Routing{Strategy: "weighted-rr"}
			c.Routes[0].Weights = []int{3, 1}
		}, ""},
		{"inherit weights without global weighted-rr rejected", func(c *Config) {
			c.Routes[0].Weights = []int{3, 1}
		}, "weights require strategy weighted-rr"},
		{"weighted-rr without weights ok (equal spread)", func(c *Config) {
			c.Routes[0].Strategy = "weighted-rr"
		}, ""},
		{"weighted-rr ok", func(c *Config) {
			c.Routes[0].Strategy = "weighted-rr"
			c.Routes[0].Weights = []int{3, 1}
		}, ""},
		{"weighted-rr weights mismatch", func(c *Config) {
			c.Routes[0].Strategy = "weighted-rr"
			c.Routes[0].Weights = []int{3}
		}, "must match chain length"},
		{"weighted-rr zero weight", func(c *Config) {
			c.Routes[0].Strategy = "weighted-rr"
			c.Routes[0].Weights = []int{0, 1}
		}, "weights must be > 0"},
		{"unknown strategy", func(c *Config) {
			c.Routes[0].Strategy = "latency"
		}, "strategy must be priority|weighted-rr"},
		{"round_robin ok", func(c *Config) {
			c.Providers[0].Rotation = "round_robin"
		}, ""},
		{"unknown rotation", func(c *Config) {
			c.Providers[0].Rotation = "random"
		}, "rotation must be first|round_robin"},
		{"global routing ok", func(c *Config) {
			c.Routing = Routing{Strategy: "weighted-rr", Rotation: "round_robin"}
		}, ""},
		{"global strategy invalid", func(c *Config) {
			c.Routing = Routing{Strategy: "latency"}
		}, "routing.strategy must be priority|weighted-rr"},
		{"global rotation invalid", func(c *Config) {
			c.Routing = Routing{Rotation: "random"}
		}, "routing.rotation must be first|round_robin"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := routingCfg(tc.mod).Validate()
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("want valid, got: %v", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("want error %q, got nil", tc.wantErr)
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("error %q does not contain %q", err, tc.wantErr)
			}
		})
	}
}
