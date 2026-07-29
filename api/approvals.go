package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"time"
)

// Approval decision strings (order frozen on the wire).
// Fail direction: only DecisionApproved permits execution — any other or
// unknown value is a rejection. Frontends must never treat an empty or
// unrecognised decision as an approval.
const (
	// DecisionApproved is the ONLY value that permits execution.
	DecisionApproved = "approved"
	// DecisionDenied is a human rejection.
	DecisionDenied = "denied"
	// DecisionTimedout means the deadline passed with no answer.
	DecisionTimedout = "timedout"
	// DecisionUnreachable means no frontend was reachable to decide.
	DecisionUnreachable = "unreachable"
	// DecisionStale means the arguments changed after the decision.
	DecisionStale = "stale"
)

// Remember scopes for an approval decision (docs/flows.md). "forever" is the
// only persistent one and is keyed by tool fingerprint, so a drifted tool
// falls out of the allowlist and must be approved again.
const (
	// RememberNone applies the decision to this call only.
	RememberNone = "none"
	// RememberSession remembers for the lifetime of the session (memory).
	RememberSession = "session"
	// RememberForever records a fingerprint-keyed allowlist entry.
	RememberForever = "forever"
)

// Approval is one tracked HITL request as listed by Approvals.List or
// streamed on the `approvals` SSE topic.
//
// Args red line (docs/modules/controlplane.md): argument bytes travel only over the
// authenticated socket and only on SSE pending frames — the REST listing
// always strips them and no audit record ever carries them. A frontend
// displays Args and drops it; it must never persist or log them.
//
// CONTRACT: mirrors internal/ctlapi.ApprovalWire field for field. api cannot
// import internal/* (canonical.md §2 rule 1), so the shape is restated here.
type Approval struct {
	Token  string `json:"token"`
	Server string `json:"server"`
	Tool   string `json:"tool"`
	// Args is populated only on SSE pending frames (see the red line above).
	Args json.RawMessage `json:"args,omitempty"`
	// ArgsHash is the binding the approval covers: the executed call must
	// hash to this value or the pipeline rejects it as stale.
	ArgsHash    string `json:"args_hash,omitempty"`
	Fingerprint string `json:"fingerprint,omitempty"`
	GateReason  string `json:"gate_reason,omitempty"`
	Client      string `json:"client,omitempty"`
	SessionID   string `json:"session_id,omitempty"`
	// Deadline is when the request auto-denies. UI countdowns must use it
	// so that the displayed countdown and the auto-deny fire at the same
	// instant.
	Deadline time.Time `json:"deadline"`
	// Decision is "" while pending, otherwise one of the Decision* values.
	Decision  string     `json:"decision,omitempty"`
	DecidedAt *time.Time `json:"decided_at,omitempty"`
	// DecidedBy names the frontend that won the race (docs/modules/controlplane.md:
	// any frontend may decide, first write wins).
	DecidedBy string `json:"decided_by,omitempty"`
}

// Pending reports whether the request is still awaiting a decision.
func (a Approval) Pending() bool { return a.Decision == "" }

// ApprovalResolution is the `approvals`/`resolved` SSE payload published
// after a decision lands anywhere. A frontend that did not make the decision
// uses it to collapse its own pending card (multi-frontend coexistence).
type ApprovalResolution struct {
	Token     string `json:"token"`
	Decision  string `json:"decision"`
	DecidedBy string `json:"decided_by,omitempty"`
}

// ApprovalDecision is the daemon's answer to Approvals.Answer.
// RememberError is set when the single decision stands but the requested
// remember grant could not be recorded — a partial success, not a failure.
type ApprovalDecision struct {
	Decision      string `json:"decision"`
	RememberError string `json:"remember_error,omitempty"`
}

// approvalDecideBody is the POST /v1/approvals/{token} request body.
type approvalDecideBody struct {
	Approve  bool   `json:"approve"`
	Remember string `json:"remember,omitempty"`
}

// ApprovalsService accesses the HITL approval queue. The GUI is never on the
// approval path: it is one subscriber among several (CLI `approval watch`
// is another), and the broker inside the daemon owns the decision.
type ApprovalsService struct{ c *Client }

// List returns the pending queue; with history=true it also returns recently
// decided requests. Arguments are never included in this listing.
func (s *ApprovalsService) List(ctx context.Context, history bool) ([]Approval, error) {
	var q url.Values
	if history {
		q = url.Values{"history": {"1"}}
	}
	var out []Approval
	if err := s.c.do(ctx, http.MethodGet, "/approvals", q, nil, &out); err != nil {
		return nil, err
	}
	return out, nil
}

// Answer decides one pending request. remember is "", RememberNone,
// RememberSession or RememberForever.
//
// Losing the race is not an error condition to hide: a request decided by
// another frontend answers with ErrCodeAlreadyDecided (HTTP 409) and the
// caller should simply drop its card. Callers can test with IsCode.
func (s *ApprovalsService) Answer(ctx context.Context, token string, approve bool, remember string) (ApprovalDecision, error) {
	var out ApprovalDecision
	err := s.c.do(ctx, http.MethodPost, "/approvals/"+url.PathEscape(token), nil,
		approvalDecideBody{Approve: approve, Remember: remember}, &out)
	return out, err
}
