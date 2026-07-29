package ratelimit

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"

	"github.com/dinstein/agent-hub/internal/mcp"
	"github.com/dinstein/agent-hub/internal/pipeline"
)

// Stage name and rejection code. Both are ABI once emitted: the stage name
// appears in logs and audit records, the code in the message a client sees.
const (
	// StageName names the enforcement point. It is deliberately NOT one of
	// the pipeline.Gate* names — this is not a gate (see the package doc).
	StageName = "rate_limit"
	// CodeRateLimited is the stable rejection code.
	CodeRateLimited = "E_RATE_LIMITED"
	// JSONRPCCode is the server-defined JSON-RPC error code for a quota
	// rejection (the -32000..-32099 range is reserved for server errors;
	// -32000 is already the gateway's "retry, busy").
	JSONRPCCode = -32001
)

// ExceededError is a quota rejection.
//
// It unwraps to TWO errors on purpose:
//
//   - *pipeline.BlockedError (and through it pipeline.ErrBlocked), so any
//     caller that classifies gate rejections keeps working unchanged;
//   - *mcp.Error, so the gateway's existing
//     `errors.As(err, &me) -> reply(me)` path answers the client a proper
//     JSON-RPC error carrying data.retryAfterMs, with no gateway change.
type ExceededError struct {
	Key        Key
	Rule       string
	RetryAfter DurationMillis

	blocked *pipeline.BlockedError
	rpc     *mcp.Error
}

// DurationMillis is a duration measured in whole milliseconds, the unit the
// retry hint travels in on the wire.
type DurationMillis int64

func (e *ExceededError) Error() string { return e.blocked.Error() }

// Unwrap exposes both faces of the rejection (Go 1.20 multi-unwrap).
func (e *ExceededError) Unwrap() []error { return []error{e.blocked, e.rpc} }

// newExceeded builds the rejection for one decision.
func newExceeded(key Key, dec Decision) *ExceededError {
	ms := DurationMillis(dec.RetryAfter.Milliseconds())
	if ms < 1 {
		ms = 1
	}
	msg := fmt.Sprintf("rate limit %q exceeded for %s; retry in %s", dec.Rule, key.String(), dec.RetryAfter)
	data, err := json.Marshal(struct {
		Rule         string `json:"rule"`
		RetryAfterMs int64  `json:"retryAfterMs"`
	}{Rule: dec.Rule, RetryAfterMs: int64(ms)})
	if err != nil { // unreachable: fixed struct of scalars
		data = nil
	}
	return &ExceededError{
		Key:        key,
		Rule:       dec.Rule,
		RetryAfter: ms,
		blocked:    pipeline.Blockedf(StageName, CodeRateLimited, "%s", msg),
		// The JSON-RPC face carries the stable code IN THE MESSAGE, because
		// that face is the one a client actually sees: the gateway's error
		// mapping prefers *mcp.Error over *pipeline.BlockedError, whose
		// Error() is where every other rejection's code becomes visible.
		// Without the prefix a quota rejection would be the one rejection a
		// client (or a log grep) cannot classify by code.
		rpc: &mcp.Error{Code: JSONRPCCode, Message: CodeRateLimited + ": " + msg, Data: data},
	}
}

// Admission is one call's quota reservation. It is created per
// pipeline.CallRequest and spends AT MOST ONE token, however many times the
// pipeline invokes the wrapped call.
//
// The single-charge rule is why this type exists instead of a plain
// closure: the 7.2 argument self-heal re-issues the same call with repaired
// arguments, and one agent intent must cost one token, not two.
type Admission struct {
	lim *Limiter
	key Key

	mu       sync.Mutex
	admitted bool
	err      error
}

// Admit creates a reservation for key. Nothing is charged until the wrapped
// call actually runs — a call the HITL gate denies never spends a token.
func (l *Limiter) Admit(key Key) *Admission {
	return &Admission{lim: l, key: key}
}

// check performs (or replays) the single admission decision.
func (a *Admission) check() error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.admitted {
		return a.err
	}
	a.admitted = true
	if dec := a.lim.Allow(a.key); !dec.Allowed {
		a.err = newExceeded(a.key, dec)
	}
	return a.err
}

// Wrap returns call guarded by this admission. A nil call returns nil so
// the wiring can wrap both CallRequest fields unconditionally.
//
// Placement invariant: the returned closure runs where the wrapped one did
// — after EVERY gate (scope, token tier, precheck, HITL) and immediately
// before the downstream call. That is the whole reason quotas are wired
// here rather than as a fifth gate.
func (a *Admission) Wrap(call pipeline.CallFunc) pipeline.CallFunc {
	if call == nil {
		return nil
	}
	return func(ctx context.Context) (*mcp.CallResult, error) {
		if err := a.check(); err != nil {
			return nil, err
		}
		return call(ctx)
	}
}

// WrapArgs is Wrap for the argument-parameterized (self-heal) form. It
// shares the admission with Wrap, so a healed retry is not charged again.
func (a *Admission) WrapArgs(call pipeline.CallWithArgs) pipeline.CallWithArgs {
	if call == nil {
		return nil
	}
	return func(ctx context.Context, args json.RawMessage) (*mcp.CallResult, error) {
		if err := a.check(); err != nil {
			return nil, err
		}
		return call(ctx, args)
	}
}

// Guard wraps both call fields of req in one admission. It is the sanctioned
// one-line wiring for an assembling gateway:
//
//	lim.Guard(ratelimit.Key{Client: id, Server: route.ServerID, Tool: route.RawTool}, &req)
//
// A limiter with no rules leaves req untouched.
func (l *Limiter) Guard(key Key, req *pipeline.CallRequest) {
	if !l.Enabled() || req == nil {
		return
	}
	adm := l.Admit(key)
	req.Call = adm.Wrap(req.Call)
	req.CallWithArgs = adm.WrapArgs(req.CallWithArgs)
}
