package server

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"slices"
	"sort"
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
// enriched with models.dev metadata. Never fails hard — falls back to the
// provider's configured model list when /models is unavailable.
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
	if e, ok := catalogCache.Load(pv.BaseURL); ok {
		ce := e.(catalogEntry)
		if time.Now().Before(ce.until) {
			return ce.models, nil
		}
	}
	ids := s.upstreamModelIDs(ctx, pv)
	if len(ids) == 0 {
		// copy: enrichCatalog sorts in place, and this slice is the live config
		// shared with routing and config saves
		ids = append([]string(nil), pv.Models...) // last resort when /models and models.dev both fail
	}
	enrichCatalog(pv.BaseURL, ids)
	out, _ := catalogCache.Load(pv.BaseURL)
	return out.(catalogEntry).models, nil
}

// upstreamModelIDs GETs {base}/models with the first key (tolerates failure).
func (s *Server) upstreamModelIDs(ctx context.Context, p *config.Provider) []string {
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
			ID string `json:"id"`
		} `json:"data"`
	}
	if json.NewDecoder(io.LimitReader(resp.Body, 16<<20)).Decode(&body) != nil {
		return nil
	}
	ids := make([]string, 0, len(body.Data))
	for _, m := range body.Data {
		if m.ID != "" {
			ids = append(ids, m.ID)
		}
	}
	return ids
}

// enrichCatalog merges models.dev metadata into upstream ids (or fabricates
// the id list from models.dev when /models was unavailable) and caches.
func enrichCatalog(baseURL string, ids []string) {
	dev, devFlat := modelsDevSnapshot()
	var prov modelsDevProvider
	if dev != nil {
		if id := modelsDevProviderID(baseURL); id != "" {
			prov = dev[id]
		}
	}
	if len(ids) == 0 {
		for id := range prov.Models {
			ids = append(ids, id)
		}
	}
	sort.Strings(ids)
	// upstream and models.dev disagree on dot/dash notation for the same
	// model (claude-haiku-4-5 vs claude-haiku-4.5) — join on a canonical key.
	devByID := make(map[string]modelsDevModel, len(prov.Models))
	for id, raw := range prov.Models {
		var m modelsDevModel
		if json.Unmarshal(raw, &m) == nil {
			devByID[canonModelID(id)] = m
		}
	}
	out := make([]ModelMeta, 0, len(ids))
	for _, id := range ids {
		mm := ModelMeta{ID: id}
		if dm, ok := devByID[canonModelID(id)]; ok {
			applyDev(&mm, dm, true)
		} else if dm, ok := devFlat[canonModelID(id)]; ok {
			applyDev(&mm, dm, false)
			if mm.Input == 0 && mm.Output == 0 {
				// cross-provider 0/0 "cost" usually means a subscription bundle
				// (token-plan vendors list cost 0), not a free model — price as
				// unknown (-1); the UI shows "—" and never "free"
				mm.Input, mm.Output = -1, -1
				mm.Free = false
			}
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
