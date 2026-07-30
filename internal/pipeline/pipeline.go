// Package pipeline is the single execute_call pipeline (canonical.md §2:
// execute path for every tool call; gateway/daemon only assemble).
//
// Every tool call — direct or, later, code-mode — flows through Execute and
// nowhere else, so the governance gates cannot fork. The gate chain order is
// frozen by docs/architecture.md §9:
//
//	scope gate → token tier gate
//
// then the downstream call, then the shaping hook (budget-shape through
// Options.ResultShaper). Success and
// error branches share defend_and_shape (docs/flows.md: a malicious server
// must not bypass scanning by answering with a JSON-RPC error).
//
// M1.5 state: every gate is real. The token tier gate enforces
// CallRequest.CallerTier (minted by internal/httpbridge's agent tokens)
// against the tool's annotation-derived ToolTier; a caller without a tier
// ("" — every stdio caller) has no tier authority and passes. Every stage
// still counts invocations atomically (the M0 counters are kept — they feed
// tests and metrics).
//
// Dependency constraint (canonical.md §2 rule 3, depguard-enforced): this
// package must never import internal/ctlapi — the data plane does not
// depend on the control plane.
package pipeline

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/dinstein/agent-hub/internal/mcp"
	"github.com/dinstein/agent-hub/internal/scope"
)

// CallFunc performs the routed downstream call (typically a closure over
// downstream.Server.Call). It runs only after every gate has allowed the
// request.
type CallFunc func(ctx context.Context) (*mcp.CallResult, error)

// CallRequest is one tool call travelling through the pipeline: the exposed
// name the client used, the routing result (RouteOf provenance — never
// derived by splitting the exposed name), the raw arguments, and the
// execution function.
type CallRequest struct {
	// Exposed is the namespaced tool name the upstream client called.
	Exposed string
	// ServerID / RawTool are the RouteOf routing result.
	ServerID string
	RawTool  string
	// Args is the raw argument JSON, passed through verbatim.
	Args json.RawMessage
	// InputSchema is the routed tool's inputSchema, verbatim from the
	// downstream tools/list (nil = unknown: the precheck gate then only
	// checks the JSON-object shape of Args).
	InputSchema json.RawMessage
	// Annotations is the routed tool's annotations object, verbatim from
	// the downstream tools/list. ABSENCE IS LOAD-BEARING: a tool without
	// annotations counts as destructive (fail-closed, docs/architecture.md §9).
	Annotations json.RawMessage
	// CallerTier is the caller's credential tier ("" = no tier authority,
	// see CallerTier). The token tier gate compares it against
	// ToolTier(Annotations).
	CallerTier CallerTier
	// Call executes the downstream call once the gates allow it.
	Call CallFunc
}

// Gate is one pre-call governance check. Gates run in chain order; the
// first error short-circuits the pipeline (the call never reaches the
// downstream server) and is propagated to the caller — rejections are
// *BlockedError so their reasons stay distinguishable (docs/architecture.md §9).
type Gate interface {
	// Name identifies the gate in Counters snapshots and audit records.
	Name() string
	// Check inspects the call before execution. nil allows; any error
	// blocks and short-circuits the remaining chain.
	Check(ctx context.Context, req *CallRequest) error
}

// ShapeFunc bounds ONE delivered result to the caller's budget
// (internal/shaping). It is the seam through which result pagination joins
// the single execute path: it runs INSIDE defend_and_shape, after the
// injection verdict, so every caller of Execute — direct tools/call and lazy
// call_tool — is shaped by the same rule. It must never return nil; when it
// cannot shape, it returns res unchanged (shaping fails OPEN,
// internal/shaping doc.go).
type ShapeFunc func(ctx context.Context, req *CallRequest, res *mcp.CallResult) *mcp.CallResult

// Shaper is the post-call defend_and_shape hook. It sees both branches —
// the result on success, the error otherwise — and returns the (possibly
// rewritten) pair delivered to the caller.
type Shaper interface {
	// Name identifies the hook in Counters snapshots.
	Name() string
	// Shape post-processes the call outcome. Exactly one of res / callErr
	// is meaningful, mirroring the (result, error) convention.
	Shape(ctx context.Context, req *CallRequest, res *mcp.CallResult, callErr error) (*mcp.CallResult, error)
}

// Counter is implemented by stages that expose an invocation count. All M0
// built-in stages implement it; Counters collects them.
type Counter interface {
	Count() uint64
}

// Pipeline executes tool calls through the frozen gate chain. It is
// immutable after construction and safe for concurrent use.
type Pipeline struct {
	gates  []Gate
	shaper Shaper
}

// Options injects the live governance inputs of the production gate chain.
// Every field is optional; a zero Options builds a pipeline that behaves
// like the M0 baseline (count + allow, pass-through shaping), which is the
// documented no-authority assembly, not an error state.
type Options struct {
	// Scope returns the caller's current effective scope. The scope gate
	// reads the SAME pointer the assembling gateway uses for its tools/list
	// projection (docs/architecture.md §7). nil func
	// or nil result = no scope authority: the scope gate allows (matching
	// M0 and the registry-unavailable cache-serving mode of docs/flows.md;
	// with no registry there is also no governance config to enforce).
	Scope func() *scope.EffectiveScope

	// ResultShaper bounds the delivered result to the caller's budget and
	// retains the remainder for fetch_result. nil = no shaping (results are
	// delivered whole — the M0/M1-A..C behaviour).
	ResultShaper ShapeFunc
}

// New returns the production pipeline with the frozen gate chain
// (docs/architecture.md §9 order) and the defend_and_shape hook, wired to opts.
func New(opts Options) *Pipeline {
	return &Pipeline{
		gates: []Gate{
			&scopeGate{scopeOf: opts.Scope},
			&tokenTierGate{},
		},
		shaper: &shapeStage{shape: opts.ResultShaper},
	}
}

// NewWithGates returns a pipeline over an explicit chain. It exists for
// tests (rejection semantics are proven with a fake denying gate) and for
// future assembly variants; production code uses New. shaper may be nil.
func NewWithGates(gates []Gate, shaper Shaper) *Pipeline {
	return &Pipeline{gates: gates, shaper: shaper}
}

// GateNames returns the chain order. The production order is pinned by test
// against docs/architecture.md §9.
func (p *Pipeline) GateNames() []string {
	names := make([]string, len(p.gates))
	for i, g := range p.gates {
		names[i] = g.Name()
	}
	return names
}

// Counters returns a snapshot of the invocation count of every counting
// stage (gates and shaper), keyed by stage name. It feeds tests today and
// metrics later.
func (p *Pipeline) Counters() map[string]uint64 {
	out := make(map[string]uint64, len(p.gates)+1)
	for _, g := range p.gates {
		if c, ok := g.(Counter); ok {
			out[g.Name()] = c.Count()
		}
	}
	if c, ok := p.shaper.(Counter); ok {
		out[p.shaper.Name()] = c.Count()
	}
	return out
}

// Execute runs one call through the pipeline: gates in chain order (first
// rejection short-circuits with its error), the downstream call, then
// defend_and_shape over the success and the error branch.
//
// Ordering invariant: defend_and_shape runs EXACTLY ONCE, over the outcome.
func (p *Pipeline) Execute(ctx context.Context, req CallRequest) (*mcp.CallResult, error) {
	if req.Call == nil {
		return nil, fmt.Errorf("pipeline: CallRequest needs Call (exposed %q)", req.Exposed)
	}
	for _, g := range p.gates {
		if err := g.Check(ctx, &req); err != nil {
			// The error travels as-is: *BlockedError already names its gate
			// and carries the stable rejection code.
			return nil, err
		}
	}
	res, callErr := req.Call(ctx)
	if p.shaper != nil {
		res, callErr = p.shaper.Shape(ctx, &req, res, callErr)
	}
	return res, callErr
}
