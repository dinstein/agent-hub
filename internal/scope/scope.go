// Package scope implements the three-layer scope resolution chain
// : Global > Profile > Session, merged
// by Merge into a content-addressed EffectiveScope.
//
// Merge is a pure function: same inputs always produce the same output and
// inputs are never mutated — the stdio gateway and the daemon call the same
// implementation (docs/architecture.md §2). Security fields merge monotonically
// tighter (intersection / union / boolean OR); experience fields are
// most-specific-wins (docs/architecture.md §7).
package scope

import (
	"github.com/dinstein/agent-hub/internal/registry"
)

// LayerKind identifies one of the three scope layers, ordered from least to
// most specific. The numeric order IS the specificity order used by
// most-specific-wins merging; do not reorder.
type LayerKind uint8

const (
	LayerGlobal LayerKind = iota
	LayerProfile
	LayerSession
)

// String returns the layer name for diagnostics and audit output.
func (k LayerKind) String() string {
	switch k {
	case LayerGlobal:
		return "global"
	case LayerProfile:
		return "profile"
	case LayerSession:
		return "session"
	default:
		return "invalid"
	}
}

// ToolSelector is the three-state tool narrowing selector. It is a type
// alias to the registry definition (single source of truth for the on-disk
// semantics): selector absent = no intervention; Allow == nil = full server
// tool set; Allow == [] = block-all; Allow = [...] = narrow to subset.
// Keys are always ORIGINAL tool names, never exposed names (docs/architecture.md §7
// invariant 1).
type ToolSelector = registry.ToolSelector

// Budget bounds result payloads; aliased from registry. Forced marks a
// tighten-only rule: merge takes the minimum instead of most-specific-wins.
type Budget = registry.Budget

// DiscoveryMode selects how tools are surfaced to the client. It is an
// experience field: most specific layer wins.
type DiscoveryMode string

const (
	DiscoveryLazy    DiscoveryMode = "lazy"
	DiscoveryGrouped DiscoveryMode = "grouped"
	DiscoveryFull    DiscoveryMode = "full"
)

// ApprovalPolicy carries the per-layer approval switches. Pointer
// three-state: nil = no intervention. Merge direction is boolean OR across
// layers — any layer requiring approval wins (tighten-only, fail-closed: a
// false can never switch off a true from another layer).
type ApprovalPolicy struct {
	HumanApproval      *bool
	ConfirmDestructive *bool
	// DenyDestructive is settable ONLY by the global governance layer
	// (governance.json) — it is NEVER agent-writable. Two mechanisms enforce
	// this: (1) the session-layer input type Overlay deliberately has no such
	// field, so no overlay can ever carry it; (2) FromRegistry populates it
	// exclusively on the LayerGlobal layer (registry.ApprovalPolicy, the
	// client/project on-disk type, does not even model the field).
	DenyDestructive *bool
}

// ScopeLayer is one layer's contribution to the merge (docs/architecture.md §7).
type ScopeLayer struct {
	Kind   LayerKind
	Origin string // audit provenance, e.g. "clients.json#claude-code" / "session:claude-code:3"

	// Servers: nil = no intervention; [] = block-all; [...] = intersect
	// visibility down to this set (security field).
	Servers []string

	// Tools maps serverID -> selector; a nil entry or missing key = no
	// intervention for that server (security field, ORIGINAL tool names).
	Tools map[string]*ToolSelector

	// Discovery: nil = no intervention (experience field).
	Discovery *DiscoveryMode

	// ResultBudget maps serverID or "*" to a budget (experience field;
	// Forced entries merge tighten-only via min).
	ResultBudget map[string]*Budget

	Approval ApprovalPolicy
}

// ToolView is the final visible ORIGINAL tool set of one server. Tools is
// sorted; an empty (non-nil semantics irrelevant here) list means the server
// is visible but all of its tools are blocked.
type ToolView struct {
	Tools []string
}

// EffectiveApproval is the folded approval outcome: pointers collapsed to
// concrete booleans by OR across layers.
type EffectiveApproval struct {
	HumanApproval      bool
	ConfirmDestructive bool
	DenyDestructive    bool
}

// Diagnostic is a non-fatal resolution warning (e.g. a dangling profile
// reference that was fail-closed to an empty scope). Diagnostics are part of
// the EffectiveScope value — never silent (docs/architecture.md §7).
type Diagnostic struct {
	Layer   LayerKind
	Origin  string
	Message string
}

// EffectiveScope is the pure, content-addressed merge result. Hash covers
// every field EXCEPT Generation (and Hash itself), so it serves as a search
// cache key and staleness check for cursors/approvals; Generation records
// which registry state the value was computed from and does not affect
// content identity.
type EffectiveScope struct {
	Generation uint64              // registry generation the scope was computed from
	Servers    map[string]ToolView // final visible server -> visible original tool set
	Discovery  DiscoveryMode       // "" when no layer set one (caller applies its default)
	Budgets    map[string]int      // serverID or "*" -> effective byte budget
	Approval   EffectiveApproval
	Diags      []Diagnostic
	Hash       [32]byte
}

// SessionID is the daemon-assigned session identity, e.g. "claude-code:17".
// (The session package owns minting; scope only keys by it.)
type SessionID string

// SessionKey identifies one resolution target (docs/architecture.md §7).
type SessionKey struct {
	ClientID  string
	SessionID SessionID
	Root      string // normalized via NormalizePath; "" for HTTP sessions without a root
}
