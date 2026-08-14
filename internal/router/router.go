// Package router aggregates the tools of many downstream servers into one
// namespaced catalog (docs/modules/dataplane.md, internal/router).
//
// Exposed names are Sanitize(serverID) + "__" + Sanitize(rawTool), with
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
// Aggregation applies no policy of its own. What a session may see is
// decided in one place — internal/scope, which intersects the server's own
// allow list with the profile's — so the catalog built here is the full
// surface and narrowing happens once, above it.
package router

import (
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"maps"
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

// entry is one aggregated tool. Exactly one of srv / prov is set for a
// callable entry; a cache-built entry has neither (listable, not callable).
//
// build groups candidates as entries too: a candidate is just an entry whose
// exposed name has not been decided yet, so assigning the name is the last
// step rather than a copy into a second identically-shaped struct.
type entry struct {
	route Route
	def   mcp.ToolDef // Name rewritten to the exposed name
	srv   *downstream.Server
	prov  Provider
}

// Router is an immutable snapshot of the aggregated catalog. Rebuild and
// atomically swap the pointer on change (docs/architecture.md#the-processes: snapshot reads, no
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

// Build aggregates the current Tools() of the given servers.
// Servers must be non-nil with unique, non-empty IDs.
func Build(servers []*downstream.Server) (*Router, error) {
	return BuildWith(servers, nil)
}

// BuildWith is Build plus host-served providers (docs/modules/config.md). Providers
// are appended AFTER the servers so a provider id colliding with a server
// id is reported as the duplicate it is — the configured server wins,
// deterministically, instead of the aggregation order deciding.
func BuildWith(servers []*downstream.Server, providers []Provider) (*Router, error) {
	sources := make([]source, 0, len(servers)+len(providers))
	for _, srv := range servers {
		if srv == nil {
			return nil, fmt.Errorf("router: nil server")
		}
		sources = append(sources, source{id: srv.ID(), tools: srv.Tools(), srv: srv})
	}
	sources = append(sources, providerSources(providers)...)
	return build(sources)
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
func BuildFromCache(cached map[string][]mcp.ToolDef) (*Router, error) {
	return BuildFromCacheWith(cached, nil)
}

// BuildFromCacheWith is BuildFromCache plus host-served providers. A
// provider is live even while every downstream is still connecting — it has
// nothing to connect to — so the cold catalog can already list and CALL its
// tools.
func BuildFromCacheWith(cached map[string][]mcp.ToolDef, providers []Provider) (*Router, error) {
	ids := slices.Sorted(maps.Keys(cached))
	sources := make([]source, 0, len(ids))
	for _, id := range ids {
		sources = append(sources, source{id: id, tools: cached[id]})
	}
	sources = append(sources, providerSources(providers)...)
	return build(sources)
}

// build is the shared aggregation core of Build / BuildFromCache.
func build(sources []source) (*Router, error) {
	seen := make(map[string]bool, len(sources))
	groups := make(map[string][]entry)
	for _, src := range sources {
		id := src.id
		if id == "" {
			return nil, fmt.Errorf("router: server with empty ID")
		}
		if seen[id] {
			return nil, fmt.Errorf("router: duplicate server ID %q", id)
		}
		seen[id] = true
		for _, def := range src.tools {
			base := Sanitize(id) + "__" + Sanitize(def.Name)
			groups[base] = append(groups[base], entry{
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
	bases := slices.Sorted(maps.Keys(groups))

	taken := make(map[string]bool)
	byExposed := make(map[string]entry)
	for _, base := range bases {
		g := groups[base]
		slices.SortFunc(g, func(a, b entry) int {
			return cmp.Or(cmp.Compare(a.route.RawTool, b.route.RawTool), cmp.Compare(a.route.ServerID, b.route.ServerID))
		})
		for i, e := range g {
			name := ""
			if i == 0 && !taken[base] {
				name = base
			} else {
				for n := 2; ; n++ {
					if suffixed := fmt.Sprintf("%s_%d", base, n); !taken[suffixed] {
						name = suffixed
						break
					}
				}
			}
			taken[name] = true
			e.def.Name = name
			byExposed[name] = e
		}
	}

	ordered := slices.Sorted(maps.Keys(byExposed))
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

// Sanitize rewrites every rune outside [a-zA-Z0-9_-] to '_'. It keeps
// exposed names within the conservative tool-name charset that upstream
// clients accept.
//
// Exported because it is not only this package's rule: internal/discovery
// builds grouped mode's "<server>_tools" entries out of the same server ids,
// under the same constraint about what an upstream client accepts, and used
// to carry a byte-identical copy of this function held in step by a golden
// test. One definition is the version of that agreement a compiler can keep.
func Sanitize(s string) string {
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
