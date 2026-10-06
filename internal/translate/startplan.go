package translate

// Start-plan (Z.ai trial entitlements) requests must carry the exact ZCode
// desktop identity system blocks — the gateway inspects the `system` field and
// rejects bare requests with biz 3012 "method not allowed". Port of zcode-api
// system-prompt.ts (ZCode 3.11.2 ContextBuilder mirror, bundle symbols in the
// JSON sidecar comments): exactly 3 wire blocks, each cache_control ephemeral:
//   1. cli_prefix alone
//   2. stable sections \n\n-joined
//   3. dynamic sections \n\n-joined with a leading "\n\n" (Environment Info
//      ends with "- You are powered by the model named zai-api/{model}.")
// Client system blocks survive AFTER the official ones with cache_control
// stripped, tools lose their markers too: the official 3 + the last-message
// marker fill Anthropic's 4-breakpoint budget (the real client owns the whole
// body the same way). The first user turn is preceded by the meta_user
// currentDate <system-reminder> every real client request carries.
//
// Start-plan mode therefore does NOT run InjectCacheControlAnthropic — it
// would add tool + message markers on top and blow the budget.

import (
	_ "embed"
	"os/exec"
	"encoding/json"
	"os"
	"runtime"
	"strings"
	"sync"
	"time"
)

//go:embed zcode_system.json
var zcodeSystemJSON []byte

type zcodeSystem struct {
	CliPrefix      string `json:"cliPrefix"`
	StableSections []string `json:"stableSections"`
	DynamicSections struct {
		BeforeEnvironment string `json:"beforeEnvironment"`
		AfterEnvironment  string `json:"afterEnvironment"`
	} `json:"dynamicSections"`
	Environment struct {
		Heading        string `json:"heading"`
		InvokedLine    string `json:"invokedLine"`
		CwdLabel       string `json:"cwdLabel"`
		GitLabel       string `json:"gitLabel"`
		GitNo          string `json:"gitNo"`
		PlatformLabel  string `json:"platformLabel"`
		ShellLabel     string `json:"shellLabel"`
		OsVersionLabel string `json:"osVersionLabel"`
		PoweredByLine  string `json:"poweredByLine"`
	} `json:"environment"`
	ContextPrefix struct {
		Intro              string `json:"intro"`
		CurrentDateHeading string `json:"currentDateHeading"`
		CurrentDateLine    string `json:"currentDateLine"`
		Outro              string `json:"outro"`
	} `json:"contextPrefix"`
	SystemReminder struct {
		Open  string `json:"open"`
		Close string `json:"close"`
	} `json:"systemReminder"`
}

var zcSys = sync.OnceValue(func() zcodeSystem {
	var z zcodeSystem
	_ = json.Unmarshal(zcodeSystemJSON, &z) // embedded asset; panic-on-zero not worth it
	return z
})

func textBlock(text string, ephemeral bool) map[string]any {
	b := map[string]any{"type": "text", "text": text}
	if ephemeral {
		b["cache_control"] = map[string]any{"type": "ephemeral"}
	}
	return b
}

// osVersionCached: darwin → macOS product version via sysctl, linux →
// kernel release, else the GOOS string. Computed once; the value is stable.
var osVersionCached = sync.OnceValue(func() string {
	if runtime.GOOS == "darwin" {
		if out, err := exec.Command("sw_vers", "-productVersion").Output(); err == nil {
			if v := strings.TrimSpace(string(out)); v != "" {
				return v
			}
		}
		return runtime.GOOS
	}
	if b, err := os.ReadFile("/proc/sys/kernel/osrelease"); err == nil {
		if s := strings.TrimSpace(string(b)); s != "" {
			return s
		}
	}
	return runtime.GOOS
})

// startPlanEnv mirrors the desktop client's env chain (resolveEnvPromptInfo):
// cwd/platform/shell/osVersion with the same "unknown" fallbacks. platform is
// `${goos}-${arch}` like the identity headers use.
func startPlanEnv() (cwd, platform, shell, osVersion string) {
	cwd, _ = os.Getwd()
	if cwd == "" {
		cwd = "unknown"
	}
	platform = runtime.GOOS + "-" + runtime.GOARCH
	if shell = os.Getenv("SHELL"); shell == "" {
		shell = "unknown"
	}
	return cwd, platform, shell, osVersionCached()
}

// InjectStartPlanSystem rewrites an anthropic-wire request body for the
// start-plan gateway. model is the UPSTREAM id (drives the powered-by line).
func InjectStartPlanSystem(req map[string]any, model string) {
	z := zcSys()
	cwd, platform, shell, osVersion := startPlanEnv()
	env := []string{
		z.Environment.Heading,
		z.Environment.InvokedLine,
		"- " + z.Environment.CwdLabel + ": " + cwd,
		"- " + z.Environment.GitLabel + ": " + z.Environment.GitNo,
		"- " + z.Environment.PlatformLabel + ": " + platform,
		"- " + z.Environment.ShellLabel + ": " + shell,
		"- " + z.Environment.OsVersionLabel + ": " + osVersion,
	}
	if model != "" {
		env = append(env, strings.ReplaceAll(z.Environment.PoweredByLine, "{provider}/{model}", "zai-api/"+model))
	}
	dynamic := strings.Join([]string{
		z.DynamicSections.BeforeEnvironment,
		strings.Join(env, "\n"),
		z.DynamicSections.AfterEnvironment,
	}, "\n\n")
	official := []any{
		textBlock(z.CliPrefix, true),
		textBlock(strings.Join(z.StableSections, "\n\n"), true),
		textBlock("\n\n"+dynamic, true),
	}
	req["system"] = append(official, userSystemBlocks(req["system"])...)

	// meta_user context prefix: currentDate reminder as a leading user turn.
	date := time.Now().Format("2006-01-02")
	currentDate := z.ContextPrefix.CurrentDateHeading + "\n" +
		strings.ReplaceAll(z.ContextPrefix.CurrentDateLine, "{date}", date)
	body := strings.Join([]string{z.ContextPrefix.Intro, currentDate, "", z.ContextPrefix.Outro}, "\n")
	prefix := map[string]any{
		"role":    "user",
		"content": []any{textBlock(z.SystemReminder.Open + body + z.SystemReminder.Close, false)},
	}
	if msgs, ok := req["messages"].([]any); ok && len(msgs) > 0 {
		req["messages"] = append([]any{prefix}, msgs...)
	} else {
		req["messages"] = []any{prefix}
	}

	// Breakpoint budget: strip tool markers, mark the last message (4 total
	// with the 3 official system blocks — mirrors the desktop body-transformer).
	if tools, ok := req["tools"].([]any); ok {
		for _, t := range tools {
			if m, ok := t.(map[string]any); ok {
				delete(m, "cache_control")
			}
		}
	}
	markLastMessage(req)
}

// userSystemBlocks normalizes the client system field to text blocks, dropping
// cache_control (the official blocks consume 3 of 4 breakpoints).
func userSystemBlocks(system any) []any {
	switch s := system.(type) {
	case string:
		if t := strings.TrimSpace(s); t != "" {
			return []any{textBlock(t, false)}
		}
	case []any:
		var out []any
		for _, item := range s {
			m, ok := item.(map[string]any)
			if !ok {
				continue
			}
			if t, ok := m["type"].(string); ok && t == "text" {
				if text, ok := m["text"].(string); ok && strings.TrimSpace(text) != "" {
					out = append(out, textBlock(text, false))
				}
			}
		}
		return out
	}
	return nil
}

// markLastMessage adds an ephemeral marker to the last message's final block
// (string content normalized to a single block first — same shape rule the
// cache-control injector uses on the coding-plan path).
func markLastMessage(req map[string]any) {
	msgs, ok := req["messages"].([]any)
	if !ok || len(msgs) == 0 {
		return
	}
	last, ok := msgs[len(msgs)-1].(map[string]any)
	if !ok {
		return
	}
	switch c := last["content"].(type) {
	case []any:
		if hasCacheControl(c) {
			return
		}
		if len(c) > 0 {
			if b, ok := c[len(c)-1].(map[string]any); ok {
				b["cache_control"] = map[string]any{"type": "ephemeral"}
			}
		}
	case string:
		if c != "" {
			last["content"] = []any{textBlock(c, true)}
		}
	}
}
