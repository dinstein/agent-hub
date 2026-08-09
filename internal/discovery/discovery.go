// Package discovery implements the three tool-exposure modes of
// docs/flows.md — full, grouped and lazy — plus the lexical ranker, the
// SearchGuard state machine, the search-trace record and the pinned-tool
// seam (docs/modules/dataplane.md, internal/discovery).
//
// Three invariants hold across the whole package:
//
//  1. ONE scope, three enforcement points (docs/architecture.md §7). tools/list, the
//     search candidate filter and the call_tool route guard all read the
//     SAME *scope.EffectiveScope. This package never re-derives visibility:
//     Visible projects a router catalog through pipeline.ScopeAllows — the
//     identical predicate the pipeline's scope gate uses — and a Surface is
//     an immutable snapshot of that projection. A tool that is not in the
//     Surface can neither be listed, searched, nor recommended.
//
//  2. Determinism is contract (canonical.md §6). Exposure sets, ranking
//     order, summary truncation and every user-visible string are frozen by
//     golden tests. Ties break on the exposed name ascending, never on map
//     iteration order.
//
//  3. Fail-closed naming. An incoming tools/call name that resolves to
//     nothing is Unknown and MUST be dropped — never promoted to a
//     meta-tool. Under a cold catalog (scope resolved before any downstream
//     answered tools/list) the visible set is empty, so every routed name is
//     Unknown; that is the closed direction and is deliberate (docs/flows.md
//     , failure branch).
//
// The package computes and formats; it never executes. call_tool and
// fetch_result are parsed and validated here, then handed to the gateway,
// which runs them through internal/pipeline (the single execute path) and
// internal/shaping.
package discovery

import (
	"slices"
	"strings"

	"github.com/dinstein/agent-hub/internal/mcp"
	"github.com/dinstein/agent-hub/internal/pipeline"
	"github.com/dinstein/agent-hub/internal/router"
	"github.com/dinstein/agent-hub/internal/scope"
)

// Mode is the tool-exposure mode. It is an alias of scope.DiscoveryMode so
// that the value read from EffectiveScope.Discovery travels here unchanged —
// there is no second enumeration to keep in sync.
type Mode = scope.DiscoveryMode

// The three modes, re-exported for callers that only import this package.
const (
	ModeFull    = scope.DiscoveryFull
	ModeGrouped = scope.DiscoveryGrouped
	ModeLazy    = scope.DiscoveryLazy
)

// DefaultMode is used when no scope layer set a mode (EffectiveScope.
// Discovery == ""): lazy.
//
// The default has to serve the gateway's whole point, which is that a client
// is wired up once and servers accumulate behind it. Nobody re-reads this
// setting when the fourth server is added, so the default is what most
// installations run forever — and full mode spends the client's context in
// proportion to how well the gateway is being used. One hosted server can
// contribute fifty tools on its own.
//
// Either default is safe in the security sense: visibility is decided by the
// scope, never by the mode, and lazy hides names from the initial list without
// taking any capability away. What is traded is discoverability — a client in
// lazy mode has to call search_tools instead of reading tool names it was
// handed. Set full explicitly (globally with `agenthub config set discovery
// full`, or per profile) on a small surface where that trade is not worth it.
const DefaultMode = ModeLazy

// ModeOf reads the exposure mode out of an effective scope. An unset or
// unrecognised value degrades to DefaultMode rather than erroring: a typo in
// a config file must not blank the tool surface.
func ModeOf(es *scope.EffectiveScope) Mode {
	if es == nil {
		return DefaultMode
	}
	switch es.Discovery {
	case ModeFull, ModeGrouped, ModeLazy:
		return es.Discovery
	default:
		return DefaultMode
	}
}

// Tool is one visible tool on the discovery surface: the exposed
// (namespaced) name the client calls, its RouteOf provenance, and the
// downstream definition with Name already rewritten to Exposed.
//
// ServerID / RawTool always come from router.RouteOf. Deriving them by
// splitting Exposed on "__" is forbidden repo-wide (router doc comment).
type Tool struct {
	Exposed  string
	ServerID string
	RawTool  string
	Def      mcp.ToolDef
}

// Key is the search-index cache key of docs/architecture.md §7: the catalog
// generation plus the content hash of the effective scope. Scope changes
// change the hash, so a stale index can never be reused — no explicit
// invalidation is needed, and two sessions that resolve to the same content
// share one index. Key is comparable and usable as a map key.
type Key struct {
	Generation uint64
	ScopeHash  [32]byte
	// Variants mirrors Options.IntentVariants. It is part of the key because
	// the variant switch changes what tools/list emits and what call_with
	// points at, while moving neither the generation nor the scope hash —
	// without it a governance flip would keep serving the old doors.
	Variants bool
}

// CacheKeyOf builds the index cache key for a catalog generation and scope,
// with intent variants off. A nil scope yields the zero hash — a distinct
// slot, never a shared one.
func CacheKeyOf(catalogGeneration uint64, es *scope.EffectiveScope) Key {
	return CacheKeyFor(catalogGeneration, es, false)
}

// CacheKeyFor is CacheKeyOf for an assembly that also carries the intent
// variant switch. Assemblies that enable variants MUST probe their cache
// with this constructor, or a flip of the switch would not invalidate.
func CacheKeyFor(catalogGeneration uint64, es *scope.EffectiveScope, variants bool) Key {
	k := Key{Generation: catalogGeneration, Variants: variants}
	if es != nil {
		k.ScopeHash = es.Hash
	}
	return k
}

// Visible projects an aggregated router catalog through the session's
// effective scope, returning the tools that tools/list, search_tools and
// call_tool must all agree on (docs/architecture.md §7, "three enforcement points").
//
// Failure direction: an unroutable listing and an invisible route are both
// dropped. A nil scope means "no scope authority at all" (the registry-less
// gateway mode) and passes everything through — the caller decides that
// before calling, exactly as pipeline's scope gate does.
func Visible(rt *router.Router, es *scope.EffectiveScope) []Tool {
	if rt == nil {
		return nil
	}
	defs := rt.List() // already sorted by exposed name
	out := make([]Tool, 0, len(defs))
	for _, def := range defs {
		route, ok := rt.RouteOf(def.Name)
		if !ok {
			continue // no route, no visibility (closed direction)
		}
		if es != nil && !pipeline.ScopeAllows(es, route.ServerID, route.RawTool) {
			continue
		}
		out = append(out, Tool{
			Exposed:  def.Name,
			ServerID: route.ServerID,
			RawTool:  route.RawTool,
			Def:      def,
		})
	}
	return out
}

// Options configures a Surface. Tools must already be scope-filtered (see
// Visible); the Surface does not filter again, because filtering twice
// would mean two predicates and eventually two answers.
type Options struct {
	// Mode is the exposure mode. The zero value ("") means DefaultMode.
	Mode Mode
	// Tools is the visible tool set. Order is irrelevant: the Surface
	// sorts by exposed name.
	Tools []Tool
	// Pins reports config-pinned tools, which stay directly exposed even in
	// lazy mode. nil = nothing pinned.
	Pins PinSet
	// Scope is kept for its content hash only (CacheKey); it is never
	// consulted for visibility — Tools is already the answer.
	Scope *scope.EffectiveScope
	// Generation is the catalog generation behind Tools, for CacheKey.
	Generation uint64
	// IntentVariants splits the lazy-mode call_tool into the three intent
	// variants (variants.go, docs/architecture.md §9). The assembling process decides:
	// registry governance carries the switch (GovernanceDoc.IntentVariants,
	// default ON per ruling #18), and each assembly reads it. The zero value
	// here is the compatibility shape — a caller that says nothing keeps the
	// single call_tool, so no existing wiring changes behaviour by accident.
	IntentVariants bool
}

// Surface is an immutable snapshot of one session's tool exposure: what
// tools/list answers, how an incoming name is classified, and what
// search_tools can rank. Safe for concurrent use; rebuild and swap on any
// catalog or scope change (docs/architecture.md §2).
//
// SearchGuard is deliberately NOT part of a Surface: guard state is
// per-session and must survive rebuilds, while it must be reset when the
// scope changes (docs/architecture.md §7). The caller owns that lifecycle.
type Surface struct {
	mode Mode
	key  Key
	// variants records whether the lazy call door is split into the three
	// intent variants. It is part of the snapshot, not a lookup, so listing
	// and classification can never disagree about which doors exist.
	variants bool

	tools     []Tool // sorted by Exposed
	byExposed map[string]Tool
	pinned    []Tool // sorted by Exposed, subset of tools

	// grouped-mode naming, assigned deterministically over sorted serverIDs.
	serverIDs  []string          // sorted, only servers with >=1 visible tool
	byServer   map[string][]Tool // serverID -> visible tools, sorted by RawTool
	groupOf    map[string]string // serverID -> group tool name
	groupOwner map[string]string // group tool name -> serverID

	index []toolIndex // parallel to tools; ranker token sets
}

// New builds the exposure snapshot for one session.
func New(opts Options) *Surface {
	mode := opts.Mode
	switch mode {
	case ModeFull, ModeGrouped, ModeLazy:
	default:
		mode = DefaultMode
	}

	tools := make([]Tool, len(opts.Tools))
	copy(tools, opts.Tools)
	slices.SortFunc(tools, func(a, b Tool) int { return strings.Compare(a.Exposed, b.Exposed) })

	s := &Surface{
		mode:       mode,
		key:        CacheKeyFor(opts.Generation, opts.Scope, opts.IntentVariants),
		variants:   opts.IntentVariants,
		tools:      tools,
		byExposed:  make(map[string]Tool, len(tools)),
		byServer:   make(map[string][]Tool),
		groupOf:    make(map[string]string),
		groupOwner: make(map[string]string),
	}
	for _, t := range tools {
		s.byExposed[t.Exposed] = t
		s.byServer[t.ServerID] = append(s.byServer[t.ServerID], t)
		if opts.Pins != nil && opts.Pins.IsPinned(t.ServerID, t.RawTool) {
			s.pinned = append(s.pinned, t)
		}
	}
	for id := range s.byServer {
		s.serverIDs = append(s.serverIDs, id)
	}
	slices.Sort(s.serverIDs)
	for _, id := range s.serverIDs {
		g := s.byServer[id]
		slices.SortFunc(g, func(a, b Tool) int { return strings.Compare(a.RawTool, b.RawTool) })
		s.byServer[id] = g
	}
	s.assignGroupNames()
	s.buildIndex()
	return s
}

// assignGroupNames gives every visible server its grouped-mode aggregate
// tool name, "<sanitized server>_tools". Sanitisation can collide (server
// "a-b" and "a.b" both sanitise to "a_b"); collisions take _2/_3/… in
// sorted serverID order, mirroring router's rule, so the mapping is a pure
// function of the visible server set.
func (s *Surface) assignGroupNames() {
	taken := make(map[string]bool, len(s.serverIDs))
	for _, id := range s.serverIDs {
		base := sanitize(id) + groupSuffix
		name := base
		for n := 2; taken[name]; n++ {
			name = base + "_" + itoa(n)
		}
		taken[name] = true
		s.groupOf[id] = name
		s.groupOwner[name] = id
	}
}

// Mode reports the exposure mode of this surface.
func (s *Surface) Mode() Mode { return s.mode }

// CacheKey reports the (generation, scope hash) key this surface was built
// under — the search-index cache key of docs/architecture.md §7.
func (s *Surface) CacheKey() Key { return s.key }

// Tools returns the visible tool set, sorted by exposed name. The slice is
// a copy; the Surface stays immutable.
func (s *Surface) Tools() []Tool {
	out := make([]Tool, len(s.tools))
	copy(out, s.tools)
	return out
}

// Pinned returns the pinned subset, sorted by exposed name (copy).
func (s *Surface) Pinned() []Tool {
	out := make([]Tool, len(s.pinned))
	copy(out, s.pinned)
	return out
}

// Servers returns the visible server IDs, sorted (copy).
func (s *Surface) Servers() []string {
	out := make([]string, len(s.serverIDs))
	copy(out, s.serverIDs)
	return out
}

// GroupName returns the grouped-mode aggregate tool name of a server.
func (s *Surface) GroupName(serverID string) (string, bool) {
	n, ok := s.groupOf[serverID]
	return n, ok
}

// Lookup resolves an exposed name back to its visible tool.
func (s *Surface) Lookup(exposed string) (Tool, bool) {
	t, ok := s.byExposed[exposed]
	return t, ok
}

// List is what tools/list answers for this session.
//
//   - full:    every visible tool, verbatim, sorted by exposed name.
//   - grouped: one aggregate tool per visible server (<server>_tools, whose
//     description names that server's tools) plus the single call_tool
//     entry. Tool COUNT collapses to servers+1 while the agent still needs
//     no search: the names it may call are printed in the descriptions.
//   - lazy:    the five meta-tools, in frozen order, plus any pinned tools
//     (directly callable, no search round-trip).
func (s *Surface) List() []mcp.ToolDef {
	switch s.mode {
	case ModeGrouped:
		return s.listGrouped()
	case ModeLazy:
		return s.listLazy()
	default:
		out := make([]mcp.ToolDef, 0, len(s.tools))
		for _, t := range s.tools {
			out = append(out, t.Def)
		}
		return out
	}
}

// listLazy: the meta-tools in frozen order (four, or six when the call door
// is split into intent variants), then pinned tools. A pinned tool whose
// exposed name collides with a meta-tool name is dropped — the meta surface
// must never be shadowed (fail-closed; router-built exposed names always
// contain "__" so this cannot happen today, but the rule is enforced rather
// than assumed).
func (s *Surface) listLazy() []mcp.ToolDef {
	defs := MetaDefsFor(s.variants)
	out := make([]mcp.ToolDef, 0, len(defs)+len(s.pinned))
	out = append(out, defs...)
	for _, t := range s.pinned {
		if IsMetaName(t.Exposed) {
			continue
		}
		out = append(out, t.Def)
	}
	return out
}

// NameKind classifies an incoming tools/call name against this surface.
type NameKind int

const (
	// KindUnknown resolves to nothing: DROP it. Never fall back to a
	// meta-tool interpretation (docs/flows.md failure branch).
	KindUnknown NameKind = iota
	// KindMeta is one of the five lazy meta-tools (call_tool is also
	// exposed in grouped mode).
	KindMeta
	// KindGroup is a grouped-mode <server>_tools aggregate entry.
	KindGroup
	// KindTool is a routable visible tool (full mode, or a pinned tool in
	// lazy mode). Every gate still applies to it: classification is naming,
	// not authorisation.
	KindTool
)

// String renders the kind for logs and audit records.
func (k NameKind) String() string {
	switch k {
	case KindMeta:
		return "meta"
	case KindGroup:
		return "group"
	case KindTool:
		return "tool"
	default:
		return "unknown"
	}
}

// Classify decides what an incoming tools/call name means on this surface.
//
// Resolution order is fixed: meta names (only where the mode exposes them),
// then grouped aggregates, then the visible tool set, then Unknown. An
// unknown BARE name — no namespace separator, therefore superficially
// meta-tool-shaped — is Unknown like any other: under a cold catalog every
// name is unknown, and guessing "it must be a meta-tool" would invent a
// capability the session does not have.
func (s *Surface) Classify(name string) NameKind {
	switch s.mode {
	case ModeLazy:
		if s.exposesMeta(name) {
			return KindMeta
		}
	case ModeGrouped:
		if name == MetaCallTool {
			return KindMeta
		}
		if _, ok := s.groupOwner[name]; ok {
			return KindGroup
		}
	}
	if _, ok := s.byExposed[name]; ok {
		return KindTool
	}
	return KindUnknown
}

// exposesMeta reports whether this lazy surface actually LISTED name as a
// meta-tool. It is deliberately narrower than IsMetaName: a name that is
// merely reserved (call_tool while variants are on, a variant while they are
// off) was never shown to this session, so classifying it as meta would
// answer a door the client cannot see. It falls through to Unknown instead,
// which is the closed direction.
func (s *Surface) exposesMeta(name string) bool {
	if s.variants && name == MetaCallTool {
		return false
	}
	if !s.variants && IsCallVariant(name) {
		return false
	}
	return IsMetaName(name)
}

// IntentVariants reports whether this surface splits the lazy call door into
// the three intent variants. Assemblies read it to decide which resolver to
// call (ResolveCall vs ResolveCallVariant).
func (s *Surface) IntentVariants() bool { return s.variants }

// ShouldDrop reports whether an incoming tools/call name must be rejected
// outright — the fail-closed decision function for unknown bare names.
func (s *Surface) ShouldDrop(name string) bool {
	return s.Classify(name) == KindUnknown
}

// IsBareName reports whether a name carries no namespace separator, i.e. it
// is not a router-built exposed name. It exists so callers can log WHY a
// name was dropped without re-implementing (and eventually mis-implementing)
// name parsing: this is the only place "__" is inspected, and the result is
// never used for routing.
func IsBareName(name string) bool { return !strings.Contains(name, "__") }

// sanitize mirrors router's exposed-name charset: every rune outside
// [a-zA-Z0-9_-] becomes '_'. Duplicated rather than imported because
// router's copy is unexported; the two must not drift, which the grouped
// golden test pins.
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

// itoa renders a small non-negative int without pulling in strconv at the
// call sites. The grouped-mode collision suffix above was its first user and
// is no longer its only one: the frozen error messages of query.go and
// describe.go build their measurements with it too, which is what keeps those
// sentences assembled from string concatenation alone and therefore trivially
// identical to their golden files.
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	return string(buf[i:])
}
