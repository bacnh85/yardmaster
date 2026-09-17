package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/bacnh85/yardmaster/internal/config"
)

// cmdOAuth manages oauth subscription accounts.
//
//	yardmaster oauth import claude-code [file]
//	yardmaster oauth import codex [file]
//	yardmaster oauth import opencode [file]
func cmdOAuth(args []string) {
	if len(args) < 2 || args[0] != "import" {
		fatal(fmt.Errorf("usage: yardmaster oauth import claude-code|codex|opencode [file]"))
	}
	kind, path := args[1], ""
	if len(args) > 2 {
		path = args[2]
	}
	var (
		prov string
		acct *config.OAuthAcct
		err  error
	)
	switch kind {
	case "claude-code":
		prov = "claude-sub"
		if path == "" {
			path = filepath.Join(home(), ".claude", ".credentials.json")
		}
		acct, err = importClaudeCode(path)
	case "codex":
		prov = "codex-sub"
		if path == "" {
			path = filepath.Join(home(), ".codex", "auth.json")
		}
		acct, err = importCodex(path)
	case "opencode":
		prov = "opencode-sub"
		if path == "" {
			path = filepath.Join(home(), ".local", "share", "opencode", "auth.json")
		}
		acct, err = importOpencode(path)
	default:
		fatal(fmt.Errorf("unknown kind %q (claude-code | codex | opencode)", kind))
	}
	if err != nil {
		fatal(err)
	}
	out, _ := json.MarshalIndent(struct {
		Name         string `json:"name"`
		Kind         string `json:"kind"`
		RefreshToken string `json:"refresh_token,omitempty"`
		AccessToken  string `json:"access_token,omitempty"`
		ExpiresAt    int64  `json:"expires_at,omitempty"`
		AccountID    string `json:"account_id,omitempty"`
	}{acct.Name, acct.Kind, acct.RefreshTok, acct.AccessTok, acct.ExpiresAt, acct.AccountID}, "", "  ")
	fmt.Printf("add to your config under providers (paste into the `auth:` block of provider %q):\n\n  - name: %s\n    auth:\n      type: oauth\n      oauth_accounts:\n%s\n\n(provider base_url/wire for this kind: see README \"OAuth subscription upstreams\")\n",
		prov, prov, indentYAML(string(out)))
}

func home() string {
	h, err := os.UserHomeDir()
	if err != nil {
		fatal(err)
	}
	return h
}

func indentYAML(s string) string {
	lines := strings.Split(s, "\n")
	for i, l := range lines {
		lines[i] = "        " + l
	}
	return strings.Join(lines, "\n")
}

// importClaudeCode reads ~/.claude/.credentials.json (Claude Code CLI login).
func importClaudeCode(path string) (*config.OAuthAcct, error) {
	var f struct {
		ClaudeAiOauth struct {
			AccessToken  string `json:"accessToken"`
			RefreshToken string `json:"refreshToken"`
			ExpiresAt    int64  `json:"expiresAt"` // unix millis
		} `json:"claudeAiOauth"`
	}
	if err := readJSON(path, &f); err != nil {
		return nil, err
	}
	if f.ClaudeAiOauth.RefreshToken == "" && f.ClaudeAiOauth.AccessToken == "" {
		return nil, fmt.Errorf("%s: no claudeAiOauth tokens found — run `claude login` first", path)
	}
	return &config.OAuthAcct{
		Name: "claude-main", Kind: "claude-code",
		RefreshTok: f.ClaudeAiOauth.RefreshToken,
		AccessTok:  f.ClaudeAiOauth.AccessToken,
		ExpiresAt:  f.ClaudeAiOauth.ExpiresAt / 1000,
	}, nil
}

// importCodex reads ~/.codex/auth.json (Codex CLI login).
func importCodex(path string) (*config.OAuthAcct, error) {
	var f struct {
		Tokens struct {
			AccessToken  string `json:"access_token"`
			RefreshToken string `json:"refresh_token"`
			AccountID    string `json:"account_id"`
		} `json:"tokens"`
		LastRefresh string `json:"last_refresh"`
	}
	if err := readJSON(path, &f); err != nil {
		return nil, err
	}
	if f.Tokens.RefreshToken == "" && f.Tokens.AccessToken == "" {
		return nil, fmt.Errorf("%s: no tokens found — run `codex login` first", path)
	}
	var exp int64
	if t, err := time.Parse(time.RFC3339, f.LastRefresh); err == nil {
		// codex access tokens last ~8h; seed an estimate — the pool refreshes on demand
		exp = t.Add(8 * time.Hour).Unix()
	}
	return &config.OAuthAcct{
		Name: "codex-main", Kind: "codex",
		RefreshTok: f.Tokens.RefreshToken,
		AccessTok:  f.Tokens.AccessToken,
		ExpiresAt:  exp,
		AccountID:  f.Tokens.AccountID,
	}, nil
}

// importOpencode reads the opencode CLI auth store and picks the first oauth
// entry (spelling varies between versions: access/access_token, refresh/…).
func importOpencode(path string) (*config.OAuthAcct, error) {
	var f map[string]map[string]any
	if err := readJSON(path, &f); err != nil {
		return nil, err
	}
	get := func(m map[string]any, keys ...string) string {
		for _, k := range keys {
			if v, ok := m[k].(string); ok && v != "" {
				return v
			}
		}
		return ""
	}
	for name, m := range f {
		if get(m, "type") != "oauth" {
			continue
		}
		refresh := get(m, "refresh", "refresh_token")
		access := get(m, "access", "access_token")
		if refresh == "" && access == "" {
			continue
		}
		acct := &config.OAuthAcct{
			Name: "opencode-" + name, Kind: "opencode",
			RefreshTok: refresh, AccessTok: access,
		}
		if exp := get(m, "expire", "expires_at"); exp != "" {
			if t, err := time.Parse(time.RFC3339, exp); err == nil {
				acct.ExpiresAt = t.Unix()
			}
		}
		// ponytail: opencode's OAuth endpoint isn't publicly documented —
		// set token_endpoint + client_id on this account once known
		return acct, nil
	}
	return nil, fmt.Errorf("%s: no oauth entries found", path)
}

func readJSON(path string, v any) error {
	b, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return fmt.Errorf("%s not found — log in with the CLI first", path)
		}
		return err
	}
	return json.Unmarshal(b, v)
}
