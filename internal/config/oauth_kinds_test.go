package config

import (
	"strings"
	"testing"
)

// Baked-in endpoint/client_id pairs per kind. Existing kinds must not drift;
// qwen rides the Qwen Code CLI's OAuth app; copilot has none by design (its
// dispatch-time exchange endpoint lives in the proxy).
func TestAuthKindDefaults(t *testing.T) {
	cases := []struct {
		kind               string
		endpoint, clientID string
		ok                 bool
	}{
		{"claude-code", "https://platform.claude.com/v1/oauth/token", "9d1c250a-e61b-44d9-88ed-5944d1962f5e", true},
		{"codex", "https://auth.openai.com/oauth/token", "app_EMoamEEZ73f0CkXaXp7hrann", true},
		{"qwen", "https://chat.qwen.ai/api/v1/oauth2/token", "f0304373b74a44d2b584a3fb70ca9e56", true},
		{"gemini-cli", "https://oauth2.googleapis.com/token", "681255809395-oo8ft2oprdrnp9e3aqf6av3hmdib135j", true},
		{"copilot", "", "", false},
		{"opencode", "", "", false},
		{"totally-new-vendor", "", "", false}, // unknown kinds stay legal, not whitelisted
	}
	for _, tc := range cases {
		ep, cid, ok := AuthKindDefaults(tc.kind)
		if ep != tc.endpoint || cid != tc.clientID || ok != tc.ok {
			t.Errorf("kind %q: got (%q, %q, %v)", tc.kind, ep, cid, ok)
		}
	}
}

// Unknown kinds are not restricted to a whitelist: a custom endpoint is the
// whole point. copilot is the one built-in kind without defaults — it must
// validate with neither token_endpoint nor client_id.
func TestValidateOAuthKinds(t *testing.T) {
	cfg := func(accts ...*OAuthAcct) *Config {
		c := &Config{Providers: []*Provider{{
			Name: "sub", BaseURL: "https://up.example", Wire: "openai",
			Auth: AuthConf{Type: "oauth", OAuth: accts},
		}}}
		c.Defaults()
		return c
	}
	// copilot: no endpoint/client_id, still valid
	a := &OAuthAcct{Name: "copilot-main", Kind: "copilot", RefreshTok: "ghp_live"}
	if err := cfg(a).Validate(); err != nil {
		t.Fatalf("copilot account rejected: %v", err)
	}
	if a.TokenEndpoint != "" || a.ClientID != "" {
		t.Fatalf("copilot must not get defaults baked in: %q %q", a.TokenEndpoint, a.ClientID)
	}
	// qwen: defaults applied
	q := &OAuthAcct{Name: "qwen-main", Kind: "qwen", RefreshTok: "rt"}
	if err := cfg(q).Validate(); err != nil {
		t.Fatalf("qwen account rejected: %v", err)
	}
	if q.TokenEndpoint != "https://chat.qwen.ai/api/v1/oauth2/token" ||
		q.ClientID != "f0304373b74a44d2b584a3fb70ca9e56" {
		t.Fatalf("qwen defaults not applied: %q %q", q.TokenEndpoint, q.ClientID)
	}
	// unknown kind without endpoint/client_id: still the old error
	u := &OAuthAcct{Name: "x", Kind: "mystery", RefreshTok: "rt"}
	err := cfg(u).Validate()
	if err == nil || !strings.Contains(err.Error(), "unknown kind") {
		t.Fatalf("unknown kind: want error, got %v", err)
	}
	// unknown kind WITH endpoint/client_id: legal (custom endpoints)
	u.TokenEndpoint, u.ClientID = "https://auth.mystery.example/token", "cid"
	if err := cfg(u).Validate(); err != nil {
		t.Fatalf("custom endpoint kind rejected: %v", err)
	}
}
