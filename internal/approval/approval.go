package approval

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/dinstein/agent-hub/internal/audit"
)

// GateReason says why a call was gated in the first place.
type GateReason string

// Frozen gate reasons. They appear in frontend prompts and audit records.
const (
	ReasonDestructive     GateReason = "destructive"
	ReasonUntrustedSource GateReason = "untrusted-source"
	ReasonPolicy          GateReason = "policy"
)

// Decision is the outcome of one approval request. Fail direction: every
// value except Approved forbids execution — a caller must treat any unknown
// or zero value as a rejection.
type Decision int

// The five decision states (order frozen: the zero value is
// Denied so an uninitialized Decision can never read as an approval).
const (
	Denied Decision = iota
	Approved
	Timedout
	Unreachable
	Stale
)

// String returns the stable lowercase name used in logs and audit records.
func (d Decision) String() string {
	switch d {
	case Denied:
		return "denied"
	case Approved:
		return "approved"
	case Timedout:
		return "timedout"
	case Unreachable:
		return "unreachable"
	case Stale:
		return "stale"
	default:
		return "invalid"
	}
}

// valid reports whether d is one of the five defined states. Fail direction:
// callers map invalid values to Unreachable (a rejection), never Approved.
func (d Decision) valid() bool { return d >= Denied && d <= Stale }

// ParseDecision maps a wire decision string (Decision.String output) back to
// its Decision. Fail direction: any unknown string returns ok=false and the
// caller must treat the value as a rejection (RemoteAsker maps it to
// Unreachable) — never default to Approved.
func ParseDecision(s string) (Decision, bool) {
	switch s {
	case "denied":
		return Denied, true
	case "approved":
		return Approved, true
	case "timedout":
		return Timedout, true
	case "unreachable":
		return Unreachable, true
	case "stale":
		return Stale, true
	default:
		return Unreachable, false
	}
}

// RememberScope says how long an approval should be remembered.
type RememberScope int

// Remember scopes (docs/flows.md: remember=session|forever).
const (
	// RememberNone approves this single call only.
	RememberNone RememberScope = iota
	// RememberSession auto-approves future identical-fingerprint calls from
	// the same session, in broker memory only (lost on daemon restart).
	RememberSession
	// RememberForever writes a fingerprint-bound entry to the on-disk
	// allowlist (state/approvals-allowlist.json, daemon single writer).
	RememberForever
)

// ParseRememberScope maps the wire remember string ("", "none", "session",
// "forever") to its RememberScope. Fail direction: an unknown string returns
// ok=false — the caller must reject the request rather than guess a scope
// (guessing "forever" would silently widen a grant).
func ParseRememberScope(s string) (RememberScope, bool) {
	switch s {
	case "", "none":
		return RememberNone, true
	case "session":
		return RememberSession, true
	case "forever":
		return RememberForever, true
	default:
		return RememberNone, false
	}
}

// Request is one gated call awaiting a human decision.
//
// ArgsJSON is memory-only: it travels to frontends over the authenticated
// control channel and is NEVER persisted (no allowlist entry, no audit
// record carries it). ArgsHash is the canonical-JSON SHA-256 of the exact
// argument bytes that will run if approved. Fingerprint is the tool
// fingerprint of the LIVE router definition at gate time — preferring the
// live definition means a drifted tool can never reuse an old approval.
type Request struct {
	// Token identifies this request towards Answer. Empty on entry; the
	// broker stamps a fresh unguessable token before fan-out.
	Token  string
	Server string
	Tool   string
	// ArgsJSON: memory-only, never persisted. See type comment.
	ArgsJSON json.RawMessage
	// ArgsHash binds the approval to the exact argument bytes
	// (audit.ArgsHash over ArgsJSON).
	ArgsHash string
	// Fingerprint of the live tool definition (integrity.Fingerprint).
	Fingerprint string
	// Deadline for the human decision. Zero on entry; the broker stamps
	// now+TTL so the UI countdown and the auto-deny share one instant.
	Deadline   time.Time
	GateReason GateReason
	// Client and SessionID identify the asking session for display and for
	// RememberSession scoping.
	Client    string
	SessionID string
}

// FillArgsHash computes ArgsHash from ArgsJSON (canonical JSON SHA-256,
// audit.ArgsHash). Fail direction: on error ArgsHash is left untouched and
// the caller must not proceed to Ask — an approval without an argument
// binding would approve unknown bytes.
func (r *Request) FillArgsHash() error {
	h, err := audit.ArgsHash(r.ArgsJSON)
	if err != nil {
		return err
	}
	r.ArgsHash = h
	return nil
}

// Broker is the daemon-resident approval broker (semantics
// frozen). Every Decision other than Approved forbids execution — the
// fail-closed family is inherited wholesale.
type Broker interface {
	// Ask blocks until a decision, the deadline, or ctx cancellation.
	// With no subscribed frontend it returns Unreachable immediately.
	Ask(ctx context.Context, req Request) Decision
	// Subscribe attaches a frontend (GUI / `agenthub approvals watch` /
	// webhook via ctlapi SSE). The returned cancel is idempotent.
	Subscribe(frontend string) (<-chan Request, func())
	// Answer records a human decision. First answer wins; later answers
	// get typed errors (ErrAlreadyDecided / ErrExpired / ErrUnknownToken).
	Answer(token string, approve bool, remember RememberScope) error
}

// Asker is the gateway-side face of the broker: the only operation the data
// plane needs. *MemBroker satisfies it in-process (daemon HTTP face); stdio
// gateways use RemoteAsker over the UDS control connection.
type Asker interface {
	Ask(ctx context.Context, req Request) Decision
}

// Typed answer errors. All are wrapped with context; match with errors.Is.
var (
	// ErrUnknownToken: the token never existed or was pruned long after its
	// terminal decision.
	ErrUnknownToken = errors.New("approval: unknown token")
	// ErrExpired: the request already timed out (or its asker vanished)
	// before this answer arrived. Late approvals never execute.
	ErrExpired = errors.New("approval: request expired")
	// ErrAlreadyDecided: another frontend decided first (first answer wins;
	// ctlapi maps this to 409 for idempotent CLI behavior).
	ErrAlreadyDecided = errors.New("approval: request already decided")
	// ErrStale: the tool definition drifted between gate time and the
	// approval; the waiting caller received Stale, not Approved.
	ErrStale = errors.New("approval: tool definition drifted since request")
	// ErrRememberFailed: the single approval stands, but the requested
	// remember grant could not be recorded (missing fingerprint or session,
	// no allowlist configured, or the allowlist write failed).
	ErrRememberFailed = errors.New("approval: remember grant not recorded")
)
