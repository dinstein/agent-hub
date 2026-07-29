package pipeline

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
	"sync/atomic"

	"github.com/dinstein/agent-hub/internal/scope"
)

// Frozen stage names (docs/architecture.md §9 chain order). They key Counters
// snapshots and appear in audit records; do not rename.
const (
	GateScope           = "scope"
	GateTokenTier       = "token_tier"
	GatePrecheck        = "precheck"
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
// the HITL gate (ruling #16): a call a read-only credential may not make is
// not worth a human's attention.
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

// precheckGate is the argument prevalidation gate (docs/modules/dataplane.md, minimal
// tier): Args must be a JSON object (or empty), required top-level fields
// must be present, and present top-level fields must match the declared
// shallow type. Full JSON Schema validation is deliberately NOT done here —
// the downstream server stays the authoritative validator.
// The gate also owns the PRE-CALL half of the 7.2 self-heal: a violation it
// could provably repair (schema default, lossless coercion) is repaired
// instead of rejected, provided the assembly can re-parameterize the call
// (CallRequest.CallWithArgs). Otherwise the rejection stands. See
// selfheal.go for the repair rules and the audit seam.
type precheckGate struct {
	n atomic.Uint64
	// onSelfHeal audits a pre-call repair. nil = unaudited (see
	// Options.OnSelfHeal).
	onSelfHeal SelfHealFunc
}

func (g *precheckGate) Name() string { return GatePrecheck }

// Count implements Counter.
func (g *precheckGate) Count() uint64 { return g.n.Load() }

func (g *precheckGate) Check(ctx context.Context, req *CallRequest) error {
	g.n.Add(1)

	violation := precheckViolation(req)
	if violation == nil {
		return nil
	}
	// Repair before rejecting: the arguments the schema itself tells us how
	// to fix are not worth a failed turn. The repair MUTATES req.Args, so
	// every later gate (notably the HITL args hash) and the call itself see
	// the arguments that actually run — "what was approved is what runs" still holds.
	if healed, ok := g.tryHeal(ctx, req); ok {
		req.Args = healed
		return nil
	}
	return violation
}

// tryHeal attempts the pre-call repair and reports the audit event.
func (g *precheckGate) tryHeal(ctx context.Context, req *CallRequest) (json.RawMessage, bool) {
	if req.CallWithArgs == nil {
		// Nothing can carry repaired arguments to the downstream (CallFunc
		// captures them in a closure), so a repair would be a lie.
		return nil, false
	}
	healed, fixes, ok := healArgs(req.Args, req.InputSchema)
	if !ok {
		return nil, false
	}
	// A repair that does not actually satisfy the precheck is not a repair.
	probe := *req
	probe.Args = healed
	if precheckViolation(&probe) != nil {
		return nil, false
	}
	if g.onSelfHeal != nil {
		g.onSelfHeal(ctx, SelfHealEvent{
			Exposed: req.Exposed, ServerID: req.ServerID, RawTool: req.RawTool,
			Fixes: fixNames(fixes), Retried: false, Recovered: true,
		})
	}
	return healed, true
}

// precheckViolation runs the shallow validation and returns the rejection it
// would produce (nil = the arguments pass). It is a pure function of the
// request so the healer can re-run it against repaired arguments.
func precheckViolation(req *CallRequest) *BlockedError {
	args, err := decodeArgsObject(req.Args)
	if err != nil {
		// Non-object arguments can never satisfy a tools/call inputSchema:
		// reject before spending a downstream round trip.
		return Blockedf(GatePrecheck, CodeArgsInvalid, "arguments must be a JSON object: %v", err)
	}

	schema, ok := decodeShallowSchema(req.InputSchema)
	if !ok {
		// Failure direction: a missing or unparsable inputSchema is
		// DOWNSTREAM data we could not understand — skipping validation
		// (fail-open toward delivery) is deliberate; blocking here would
		// veto valid calls on our own parse limitation. The downstream
		// server still validates authoritatively.
		return nil
	}
	for _, want := range schema.Required {
		if _, present := args[want]; !present {
			return Blockedf(GatePrecheck, CodeArgsInvalid,
				"required argument %q is missing (tool %q)", want, req.RawTool)
		}
	}
	// Deterministic report: a multi-violation call must always name the same
	// argument, or the error text stops being a contract.
	names := slices.Sorted(maps.Keys(args))
	for _, name := range names {
		prop, declared := schema.Properties[name]
		if !declared {
			continue // shallow check only: undeclared keys pass through
		}
		if val := args[name]; !typeMatches(prop.Type, val) {
			return Blockedf(GatePrecheck, CodeArgsInvalid,
				"argument %q has JSON type %s, schema wants %s (tool %q)",
				name, jsonTypeName(val), declaredTypeNames(prop.Type), req.RawTool)
		}
	}
	return nil
}

// decodeArgsObject decodes raw tools/call arguments into a top-level map.
// Empty and JSON null both mean "no arguments" (an empty object). Numbers
// are kept as json.Number so integer checks do not lose precision.
func decodeArgsObject(raw json.RawMessage) (map[string]any, error) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) {
		return map[string]any{}, nil
	}
	dec := json.NewDecoder(bytes.NewReader(trimmed))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return nil, err
	}
	obj, ok := v.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("got JSON %s", jsonTypeName(v))
	}
	return obj, nil
}

// shallowSchema is the minimal inputSchema projection the precheck reads:
// required + top-level property types. Everything else is ignored.
type shallowSchema struct {
	Required   []string                   `json:"required"`
	Properties map[string]shallowProperty `json:"properties"`
}

type shallowProperty struct {
	// Type is a JSON Schema type: a string or an array of strings. Kept as
	// raw any so both forms decode; absent/other forms = no constraint.
	Type any `json:"type"`
}

// decodeShallowSchema parses the schema projection; ok=false means "do not
// validate" (absent or unparsable schema — see precheckGate for direction).
func decodeShallowSchema(raw json.RawMessage) (shallowSchema, bool) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) {
		return shallowSchema{}, false
	}
	var s shallowSchema
	if err := json.Unmarshal(trimmed, &s); err != nil {
		return shallowSchema{}, false
	}
	return s, true
}

// typeMatches reports whether v satisfies the declared JSON Schema type
// (string or array-of-strings form). Unknown declarations match everything
// — the precheck only rejects PROVABLE mismatches (fail-open on schema
// constructs it does not model; the downstream validates authoritatively).
func typeMatches(declared any, v any) bool {
	switch d := declared.(type) {
	case string:
		return valueIsType(d, v)
	case []any:
		for _, alt := range d {
			s, ok := alt.(string)
			if !ok {
				return true // unmodeled declaration: no provable mismatch
			}
			if valueIsType(s, v) {
				return true
			}
		}
		return len(d) == 0 // empty union constrains nothing
	default:
		return true // absent or unmodeled: no constraint
	}
}

// valueIsType checks one decoded JSON value against one JSON Schema type
// name. Unknown names match (no provable mismatch).
func valueIsType(name string, v any) bool {
	switch name {
	case "object":
		_, ok := v.(map[string]any)
		return ok
	case "array":
		_, ok := v.([]any)
		return ok
	case "string":
		_, ok := v.(string)
		return ok
	case "boolean":
		_, ok := v.(bool)
		return ok
	case "null":
		return v == nil
	case "number":
		_, ok := v.(json.Number)
		return ok
	case "integer":
		n, ok := v.(json.Number)
		if !ok {
			return false
		}
		_, err := n.Int64()
		return err == nil
	default:
		return true
	}
}

// jsonTypeName names the JSON type of a decoded value for error messages.
func jsonTypeName(v any) string {
	switch v.(type) {
	case nil:
		return "null"
	case bool:
		return "boolean"
	case string:
		return "string"
	case json.Number, float64:
		return "number"
	case []any:
		return "array"
	case map[string]any:
		return "object"
	default:
		return fmt.Sprintf("%T", v)
	}
}

// declaredTypeNames renders a schema type declaration for error messages.
func declaredTypeNames(declared any) string {
	switch d := declared.(type) {
	case string:
		return d
	case []any:
		parts := make([]string, 0, len(d))
		for _, alt := range d {
			parts = append(parts, fmt.Sprint(alt))
		}
		return strings.Join(parts, "|")
	default:
		return fmt.Sprint(declared)
	}
}
