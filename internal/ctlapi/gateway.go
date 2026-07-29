package ctlapi

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/dinstein/agent-hub/internal/audit"
	"github.com/dinstein/agent-hub/internal/event"
	"github.com/dinstein/agent-hub/internal/registry"
	"github.com/dinstein/agent-hub/internal/scope"
	"github.com/dinstein/agent-hub/internal/session"
)

// Gateway-face wire contract (docs/architecture.md §2). A stdio gateway registers over
// the control socket, then keeps one long-lived SSE link:
//
//	POST /v1/gateway/register             GatewayHelloWire -> GatewayRegistered
//	GET  /v1/gateway/{sid}/link           SSE: "overlay" / "registry" frames
//	POST /v1/gateway/{sid}/ack            GatewayAck (answers one overlay frame)
//	POST /v1/gateway/{sid}/servers        GatewayServersReport (gatewaystate.go)
//
// The last one is the ONLY upward-flowing state on this face: the gateway is
// the process that actually holds the downstream connections, so it is the
// one that can say what they are doing (gatewaystate.go explains why the
// daemon does not observe them itself).
//
// The daemon-side session.ControlLink implementation (gatewayLink below)
// pushes overlays as SSE frames and blocks until the matching ack arrives —
// push-then-commit: SessionManager.Mutate commits only after the gateway
// acked, so daemon and gateway can never diverge.

// GatewayHelloWire is the register request body. Pid and ScopeHash are
// informational in M1-B (logged; scope wiring lands in M1-C).
type GatewayHelloWire struct {
	ClientID  string `json:"client_id"`
	Pid       int    `json:"pid"`
	Root      string `json:"root,omitempty"`
	ScopeHash string `json:"scope_hash,omitempty"`
}

// GatewayRegistered is the register response: the minted session identity
// plus the current authoritative overlay (normally absent — a re-registered
// gateway always gets a fresh session whose overlay starts empty).
type GatewayRegistered struct {
	SessionID string          `json:"session_id"`
	Overlay   json.RawMessage `json:"overlay,omitempty"`
}

// GatewayAck answers one pushed overlay frame. OK=false reports that the
// gateway could not apply the overlay; the daemon then commits nothing.
type GatewayAck struct {
	ID    uint64 `json:"id"`
	OK    bool   `json:"ok"`
	Error string `json:"error,omitempty"`
}

// SSE event names on the gateway link.
const (
	// LinkEventOverlay carries an OverlayFrame and requires an ack.
	LinkEventOverlay = "overlay"
	// LinkEventRegistry carries a RegistryFrame (notification only, no ack;
	// the gateway re-reads the registry itself, canonical.md §5c #2).
	LinkEventRegistry = "registry"
)

// OverlayFrame is the LinkEventOverlay payload: the full authoritative
// overlay (scope.Overlay JSON) plus the ack correlation id.
type OverlayFrame struct {
	ID      uint64          `json:"id"`
	Overlay json.RawMessage `json:"overlay"`
}

// RegistryFrame is the LinkEventRegistry payload. Rev is a hint only —
// consumers re-read and adopt iff read generation >= applied generation.
type RegistryFrame struct {
	Kind string `json:"kind"`
	Rev  uint64 `json:"rev"`
}

// TopicRegistry is the bus topic the daemon publishes registry changes on
// (payload registry.Change). The gateway link handler forwards it as
// LinkEventRegistry frames; it is deliberately absent from the /v1/events
// prefix map (frontends get the coalesced `servers` topic instead).
const TopicRegistry event.Topic = "registry.changed"

// Defaults for the link timing options.
const (
	// DefaultLinkAckTimeout bounds one overlay push waiting for its ack.
	DefaultLinkAckTimeout = 30 * time.Second
	// DefaultLinkAttachTimeout bounds registration-to-link-attach: a gateway
	// that registers but never opens its link is presumed dead and its
	// session is closed (otherwise a crash between the two calls would leak
	// a stdio session forever — stdio sessions are never TTL-reaped).
	DefaultLinkAttachTimeout = 30 * time.Second
)

// linkFrameBuffer bounds the queue between overlay pushers and the SSE
// writer. Pushers block (with ctx/timeout) rather than drop: an overlay
// frame lost would strand its Mutate call until the ack timeout.
const linkFrameBuffer = 16

// linkFrame is one ready-to-write SSE frame on a gateway link.
type linkFrame struct {
	event string
	data  []byte
}

// ackResult is the outcome delivered to a waiting overlay push.
type ackResult struct {
	ok     bool
	errMsg string
}

// gatewayLink is the daemon-side session.ControlLink over the register/link/
// ack HTTP triple. One instance exists per registered gateway; it dies with
// the session (SessionManager.Close calls Close) or when the SSE connection
// drops (the handler then closes the session).
type gatewayLink struct {
	ackTimeout time.Duration
	// clientID is the AI client this gateway serves. It is captured at
	// register time so a runtime report can be attributed to a human-
	// meaningful name ("whose state is this?") instead of a session id.
	clientID string

	frames chan linkFrame
	closed chan struct{}

	mu          sync.Mutex
	closedFlag  bool
	attachedNow bool
	attachTimer *time.Timer // armed at register time, disarmed by attach
	pending     map[uint64]chan ackResult
	nextID      uint64
}

var _ session.ControlLink = (*gatewayLink)(nil)

func newGatewayLink(ackTimeout time.Duration, clientID string) *gatewayLink {
	return &gatewayLink{
		ackTimeout: ackTimeout,
		clientID:   clientID,
		frames:     make(chan linkFrame, linkFrameBuffer),
		closed:     make(chan struct{}),
		pending:    make(map[uint64]chan ackResult),
	}
}

// PushOverlay implements session.ControlLink: enqueue one overlay frame and
// wait for the gateway's ack.
//
// Failure direction: EVERY outcome other than a positive ack — link closed,
// ctx done, ack timeout, gateway nack — returns an error, and the caller
// (SessionManager.Mutate) then commits NOTHING. An unacked overlay must
// never be considered applied.
func (l *gatewayLink) PushOverlay(ctx context.Context, ov *scope.Overlay) error {
	raw, err := json.Marshal(ov)
	if err != nil {
		return fmt.Errorf("ctlapi: encoding overlay: %w", err)
	}

	l.mu.Lock()
	if l.closedFlag {
		l.mu.Unlock()
		return fmt.Errorf("ctlapi: gateway link is closed")
	}
	l.nextID++
	id := l.nextID
	ch := make(chan ackResult, 1)
	l.pending[id] = ch
	l.mu.Unlock()
	defer func() {
		l.mu.Lock()
		delete(l.pending, id)
		l.mu.Unlock()
	}()

	data, err := json.Marshal(OverlayFrame{ID: id, Overlay: raw})
	if err != nil {
		return fmt.Errorf("ctlapi: encoding overlay frame: %w", err)
	}

	timer := time.NewTimer(l.ackTimeout)
	defer timer.Stop()

	select {
	case l.frames <- linkFrame{event: LinkEventOverlay, data: data}:
	case <-ctx.Done():
		return fmt.Errorf("ctlapi: overlay push: %w", context.Cause(ctx))
	case <-l.closed:
		return fmt.Errorf("ctlapi: overlay push: gateway link closed")
	case <-timer.C:
		return fmt.Errorf("ctlapi: overlay push: enqueue timed out after %v", l.ackTimeout)
	}

	select {
	case res := <-ch:
		if !res.ok {
			return fmt.Errorf("ctlapi: gateway rejected overlay: %s", res.errMsg)
		}
		return nil
	case <-ctx.Done():
		return fmt.Errorf("ctlapi: overlay ack: %w", context.Cause(ctx))
	case <-l.closed:
		return fmt.Errorf("ctlapi: overlay ack: gateway link closed")
	case <-timer.C:
		return fmt.Errorf("ctlapi: overlay ack: timed out after %v", l.ackTimeout)
	}
}

// Close implements session.ControlLink. Idempotent; wakes every waiter
// (pushers and the SSE writer) with the failure direction "not applied".
func (l *gatewayLink) Close() error {
	l.mu.Lock()
	if l.closedFlag {
		l.mu.Unlock()
		return nil
	}
	l.closedFlag = true
	if l.attachTimer != nil {
		l.attachTimer.Stop()
	}
	l.mu.Unlock()
	close(l.closed)
	return nil
}

// attach marks the SSE connection as established (single-shot: a second
// connection is rejected; a re-registering gateway gets a NEW session and
// link). It disarms the attach watchdog.
func (l *gatewayLink) attach() bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.closedFlag || l.attachedNow {
		return false
	}
	l.attachedNow = true
	if l.attachTimer != nil {
		l.attachTimer.Stop()
		l.attachTimer = nil
	}
	return true
}

// armAttachWatchdog closes the session if no link attaches within d.
func (l *gatewayLink) armAttachWatchdog(d time.Duration, expire func()) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.closedFlag || l.attachedNow {
		return
	}
	l.attachTimer = time.AfterFunc(d, expire)
}

// deliverAck routes one GatewayAck to its waiting push. Unknown ids are
// ignored: a late ack after the push already timed out is a benign race.
func (l *gatewayLink) deliverAck(ack GatewayAck) {
	l.mu.Lock()
	ch := l.pending[ack.ID]
	delete(l.pending, ack.ID)
	l.mu.Unlock()
	if ch != nil {
		ch <- ackResult{ok: ack.OK, errMsg: ack.Error} // buffered(1): never blocks
	}
}

// gatewayFor looks up the live link for a session id.
func (s *Server) gatewayFor(sid string) (*gatewayLink, bool) {
	s.gwMu.Lock()
	defer s.gwMu.Unlock()
	l, ok := s.gateways[sid]
	return l, ok
}

// gatewayPath matches /v1/gateway/{sid}/link and /v1/gateway/{sid}/ack on
// the ESCAPED path (an id containing %2F cannot smuggle extra segments),
// returning the unescaped sid and the action ("link" or "ack").
func gatewayPath(r *http.Request) (sid, action string, ok bool) {
	p := r.URL.EscapedPath()
	rest, found := strings.CutPrefix(p, "/v1/gateway/")
	if !found {
		return "", "", false
	}
	seg, action, found := strings.Cut(rest, "/")
	if !found || seg == "" || strings.Contains(action, "/") {
		return "", "", false
	}
	if action != "link" && action != "ack" && action != "servers" {
		return "", "", false
	}
	id, err := url.PathUnescape(seg)
	if err != nil || id == "" {
		return "", "", false
	}
	return id, action, true
}

// handleGatewayRegister implements POST /v1/gateway/register: mint a stdio
// session bound to a fresh control link and report the current overlay.
func (s *Server) handleGatewayRegister(w http.ResponseWriter, r *http.Request) {
	reqID := requestIDFrom(r.Context())
	body, err := readBody(r)
	if err != nil {
		writeErr(w, http.StatusBadRequest, CodeBadRequest, err.Error(), "", reqID)
		return
	}
	var hello GatewayHelloWire
	if err := json.Unmarshal(body, &hello); err != nil {
		writeErr(w, http.StatusBadRequest, CodeBadRequest, "decoding register body: "+err.Error(), "", reqID)
		return
	}
	if hello.ClientID == "" {
		writeErr(w, http.StatusBadRequest, CodeBadRequest, "client_id must not be empty", "", reqID)
		return
	}

	var roots []string
	if hello.Root != "" {
		roots = []string{hello.Root}
	}
	link := newGatewayLink(s.opts.LinkAckTimeout, hello.ClientID)
	sess, err := s.opts.Sessions.Register(r.Context(), session.GatewayHello{
		ClientID: hello.ClientID,
		Roots:    roots,
	}, link)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, CodeInternal, err.Error(), "", reqID)
		return
	}
	sid := string(sess.ID)

	s.gwMu.Lock()
	s.gateways[sid] = link
	s.gwMu.Unlock()

	// Single cleanup path: whatever closes the link (session close, SSE
	// drop, attach watchdog) funnels through here. Sessions.Close is
	// idempotent, so double-close races are harmless.
	go func() {
		<-link.closed
		s.gwMu.Lock()
		if s.gateways[sid] == link {
			delete(s.gateways, sid)
		}
		s.gwMu.Unlock()
		// The observer is gone, so its observations go with it: runtime
		// state must never outlive the process that produced it, or
		// /v1/servers would keep showing a connection nobody holds.
		s.dropServerReports(sid)
		s.opts.Sessions.Close(sess.ID)
	}()
	link.armAttachWatchdog(s.opts.LinkAttachTimeout, func() {
		s.log.Warn("ctlapi: gateway never attached its link; closing session",
			"session", sid, "client", hello.ClientID)
		s.opts.Sessions.Close(sess.ID)
	})

	s.auditGatewayRegister(r, hello, sid, body)
	s.log.Info("ctlapi: gateway registered",
		"session", sid, "client", hello.ClientID,
		"gateway_pid", hello.Pid, "scope_hash", hello.ScopeHash)

	res := GatewayRegistered{SessionID: sid}
	if ov := sess.Overlay(); ov != nil {
		if raw, err := json.Marshal(ov); err == nil {
			res.Overlay = raw
		}
	}
	writeOK(w, http.StatusOK, res)
}

// auditGatewayRegister records the registration (canonical.md 1.6: every
// control-plane action is audited).
func (s *Server) auditGatewayRegister(r *http.Request, hello GatewayHelloWire, sid string, body []byte) {
	if s.opts.Audit == nil {
		return
	}
	hash, err := audit.ArgsHash(body)
	if err != nil {
		hash = "unhashable"
	}
	s.opts.Audit.Append(audit.Record{
		Actor:     actorFrom(r.Context()),
		Client:    hello.ClientID,
		Session:   sid,
		Tool:      "gateway/register",
		ArgsHash:  hash,
		Decision:  audit.DecisionAllowed,
		RequestID: requestIDFrom(r.Context()),
	})
}

// handleGatewayLink implements GET /v1/gateway/{sid}/link: the long-lived
// SSE stream carrying overlay pushes and registry change notifications.
//
// Lifecycle invariant (docs/architecture.md §2): the stdio session lives exactly as
// long as its link. When this handler returns for any reason — connection
// drop, daemon shutdown, link closed elsewhere — the session is closed; the
// gateway re-registers and receives a NEW identity (overlay authority died
// with the old one).
func (s *Server) handleGatewayLink(w http.ResponseWriter, r *http.Request, sid string) {
	link, ok := s.gatewayFor(sid)
	if !ok {
		writeNotFound(w, r)
		return
	}
	if !link.attach() {
		writeErr(w, http.StatusConflict, CodeConflict,
			"gateway link already attached or closed",
			"re-register to obtain a fresh session", requestIDFrom(r.Context()))
		return
	}
	// From here on the link is consumed: the session dies with this stream.
	defer s.opts.Sessions.Close(session.SessionID(sid))

	sub := s.opts.Bus.Subscribe(event.DefaultBuffer, TopicRegistry)
	defer sub.Close()

	rc := http.NewResponseController(w)
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.WriteHeader(http.StatusOK)
	_ = rc.Flush()

	var keepalive <-chan time.Time
	if s.opts.KeepAlive > 0 {
		t := time.NewTicker(s.opts.KeepAlive)
		defer t.Stop()
		keepalive = t.C
	}

	ctx := r.Context()
	for {
		select {
		case <-ctx.Done():
			return
		case <-link.closed:
			return
		case <-keepalive:
			if _, err := fmt.Fprint(w, ":ka\n\n"); err != nil {
				return
			}
			_ = rc.Flush()
		case f := <-link.frames:
			if !s.writeLinkFrame(w, rc, f) {
				return
			}
		case ev, ok := <-sub.Events():
			if !ok {
				return
			}
			ch, ok := ev.Payload.(registry.Change)
			if !ok {
				continue
			}
			data, err := json.Marshal(RegistryFrame{Kind: string(ch.Kind), Rev: ch.Rev})
			if err != nil {
				continue
			}
			if !s.writeLinkFrame(w, rc, linkFrame{event: LinkEventRegistry, data: data}) {
				return
			}
		}
	}
}

// writeLinkFrame writes one SSE event block and flushes. Returns false when
// the connection is gone.
func (s *Server) writeLinkFrame(w http.ResponseWriter, rc *http.ResponseController, f linkFrame) bool {
	id := s.eventSeq.Add(1)
	if _, err := fmt.Fprintf(w, "id: %d\nevent: %s\ndata: %s\n\n", id, f.event, f.data); err != nil {
		return false
	}
	return rc.Flush() == nil
}

// handleGatewayAck implements POST /v1/gateway/{sid}/ack.
func (s *Server) handleGatewayAck(w http.ResponseWriter, r *http.Request, sid string) {
	reqID := requestIDFrom(r.Context())
	link, ok := s.gatewayFor(sid)
	if !ok {
		writeNotFound(w, r)
		return
	}
	body, err := readBody(r)
	if err != nil {
		writeErr(w, http.StatusBadRequest, CodeBadRequest, err.Error(), "", reqID)
		return
	}
	var ack GatewayAck
	if err := json.Unmarshal(body, &ack); err != nil {
		writeErr(w, http.StatusBadRequest, CodeBadRequest, "decoding ack body: "+err.Error(), "", reqID)
		return
	}
	link.deliverAck(ack)
	writeOK(w, http.StatusOK, struct{}{})
}
