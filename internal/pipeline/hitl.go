package pipeline

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"sync/atomic"

	"github.com/dinstein/agent-hub/internal/scope"
)

// Decision is the outcome of one human-in-the-loop approval request.
type Decision string

// Approval decisions. Everything except DecisionApproved blocks the call
// (fail-closed); each maps to its own rejection code so audit can tell a
// human "no" from a timeout from a dead broker.
const (
	DecisionApproved    Decision = "approved"
	DecisionDenied      Decision = "denied"
	DecisionTimeout     Decision = "timeout"
	DecisionUnavailable Decision = "unavailable"
)

// ApprovalRequest is what the HITL gate presents to the broker: routed
// provenance plus the argument hash the approval is bound to (docs/flows.md
// : an approval covers exactly these arguments, nothing else).
type ApprovalRequest struct {
	Exposed  string
	ServerID string
	RawTool  string
	// ArgsHash is hex(sha256(raw argument JSON)); empty arguments hash the
	// empty string. The broker must bind its answer to this hash.
	ArgsHash string
	// Destructive records the annotation-derived classification that
	// triggered (or accompanies) the request.
	Destructive bool
}

// Asker is the HITL approval broker seam. Ask blocks until a decision is
// reached (respecting ctx). An error means the broker itself failed —
// callers treat that as DecisionUnavailable (fail-closed).
type Asker interface {
	Ask(ctx context.Context, req ApprovalRequest) (Decision, error)
}

// DefaultDestructive classifies a tool from its verbatim annotations JSON
// (docs/architecture.md §9 tier derivation):
//
//	readOnlyHint == true              → not destructive
//	destructiveHint present           → its value
//	annotations missing / unparsable  → destructive (FAIL-CLOSED: an
//	                                    unannotated tool must not slip past
//	                                    destructive-only governance; this
//	                                    also matches the MCP default of
//	                                    destructiveHint = true)
func DefaultDestructive(annotations json.RawMessage) bool {
	if len(annotations) == 0 {
		return true
	}
	var a struct {
		ReadOnlyHint    *bool `json:"readOnlyHint"`
		DestructiveHint *bool `json:"destructiveHint"`
	}
	if err := json.Unmarshal(annotations, &a); err != nil {
		return true // unparsable annotations: fail-closed, treat as destructive
	}
	if a.ReadOnlyHint != nil && *a.ReadOnlyHint {
		return false
	}
	if a.DestructiveHint != nil {
		return *a.DestructiveHint
	}
	return true // absent hint defaults to destructive (spec default, fail-closed)
}

// HashArgs computes the ApprovalRequest.ArgsHash binding for raw argument
// JSON.
func HashArgs(args json.RawMessage) string {
	sum := sha256.Sum256(args)
	return hex.EncodeToString(sum[:])
}

// hitlGate is the human-in-the-loop gate: DenyDestructive enforcement plus
// single-call approval bound to the argument hash (docs/flows.md §3, docs/architecture.md §9).
type hitlGate struct {
	n atomic.Uint64
	// scopeOf supplies the folded approval switches
	// (EffectiveScope.Approval). nil func / nil scope = no scope authority:
	// nothing to enforce (see Options.Scope).
	scopeOf func() *scope.EffectiveScope
	// destructive is the injected trigger predicate (never nil; New
	// defaults it to DefaultDestructive).
	destructive func(annotations json.RawMessage) bool
	// asker is the approval broker. A nil asker with a triggering call
	// FAILS CLOSED (CodeHITLUnavailable).
	//
	// It used to pass — the M1 baseline, when no broker existed yet — and the
	// flip was owed as soon as one shipped. The branch is unreachable in the
	// product today, because the single production assembly
	// (internal/gateway) always wires a gwAsker, and a gateway whose daemon
	// has died already fails closed through the broker-error path below. What
	// the old default was is a trap for the NEXT assembly: any pipeline built
	// without an Asker would have silently auto-approved every call its scope
	// said a human must see, and nothing in the type system asks for one.
	asker Asker
}

func (g *hitlGate) Name() string { return GateHITL }

// Count implements Counter.
func (g *hitlGate) Count() uint64 { return g.n.Load() }

func (g *hitlGate) Check(ctx context.Context, req *CallRequest) error {
	g.n.Add(1)
	if g.scopeOf == nil {
		return nil // no scope authority injected (M0-compat assembly)
	}
	es := g.scopeOf()
	if es == nil {
		return nil // registry unavailable: no governance config exists to enforce
	}

	destructive := g.destructive(req.Annotations)

	// DenyDestructive is the machine-decidable global switch (docs/architecture.md §9
	// baseline governance); it needs no broker and is enforced regardless
	// of whether an Asker is wired.
	if destructive && es.Approval.DenyDestructive {
		return Blockedf(GateHITL, CodeDestructiveDenied,
			"tool %q of server %q is destructive and denyDestructive is set", req.RawTool, req.ServerID)
	}

	need := es.Approval.HumanApproval || (destructive && es.Approval.ConfirmDestructive)
	if !need {
		return nil
	}
	if g.asker == nil {
		return Blockedf(GateHITL, CodeHITLUnavailable,
			"call to %q needs human approval but no approval broker is wired", req.Exposed)
	}

	dec, err := g.asker.Ask(ctx, ApprovalRequest{
		Exposed:     req.Exposed,
		ServerID:    req.ServerID,
		RawTool:     req.RawTool,
		ArgsHash:    HashArgs(req.Args),
		Destructive: destructive,
	})
	if err != nil {
		// Broker failure blocks (fail-closed): a dead broker must never
		// degrade into silent auto-approval.
		return Blockedf(GateHITL, CodeHITLUnavailable, "approval broker failed: %v", err)
	}
	switch dec {
	case DecisionApproved:
		return nil
	case DecisionDenied:
		return Blockedf(GateHITL, CodeHITLDenied, "human denied the call to %q", req.Exposed)
	case DecisionTimeout:
		return Blockedf(GateHITL, CodeHITLTimeout, "approval for %q timed out", req.Exposed)
	default:
		// Unknown decisions block (fail-closed): only an explicit approval
		// opens the gate.
		return Blockedf(GateHITL, CodeHITLUnavailable, "approval broker returned %q", dec)
	}
}
