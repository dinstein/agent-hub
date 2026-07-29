package ctlapi

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/dinstein/agent-hub/internal/audit"
	"github.com/dinstein/agent-hub/internal/event"
	"github.com/dinstein/agent-hub/internal/scope"
	"github.com/dinstein/agent-hub/internal/session"
)

// Grants wire contract (A.1 #8: agent self-service only tightens; a HUMAN
// grant may temporarily widen a session overlay — volatile, TTL-bounded):
//
//	POST /v1/grants              GrantRequestWire -> GrantWire (pending)
//	GET  /v1/grants[?history]    []GrantWire
//	POST /v1/grants/{id}         GrantDecideWire -> GrantWire
//
// Approval injects the widening through SessionManager.Mutate with the
// human-grant flag (the ONLY path allowed to loosen); an AfterFunc reaper
// reverts exactly what was added when the TTL expires. Nothing here is ever
// persisted: grants and overlays die with the daemon by design (A.1 #6).

// Grant lifecycle states.
const (
	GrantPending  = "pending"
	GrantApproved = "approved" // widening active, reaper armed
	GrantDenied   = "denied"
	GrantExpired  = "expired" // TTL passed (or session died); widening reverted
)

// Grant timing defaults.
const (
	// DefaultGrantTTL is the widening lifetime after approval (volatile, default TTL 1h).
	DefaultGrantTTL = time.Hour
	// maxGrantTTL caps a caller-supplied TTL: a "temporary" widening must
	// stay temporary.
	maxGrantTTL = 24 * time.Hour
	// grantRetention keeps terminal grants visible to `grant ls --history`.
	grantRetention = 10 * time.Minute
)

// GrantRequestWire is the create body (POST /v1/grants). M1: submitted by a
// CLI/GUI frontend on behalf of a session; the agent-side request_widen
// meta-tool (M1-D) will produce the same shape.
type GrantRequestWire struct {
	SessionID string   `json:"session_id"`
	Server    string   `json:"server"`
	Tools     []string `json:"tools"`
	Reason    string   `json:"reason,omitempty"`
	// TTLSeconds is the widening lifetime after approval (0 = default 1h,
	// capped at 24h).
	TTLSeconds int64 `json:"ttl_seconds,omitempty"`
}

// GrantDecideWire is the decision body (POST /v1/grants/{id}).
type GrantDecideWire struct {
	Approve bool `json:"approve"`
}

// GrantWire is one grant as listed and returned.
type GrantWire struct {
	ID         string     `json:"id"`
	SessionID  string     `json:"session_id"`
	Client     string     `json:"client,omitempty"`
	Server     string     `json:"server"`
	Tools      []string   `json:"tools"`
	Reason     string     `json:"reason,omitempty"`
	Status     string     `json:"status"`
	TTLSeconds int64      `json:"ttl_seconds"`
	CreatedAt  time.Time  `json:"created_at"`
	DecidedAt  *time.Time `json:"decided_at,omitempty"`
	DecidedBy  string     `json:"decided_by,omitempty"`
	ExpiresAt  *time.Time `json:"expires_at,omitempty"`
}

// Bus topics for grant lifecycle. The "approval" prefix maps them onto the
// frontend `approvals` SSE topic (grants are the second approval surface).
const (
	topicGrantPending  event.Topic = "approval.grant_pending"
	topicGrantResolved event.Topic = "approval.grant_resolved"
)

// grantUndo records EXACTLY what applyGrantWiden added, so expiry can revert
// element-wise. Every undo edit moves in the tightening direction by
// construction (remove added allows/servers, restore removed denies), so the
// revert never needs — and never gets — the human-grant flag: even a buggy
// undo cannot loosen anything.
type grantUndo struct {
	server      string
	serverAdded bool
	allowAdded  []string
	denyRemoved []string
}

// grantRec is one in-memory grant. Mutable fields are guarded by
// grantManager.mu.
type grantRec struct {
	id        string
	sessionID string
	client    string
	server    string
	tools     []string
	reason    string
	ttl       time.Duration
	createdAt time.Time

	status    string
	deciding  bool // a decision (and its Mutate) is in flight
	decidedAt time.Time
	decidedBy string
	expiresAt time.Time
	undo      *grantUndo
	timer     *time.Timer
}

func (g *grantRec) wire() GrantWire {
	w := GrantWire{
		ID:         g.id,
		SessionID:  g.sessionID,
		Client:     g.client,
		Server:     g.server,
		Tools:      slices.Clone(g.tools),
		Reason:     g.reason,
		Status:     g.status,
		TTLSeconds: int64(g.ttl / time.Second),
		CreatedAt:  g.createdAt,
		DecidedBy:  g.decidedBy,
	}
	if !g.decidedAt.IsZero() {
		t := g.decidedAt
		w.DecidedAt = &t
	}
	if !g.expiresAt.IsZero() && g.status == GrantApproved {
		t := g.expiresAt
		w.ExpiresAt = &t
	}
	return w
}

// grantManager owns the volatile grant table and the expiry reaper timers.
// Memory-only by design: a daemon restart drops every grant together with
// the overlays they widened (A.1 #6 — a resurrected widening would be a
// security incident).
type grantManager struct {
	mu     sync.Mutex
	grants map[string]*grantRec
	order  []string // creation order for stable listing
}

func newGrantManager() *grantManager {
	return &grantManager{grants: map[string]*grantRec{}}
}

// pruneLocked drops terminal grants past retention. Caller holds mu.
func (gm *grantManager) pruneLocked(now time.Time) {
	kept := gm.order[:0]
	for _, id := range gm.order {
		g := gm.grants[id]
		if g == nil {
			continue
		}
		if g.status != GrantPending && g.status != GrantApproved &&
			now.Sub(g.decidedAt) > grantRetention {
			delete(gm.grants, id)
			continue
		}
		kept = append(kept, id)
	}
	gm.order = kept
}

// newGrantID returns an 8-byte hex id — short enough to type, random enough
// to not collide in a daemon lifetime.
func newGrantID() string {
	var b [8]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

// grantsPathID matches POST /v1/grants/{id} on the ESCAPED path.
func grantsPathID(r *http.Request) (string, bool) {
	p := r.URL.EscapedPath()
	rest, found := strings.CutPrefix(p, "/v1/grants/")
	if !found || rest == "" || strings.Contains(rest, "/") {
		return "", false
	}
	id, err := url.PathUnescape(rest)
	if err != nil || id == "" {
		return "", false
	}
	return id, true
}

// handleGrantCreate implements POST /v1/grants: register a pending widen
// request for a live session. The request itself changes nothing — only an
// approved grant touches the overlay.
func (s *Server) handleGrantCreate(w http.ResponseWriter, r *http.Request) {
	reqID := requestIDFrom(r.Context())
	body, err := readBody(r)
	if err != nil {
		writeErr(w, http.StatusBadRequest, CodeBadRequest, err.Error(), "", reqID)
		return
	}
	var req GrantRequestWire
	if err := json.Unmarshal(body, &req); err != nil {
		writeErr(w, http.StatusBadRequest, CodeBadRequest, "decoding grant body: "+err.Error(), "", reqID)
		return
	}
	if req.Server == "" {
		writeErr(w, http.StatusBadRequest, CodeBadRequest, "server must not be empty", "", reqID)
		return
	}
	if len(req.Tools) == 0 {
		// Ruling for M1: a grant names explicit tools. A whole-server widen
		// would need "restore previous selector" semantics whose revert can
		// race agent narrowing; element-wise tools keep the undo provably
		// tightening.
		writeErr(w, http.StatusBadRequest, CodeBadRequest, "tools must name at least one tool",
			"pass the raw downstream tool names to widen", reqID)
		return
	}
	if req.TTLSeconds < 0 {
		writeErr(w, http.StatusBadRequest, CodeBadRequest, "ttl_seconds must not be negative", "", reqID)
		return
	}
	sess, ok := s.opts.Sessions.Get(session.SessionID(req.SessionID))
	if !ok {
		writeNotFound(w, r) // unknown session reads like an unknown route
		return
	}

	ttl := time.Duration(req.TTLSeconds) * time.Second
	if ttl <= 0 {
		ttl = s.opts.GrantTTL
	}
	if ttl > maxGrantTTL {
		ttl = maxGrantTTL
	}
	g := &grantRec{
		id:        newGrantID(),
		sessionID: req.SessionID,
		client:    sess.ClientID,
		server:    req.Server,
		tools:     slices.Clone(req.Tools),
		reason:    req.Reason,
		ttl:       ttl,
		createdAt: time.Now(),
		status:    GrantPending,
	}
	gm := s.grants
	gm.mu.Lock()
	gm.pruneLocked(time.Now())
	gm.grants[g.id] = g
	gm.order = append(gm.order, g.id)
	wire := g.wire()
	gm.mu.Unlock()

	s.auditGrant(r, "grants/request", g, body, true)
	s.opts.Bus.Publish(event.Event{Topic: topicGrantPending, Key: g.id, Payload: wire})
	writeOK(w, http.StatusOK, wire)
}

// handleGrantsList implements GET /v1/grants: pending + active by default,
// terminal ones (within retention) with ?history=1.
func (s *Server) handleGrantsList(w http.ResponseWriter, r *http.Request) {
	history := r.URL.Query().Get("history") != ""
	gm := s.grants
	gm.mu.Lock()
	gm.pruneLocked(time.Now())
	out := []GrantWire{}
	for _, id := range gm.order {
		g := gm.grants[id]
		if g == nil {
			continue
		}
		if !history && g.status != GrantPending && g.status != GrantApproved {
			continue
		}
		out = append(out, g.wire())
	}
	gm.mu.Unlock()
	writeOK(w, http.StatusOK, out)
}

// handleGrantDecide implements POST /v1/grants/{id}. Approval is
// push-then-commit like every overlay mutation: the widening is applied via
// SessionManager.Mutate (human-grant flag set — the one sanctioned loosening
// path) and the grant flips to approved only after Mutate committed; on any
// failure the grant stays pending and nothing widened.
func (s *Server) handleGrantDecide(w http.ResponseWriter, r *http.Request, id string) {
	reqID := requestIDFrom(r.Context())
	body, err := readBody(r)
	if err != nil {
		writeErr(w, http.StatusBadRequest, CodeBadRequest, err.Error(), "", reqID)
		return
	}
	var decide GrantDecideWire
	if err := json.Unmarshal(body, &decide); err != nil {
		writeErr(w, http.StatusBadRequest, CodeBadRequest, "decoding decide body: "+err.Error(), "", reqID)
		return
	}

	gm := s.grants
	gm.mu.Lock()
	g := gm.grants[id]
	if g == nil {
		gm.mu.Unlock()
		writeNotFound(w, r)
		return
	}
	if g.status != GrantPending || g.deciding {
		st := g.status
		by := g.decidedBy
		gm.mu.Unlock()
		msg := "grant " + id + " already " + st
		if by != "" {
			msg += " by " + by
		}
		writeErr(w, http.StatusConflict, CodeAlreadyDecided, msg,
			"first decision wins (idempotent: nothing to do)", reqID)
		return
	}
	actor := actorFrom(r.Context())
	if !decide.Approve {
		g.status = GrantDenied
		g.decidedAt = time.Now()
		g.decidedBy = actor
		wire := g.wire()
		gm.mu.Unlock()
		s.auditGrant(r, "grants/decide", g, body, false)
		s.opts.Bus.Publish(event.Event{Topic: topicGrantResolved, Key: g.id, Payload: wire})
		writeOK(w, http.StatusOK, wire)
		return
	}
	// Approve path: mark in-flight so a concurrent decision 409s, then run
	// Mutate OUTSIDE the lock (a stdio session blocks on its gateway ack).
	g.deciding = true
	gm.mu.Unlock()

	var undo grantUndo
	mutErr := s.opts.Sessions.Mutate(r.Context(), session.SessionID(g.sessionID),
		func(ov *scope.Overlay) {
			undo = applyGrantWiden(ov, g.server, g.tools)
		}, session.WithHumanGrant())

	gm.mu.Lock()
	g.deciding = false
	if mutErr != nil {
		// Commit failed: the grant stays pending (retryable) unless the
		// session is gone, in which case it can never be applied.
		if errors.Is(mutErr, session.ErrNotFound) {
			g.status = GrantExpired
			g.decidedAt = time.Now()
		}
		gm.mu.Unlock()
		s.auditGrant(r, "grants/decide", g, body, false)
		if errors.Is(mutErr, session.ErrNotFound) {
			writeNotFound(w, r)
			return
		}
		writeErr(w, http.StatusInternalServerError, CodeInternal,
			"applying grant overlay: "+mutErr.Error(), "", reqID)
		return
	}
	now := time.Now()
	g.status = GrantApproved
	g.decidedAt = now
	g.decidedBy = actor
	g.expiresAt = now.Add(g.ttl)
	g.undo = &undo
	g.timer = time.AfterFunc(g.ttl, func() { s.expireGrant(id) })
	wire := g.wire()
	gm.mu.Unlock()

	s.auditGrant(r, "grants/decide", g, body, true)
	s.opts.Bus.Publish(event.Event{Topic: topicGrantResolved, Key: g.id, Payload: wire})
	writeOK(w, http.StatusOK, wire)
}

// expireGrant is the reaper callback: revert the widening element-wise and
// mark the grant expired. A vanished session is fine — its overlay already
// died with it (cascade in session.Close).
func (s *Server) expireGrant(id string) {
	gm := s.grants
	gm.mu.Lock()
	g := gm.grants[id]
	if g == nil || g.status != GrantApproved {
		gm.mu.Unlock()
		return
	}
	undo := g.undo
	sid := g.sessionID
	gm.mu.Unlock()

	if undo != nil {
		// Tighten-only revert: NO human-grant flag, so even a buggy undo is
		// rejected rather than loosening (failure direction).
		err := s.opts.Sessions.Mutate(context.Background(), session.SessionID(sid),
			func(ov *scope.Overlay) { undo.apply(ov) })
		if err != nil && !errors.Is(err, session.ErrNotFound) {
			s.log.Warn("ctlapi: grant expiry revert failed; overlay may stay widened until session ends",
				"grant", id, "session", sid, "error", err)
		}
	}

	gm.mu.Lock()
	g.status = GrantExpired
	g.decidedAt = time.Now()
	wire := g.wire()
	gm.mu.Unlock()
	if s.opts.Audit != nil {
		s.opts.Audit.Append(audit.Record{
			Actor:    "system",
			Client:   g.client,
			Session:  sid,
			Server:   g.server,
			Tool:     "grants/expire:" + id,
			Decision: audit.DecisionAllowed,
		})
	}
	s.opts.Bus.Publish(event.Event{Topic: topicGrantResolved, Key: id, Payload: wire})
	s.log.Info("ctlapi: grant expired; widening reverted", "grant", id, "session", sid)
}

// applyGrantWiden widens ov for (server, tools) and reports exactly what it
// added. It only ever RELAXES narrowing recorded in the overlay itself —
// the static three-layer waterline still applies at merge time, so a grant
// can never expose more than the operator's persisted configuration.
func applyGrantWiden(ov *scope.Overlay, server string, tools []string) grantUndo {
	undo := grantUndo{server: server}
	if ov.Servers != nil && !slices.Contains(ov.Servers, server) {
		ov.Servers = append(ov.Servers, server)
		undo.serverAdded = true
	}
	sel := ov.Tools[server]
	if sel == nil {
		return undo // no per-server narrowing to relax
	}
	for _, tool := range tools {
		if sel.Allow != nil && !slices.Contains(sel.Allow, tool) {
			sel.Allow = append(sel.Allow, tool)
			undo.allowAdded = append(undo.allowAdded, tool)
		}
		if slices.Contains(sel.Deny, tool) {
			sel.Deny = slices.DeleteFunc(sel.Deny, func(d string) bool { return d == tool })
			undo.denyRemoved = append(undo.denyRemoved, tool)
		}
	}
	return undo
}

// apply reverts the recorded widening. Every edit tightens (see grantUndo).
func (u *grantUndo) apply(ov *scope.Overlay) {
	if u.serverAdded && ov.Servers != nil {
		ov.Servers = slices.DeleteFunc(ov.Servers, func(s string) bool { return s == u.server })
	}
	sel := ov.Tools[u.server]
	if len(u.denyRemoved) > 0 {
		if sel == nil {
			// The selector vanished meanwhile; re-adding the denies is still
			// a pure tightening, so restore them on a fresh selector.
			if ov.Tools == nil {
				ov.Tools = make(map[string]*scope.ToolSelector)
			}
			sel = &scope.ToolSelector{}
			ov.Tools[u.server] = sel
		}
		for _, d := range u.denyRemoved {
			if !slices.Contains(sel.Deny, d) {
				sel.Deny = append(sel.Deny, d)
			}
		}
	}
	if sel != nil && sel.Allow != nil {
		for _, a := range u.allowAdded {
			sel.Allow = slices.DeleteFunc(sel.Allow, func(t string) bool { return t == a })
		}
	}
}

// auditGrant records one grant-flow control-plane action, binding the exact
// request body via ArgsHash (never grant content itself beyond ids/names).
func (s *Server) auditGrant(r *http.Request, action string, g *grantRec, body []byte, allowed bool) {
	if s.opts.Audit == nil {
		return
	}
	decision := audit.DecisionDenied
	if allowed {
		decision = audit.DecisionAllowed
	}
	hash, err := audit.ArgsHash(body)
	if err != nil {
		hash = "unhashable"
	}
	s.opts.Audit.Append(audit.Record{
		Actor:     actorFrom(r.Context()),
		Client:    g.client,
		Session:   g.sessionID,
		Server:    g.server,
		Tool:      action + ":" + g.id,
		ArgsHash:  hash,
		Decision:  decision,
		RequestID: requestIDFrom(r.Context()),
	})
}
