package ctlapi

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/dinstein/agent-hub/internal/approval"
	"github.com/dinstein/agent-hub/internal/audit"
	"github.com/dinstein/agent-hub/internal/event"
)

// Approvals wire contract (docs/flows.md §3):
//
//	POST /v1/approvals/ask       ApprovalAskWire -> ApprovalDecisionWire
//	                             (gateway-internal; blocks until decided)
//	GET  /v1/approvals[?history] []ApprovalWire (pending; +decided history)
//	POST /v1/approvals/{token}   ApprovalDecideWire -> ApprovalDecisionWire
//	SSE  /v1/events topic "approvals": kind "pending" (ApprovalWire, WITH
//	                             args) / kind "resolved" (ApprovalResolved)
//
// ArgsJSON red line: argument bytes travel only over the authenticated
// socket (ask body, SSE pending frames). The GET listing strips them and no
// audit record ever carries them — hashes only.

// ApprovalAskWire is the ask request body sent by a stdio gateway when the
// HITL gate fires. Args is display payload for frontends; ArgsHash is the
// binding the approval covers.
type ApprovalAskWire struct {
	Server      string          `json:"server"`
	Tool        string          `json:"tool"`
	Args        json.RawMessage `json:"args,omitempty"`
	ArgsHash    string          `json:"args_hash,omitempty"`
	Fingerprint string          `json:"fingerprint,omitempty"`
	GateReason  string          `json:"gate_reason,omitempty"`
	Client      string          `json:"client,omitempty"`
	SessionID   string          `json:"session_id,omitempty"`
}

// ApprovalDecisionWire reports the terminal decision of one request.
// RememberError is set when the single approval stands but the requested
// remember grant could not be recorded.
type ApprovalDecisionWire struct {
	Decision      string `json:"decision"`
	RememberError string `json:"remember_error,omitempty"`
}

// ApprovalDecideWire is the human decision body (POST /v1/approvals/{token}).
type ApprovalDecideWire struct {
	Approve bool `json:"approve"`
	// Remember is "", "none", "session" or "forever" (docs/flows.md).
	Remember string `json:"remember,omitempty"`
}

// ApprovalWire is one tracked approval request as listed/streamed to
// frontends. Args is populated ONLY on SSE pending frames (memory/socket
// red line); Decision/DecidedAt/DecidedBy are set only on history entries.
type ApprovalWire struct {
	Token       string          `json:"token"`
	Server      string          `json:"server"`
	Tool        string          `json:"tool"`
	Args        json.RawMessage `json:"args,omitempty"`
	ArgsHash    string          `json:"args_hash,omitempty"`
	Fingerprint string          `json:"fingerprint,omitempty"`
	GateReason  string          `json:"gate_reason,omitempty"`
	Client      string          `json:"client,omitempty"`
	SessionID   string          `json:"session_id,omitempty"`
	Deadline    time.Time       `json:"deadline"`
	Decision    string          `json:"decision,omitempty"` // "" = pending
	DecidedAt   *time.Time      `json:"decided_at,omitempty"`
	DecidedBy   string          `json:"decided_by,omitempty"`
}

// ApprovalResolved is the SSE "resolved" payload published after a human
// decision lands (late frontends collapse their pending card).
type ApprovalResolved struct {
	Token     string `json:"token"`
	Decision  string `json:"decision"`
	DecidedBy string `json:"decided_by,omitempty"`
}

// topicApprovalResolved is the bus topic the decide handler publishes on;
// the "approval" prefix maps it onto the frontend `approvals` SSE topic.
const topicApprovalResolved event.Topic = "approval.resolved"

// approvalWire converts a broker snapshot entry. includeArgs gates the
// ArgsJSON red line: only the SSE bridge passes true.
func approvalWire(st approval.RequestStatus, includeArgs bool) ApprovalWire {
	w := ApprovalWire{
		Token:       st.Request.Token,
		Server:      st.Request.Server,
		Tool:        st.Request.Tool,
		ArgsHash:    st.Request.ArgsHash,
		Fingerprint: st.Request.Fingerprint,
		GateReason:  string(st.Request.GateReason),
		Client:      st.Request.Client,
		SessionID:   st.Request.SessionID,
		Deadline:    st.Request.Deadline,
		DecidedBy:   st.DecidedBy,
	}
	if includeArgs {
		w.Args = st.Request.ArgsJSON
	}
	if st.Decision != nil {
		w.Decision = st.Decision.String()
		t := st.DecidedAt
		w.DecidedAt = &t
	}
	return w
}

// pendingWire is the SSE bridge conversion for a fanned-out request (always
// pending, args included — the frontend must see what it approves).
func pendingWire(req approval.Request) ApprovalWire {
	return approvalWire(approval.RequestStatus{Request: req}, true)
}

// approvalsToken matches POST /v1/approvals/{token} on the ESCAPED path
// (a token containing %2F cannot smuggle segments). "/v1/approvals/ask" is
// routed before this matcher, so "ask" is never a token here.
func approvalsToken(r *http.Request) (string, bool) {
	p := r.URL.EscapedPath()
	rest, found := strings.CutPrefix(p, "/v1/approvals/")
	if !found || rest == "" || strings.Contains(rest, "/") {
		return "", false
	}
	tok, err := url.PathUnescape(rest)
	if err != nil || tok == "" {
		return "", false
	}
	return tok, true
}

// handleApprovalAsk implements POST /v1/approvals/ask: build the broker
// request and BLOCK until a decision (first answer, deadline, or the caller
// hanging up). Every outcome — including transport-level failure modes the
// gateway maps to Unreachable — forbids execution except "approved".
func (s *Server) handleApprovalAsk(w http.ResponseWriter, r *http.Request) {
	reqID := requestIDFrom(r.Context())
	if s.opts.Approvals == nil {
		writeNotFound(w, r)
		return
	}
	body, err := readBody(r)
	if err != nil {
		writeErr(w, http.StatusBadRequest, CodeBadRequest, err.Error(), "", reqID)
		return
	}
	var ask ApprovalAskWire
	if err := json.Unmarshal(body, &ask); err != nil {
		writeErr(w, http.StatusBadRequest, CodeBadRequest, "decoding ask body: "+err.Error(), "", reqID)
		return
	}
	if ask.Server == "" || ask.Tool == "" {
		writeErr(w, http.StatusBadRequest, CodeBadRequest, "server and tool must not be empty", "", reqID)
		return
	}
	req := approval.Request{
		Server:      ask.Server,
		Tool:        ask.Tool,
		ArgsJSON:    ask.Args,
		ArgsHash:    ask.ArgsHash,
		Fingerprint: ask.Fingerprint,
		GateReason:  approval.GateReason(ask.GateReason),
		Client:      ask.Client,
		SessionID:   ask.SessionID,
	}
	if req.ArgsHash == "" {
		// The asker did not bind the arguments; bind them here so the audit
		// line and any remember grant still cover exact bytes. Failure
		// direction: unhashable args reject the ask outright.
		if err := req.FillArgsHash(); err != nil {
			writeErr(w, http.StatusBadRequest, CodeBadRequest, "hashing args: "+err.Error(), "", reqID)
			return
		}
	}

	start := time.Now()
	dec := s.opts.Approvals.Ask(r.Context(), req)
	s.auditApproval(r, "approvals/ask", req.SessionID, ask.Client, ask.Server, ask.Tool,
		req.ArgsHash, dec == approval.Approved, time.Since(start))
	writeOK(w, http.StatusOK, ApprovalDecisionWire{Decision: dec.String()})
}

// handleApprovalsList implements GET /v1/approvals: the pending queue, plus
// (with ?history=1) decided requests still inside the broker retention
// window. Argument bytes are STRIPPED — this is a poll surface, not the
// authenticated push channel.
func (s *Server) handleApprovalsList(w http.ResponseWriter, r *http.Request) {
	if s.opts.Approvals == nil {
		writeNotFound(w, r)
		return
	}
	history := r.URL.Query().Get("history") != ""
	out := []ApprovalWire{}
	for _, st := range s.opts.Approvals.Requests() {
		if st.Decision != nil && !history {
			continue
		}
		out = append(out, approvalWire(st, false))
	}
	writeOK(w, http.StatusOK, out)
}

// handleApprovalDecide implements POST /v1/approvals/{token}. First answer
// wins; a later answer gets 409 with the first decider (idempotent for
// scripts), an expired/vanished request gets 410, an unknown token the
// uniform 404.
func (s *Server) handleApprovalDecide(w http.ResponseWriter, r *http.Request, token string) {
	reqID := requestIDFrom(r.Context())
	if s.opts.Approvals == nil {
		writeNotFound(w, r)
		return
	}
	body, err := readBody(r)
	if err != nil {
		writeErr(w, http.StatusBadRequest, CodeBadRequest, err.Error(), "", reqID)
		return
	}
	var decide ApprovalDecideWire
	if err := json.Unmarshal(body, &decide); err != nil {
		writeErr(w, http.StatusBadRequest, CodeBadRequest, "decoding decide body: "+err.Error(), "", reqID)
		return
	}
	remember, ok := approval.ParseRememberScope(decide.Remember)
	if !ok {
		writeErr(w, http.StatusBadRequest, CodeBadRequest,
			"unknown remember scope "+decide.Remember, `use "session" or "forever"`, reqID)
		return
	}

	actor := actorFrom(r.Context())
	start := time.Now()
	err = s.opts.Approvals.AnswerAs(token, decide.Approve, remember, actor)
	s.auditApproval(r, "approvals/decide", "", "", "", token, "", err == nil && decide.Approve, time.Since(start))

	decision := "denied"
	if decide.Approve {
		decision = "approved"
	}
	switch {
	case err == nil:
		s.publishResolved(token, decision, actor)
		writeOK(w, http.StatusOK, ApprovalDecisionWire{Decision: decision})
	case errors.Is(err, approval.ErrRememberFailed):
		// The single approval stands; only the remember grant failed.
		s.publishResolved(token, decision, actor)
		writeOK(w, http.StatusOK, ApprovalDecisionWire{Decision: decision, RememberError: err.Error()})
	case errors.Is(err, approval.ErrUnknownToken):
		writeNotFound(w, r)
	case errors.Is(err, approval.ErrExpired):
		writeErr(w, http.StatusGone, CodeExpired, err.Error(),
			"the request timed out or its caller went away; late approvals never execute", reqID)
	case errors.Is(err, approval.ErrStale):
		// The decision was recorded — as Stale, a rejection — because the
		// tool definition drifted since gate time.
		s.publishResolved(token, approval.Stale.String(), actor)
		writeErr(w, http.StatusConflict, CodeStale, err.Error(),
			"the tool definition drifted since the request; re-run the call to re-gate", reqID)
	case errors.Is(err, approval.ErrAlreadyDecided):
		writeErr(w, http.StatusConflict, CodeAlreadyDecided, err.Error(),
			"another frontend decided first (idempotent: nothing to do)", reqID)
	default:
		writeErr(w, http.StatusInternalServerError, CodeInternal, err.Error(), "", reqID)
	}
}

// publishResolved announces a human decision on the bus so every subscribed
// frontend collapses its pending card. Timeouts publish nothing: frontends
// hold the deadline and expire cards themselves.
func (s *Server) publishResolved(token, decision, by string) {
	s.opts.Bus.Publish(event.Event{
		Topic:   topicApprovalResolved,
		Key:     token,
		Payload: ApprovalResolved{Token: token, Decision: decision, DecidedBy: by},
	})
}

// auditApproval records one approval-flow control-plane action. Never
// carries argument bytes — the hash is the binding (docs/architecture.md §10).
func (s *Server) auditApproval(r *http.Request, action, sessionID, client, server, tool, argsHash string, allowed bool, dur time.Duration) {
	if s.opts.Audit == nil {
		return
	}
	decision := audit.DecisionDenied
	if allowed {
		decision = audit.DecisionAllowed
	}
	s.opts.Audit.Append(audit.Record{
		Actor:     actorFrom(r.Context()),
		Client:    client,
		Session:   sessionID,
		Server:    server,
		Tool:      action + ":" + tool,
		ArgsHash:  argsHash,
		Decision:  decision,
		DurMs:     dur.Milliseconds(),
		RequestID: requestIDFrom(r.Context()),
	})
}
