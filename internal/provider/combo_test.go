package provider

import (
	"testing"

	"github.com/bacnh85/yardmaster/internal/config"
)

// comboConfig: deepseek-v4.1-flash served by two providers (the OpenCode Go +
// Command Code pooling case) plus an uninvolved third.
func comboConfig() *config.Config {
	return &config.Config{
		Providers: []*config.Provider{
			{Name: "opencode-go", Wire: "openai", BaseURL: "https://zen",
				Models: []string{"deepseek-v4-flash"},
				Auth:   config.AuthConf{Type: "static", Keys: []string{"oc1", "oc2"}, KeyLabels: []string{"main", "laptop"}}},
			{Name: "cmdcode", Wire: "openai", BaseURL: "https://cmd",
				Models: []string{"deepseek/deepseek-v4.1-flash"},
				Auth:   config.AuthConf{Type: "static", Keys: []string{"cc1", "cc2"}}},
			{Name: "deepseek", Wire: "openai", BaseURL: "https://ds",
				Models: []string{"deepseek-flash"},
				Auth:   config.AuthConf{Type: "static", Keys: []string{"ds1"}}},
		},
		Combos: []*config.Combo{
			{Name: "deepseek-v4.1-flash", Model: "deepseek-v4.1-flash", Members: []*config.ComboMember{
				{Provider: "opencode-go", Model: "deepseek-v4-flash"},
				{Provider: "cmdcode", Model: "deepseek/deepseek-v4.1-flash"},
			}},
			{Name: "pinned", Model: "deepseek-v4.1-flash", Members: []*config.ComboMember{
				{Provider: "opencode-go", Model: "deepseek-v4-flash", Keys: []string{"laptop"}},
			}},
		},
		Keys: []*config.Key{{Key: "ar-x", Name: "pi", Allow: []string{"*"}}},
	}
}

func keysOf(tgts []*Target) []string {
	out := make([]string, 0, len(tgts))
	for _, tgt := range tgts {
		out = append(out, tgt.APIKey)
	}
	return out
}

func TestComboResolve(t *testing.T) {
	reg := New(comboConfig())
	tgts := reg.Resolve("combo/deepseek-v4.1-flash", []string{"*"})
	// priority: member order, all enabled keys of each member
	if !eq(names(tgts), []string{"opencode-go", "opencode-go", "cmdcode", "cmdcode"}) {
		t.Fatalf("combo targets: %v", names(tgts))
	}
	if !eq(keysOf(tgts), []string{"oc1", "oc2", "cc1", "cc2"}) {
		t.Fatalf("combo keys: %v", keysOf(tgts))
	}
	// every target dispatches its member's upstream model id, not the virtual one
	want := map[string]string{"opencode-go": "deepseek-v4-flash", "cmdcode": "deepseek/deepseek-v4.1-flash"}
	for _, tgt := range tgts {
		if tgt.ModelOverride != want[tgt.Provider.Name] {
			t.Fatalf("target %s: override %q, want %q", tgt.Provider.Name, tgt.ModelOverride, want[tgt.Provider.Name])
		}
	}
}

func TestComboKeySelection(t *testing.T) {
	reg := New(comboConfig())
	tgts := reg.Resolve("combo/pinned", []string{"*"})
	if !eq(names(tgts), []string{"opencode-go"}) || !eq(keysOf(tgts), []string{"oc2"}) {
		t.Fatalf("pinned combo: %v %v", names(tgts), keysOf(tgts))
	}
}

func TestComboUnknownAndAllow(t *testing.T) {
	reg := New(comboConfig())
	if tgts := reg.Resolve("combo/nope", []string{"*"}); tgts != nil {
		t.Fatalf("unknown combo resolved: %v", names(tgts))
	}
	// allow patterns gate the full virtual id, same as any model
	if tgts := reg.Resolve("combo/deepseek-v4.1-flash", []string{"other*"}); tgts != nil {
		t.Fatalf("combo passed deny-all allow: %v", names(tgts))
	}
	if tgts := reg.Resolve("combo/deepseek-v4.1-flash", []string{"combo*"}); len(tgts) == 0 {
		t.Fatal("combo* allow should admit combo ids")
	}
}

func TestComboWeightedRR(t *testing.T) {
	cfg := comboConfig()
	cfg.Combos[0].Strategy = "weighted-rr"
	cfg.Combos[0].Members[0].Weight = 3
	cfg.Combos[0].Members[1].Weight = 1
	reg := New(cfg)
	counts := map[string]int{}
	for i := 0; i < 8; i++ {
		tgts := reg.Resolve("combo/deepseek-v4.1-flash", []string{"*"})
		counts[names(tgts)[0]]++
	}
	if counts["opencode-go"] != 6 || counts["cmdcode"] != 2 {
		t.Fatalf("weighted-rr heads over 8 reqs (3:1): %v", counts)
	}
}

func TestComboModelsAdvertised(t *testing.T) {
	reg := New(comboConfig())
	found := false
	for _, m := range reg.Models() {
		if m == "combo/deepseek-v4.1-flash" {
			found = true
		}
	}
	if !found {
		t.Fatal("combo id missing from /v1/models")
	}
}

// A decision combo must resolve to its classifier providers (member ids as
// overrides) and stay OUT of the chat /v1/models advertisement.
func TestClassifierComboResolveAndAdvertisement(t *testing.T) {
	cfg := comboConfig()
	cfg.Providers = append(cfg.Providers,
		&config.Provider{Name: "typesafe", Wire: "classifier", BaseURL: "https://ts", Models: []string{"jev-latest"},
			Auth: config.AuthConf{Type: "static", Keys: []string{"ts1"}}},
		&config.Provider{Name: "or-clf", Wire: "classifier", BaseURL: "https://or", Models: []string{"typesafe/jev-1.13"},
			Auth: config.AuthConf{Type: "static", Keys: []string{"or1", "or2"}}})
	cfg.Combos = append(cfg.Combos, &config.Combo{Name: "jev", Type: "decision", Members: []*config.ComboMember{
		{Provider: "typesafe", Model: "jev-latest"},
		{Provider: "or-clf", Model: "typesafe/jev-1.13"},
	}})
	reg := New(cfg)

	tgts := reg.Resolve("combo/jev", []string{"*"})
	gotNames, gotModels := []string{}, []string{}
	for _, tgt := range tgts {
		gotNames = append(gotNames, tgt.Provider.Name)
		gotModels = append(gotModels, tgt.ModelOverride)
	}
	if !eq(gotNames, []string{"typesafe", "or-clf", "or-clf"}) || !eq(gotModels, []string{"jev-latest", "typesafe/jev-1.13", "typesafe/jev-1.13"}) {
		t.Fatalf("classifier combo targets: %v %v", gotNames, gotModels)
	}
	for _, m := range reg.Models() {
		if m == "combo/jev" {
			t.Fatal("classifier combo leaked into chat /v1/models")
		}
	}
}

func TestComboDisabledMemberSkipped(t *testing.T) {
	cfg := comboConfig()
	cfg.Providers[1].Disabled = true
	reg := New(cfg)
	tgts := reg.Resolve("combo/deepseek-v4.1-flash", []string{"*"})
	if !eq(names(tgts), []string{"opencode-go", "opencode-go"}) {
		t.Fatalf("disabled member not skipped: %v", names(tgts))
	}
}

// Blank weights must not starve a member: wrrPick never picks x < 0, so a
// literal 0 head would serve 0% — the UI weight field is optional per row.
func TestComboWeightedRRBlankWeight(t *testing.T) {
	cfg := comboConfig()
	cfg.Combos[0].Strategy = "weighted-rr"
	cfg.Combos[0].Members[0].Weight = 3
	// Members[1].Weight stays 0
	reg := New(cfg)
	counts := map[string]int{}
	for i := 0; i < 8; i++ {
		tgts := reg.Resolve("combo/deepseek-v4.1-flash", []string{"*"})
		counts[names(tgts)[0]]++
	}
	if counts["cmdcode"] == 0 {
		t.Fatalf("zero-weight member starved: %v", counts)
	}
	if counts["opencode-go"]+counts["cmdcode"] != 8 || counts["cmdcode"] > 4 {
		t.Fatalf("spread not ~3:1: %v", counts)
	}
}
