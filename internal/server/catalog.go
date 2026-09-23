package server

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/bacnh85/yardmaster/internal/config"
	"github.com/bacnh85/yardmaster/internal/proxy"
)

// ModelMeta is one catalog entry: upstream id + enrichment metadata.
type ModelMeta struct {
	ID         string  `json:"id"`
	Name       string  `json:"name,omitempty"`
	Family     string  `json:"family"` // chat | anthropic | responses | gemini
	Context    int     `json:"context,omitempty"`
	MaxOutput  int     `json:"max_output,omitempty"`
	Input      float64 `json:"input"`      // $/Mtok
	Output     float64 `json:"output"`     // $/Mtok
	CacheRead  float64 `json:"cache_read"` // $/Mtok
	CacheWrite float64 `json:"cache_write"`
	Reasoning  bool    `json:"reasoning,omitempty"`
	ToolCall   bool    `json:"tool_call,omitempty"`
	Image      bool    `json:"image,omitempty"`
	Free       bool    `json:"free,omitempty"`
	// Manual marks a curated id the upstream /models did not list (added by
	// hand in the dashboard) — the UI badges it and skips plan-tier filters.
	Manual bool `json:"manual,omitempty"`
}

// modelsDevURL is a var so tests can stub it.
var modelsDevURL = "https://models.dev/api.json"

var (
	modelsDevMu    sync.Mutex
	modelsDevData  map[string]modelsDevProvider
	modelsDevUntil time.Time
	modelsDevFlat  map[string]modelsDevModel // canon id → meta across ALL providers
)

type modelsDevModel struct {
	Name        string `json:"name"`
	Reasoning   bool   `json:"reasoning"`
	ToolCall    bool   `json:"tool_call"`
	Modalities  map[string][]string
	Limit       map[string]int
	Cost        flexCost
	NPMProvider struct {
		NPM string `json:"npm"`
	} `json:"provider"`
}

// flexCost ignores non-numeric entries in a models.dev cost map — some models
// carry "tiers" arrays or "context_over_200k" objects alongside input/output,
// which would fail a plain map[string]float64 and drop the model's metadata.
type flexCost map[string]float64

func (c *flexCost) UnmarshalJSON(b []byte) error {
	raw := map[string]json.RawMessage{}
	if err := json.Unmarshal(b, &raw); err != nil {
		return err
	}
	*c = flexCost{}
	for k, v := range raw {
		var f float64
		if json.Unmarshal(v, &f) == nil {
			(*c)[k] = f
		}
	}
	return nil
}

type modelsDevProvider struct {
	Name   string                     `json:"name"`
	Models map[string]json.RawMessage `json:"models"` // some entries are not objects; skip on unmarshal
}

func modelsDevProviderID(baseURL string) string {
	b := strings.ToLower(baseURL)
	switch {
	case strings.Contains(b, "opencode.ai"):
		if strings.Contains(b, "/go") {
			return "opencode-go" // zen/go subscription has its own models.dev entry with Go-specific pricing
		}
		return "opencode"
	case strings.Contains(b, "deepseek"):
		return "deepseek"
	case strings.Contains(b, "z.ai"):
		return "zai"
	case strings.Contains(b, "bigmodel"):
		return "zhipuai"
	case strings.Contains(b, "openrouter"):
		return "openrouter"
	}
	return ""
}

// loadModelsDev fetches + parses models.dev (1 h cache). nil on any failure —
// the catalog degrades to upstream ids only.
func loadModelsDev() map[string]modelsDevProvider {
	modelsDevMu.Lock()
	defer modelsDevMu.Unlock()
	if modelsDevData != nil && time.Now().Before(modelsDevUntil) {
		return modelsDevData
	}
	client := &http.Client{Timeout: 15 * time.Second}
	resp, err := client.Get(modelsDevURL)
	if err != nil {
		return nil
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return nil
	}
	var data map[string]modelsDevProvider
	if err := json.NewDecoder(io.LimitReader(resp.Body, 64<<20)).Decode(&data); err != nil {
		return nil
	}
	flat := make(map[string]modelsDevModel)
	// sorted provider order: a model id under multiple vendors must resolve to
	// the same entry on every run (map iteration order is randomized)
	ids := make([]string, 0, len(data))
	for id := range data {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, pid := range ids {
		pv := data[pid]
		for id, raw := range pv.Models {
			var m modelsDevModel
			if json.Unmarshal(raw, &m) == nil {
				if _, dup := flat[canonModelID(id)]; !dup {
					flat[canonModelID(id)] = m
				}
			}
		}
	}
	modelsDevData, modelsDevUntil = data, time.Now().Add(time.Hour)
	modelsDevFlat = flat
	return modelsDevData
}

// modelsDevSnapshot returns the parsed models.dev data + cross-provider flat
// index under the mutex — enrichCatalog runs concurrently (admin handler +
// WarmCatalogs), so the package vars must never be read unlocked.
func modelsDevSnapshot() (map[string]modelsDevProvider, map[string]modelsDevModel) {
	loadModelsDev() // fetch/refresh when stale (locks internally)
	modelsDevMu.Lock()
	defer modelsDevMu.Unlock()
	return modelsDevData, modelsDevFlat
}

// modelsDevCached returns whatever models.dev data is already loaded WITHOUT
// fetching — for hot paths (/v1/models) that must never block on a network
// round trip. A cold/stale snapshot yields nil (callers degrade to un-enriched
// ids); loadModelsDev has no negative cache, so triggering it here would retry
// a 15s fetch on every request, serialized behind modelsDevMu.
func modelsDevCached() (map[string]modelsDevProvider, map[string]modelsDevModel) {
	modelsDevMu.Lock()
	defer modelsDevMu.Unlock()
	return modelsDevData, modelsDevFlat
}

// applyDev merges one models.dev entry into a ModelMeta. withFamily=false is
// the cross-provider fallback: newer models are filed under their vendor, not
// the router's provider — take limits/pricing/flags but NEVER family, which
// drives UI wire grouping and must reflect the actual serving wire.
func applyDev(mm *ModelMeta, dm modelsDevModel, withFamily bool) {
	mm.Name = dm.Name
	mm.Context = dm.Limit["context"]
	mm.MaxOutput = dm.Limit["output"]
	mm.Input = dm.Cost["input"]
	mm.Output = dm.Cost["output"]
	mm.CacheRead = dm.Cost["cache_read"]
	mm.CacheWrite = dm.Cost["cache_write"]
	mm.Reasoning = dm.Reasoning
	mm.ToolCall = dm.ToolCall
	mm.Image = slices.Contains(dm.Modalities["input"], "image")
	if withFamily {
		mm.Family = familyFromNPM(dm.NPMProvider.NPM)
	}
	mm.Free = mm.Input == 0 && mm.Output == 0
}

// familyFromNPM maps the models.dev AI-SDK package to a wire family.
// The table is verified live per family before the UI trusts the pre-filter
// (see plan verification step 3).
func familyFromNPM(npm string) string {
	switch npm {
	case "@ai-sdk/anthropic":
		return "anthropic"
	case "@ai-sdk/openai":
		return "responses"
	case "@ai-sdk/google":
		return "gemini"
	default:
		return "chat"
	}
}

type catalogEntry struct {
	models []ModelMeta
	until  time.Time
}

var catalogCache sync.Map // provider base_url -> catalogEntry

// catalog returns the model catalog for one provider: upstream /models ids
// enriched with models.dev metadata, plus the provider's curated ids the
// upstream omits (manually added models) enriched the same way. Never fails
// hard — falls back to the provider's configured model list when /models is
// unavailable.
func (s *Server) catalog(ctx context.Context, name string) ([]ModelMeta, error) {
	var pv *config.Provider
	for _, x := range s.Proxy.Reg.Config().Providers {
		if x.Name == name {
			pv = x
			break
		}
	}
	if pv == nil {
		return nil, fmt.Errorf("no provider %q", name)
	}
	var cached []ModelMeta
	if e, ok := catalogCache.Load(pv.BaseURL); ok {
		ce := e.(catalogEntry)
		if time.Now().Before(ce.until) {
			cached = ce.models
		}
	}
	if cached == nil {
		ups := s.upstreamModels(ctx, pv)
		if len(ups) == 0 {
			// last resort when /models and models.dev both fail; fresh slice —
			// enrichCatalog sorts in place and the config Models list is live
			ups = make([]ModelMeta, 0, len(pv.Models))
			for _, id := range pv.Models {
				ups = append(ups, ModelMeta{ID: id})
			}
		}
		enrichCatalog(pv.BaseURL, ups)
		out, _ := catalogCache.Load(pv.BaseURL)
		cached = out.(catalogEntry).models
	}
	// curated ids missing from the cached catalog: appended at read time, not
	// cached — the cache is keyed by base_url and SHARED by providers over one
	// gateway (cmdcode + cmdcode-claude), so per-provider curation must stay out
	// of it. Copy before appending: never grow the cached array in place.
	dev, devFlat := modelsDevSnapshot()
	extra := curatedMetas(pv, cached, dev, devFlat)
	if len(extra) == 0 {
		return cached, nil
	}
	metas := make([]ModelMeta, 0, len(cached)+len(extra))
	metas = append(metas, cached...)
	return append(metas, extra...), nil
}

// curatedMetas enriches the provider's curated ids that the cached catalog
// doesn't list — manually added models get the same models.dev metadata
// (context, pricing, capabilities) as upstream-listed ones. Family is never
// taken from models.dev here: the curating entry's wire decides it.
func curatedMetas(pv *config.Provider, cached []ModelMeta, dev map[string]modelsDevProvider, devFlat map[string]modelsDevModel) []ModelMeta {
	known := make(map[string]bool, len(cached))
	for _, m := range cached {
		known[canonModelID(m.ID)] = true
	}
	var prov modelsDevProvider
	if dev != nil {
		prov = dev[modelsDevProviderID(pv.BaseURL)]
	}
	byID := make(map[string]modelsDevModel, len(prov.Models))
	for id, raw := range prov.Models {
		var m modelsDevModel
		if json.Unmarshal(raw, &m) == nil {
			byID[canonModelID(id)] = m
		}
	}
	var out []ModelMeta
	for _, id := range pv.Models {
		if known[canonModelID(id)] {
			continue // upstream already lists it (or the curated list repeats it)
		}
		known[canonModelID(id)] = true
		// -1 = unknown pricing, never 0 (fmtPrice renders 0 as "free"): a cold
		// models.dev snapshot must degrade to "—", not to a free claim
		mm := ModelMeta{ID: id, Manual: true, Input: -1, Output: -1}
		if dm, ok := byID[canonModelID(id)]; ok {
			applyDev(&mm, dm, false)
		} else if dm, ok := devFlat[canonModelID(id)]; ok {
			applyFlat(&mm, dm)
		} else if i := strings.LastIndex(id, "/"); i >= 0 {
			// vendor-namespaced id: price via the bare id under any vendor
			if dm, ok := devFlat[canonModelID(id[i+1:])]; ok {
				applyFlat(&mm, dm)
			}
		}
		out = append(out, mm)
	}
	return out
}

// upstreamModels GETs {base}/models with the first key (tolerates failure).
// Carries per-model wire info when the upstream reports it
// (supported_endpoints, e.g. CommandCode's ["/messages"] → anthropic).
func (s *Server) upstreamModels(ctx context.Context, p *config.Provider) []ModelMeta {
	url := strings.TrimRight(p.BaseURL, "/") + "/models"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil
	}
	if len(p.Auth.Keys) > 0 {
		proxy.SetAuth(req, p.Wire, p.Auth.Keys[0]) // wire-appropriate auth, same as dispatch
	}
	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return nil
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return nil
	}
	var body struct {
		Data []struct {
			ID                 string   `json:"id"`
			Name               string   `json:"name"`
			ContextLength      int      `json:"context_length"`
			SupportedEndpoints []string `json:"supported_endpoints"`
			Pricing            struct {
				Prompt         flexString
				Completion     flexString
				InputCacheRead flexString `json:"input_cache_read"`
			}
			TopProvider struct {
				MaxCompletionTokens int `json:"max_completion_tokens"`
			} `json:"top_provider"`
			Architecture struct {
				InputModalities []string `json:"input_modalities"`
			} `json:"architecture"`
			SupportedParameters []string `json:"supported_parameters"`
		} `json:"data"`
	}
	if json.NewDecoder(io.LimitReader(resp.Body, 16<<20)).Decode(&body) != nil {
		return nil
	}
	out := make([]ModelMeta, 0, len(body.Data))
	for _, m := range body.Data {
		if m.ID == "" {
			continue
		}
		mm := ModelMeta{ID: m.ID, Name: m.Name, Context: m.ContextLength}
		if len(m.SupportedEndpoints) > 0 {
			switch {
			case len(m.SupportedEndpoints) == 1 && m.SupportedEndpoints[0] == "/messages":
				mm.Family = "anthropic"
			case len(m.SupportedEndpoints) == 1 && m.SupportedEndpoints[0] == "/responses":
				mm.Family = "responses"
			default:
				mm.Family = "chat"
			}
		}
		// OpenRouter-style live pricing: per-token strings → $/Mtok. Gated on
		// the pricing object being present — providers whose /models carries no
		// pricing (CommandCode) must not flip Free or zero models.dev prices in
		// enrichCatalog. -1 = BYO-key (unmanaged) → the -1 unknown-price
		// convention, never 0/"free".
		if m.Pricing.Prompt != "" || m.Pricing.Completion != "" {
			mm.Input = tokToMtok(m.Pricing.Prompt)
			mm.Output = tokToMtok(m.Pricing.Completion)
			mm.CacheRead = tokToMtok(m.Pricing.InputCacheRead)
			mm.Free = mm.Input == 0 && mm.Output == 0
		}
		mm.MaxOutput = m.TopProvider.MaxCompletionTokens
		mm.Image = slices.Contains(m.Architecture.InputModalities, "image")
		mm.Reasoning = slices.Contains(m.SupportedParameters, "reasoning")
		mm.ToolCall = slices.Contains(m.SupportedParameters, "tool_choice")
		out = append(out, mm)
	}
	return out
}

// flexString accepts the per-token pricing strings OpenRouter /models emits
// ("-1", "0.0000007") without failing whole-payload decode.
type flexString string

func (f *flexString) UnmarshalJSON(b []byte) error {
	*f = flexString(strings.Trim(string(b), `"`))
	return nil
}

// tokToMtok converts a $/token price string to $/Mtok; -1 (unmanaged) and
// unparsable values map to the -1 unknown-price convention.
func tokToMtok(s flexString) float64 {
	f, err := strconv.ParseFloat(string(s), 64)
	if err != nil {
		return 0
	}
	if f < 0 {
		return -1
	}
	return f * 1_000_000
}

// enrichCatalog merges models.dev metadata into upstream metas (or fabricates
// the id list from models.dev when /models was unavailable) and caches.
func enrichCatalog(baseURL string, ups []ModelMeta) {
	dev, devFlat := modelsDevSnapshot()
	var prov modelsDevProvider
	if dev != nil {
		if id := modelsDevProviderID(baseURL); id != "" {
			prov = dev[id]
		}
	}
	if len(ups) == 0 {
		for id := range prov.Models {
			ups = append(ups, ModelMeta{ID: id})
		}
	}
	sort.Slice(ups, func(i, j int) bool { return ups[i].ID < ups[j].ID })
	// upstream and models.dev disagree on dot/dash notation for the same
	// model (claude-haiku-4-5 vs claude-haiku-4.5) — join on a canonical key.
	devByID := make(map[string]modelsDevModel, len(prov.Models))
	for id, raw := range prov.Models {
		var m modelsDevModel
		if json.Unmarshal(raw, &m) == nil {
			devByID[canonModelID(id)] = m
		}
	}
	out := make([]ModelMeta, 0, len(ups))
	for _, up := range ups {
		mm := ModelMeta{ID: up.ID}
		if dm, ok := devByID[canonModelID(up.ID)]; ok {
			applyDev(&mm, dm, true)
		} else if dm, ok := devFlat[canonModelID(up.ID)]; ok {
			applyFlat(&mm, dm)
		} else if i := strings.LastIndex(up.ID, "/"); i >= 0 {
			// vendor-namespaced id (zai-org/GLM-5.3): price via the bare id
			// under any vendor — display metadata only, never family
			if dm, ok := devFlat[canonModelID(up.ID[i+1:])]; ok {
				applyFlat(&mm, dm)
			}
		}
		// upstream-reported metadata wins (it's the serving provider);
		// models.dev fills only what upstream doesn't report. Upstream pricing
		// must be non-zero to win — 0 means the upstream reported no pricing
		// (zeroing models.dev prices would flip paid models to "free"). -1
		// upstream (BYO-key) also wins: never overwrite unknown with unknown.
		if up.Name != "" {
			mm.Name = up.Name
		}
		if up.Context > 0 {
			mm.Context = up.Context
		}
		if up.MaxOutput > 0 {
			mm.MaxOutput = up.MaxOutput
		}
		if up.Input != 0 {
			mm.Input = up.Input
		}
		if up.Output != 0 {
			mm.Output = up.Output
		}
		if up.CacheRead > 0 {
			mm.CacheRead = up.CacheRead
		}
		if up.Image {
			mm.Image = true
		}
		if up.Reasoning {
			mm.Reasoning = true
		}
		if up.ToolCall {
			mm.ToolCall = true
		}
		if up.Free {
			mm.Free = true
		}
		// upstream -1 pricing (BYO-key) must never coexist with a models.dev
		// free flag: unknown price is not a free model
		if mm.Input < 0 || mm.Output < 0 {
			mm.Free = false
		}
		if up.Family != "" {
			mm.Family = up.Family // endpoint-reported wire beats the npm guess
		}
		out = append(out, mm)
	}
	ttl := time.Hour
	if len(out) == 0 {
		// empty catalog (transient upstream failure) — retry soon instead of
		// sticking to 0 models for an hour
		ttl = 2 * time.Minute
	}
	catalogCache.Store(baseURL, catalogEntry{models: out, until: time.Now().Add(ttl)})
}

// applyFlat merges a cross-provider models.dev entry: limits/pricing/flags but
// never family, and 0/0 "cost" usually means a subscription bundle (token-plan
// vendors list cost 0), not a free model — price as unknown (-1); the UI shows
// "—" and never "free".
func applyFlat(mm *ModelMeta, dm modelsDevModel) {
	applyDev(mm, dm, false)
	if mm.Input == 0 && mm.Output == 0 {
		mm.Input, mm.Output = -1, -1
		mm.Free = false
	}
}

func canonModelID(id string) string {
	return strings.ReplaceAll(strings.ToLower(id), ".", "-")
}

// WarmCatalogs fetches every enabled provider's catalog, then refreshes on a
// ticker so /v1/models enrichment and the detail page never start cold.
func (s *Server) WarmCatalogs(ctx context.Context) {
	for {
		for _, p := range s.Proxy.Reg.Config().Providers {
			if p.Disabled {
				continue
			}
			_, _ = s.catalog(ctx, p.Name) // best-effort; catalog() never fails hard
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(30 * time.Minute): // TTL is 1h; tick at half-life
		}
	}
}
