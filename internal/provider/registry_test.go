package provider

import (
	"testing"

	"github.com/bacnh85/yardmaster/internal/config"
)

// zenConfig mirrors the OpenCode Zen setup: three wire entries over one
// base_url sharing the "ocg" prefix, plus a separate zai provider.
func zenConfig() *config.Config {
	return &config.Config{
		Providers: []*config.Provider{
			{Name: "opencode-go", Prefix: "ocg", Wire: "openai", BaseURL: "https://zen",
				Models: []string{"big-pickle", "glm-5.2"}, Auth: config.AuthConf{Type: "static", Keys: []string{"k1"}}},
			{Name: "opencode-go-claude", Prefix: "ocg", Wire: "anthropic", BaseURL: "https://zen",
				Models: []string{"claude-haiku-4-5"}, Auth: config.AuthConf{Type: "static", Keys: []string{"k2"}}},
			{Name: "opencode-go-gpt", Prefix: "ocg", Wire: "responses", BaseURL: "https://zen",
				Auth: config.AuthConf{Type: "static", Keys: []string{"k3"}}},
			{Name: "zai", Prefix: "zai", Wire: "anthropic", BaseURL: "https://zai",
				Models: []string{"glm-5.2"}, Auth: config.AuthConf{Type: "static", Keys: []string{"k4"}}},
		},
		Keys: []*config.Key{{Key: "ar-x", Name: "pi", Allow: []string{"*"}}},
	}
}

func names(tgts []*Target) []string {
	out := make([]string, 0, len(tgts))
	for _, tgt := range tgts {
		out = append(out, tgt.Provider.Name)
	}
	return out
}

func eq(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func TestPrefixResolve(t *testing.T) {
	reg := New(zenConfig())
	cases := []struct {
		model string
		want  []string
	}{
		{"ocg/claude-haiku-4-5", []string{"opencode-go-claude"}},
		{"ocg/glm-5.2", []string{"opencode-go"}},
		{"ocg/big-pickle", []string{"opencode-go"}},
		{"zai/glm-5.2", []string{"zai"}},
		// wildcard entry (empty Models) catches uncurated unprefixed ids
		{"org/model", []string{"opencode-go-gpt"}},
		// prefixed ids must be curated: uncurated → no target (404)
		{"ocg/kimi-k3", nil},
		// unprefixed: back-compat — curated providers + wildcard entry, config order
		{"glm-5.2", []string{"opencode-go", "opencode-go-gpt", "zai"}},
		// natural "/" ids are not prefixes
		{"org/model", []string{"opencode-go-gpt"}},
	}
	for _, tc := range cases {
		got := names(reg.Resolve(tc.model, []string{"*"}))
		if !eq(got, tc.want) {
			t.Errorf("%s → %v, want %v", tc.model, got, tc.want)
		}
	}
}

func TestPrefixAllowPatterns(t *testing.T) {
	reg := New(zenConfig())
	if got := reg.Resolve("ocg/glm-5.2", []string{"ocg/*"}); len(got) == 0 {
		t.Fatal("ocg/* should allow ocg models")
	}
	if got := reg.Resolve("ocg/glm-5.2", []string{"zai/*"}); len(got) != 0 {
		t.Fatal("zai/* must not allow ocg models")
	}
	if got := reg.Resolve("zai/glm-5.2", []string{"zai/*"}); len(got) == 0 {
		t.Fatal("zai/* should allow zai models")
	}
}

func TestModelsPrefixed(t *testing.T) {
	reg := New(zenConfig())
	got := reg.Models()
	for _, want := range []string{"ocg/big-pickle", "ocg/claude-haiku-4-5", "zai/glm-5.2"} {
		found := false
		for _, m := range got {
			if m == want {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("Models() missing %q (got %v)", want, got)
		}
	}
	for _, bare := range []string{"big-pickle", "claude-haiku-4-5", "glm-5.2"} {
		for _, m := range got {
			if m == bare {
				t.Errorf("Models() leaked bare id %q on a prefixed provider (got %v)", bare, got)
			}
		}
	}
}

func TestUpstreamModelPrefix(t *testing.T) {
	p := &config.Provider{Name: "opencode-go", Prefix: "ocg",
		ModelMap: map[string]string{"big-pickle": "bp"}}
	if got := UpstreamModel(p, "ocg/big-pickle"); got != "bp" {
		t.Fatalf("prefixed + map: got %q", got)
	}
	if got := UpstreamModel(p, "big-pickle"); got != "bp" {
		t.Fatalf("bare + map: got %q", got)
	}
	if got := UpstreamModel(p, "ocg/other"); got != "other" {
		t.Fatalf("prefixed unmapped: got %q", got)
	}
	// natural id whose first segment equals the prefix must not be stripped
	np := &config.Provider{Name: "openrouter", Prefix: "openrouter",
		Models: []string{"openrouter/auto", "deepseek/deepseek-chat"}}
	if got := UpstreamModel(np, "openrouter/auto"); got != "openrouter/auto" {
		t.Fatalf("natural id under colliding prefix: got %q", got)
	}
	if got := UpstreamModel(np, "openrouter/deepseek/deepseek-chat"); got != "deepseek/deepseek-chat" {
		t.Fatalf("genuinely prefixed id: got %q", got)
	}
}

func TestSplitPrefix(t *testing.T) {
	cfg := zenConfig()
	if _, bare, ok := SplitPrefix(cfg, "ocg/glm-5.2"); !ok || bare != "glm-5.2" {
		t.Fatalf("ocg split: ok=%v bare=%q", ok, bare)
	}
	if _, bare, ok := SplitPrefix(cfg, "unknown/glm"); ok || bare != "unknown/glm" {
		t.Fatalf("unknown prefix must not match: ok=%v bare=%q", ok, bare)
	}
	if _, _, ok := SplitPrefix(cfg, "nodash"); ok {
		t.Fatal("plain id must not match")
	}
	if _, bare, ok := SplitPrefix(cfg, "ocg/"); ok || bare != "ocg/" {
		t.Fatal("trailing slash must not match")
	}
}

// collidingConfig: a provider whose prefix equals a vendor namespace of its
// own curated natural ids (the openrouter/auto trap).
func collidingConfig() *config.Config {
	return &config.Config{
		Providers: []*config.Provider{
			{Name: "openrouter", Prefix: "openrouter", Wire: "openai", BaseURL: "https://openrouter.ai/api/v1",
				Models: []string{"openrouter/auto", "deepseek/deepseek-chat"},
				Auth:   config.AuthConf{Type: "static", Keys: []string{"k1"}}},
		},
		Keys: []*config.Key{{Key: "ar-x", Name: "pi", Allow: []string{"*"}}},
	}
}

// End to end: a natural id whose first segment equals the configured prefix
// must resolve (not 404) and reach upstream whole; genuinely prefixed ids
// strip as usual; uncurated prefixed ids still 404.
func TestCollidingPrefixResolve(t *testing.T) {
	reg := New(collidingConfig())

	// natural id, bare request: SplitPrefix sees prefix="openrouter" and
	// strips to "auto" — Resolve must still match the full curated id
	tgts := reg.Resolve("openrouter/auto", []string{"*"})
	if len(tgts) != 1 || names(tgts)[0] != "openrouter" {
		t.Fatalf("natural openrouter/auto: got %v, want [openrouter]", names(tgts))
	}
	if got := UpstreamModel(tgts[0].Provider, "openrouter/auto"); got != "openrouter/auto" {
		t.Fatalf("natural id upstream: got %q, want unstripped", got)
	}

	// genuinely prefixed: bare id curated → resolves and strips
	tgts = reg.Resolve("openrouter/deepseek/deepseek-chat", []string{"*"})
	if len(tgts) != 1 {
		t.Fatalf("prefixed deepseek: got %v", names(tgts))
	}
	if got := UpstreamModel(tgts[0].Provider, "openrouter/deepseek/deepseek-chat"); got != "deepseek/deepseek-chat" {
		t.Fatalf("prefixed upstream: got %q", got)
	}

	// uncurated prefixed id → no target (404)
	if tgts := reg.Resolve("openrouter/z-ai/glm-5.3", []string{"*"}); len(tgts) != 0 {
		t.Fatalf("uncurated prefixed: got %v, want none", names(tgts))
	}

	// advertisement: both curated ids, prefixed (no doubled prefix segment)
	ids := map[string]bool{}
	for _, id := range reg.Models() {
		ids[id] = true
	}
	if !ids["openrouter/openrouter/auto"] || !ids["openrouter/deepseek/deepseek-chat"] {
		t.Fatalf("advertised ids missing: %v", ids)
	}
}

// The per-provider name match: a DISABLED full-id-curated provider sharing the
// prefix must not flip the curation name for an enabled bare-curated sibling.
func TestCollidingPrefixDisabledSibling(t *testing.T) {
	cfg := &config.Config{
		Providers: []*config.Provider{
			{Name: "or-natural", Prefix: "openrouter", Wire: "openai", BaseURL: "https://x",
				Models: []string{"openrouter/auto"}, Disabled: true,
				Auth: config.AuthConf{Type: "static", Keys: []string{"k0"}}},
			{Name: "or-main", Prefix: "openrouter", Wire: "openai", BaseURL: "https://y",
				Models: []string{"deepseek/deepseek-chat"},
				Auth:   config.AuthConf{Type: "static", Keys: []string{"k1"}}},
		},
		Keys: []*config.Key{{Key: "ar-x", Name: "pi", Allow: []string{"*"}}},
	}
	reg := New(cfg)
	tgts := reg.Resolve("openrouter/deepseek/deepseek-chat", []string{"*"})
	if len(tgts) != 1 || names(tgts)[0] != "or-main" {
		t.Fatalf("bare-curated sibling behind disabled full-id sibling: got %v", names(tgts))
	}
}
