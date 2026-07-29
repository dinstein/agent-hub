package router

import (
	"slices"

	"github.com/dinstein/agent-hub/internal/downstream"
)

// Catalog is the tool-directory snapshot consumed by internal/scope
// (canonical.md A.4 ruling: the tool-directory snapshot is router.Catalog — distinct from
// internal/catalog, which is the curated server directory): every currently
// known server mapped to its ORIGINAL (raw) tool names. Exposed names never
// appear here — scope intersections are keyed by original names only
// (docs/architecture.md §7, invariant 1).
//
// Invariant: Servers values are sorted and deduplicated when built through
// NewCatalog / CatalogOf. A Catalog is an immutable snapshot; callers must
// not mutate it after construction.
type Catalog struct {
	Servers map[string][]string // serverID -> sorted raw tool names
}

// NewCatalog builds a Catalog from raw tool names per server. The input is
// copied, sorted and deduplicated; the caller keeps ownership of its maps.
func NewCatalog(servers map[string][]string) Catalog {
	out := make(map[string][]string, len(servers))
	for id, tools := range servers {
		cp := make([]string, len(tools))
		copy(cp, tools)
		slices.Sort(cp)
		out[id] = slices.Compact(cp)
	}
	return Catalog{Servers: out}
}

// CatalogOf snapshots the current Tools() of live servers. Nil entries are
// skipped (a vanished server simply contributes nothing — the scope layer
// treats absence as invisible, which is the closed direction).
func CatalogOf(servers []*downstream.Server) Catalog {
	m := make(map[string][]string, len(servers))
	for _, srv := range servers {
		if srv == nil {
			continue
		}
		defs := srv.Tools()
		names := make([]string, 0, len(defs))
		for _, d := range defs {
			names = append(names, d.Name)
		}
		m[srv.ID()] = names
	}
	return NewCatalog(m)
}
