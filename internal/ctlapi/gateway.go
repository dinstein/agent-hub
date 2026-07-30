package ctlapi

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/dinstein/agent-hub/internal/event"
	"github.com/dinstein/agent-hub/internal/registry"
	"github.com/dinstein/agent-hub/internal/session"
)

// Gateway-face wire contract (docs/architecture.md §2). A stdio gateway registers over
// the control socket, then keeps one long-lived SSE link:
//
//	POST /v1/gateway/register             GatewayHelloWire -> GatewayRegistered
//	GET  /v1/gateway/{sid}/link           SSE: "overlay" / "registry" frames
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

// GatewayRegistered is the register response: the minted session identity.
type GatewayRegistered struct {
	SessionID string `json:"session_id"`
}

// SSE event names on the gateway link.
const (
	// LinkEventRegistry carries a RegistryFrame (notification only, no ack;
	// the gateway re-reads the registry itself, canonical.md §5c #2).
	LinkEventRegistry = "registry"
)

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

// gatewayLink is the daemon-side session.ControlLink over the register/link/
// ack HTTP triple. One instance exists per registered gateway; it dies with
// the session (SessionManager.Close calls Close) or when the SSE connection
// drops (the handler then closes the session).
type gatewayLink struct {
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
}

var _ session.ControlLink = (*gatewayLink)(nil)

func newGatewayLink(clientID string) *gatewayLink {
	return &gatewayLink{
		clientID: clientID,
		frames:   make(chan linkFrame, linkFrameBuffer),
		closed:   make(chan struct{}),
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
	link := newGatewayLink(hello.ClientID)
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

	s.log.Info("ctlapi: gateway registered",
		"session", sid, "client", hello.ClientID,
		"gateway_pid", hello.Pid, "scope_hash", hello.ScopeHash)

	writeOK(w, http.StatusOK, GatewayRegistered{SessionID: sid})
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
