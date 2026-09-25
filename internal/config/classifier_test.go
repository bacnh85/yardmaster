package config

import "testing"

func TestClassifierWireValidation(t *testing.T) {
	base := func(wire string, models []string) *Config {
		c := &Config{
			Providers: []*Provider{
				{Name: "ts", BaseURL: "https://api.typesafe.ai/v1", Wire: wire, Models: models,
					Auth: AuthConf{Type: "static", Keys: []string{"k"}}},
			},
			Keys: []*Key{{Key: "ar-agent", Name: "pi"}},
		}
		c.Defaults()
		return c
	}
	if err := base("classifier", []string{"jev-latest"}).Validate(); err != nil {
		t.Fatalf("valid classifier provider rejected: %v", err)
	}
	if err := base("classifier", nil).Validate(); err == nil {
		t.Fatal("classifier wildcard (empty models) accepted")
	}
	if err := base("bogus", nil).Validate(); err == nil {
		t.Fatal("bogus wire accepted")
	}
}

func TestClassifierRouteForbidden(t *testing.T) {
	c := &Config{
		Providers: []*Provider{
			{Name: "chat", BaseURL: "https://x.example", Wire: "openai",
				Auth: AuthConf{Type: "static", Keys: []string{"k"}}},
			{Name: "ts", BaseURL: "https://api.typesafe.ai/v1", Wire: "classifier", Models: []string{"jev-latest"},
				Auth: AuthConf{Type: "static", Keys: []string{"k"}}},
		},
		Routes: []*Route{{Match: "jev-latest", Chain: []string{"ts"}}},
		Keys:   []*Key{{Key: "ar-agent", Name: "pi"}},
	}
	c.Defaults()
	if err := c.Validate(); err == nil {
		t.Fatal("route chaining a classifier provider accepted")
	}
	c.Routes[0].Chain = []string{"chat"}
	if err := c.Validate(); err != nil {
		t.Fatalf("chat route rejected: %v", err)
	}
}

func TestJevCostLookup(t *testing.T) {
	c := &Config{}
	c.Defaults()
	for _, m := range []string{"typesafe/jev-1.13", "jev-latest", "jev-1.13.0", "or/typesafe/jev-1.13", "jev/jev-latest"} {
		got := c.CostFor(m)
		if got.Input != 0.042 {
			t.Fatalf("CostFor(%s) input = %v, want 0.042", m, got.Input)
		}
		if got.Output != 0 {
			t.Fatalf("CostFor(%s) output = %v, want 0 (output tokens free)", m, got.Output)
		}
	}
	// Command Code's bare id bills $0.04/M on GOAT+ — must hit its own entry,
	// not the "jev" prefix (checked before the family-prefix fallback, but the
	// exact key must exist so cmd/typesafe/jev resolves to 0.04, not 0.042)
	for _, m := range []string{"typesafe/jev", "cmd/typesafe/jev"} {
		if got := c.CostFor(m).Input; got != 0.04 {
			t.Fatalf("CostFor(%s) input = %v, want 0.04 (Command Code rate)", m, got)
		}
	}
}
