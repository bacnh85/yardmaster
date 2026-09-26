package config

import (
	"strings"
	"testing"
)

func comboCfg(mod func(*Config)) *Config {
	c := &Config{
		Providers: []*Provider{
			{Name: "a", Wire: "openai", BaseURL: "https://a",
				Auth: AuthConf{Keys: []string{"k1", "k2"}, KeyLabels: []string{"main", "laptop"}}},
			{Name: "b", Wire: "openai", BaseURL: "https://b",
				Auth: AuthConf{Type: "oauth", OAuth: []*OAuthAcct{{Name: "acct1", Kind: "claude-code", RefreshTok: "r"}}}},
		},
		Combos: []*Combo{{
			Name: "x", Model: "m",
			Members: []*ComboMember{{Provider: "a"}, {Provider: "b"}},
		}},
	}
	if mod != nil {
		mod(c)
	}
	return c
}

func TestComboValidation(t *testing.T) {
	cases := []struct {
		name    string
		mod     func(*Config)
		wantErr string // "" = valid
	}{
		{"default ok", nil, ""},
		{"member default model ok", func(c *Config) {
			c.Combos[0].Members[0].Model = "m2" // explicit member ids are legal
		}, ""},
		{"empty name", func(c *Config) { c.Combos[0].Name = "" }, "name must be"},
		{"bad name chars", func(c *Config) { c.Combos[0].Name = "Deep_X" }, "name must be"},
		{"slash in name", func(c *Config) { c.Combos[0].Name = "a/b" }, "name must be"},
		{"duplicate combo", func(c *Config) {
			c.Combos = append(c.Combos, &Combo{Name: "x", Model: "m", Members: []*ComboMember{{Provider: "a"}}})
		}, "duplicate combo"},
		{"no members", func(c *Config) { c.Combos[0].Members = nil }, "at least one member"},
		{"unknown provider", func(c *Config) { c.Combos[0].Members[0].Provider = "zzz" }, "unknown provider"},
		{"classifier member", func(c *Config) {
			c.Providers = append(c.Providers, &Provider{Name: "j", Wire: "classifier", BaseURL: "https://j", Models: []string{"m"}, Auth: AuthConf{Keys: []string{"k"}}})
			c.Combos[0].Members[0].Provider = "j"
		}, "cannot mix"},
		{"combo model optional — per-member ids", func(c *Config) {
			c.Combos[0].Model = ""
			c.Combos[0].Members[0].Model = "m-a"
			c.Combos[0].Members[1].Model = "m-b"
		}, ""},
		{"no model anywhere", func(c *Config) {
			c.Combos[0].Model = ""
		}, "needs a model"},
		{"all-classifier combo ok", func(c *Config) {
			c.Combos[0].Type = "decision"
			c.Combos[0].Model = ""
			c.Providers = append(c.Providers,
				&Provider{Name: "t1", Wire: "classifier", BaseURL: "https://t1", Models: []string{"jev-latest"}, Auth: AuthConf{Keys: []string{"k"}}},
				&Provider{Name: "t2", Wire: "classifier", BaseURL: "https://t2", Models: []string{"typesafe/jev"}, Auth: AuthConf{Keys: []string{"k"}}})
			c.Combos[0].Members = []*ComboMember{{Provider: "t1", Model: "jev-latest"}, {Provider: "t2", Model: "typesafe/jev"}}
		}, ""},
		{"decision type on chat members", func(c *Config) {
			c.Combos[0].Type = "decision"
		}, "decision type requires"},
		{"decision members without type ok (inferred chat reject)", func(c *Config) { c.Combos[0].Type = "bad" }, "type must be"},
		{"mixed classifier+chat", func(c *Config) {
			c.Providers = append(c.Providers, &Provider{Name: "j", Wire: "classifier", BaseURL: "https://j", Models: []string{"jev"}, Auth: AuthConf{Keys: []string{"k"}}})
			c.Combos[0].Members = []*ComboMember{{Provider: "a"}, {Provider: "j", Model: "jev"}}
		}, "cannot mix"},
		{"unknown strategy", func(c *Config) { c.Combos[0].Strategy = "latency" }, "strategy must be priority|weighted-rr"},
		{"weight on priority", func(c *Config) { c.Combos[0].Members[0].Weight = 3 }, "weights require strategy weighted-rr"},
		{"weight with global weighted-rr ok", func(c *Config) {
			c.Routing = Routing{Strategy: "weighted-rr"}
			c.Combos[0].Members[0].Weight = 3
		}, ""},
		{"weight with combo weighted-rr ok", func(c *Config) {
			c.Combos[0].Strategy = "weighted-rr"
			c.Combos[0].Members[0].Weight = 3
		}, ""},
		{"unknown key label", func(c *Config) { c.Combos[0].Members[0].Keys = []string{"nope"} }, "no key/account labeled"},
		{"known key label ok", func(c *Config) { c.Combos[0].Members[0].Keys = []string{"laptop"} }, ""},
		{"known oauth account ok", func(c *Config) { c.Combos[0].Members[1].Keys = []string{"acct1"} }, ""},
		{"member not serving combo model", func(c *Config) {
			c.Providers[0].Models = []string{"other-model"} // curated list without the combo's model, no member override
		}, "does not serve"},
		{"member override curating a different id ok", func(c *Config) {
			c.Providers[0].Models = []string{"other-model"}
			c.Combos[0].Members[0].Model = "other-model"
		}, ""},
		{"member against wildcard provider ok", func(c *Config) {
			c.Providers[0].Models = nil // empty Models = serves anything
		}, ""},
		{"reserved provider prefix", func(c *Config) { c.Providers[0].Prefix = "combo" }, "reserved for combo"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := comboCfg(tc.mod).Validate()
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("want valid, got: %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("want error containing %q, got: %v", tc.wantErr, err)
			}
		})
	}
}

// yaml round-trip: combos survive Save → Load (dashboard edits rewrite config.yaml).
func TestComboYAMLRoundTrip(t *testing.T) {
	c := comboCfg(func(c *Config) {
		c.Combos[0].Strategy = "weighted-rr"
		c.Combos[0].Members[0].Keys = []string{"laptop"}
		c.Combos[0].Members[0].Weight = 3
	})
	path := t.TempDir() + "/config.yaml"
	if err := c.Save(path); err != nil {
		t.Fatal(err)
	}
	c2, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(c2.Combos) != 1 {
		t.Fatalf("combos lost on round-trip: %d", len(c2.Combos))
	}
	cb := c2.Combos[0]
	if cb.Name != "x" || cb.Model != "m" || cb.Strategy != "weighted-rr" || len(cb.Members) != 2 {
		t.Fatalf("combo round-trip mismatch: %+v", cb)
	}
	m := cb.Members[0]
	if m.Provider != "a" || len(m.Keys) != 1 || m.Keys[0] != "laptop" || m.Weight != 3 {
		t.Fatalf("member round-trip mismatch: %+v", m)
	}
}
