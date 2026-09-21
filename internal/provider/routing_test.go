package provider

import (
	"testing"
	"time"

	"github.com/bacnh85/yardmaster/internal/config"
)

// two-keyed providers + an optional weighted route over them
func routingReg(mod func(*config.Config)) *Registry {
	cfg := &config.Config{
		Providers: []*config.Provider{
			{Name: "a", Wire: "openai", BaseURL: "https://a", Auth: config.AuthConf{Keys: []string{"k1", "k2"}}},
			{Name: "b", Wire: "openai", BaseURL: "https://b", Auth: config.AuthConf{Keys: []string{"k3"}}},
		},
		Keys: []*config.Key{{Key: "ar-x", Name: "pi", Allow: []string{"*"}}},
	}
	if mod != nil {
		mod(cfg)
	}
	return New(cfg)
}

func keys(tgts []*Target) []string {
	out := make([]string, 0, len(tgts))
	for _, tgt := range tgts {
		out = append(out, tgt.APIKey+tgt.AcctName)
	}
	return out
}

func TestRotationRoundRobin(t *testing.T) {
	reg := routingReg(func(c *config.Config) {
		c.Providers[0].Rotation = "round_robin"
		c.Routes = []*config.Route{{Match: "m", Chain: []string{"a"}}}
	})
	want := [][]string{{"k1"}, {"k2"}, {"k1"}, {"k2"}}
	for i, w := range want {
		got := keys(reg.Resolve("m", []string{"*"}))
		if len(got) != 2 || got[0] != w[0] {
			t.Fatalf("resolve %d: got %v, want head %v", i, got, w)
		}
	}

	// default (first) never rotates
	reg2 := routingReg(func(c *config.Config) {
		c.Routes = []*config.Route{{Match: "m", Chain: []string{"a"}}}
	})
	for i := 0; i < 3; i++ {
		got := keys(reg2.Resolve("m", []string{"*"}))
		if got[0] != "k1" {
			t.Fatalf("default rotation resolve %d starts at %q", i, got[0])
		}
	}
}

func TestRotationRoundRobinOAuth(t *testing.T) {
	reg := routingReg(func(c *config.Config) {
		c.Providers[1].Rotation = "round_robin"
		c.Providers[1].Auth = config.AuthConf{Type: "oauth", OAuth: []*config.OAuthAcct{
			{Name: "o1"}, {Name: "o2"}, {Name: "o3"},
		}}
		c.Routes = []*config.Route{{Match: "m", Chain: []string{"b"}}}
	})
	seen := []string{}
	for i := 0; i < 3; i++ {
		seen = append(seen, keys(reg.Resolve("m", []string{"*"}))[0])
	}
	// head order rotates o1 → o2 → o3 across requests
	if seen[0] != "o1" || seen[1] != "o2" || seen[2] != "o3" {
		t.Fatalf("oauth rotation: %v", seen)
	}
}

func TestWeightedRRHead(t *testing.T) {
	reg := routingReg(func(c *config.Config) {
		c.Routes = []*config.Route{{Match: "m", Chain: []string{"a", "b"}, Strategy: "weighted-rr", Weights: []int{3, 1}}}
	})
	counts := map[string]int{}
	for i := 0; i < 40; i++ {
		tgts := reg.Resolve("m", []string{"*"})
		if len(tgts) != 3 { // head (2 keys) + fallback chain entry
			t.Fatalf("resolve %d: %d targets", i, len(tgts))
		}
		counts[tgts[0].Provider.Name]++
		if tgts[0].Provider.Name == "b" && tgts[2].Provider.Name != "a" {
			t.Fatalf("b head must keep a as fallback, got %v", names(tgts))
		}
	}
	if counts["a"] != 30 || counts["b"] != 10 {
		t.Fatalf("weights 3:1 over 40 requests: %v", counts)
	}
}

func TestPriorityDefaultOrderUnchanged(t *testing.T) {
	reg := routingReg(func(c *config.Config) {
		c.Routes = []*config.Route{{Match: "m", Chain: []string{"b", "a"}}}
	})
	got := names(reg.Resolve("m", []string{"*"}))
	if !eq(got, []string{"b", "a", "a"}) {
		t.Fatalf("priority order: %v", got)
	}
}

func TestGlobalRoutingInheritance(t *testing.T) {
	// global rotation round_robin applies when the provider doesn't set one
	reg := routingReg(func(c *config.Config) {
		c.Routing = config.Routing{Rotation: "round_robin"}
		c.Routes = []*config.Route{{Match: "m", Chain: []string{"a"}}}
	})
	if got := keys(reg.Resolve("m", []string{"*"}))[0]; got != "k1" {
		t.Fatalf("first request: %q", got)
	}
	if got := keys(reg.Resolve("m", []string{"*"}))[0]; got != "k2" {
		t.Fatalf("second request should rotate via global default: %q", got)
	}

	// explicit provider rotation overrides the global default
	reg = routingReg(func(c *config.Config) {
		c.Routing = config.Routing{Rotation: "round_robin"}
		c.Providers[0].Rotation = "first"
		c.Routes = []*config.Route{{Match: "m", Chain: []string{"a"}}}
	})
	for i := 0; i < 2; i++ {
		if got := keys(reg.Resolve("m", []string{"*"}))[0]; got != "k1" {
			t.Fatalf("explicit provider rotation should win: %q", got)
		}
	}

	// global weighted-rr applies to routes without an explicit strategy
	reg = routingReg(func(c *config.Config) {
		c.Routing = config.Routing{Strategy: "weighted-rr", Rotation: "first"}
		c.Routes = []*config.Route{{Match: "m", Chain: []string{"a", "b"}, Weights: []int{3, 1}}}
	})
	counts := map[string]int{}
	for i := 0; i < 40; i++ {
		counts[reg.Resolve("m", []string{"*"})[0].Provider.Name]++
	}
	if counts["a"] != 30 || counts["b"] != 10 {
		t.Fatalf("global strategy not inherited: %v", counts)
	}

	// inherited weighted-rr with NO route weights → equal spread
	reg = routingReg(func(c *config.Config) {
		c.Routing = config.Routing{Strategy: "weighted-rr", Rotation: "first"}
		c.Routes = []*config.Route{{Match: "m", Chain: []string{"a", "b"}}}
	})
	counts = map[string]int{}
	for i := 0; i < 40; i++ {
		counts[reg.Resolve("m", []string{"*"})[0].Provider.Name]++
	}
	if counts["a"] != 20 || counts["b"] != 20 {
		t.Fatalf("equal spread default broken: %v", counts)
	}

	// explicit route strategy beats the global default
	reg = routingReg(func(c *config.Config) {
		c.Routing = config.Routing{Strategy: "weighted-rr", Rotation: "first"}
		c.Routes = []*config.Route{{Match: "m", Chain: []string{"b", "a"}}}
	})
	if got := names(reg.Resolve("m", []string{"*"})); !eq(got, []string{"b", "a", "a"}) {
		t.Fatalf("route strategy should override global: %v", got)
	}
}

func TestCooldownsLifecycle(t *testing.T) {
	now := time.Unix(1700000000, 0)
	cd := NewCooldowns()
	cd.now = func() time.Time { return now }

	// 5xx breaker: 2 failures don't trip, 3rd does
	cd.MarkFail("a", "k1")
	cd.MarkFail("a", "k1")
	if cd.Cooling("a", "k1") {
		t.Fatal("breaker tripped after 2 failures")
	}
	cd.MarkFail("a", "k1")
	if !cd.Cooling("a", "k1") {
		t.Fatal("breaker did not trip after 3 failures")
	}

	// expiry
	now = now.Add(31 * time.Second)
	if cd.Cooling("a", "k1") {
		t.Fatal("cooldown did not expire")
	}

	// 429 default 30s
	cd.Mark429("a", "k2", "")
	if !cd.Cooling("a", "k2") {
		t.Fatal("429 did not cool")
	}
	now = now.Add(29 * time.Second)
	if !cd.Cooling("a", "k2") {
		t.Fatal("429 default expired early")
	}
	now = now.Add(2 * time.Second)
	if cd.Cooling("a", "k2") {
		t.Fatal("429 default did not expire")
	}

	// Retry-After seconds honored (and capped at 5min)
	cd.Mark429("a", "k3", "120")
	now = now.Add(119 * time.Second)
	if !cd.Cooling("a", "k3") {
		t.Fatal("Retry-After 120s not honored")
	}
	cd.Mark429("a", "k3", "9999")
	now = now.Add(2 * time.Second) // 121s since k3 cooling started
	if !cd.Cooling("a", "k3") {
		t.Fatal("Retry-After cap should hold 5min")
	}

	// Retry-After HTTP-date form
	cd.Mark429("a", "k4", now.Add(90*time.Second).UTC().Format("Mon, 02 Jan 2006 15:04:05 GMT"))
	now = now.Add(89 * time.Second)
	if !cd.Cooling("a", "k4") {
		t.Fatal("Retry-After HTTP-date not honored")
	}

	// Reset clears
	cd.Reset("a", "k4")
	if cd.Cooling("a", "k4") {
		t.Fatal("Reset did not clear")
	}

	// Snapshot redacts keys and reports only active entries
	now = now.Add(10 * time.Second)
	cd.Mark429("snap-provider", "secret-key-123456", "60")
	snap := cd.Snapshot()
	if len(snap) == 0 {
		t.Fatal("snapshot empty")
	}
	found := false
	for _, e := range snap {
		if e.Provider == "snap-provider" {
			found = true
			if e.Key != "…123456" {
				t.Fatalf("snapshot key not redacted: %q", e.Key)
			}
		}
	}
	if !found {
		t.Fatal("snapshot missing provider")
	}
}

// Mark429 extends, never shortens: a headerless 429 landing late must not cut
// a live 300s cooldown to the 30s default.
func TestMark429NeverShortens(t *testing.T) {
	now := time.Unix(1700000000, 0)
	cd := NewCooldowns()
	cd.now = func() time.Time { return now }

	cd.Mark429("a", "k1", "300")
	now = now.Add(31 * time.Second)
	if !cd.Cooling("a", "k1") {
		t.Fatal("300s cooldown expired at 31s")
	}
	cd.Mark429("a", "k1", "") // concurrent headerless 429 lands late
	now = now.Add(1 * time.Second)
	if !cd.Cooling("a", "k1") {
		t.Fatal("headerless 429 shortened the live 300s cooldown")
	}
	now = now.Add(269 * time.Second) // +301s since the first 429
	if cd.Cooling("a", "k1") {
		t.Fatal("cooldown did not expire after 300s")
	}
}

// A present, parseable Retry-After is honored even below the 30s default.
func TestMark429HonorsShortRetryAfter(t *testing.T) {
	now := time.Unix(1700000000, 0)
	cd := NewCooldowns()
	cd.now = func() time.Time { return now }

	cd.Mark429("a", "k1", "8")
	now = now.Add(7 * time.Second)
	if !cd.Cooling("a", "k1") {
		t.Fatal("8s Retry-After expired early")
	}
	now = now.Add(2 * time.Second)
	if cd.Cooling("a", "k1") {
		t.Fatal("8s Retry-After did not expire")
	}
}

// The 5xx breaker trip must not shorten a live 429 window either.
func TestBreakerNeverShortensLive429(t *testing.T) {
	now := time.Unix(1700000000, 0)
	cd := NewCooldowns()
	cd.now = func() time.Time { return now }

	cd.Mark429("a", "k1", "300")
	cd.MarkFail("a", "k1")
	cd.MarkFail("a", "k1")
	cd.MarkFail("a", "k1") // breaker trips
	now = now.Add(31 * time.Second)
	if !cd.Cooling("a", "k1") {
		t.Fatal("breaker shortened the live 300s cooldown to 30s")
	}
	now = now.Add(269 * time.Second)
	if cd.Cooling("a", "k1") {
		t.Fatal("cooldown outlived the 300s 429 window")
	}
}

func TestCooldownPerTargetIsolation(t *testing.T) {
	cd := NewCooldowns()
	cd.Mark429("a", "k1", "")
	if cd.Cooling("a", "k2") || cd.Cooling("b", "k1") {
		t.Fatal("cooldown leaked across targets")
	}
}
