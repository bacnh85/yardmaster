package server

import (
	"context"
	"sort"

	"github.com/bacnh85/yardmaster/internal/config"
)

// CatalogProvider names one provider serving a catalog model. ServedAs is the
// id a client must request to reach this model on this provider —
// "prefix/<id>" for prefixed providers (that's all they advertise), the bare
// id otherwise (wildcard providers serve unprefixed ids only). Input/Output/
// CacheRead are THIS provider's own catalog prices ($/Mtok, -1 unknown) — the
// same id can cost differently per provider (free tier vs paid proxy).
type CatalogProvider struct {
	Name      string  `json:"name"`
	Wire      string  `json:"wire"`
	Prefix    string  `json:"prefix,omitempty"`
	Exposed   bool    `json:"exposed"`
	ServedAs  string  `json:"served_as"`
	Input     float64 `json:"input"`
	Output    float64 `json:"output"`
	CacheRead float64 `json:"cache_read"`
}

// CatalogRow is one deduped model across all enabled providers. Same id on two
// providers (or one gateway shared by two wire entries) merges into one row.
// Combos lists combo/<name> ids that pool this model on any member.
type CatalogRow struct {
	ModelMeta
	Providers []CatalogProvider `json:"providers"`
	Combos    []string          `json:"combos"`
}

func providerByName(provs []*config.Provider, name string) *config.Provider {
	for _, p := range provs {
		if p.Name == name {
			return p
		}
	}
	return nil
}

// catalogRows merges every enabled provider's (warm-cached) catalog into
// deduped rows sorted by id. exposed mirrors the UI's visibility rule: the id
// is in the provider's models list, or the list is empty (wildcard = serves
// everything). exposedOnly=true drops rows no provider exposes.
func (s *Server) catalogRows(ctx context.Context, exposedOnly bool) []CatalogRow {
	byID := map[string]*CatalogRow{}
	ids := []string{}
	// combo/<name> ids per pooled model id — chat combos only; decision
	// combos never answer chat and stay off the models page.
	combosOf := map[string][]string{}
	for _, cb := range s.Proxy.Reg.Config().Combos {
		if s.Proxy.Reg.ClassifierCombo(cb) || cb == nil {
			continue
		}
		id := config.ComboID(cb.Name)
		for _, mem := range cb.Members {
			if mem == nil || mem.Model == "" {
				continue
			}
			k := canonModelID(mem.Model)
			combosOf[k] = append(combosOf[k], id)
		}
	}
	for _, p := range s.Proxy.Reg.Config().Providers {
		if p.Disabled {
			continue
		}
		metas, err := s.catalog(ctx, p.Name, false)
		if err != nil {
			continue
		}
		exposed := map[string]bool{}
		for _, id := range p.Models {
			exposed[canonModelID(id)] = true
		}
		for _, m := range metas {
			key := canonModelID(m.ID)
			row := byID[key]
			if row == nil {
				row = &CatalogRow{ModelMeta: m, Providers: []CatalogProvider{}, Combos: []string{}}
				byID[key] = row
				ids = append(ids, key)
			}
			row.Providers = append(row.Providers, CatalogProvider{
				Name: p.Name, Wire: p.Wire, Prefix: p.Prefix,
				Exposed:  len(p.Models) == 0 || exposed[key],
				ServedAs: servedAs(p.Prefix, m.ID),
				Input:    m.Input, Output: m.Output, CacheRead: m.CacheRead,
			})
		}
	}
	sort.Strings(ids)
	out := make([]CatalogRow, 0, len(ids))
	for _, id := range ids {
		row := byID[id]
		row.Combos = dedupeNonNil(combosOf[id])
		if exposedOnly && !row.exposedAny() {
			continue
		}
		out = append(out, *row)
	}
	return out
}

// servedAs mirrors provider.advertised: prefixed providers expose only
// "prefix/model"; unprefixed ones expose the bare id.
func servedAs(prefix, id string) string {
	if prefix == "" {
		return id
	}
	return prefix + "/" + id
}

// dedupeNonNil sorts+dereps combo ids (two members may pool the same model on
// the same combo) and keeps [] instead of nil for stable JSON.
func dedupeNonNil(in []string) []string {
	if len(in) == 0 {
		return []string{}
	}
	sort.Strings(in)
	out := in[:1]
	for _, v := range in[1:] {
		if v != out[len(out)-1] {
			out = append(out, v)
		}
	}
	return out
}

func (r *CatalogRow) exposedAny() bool {
	for _, p := range r.Providers {
		if p.Exposed {
			return true
		}
	}
	return false
}
