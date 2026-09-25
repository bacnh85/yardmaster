package server

import (
	"context"
	"sort"

	"github.com/bacnh85/yardmaster/internal/config"
)

// CatalogProvider names one provider serving a catalog model.
type CatalogProvider struct {
	Name    string `json:"name"`
	Wire    string `json:"wire"`
	Prefix  string `json:"prefix,omitempty"`
	Exposed bool   `json:"exposed"`
}

// CatalogRow is one deduped model across all enabled providers. Same id on two
// providers (or one gateway shared by two wire entries) merges into one row.
type CatalogRow struct {
	ModelMeta
	Providers []CatalogProvider `json:"providers"`
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
				row = &CatalogRow{ModelMeta: m, Providers: []CatalogProvider{}}
				byID[key] = row
				ids = append(ids, key)
			}
			row.Providers = append(row.Providers, CatalogProvider{
				Name: p.Name, Wire: p.Wire, Prefix: p.Prefix,
				Exposed: len(p.Models) == 0 || exposed[key],
			})
		}
	}
	sort.Strings(ids)
	out := make([]CatalogRow, 0, len(ids))
	for _, id := range ids {
		row := byID[id]
		if exposedOnly && !row.exposedAny() {
			continue
		}
		out = append(out, *row)
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
