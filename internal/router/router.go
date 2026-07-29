// Package router aggregates the tools of many downstream servers into one
// namespaced catalog (task M0-7 minimal version).
//
// Exposed names are sanitize(serverID) + "__" + sanitize(rawTool), with
// deterministic _2/_3 suffixes on collisions. The mapping back to
// (server, raw tool) is RouteOf — the ONLY legitimate reverse lookup. The
// exposed name is an opaque handle: because serverIDs and tool names may
// themselves contain "__", splitting an exposed name on "__" is ambiguous
// and therefore forbidden anywhere in the codebase; this package never
// does it (everything goes through the map built at Build time).
//
// Build output is deterministic: the same servers/tools/policy always
// produce the same exposed names in the same List order (golden-tested —
// determinism is contract, canonical.md §6).
//
// Besides downstream servers, a build may include host-served Providers
// (the "skills" pseudo-server of docs/modules/config.md). They aggregate under the
// same rules and are distinguished only by LookupProvider, so scope
// projection, RouteOf provenance and the execute pipeline treat them as
// ordinary servers.
//
// Policy carries the two deny sets the aggregation step enforces —
// Disabled (the operator kill switch) and Quarantined (the integrity
// isolation set). Allow / DenyDestructive and the per-client/session View
// layer are still seams.
package router

import (
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"

	"github.com/dinstein/agent-hub/internal/downstream"
	"github.com/dinstein/agent-hub/internal/mcp"
)

// Provider is a tool source the HOST serves itself, with no downstream
// connection behind it: the "skills" pseudo-server of docs/modules/config.md.
//
// It aggregates exactly like a real server — same exposed-name rules, same
// collision suffixes, same RouteOf provenance, same Catalog projection —
// which is the whole point. A provider tool is therefore subject to the
// same scope intersection and, because the assembling gateway routes it
// into the identical pipeline.Execute, to the same gate chain. There is no
// second governance surface; only the transport differs.
//
// ID must not collide with a registry server id. On collision the provider
// LOSES (build reports an error), because a configured server is the thing
// the operator can see and edit.
type Provider interface {
	// ID is the pseudo-server id used as the exposed-name prefix and as the
	// scope key.
	ID() string
	// Tools lists the currently offered tools under their RAW names. It is
	// called on every catalog build and must be cheap and non-blocking
	// (snapshot, never I/O).
	Tools() []mcp.ToolDef
	// Call executes one tool by RAW name. Implementations answer errors as
	// an is-error CallResult where that is the honest shape; a returned
	// error becomes a JSON-RPC error upstream.
	Call(ctx context.Context, rawTool string, args json.RawMessage) (*mcp.CallResult, error)
}

// Route names the real (server, raw tool) pair behind an exposed name.
type Route struct {
	ServerID string
	RawTool  string
}

// Policy is the tool policy applied at AGGREGATION time. Both sets below
// remove a tool from the catalog outright — not listed, not searchable, not
// describable, not routable — which is why they are enforced here and not
// as a gate: a gate would leave the name visible and would have to be
// reproduced in every discovery mode. The struct is still the seam for the
// remaining fields (Allow lists, DenyDestructive).
//
// The two sets are keyed differently ON PURPOSE, each matching the store
// that produces it (internal/integrity):
//
//   - Disabled is keyed by the RAW downstream tool name, like the approval
//     record it comes from, so a downstream that renames a tool cannot move
//     it out from under its own kill switch.
//   - Quarantined is keyed by the CLIENT-VISIBLE exposed name, like the
//     quarantine entry it comes from (integrity doc.go, #423): quarantine
//     tracks what an agent could actually call.
//
// Fail direction of the CALLER, stated here because this struct is where it
// becomes visible: a zero Policy means "nothing is denied". Anyone building
// a Policy from a store must therefore never turn a read failure into a
// zero Policy — an unreadable deny set has to keep (or widen) the previous
// denial, never clear it (fail-closed).
type Policy struct {
	// Disabled maps serverID → raw tool name → true. Disabled tools are
	// excluded from aggregation entirely: not listed, not routable.
	Disabled map[string]map[string]bool
	// Quarantined maps the EXPOSED name → true. Because the exposed name
	// only exists after collision suffixes are assigned, quarantined entries
	// are dropped at the end of the build rather than skipped at the start:
	// isolating one tool must not renumber the names of the tools it
	// collided with (an agent's other tools cannot silently change identity
	// because a neighbour was quarantined).
	Quarantined map[string]bool
}

// entry is one aggregated tool. Exactly one of srv / prov is set for a
// callable entry; a cache-built entry has neither (listable, not callable).
type entry struct {
	route Route
	def   mcp.ToolDef // Name rewritten to the exposed name
	srv   *downstream.Server
	prov  Provider
}

// Router is an immutable snapshot of the aggregated catalog. Rebuild and
// atomically swap the pointer on change (docs/architecture.md §2: snapshot reads, no
// locks); a Router itself is safe for concurrent use.
type Router struct {
	byExposed map[string]entry
	ordered   []string // exposed names, sorted — the List order
}

// source is one server's tool contribution to a build: a live server, a
// host-served provider, or a cache-only snapshot (both nil).
type source struct {
	id    string
	tools []mcp.ToolDef
	srv   *downstream.Server // nil for cache-only and provider sources
	prov  Provider           // nil for downstream and cache-only sources
}

// Build aggregates the current Tools() of the given servers under pol.
// Servers must be non-nil with unique, non-empty IDs.
func Build(servers []*downstream.Server, pol Policy) (*Router, error) {
	return BuildWith(servers, nil, pol)
}

// BuildWith is Build plus host-served providers (docs/modules/config.md). Providers
// are appended AFTER the servers so a provider id colliding with a server
// id is reported as the duplicate it is — the configured server wins,
// deterministically, instead of the aggregation order deciding.
func BuildWith(servers []*downstream.Server, providers []Provider, pol Policy) (*Router, error) {
	sources := make([]source, 0, len(servers)+len(providers))
	for _, srv := range servers {
		if srv == nil {
			return nil, fmt.Errorf("router: nil server")
		}
		sources = append(sources, source{id: srv.ID(), tools: srv.Tools(), srv: srv})
	}
	sources = append(sources, providerSources(providers)...)
	return build(sources, pol)
}

// providerSources snapshots each provider's tool list once per build.
func providerSources(providers []Provider) []source {
	out := make([]source, 0, len(providers))
	for _, p := range providers {
		if p == nil {
			continue
		}
		out = append(out, source{id: p.ID(), tools: p.Tools(), prov: p})
	}
	return out
}

// BuildFromCache aggregates persisted tool definitions (the gateway's disk
// tool cache) under the exact same exposed-naming and ordering rules as
// Build, so a cache-served tools/list can never drift from the live one.
// Lookup on the result reports a nil server: cache entries are listable and
// routable but not callable.
func BuildFromCache(cached map[string][]mcp.ToolDef, pol Policy) (*Router, error) {
	return BuildFromCacheWith(cached, nil, pol)
}

// BuildFromCacheWith is BuildFromCache plus host-served providers. A
// provider is live even while every downstream is still connecting — it has
// nothing to connect to — so the cold catalog can already list and CALL its
// tools.
func BuildFromCacheWith(cached map[string][]mcp.ToolDef, providers []Provider, pol Policy) (*Router, error) {
	ids := make([]string, 0, len(cached))
	for id := range cached {
		ids = append(ids, id)
	}
	slices.Sort(ids)
	sources := make([]source, 0, len(ids))
	for _, id := range ids {
		sources = append(sources, source{id: id, tools: cached[id]})
	}
	sources = append(sources, providerSources(providers)...)
	return build(sources, pol)
}

// build is the shared aggregation core of Build / BuildFromCache.
func build(sources []source, pol Policy) (*Router, error) {
	type cand struct {
		route Route
		def   mcp.ToolDef
		srv   *downstream.Server
		prov  Provider
	}
	seen := make(map[string]bool, len(sources))
	groups := make(map[string][]cand)
	for _, src := range sources {
		id := src.id
		if id == "" {
			return nil, fmt.Errorf("router: server with empty ID")
		}
		if seen[id] {
			return nil, fmt.Errorf("router: duplicate server ID %q", id)
		}
		seen[id] = true
		disabled := pol.Disabled[id]
		for _, def := range src.tools {
			if disabled[def.Name] {
				continue
			}
			base := sanitize(id) + "__" + sanitize(def.Name)
			groups[base] = append(groups[base], cand{
				route: Route{ServerID: id, RawTool: def.Name},
				def:   def,
				srv:   src.srv,
				prov:  src.prov,
			})
		}
	}

	// Deterministic assignment: bases in sorted order; within a colliding
	// group, suffixes _2/_3/… are assigned in raw-name order (raw tool
	// name, then serverID as tiebreaker). A suffixed name that is itself
	// already taken (e.g. group "x" produced "x_2" and a base "x_2" also
	// exists) keeps scanning upward — order stays fully deterministic.
	bases := make([]string, 0, len(groups))
	for b := range groups {
		bases = append(bases, b)
	}
	slices.Sort(bases)

	taken := make(map[string]bool)
	byExposed := make(map[string]entry)
	for _, base := range bases {
		g := groups[base]
		slices.SortFunc(g, func(a, b cand) int {
			return cmp.Or(cmp.Compare(a.route.RawTool, b.route.RawTool), cmp.Compare(a.route.ServerID, b.route.ServerID))
		})
		for i, c := range g {
			name := ""
			if i == 0 && !taken[base] {
				name = base
			} else {
				for n := 2; ; n++ {
					if cand := fmt.Sprintf("%s_%d", base, n); !taken[cand] {
						name = cand
						break
					}
				}
			}
			// Reserved BEFORE the quarantine check: a dropped entry still
			// owns its exposed name, so removing it leaves every other name
			// in this group exactly where it was.
			taken[name] = true
			if pol.Quarantined[name] {
				continue
			}
			def := c.def
			def.Name = name
			byExposed[name] = entry{route: c.route, def: def, srv: c.srv, prov: c.prov}
		}
	}

	ordered := make([]string, 0, len(byExposed))
	for name := range byExposed {
		ordered = append(ordered, name)
	}
	slices.Sort(ordered)
	return &Router{byExposed: byExposed, ordered: ordered}, nil
}

// RouteOf resolves an exposed name to its (server, raw tool) route. It is
// the only legitimate reverse mapping — pure map lookup, no string
// parsing.
func (r *Router) RouteOf(exposed string) (Route, bool) {
	e, ok := r.byExposed[exposed]
	if !ok {
		return Route{}, false
	}
	return e.route, true
}

// Lookup resolves an exposed name to the live server (for issuing the
// call) and its route.
func (r *Router) Lookup(exposed string) (*downstream.Server, Route, bool) {
	e, ok := r.byExposed[exposed]
	if !ok {
		return nil, Route{}, false
	}
	return e.srv, e.route, true
}

// LookupProvider resolves an exposed name to the host-served provider
// behind it (docs/modules/config.md). ok is false for every downstream and every
// cache-only entry, so a caller cannot accidentally treat a real server's
// tool as host-served.
func (r *Router) LookupProvider(exposed string) (Provider, Route, bool) {
	e, ok := r.byExposed[exposed]
	if !ok || e.prov == nil {
		return nil, Route{}, false
	}
	return e.prov, e.route, true
}

// Def returns the aggregated definition of an exposed name (Name already
// rewritten). It is how a caller reads inputSchema/annotations without
// re-scanning a server's tool list.
func (r *Router) Def(exposed string) (mcp.ToolDef, bool) {
	e, ok := r.byExposed[exposed]
	if !ok {
		return mcp.ToolDef{}, false
	}
	return e.def, true
}

// List returns the aggregated tool definitions under their exposed names,
// sorted by exposed name. Descriptions and schemas are the downstream
// originals, passed through verbatim.
func (r *Router) List() []mcp.ToolDef {
	out := make([]mcp.ToolDef, 0, len(r.ordered))
	for _, name := range r.ordered {
		out = append(out, r.byExposed[name].def)
	}
	return out
}

// sanitize rewrites every rune outside [a-zA-Z0-9_-] to '_'. It keeps
// exposed names within the conservative tool-name charset that upstream
// clients accept.
func sanitize(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '_', r == '-':
			b.WriteRune(r)
		default:
			b.WriteByte('_')
		}
	}
	return b.String()
}
