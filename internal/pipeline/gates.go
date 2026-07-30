package pipeline

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sync/atomic"

	"github.com/dinstein/agent-hub/internal/scope"
)

// Frozen stage names (docs/architecture.md §9 chain order). They key Counters
// snapshots and appear in audit records; do not rename.
const (
	GateScope           = "scope"
	GateTokenTier       = "token_tier"
	GateHITL            = "hitl"
	StageDefendAndShape = "defend_and_shape"
)

// Stable rejection codes so audit can tell rejection layers apart
// (docs/architecture.md §9: "rejection reasons are individually distinguishable"). They are ABI once emitted; do not
// rename. The HITL gate has one code per non-approved Decision plus the
// broker-free DenyDestructive rejection.
const (
	CodeScopeDenied       = "E_SCOPE_DENIED"
	CodeTokenTierDenied   = "E_TOKEN_TIER_DENIED"
	CodeArgsInvalid       = "E_ARGS_INVALID"
	CodeHITLDenied        = "E_HITL_DENIED"
	CodeHITLTimeout       = "E_HITL_TIMEOUT"
	CodeHITLUnavailable   = "E_HITL_UNAVAILABLE"
	CodeDestructiveDenied = "E_DESTRUCTIVE_DENIED"
)

// ErrBlocked is the decidable sentinel for a gate rejection: every
// *BlockedError satisfies errors.Is(err, ErrBlocked).
var ErrBlocked = errors.New("pipeline: call blocked")

// BlockedError is the typed rejection a gate returns. Gate names the
// rejecting gate, Code is its stable machine-readable rejection code.
type BlockedError struct {
	Gate    string
	Code    string
	Message string
}

func (e *BlockedError) Error() string {
	return fmt.Sprintf("pipeline: %s gate blocked call (%s): %s", e.Gate, e.Code, e.Message)
}

// Unwrap ties every BlockedError to the ErrBlocked sentinel.
func (e *BlockedError) Unwrap() error { return ErrBlocked }

// Blockedf builds a BlockedError for gate with a stable code.
func Blockedf(gate, code, format string, a ...any) *BlockedError {
	return &BlockedError{Gate: gate, Code: code, Message: fmt.Sprintf(format, a...)}
}

// ScopeAllows reports whether the routed (server, raw tool) pair is visible
// under es. It is shared by the scope gate and the gateway's tools/list
// projection so listing and calling can never disagree (docs/architecture.md §7).
//
// Failure direction: a nil es, an invisible server and an invisible tool
// all report false (fail-closed). Callers that mean "no scope authority at
// all" must decide that BEFORE calling (see scopeGate).
func ScopeAllows(es *scope.EffectiveScope, serverID, rawTool string) bool {
	if es == nil {
		return false
	}
	view, ok := es.Servers[serverID]
	if !ok {
		return false
	}
	// ToolView.Tools is sorted by scope.Merge (invariant).
	_, found := slices.BinarySearch(view.Tools, rawTool)
	return found
}

// scopeGate is the visibility gate: is the routed (server, tool) visible to
// this session under the effective scope? (docs/architecture.md §9, first gate.)
type scopeGate struct {
	n atomic.Uint64
	// scopeOf returns the caller's effective scope; the SAME pointer the
	// assembling gateway serves tools/list from (cache-shared, docs/architecture.md §7
	// ). nil func / nil scope = no scope authority (see Options.Scope):
	// allow — that mode has no registry, hence nothing to enforce.
	scopeOf func() *scope.EffectiveScope
}

func (g *scopeGate) Name() string { return GateScope }

// Count implements Counter.
func (g *scopeGate) Count() uint64 { return g.n.Load() }

func (g *scopeGate) Check(_ context.Context, req *CallRequest) error {
	g.n.Add(1)
	if g.scopeOf == nil {
		return nil // no scope authority injected (M0-compat assembly)
	}
	es := g.scopeOf()
	if es == nil {
		// The provider has no authority to resolve against right now
		// (registry unavailable — docs/flows.md cache-serving mode). There
		// is no scope config in that state, so there is nothing to enforce.
		return nil
	}
	if !ScopeAllows(es, req.ServerID, req.RawTool) {
		return Blockedf(GateScope, CodeScopeDenied,
			"tool %q of server %q is outside the session's effective scope", req.RawTool, req.ServerID)
	}
	return nil
}

// tokenTierGate is the operation-tier gate (docs/architecture.md §9, second defence
// line): does the caller's credential tier (CallRequest.CallerTier —
// read | write | destructive, carried by an agent token) cover the tool's
// annotation-derived tier (ToolTier)?
//
// It is the MACHINE half of the layered governance and therefore runs before
// the downstream call: a call a read-only credential may not make must not
// reach the server at all.
//
// Two failure directions, both closed:
//   - a tool whose annotations are absent/unparsable classifies as
//     destructive, so only a destructive credential reaches it;
//   - an unrecognised CallerTier string covers nothing (TierCovers).
//
// The empty CallerTier is the one ALLOW case and it is not a hole: it means
// "no tier authority in this assembly" — a stdio gateway serves the human's
// own session over a pipe that carries no credential, so there is no tier to
// enforce. Only the HTTP face (internal/httpbridge) mints tiers.
type tokenTierGate struct{ n atomic.Uint64 }

func (g *tokenTierGate) Name() string { return GateTokenTier }

// Count implements Counter.
func (g *tokenTierGate) Count() uint64 { return g.n.Load() }

func (g *tokenTierGate) Check(_ context.Context, req *CallRequest) error {
	g.n.Add(1)
	if req.CallerTier == "" {
		return nil // no tier authority (stdio: the human's own session)
	}
	need := ToolTier(req.Annotations)
	if TierCovers(req.CallerTier, need) {
		return nil
	}
	return Blockedf(GateTokenTier, CodeTokenTierDenied,
		"caller tier %q may not invoke tool %q of server %q (tool tier %q)",
		string(req.CallerTier), req.RawTool, req.ServerID, string(need))
}
