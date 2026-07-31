package gateway

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/dinstein/agent-hub/api"
	"github.com/dinstein/agent-hub/internal/ctlapi"
	"github.com/dinstein/agent-hub/internal/registry"
)

// defaultLinkRetry is the reconnect/re-register interval (docs/architecture.md §2:
// "retry registration every 30s while the daemon is absent").
const defaultLinkRetry = 30 * time.Second

// linkDialTimeout bounds only the connect phase of control-socket
// requests; established long-poll calls (link SSE, approval Ask) still
// run without a deadline.
const linkDialTimeout = 5 * time.Second

// ctlLink is the gateway side of the gateway↔daemon control connection —
// strictly best-effort (docs/architecture.md §2): the stdio data plane has ZERO
// dependency on it. Every failure here is logged and retried; none may ever
// reach the upstream protocol channel.
//
// Lifecycle per attempt: register (POST /v1/gateway/register) → remember
// the daemon-assigned SessionID → consume the
// SSE link (registry change
// notifications) → on ANY disconnect: discard the session id (the authority
// died with the daemon-side session — fall back to the static three-layer
// scope), back off, re-register for a NEW identity.
type ctlLink struct {
	g      *gateway
	socket string
	retry  time.Duration
	hc     *http.Client

	mu        sync.Mutex
	sessionID string
	// aliveCtx is cancelled whenever this link's connection to the daemon
	// drops, so anything waiting on the daemon is released at once rather
	// than parked on a connection whose peer is gone.
	aliveCtx    context.Context
	aliveCancel context.CancelFunc

	loggedDown bool // first dial failure logs at Info, repeats at Debug
}

// armLocked replaces the alive context, cancelling whatever waited on the
// previous connection. Callers must hold l.mu.
func (l *ctlLink) armLocked(parent context.Context) {
	if l.aliveCancel != nil {
		l.aliveCancel()
	}
	l.aliveCtx, l.aliveCancel = context.WithCancel(parent)
}

// linkUp arms a fresh alive context for a newly established connection.
func (l *ctlLink) linkUp(parent context.Context) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.armLocked(parent)
}

// linkDown releases everything waiting on the connection that just died and
// arms a fresh context for the next attempt.
func (l *ctlLink) linkDown() {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.armLocked(context.Background())
}

// newCtlLink builds the link client for the control socket at socket.
func newCtlLink(g *gateway, socket string, retry time.Duration) *ctlLink {
	if retry <= 0 {
		retry = defaultLinkRetry
	}
	l := &ctlLink{
		g:      g,
		socket: socket,
		retry:  retry,
		hc: &http.Client{
			Transport: &http.Transport{
				DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
					// Bounded connect: requests over this client are
					// long-poll by design (link SSE, approval Ask), so the
					// client has no overall timeout — but a socket file
					// left behind by a dead or wedged daemon must not hold
					// a gated call open forever. Only the connect phase is
					// bounded; an established call still waits for a human.
					d := net.Dialer{Timeout: linkDialTimeout}
					return d.DialContext(ctx, "unix", socket)
				},
				// No connection reuse. Control traffic is a handful of
				// long-lived requests, so pooling buys nothing — and a
				// pooled connection to a daemon that has since died can
				// swallow a gated call: the request goes out on a socket
				// whose peer is gone and the reply never comes. Dialing
				// fresh turns that into an immediate connect failure,
				// which the fail-closed path reports as Unreachable.
				DisableKeepAlives: true,
			},
			// No client timeout: the link subscription is long-lived.
		},
	}
	l.aliveCtx, l.aliveCancel = context.WithCancel(context.Background())
	return l
}

// Session returns the current daemon-assigned session id ("" = not
// registered).
func (l *ctlLink) Session() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.sessionID
}

// run is the connection loop; it lives on the gateway lifetime context.
func (l *ctlLink) run(ctx context.Context) {
	for {
		reg, err := l.register(ctx)
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			l.reportDown(err)
			l.clear("register failed")
		} else {
			l.loggedDown = false
			l.linkUp(ctx)
			// Registration switched the session identity: re-project the
			// scope, whose cache is keyed by it.
			l.g.onSessionChanged()
			// Seed the new session's runtime state. Reports are keyed by
			// session id and are full snapshots, so everything the gateway
			// learned before it had a session (downstreams connect and the
			// link registers concurrently) arrives here in one go.
			l.g.reportServers()
			l.serveLink(ctx, reg.SessionID)
			l.linkDown()
			if ctx.Err() != nil {
				return
			}
			// Disconnected: the daemon-side session is gone. Drop the id and
			// re-register on the next tick; the data plane is unaffected,
			// because a stdio gateway's scope comes from the registry files.
			l.clear("control link lost")
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(l.retry):
		}
	}
}

// register performs POST /v1/gateway/register and applies the returned
// state (session id + initial overlay).
func (l *ctlLink) register(ctx context.Context) (ctlapi.GatewayRegistered, error) {
	hello := ctlapi.GatewayHelloWire{
		ClientID: l.g.cfg.ClientID,
		Pid:      os.Getpid(),
		// Root and ScopeHash stay unset. Register runs at gateway start,
		// BEFORE the upstream client has initialized, so no root exists yet
		// to report — and the daemon has no endpoint to amend a session's
		// roots afterwards (session.SetRoots has no route).
		//
		// This does NOT hold back per-project scope: the stdio gateway
		// resolves its own scope locally through scopeKey(), which reads the
		// root from the cache as soon as the prefetch lands. The daemon's
		// copy is operator-facing metadata (GET /v1/sessions), so what is
		// missing here is a display field, not an authority.
	}
	var reg ctlapi.GatewayRegistered
	if err := l.post(ctx, "/v1/gateway/register", "gateway:"+l.g.cfg.ClientID, hello, &reg); err != nil {
		return reg, err
	}
	if reg.SessionID == "" {
		return reg, fmt.Errorf("gateway: daemon returned an empty session id")
	}

	l.mu.Lock()
	l.sessionID = reg.SessionID
	l.mu.Unlock()
	l.g.log.Info("registered with daemon", "session", reg.SessionID)
	return reg, nil
}

// serveLink consumes the SSE link stream until it fails or ctx is done.
func (l *ctlLink) serveLink(ctx context.Context, sid string) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		"http://agenthub/v1/gateway/"+url.PathEscape(sid)+"/link", nil)
	if err != nil {
		l.g.log.Warn("control link request build failed", "error", err)
		return
	}
	l.setHeaders(req, "gateway:"+sid)
	req.Header.Set("Accept", "text/event-stream")
	resp, err := l.hc.Do(req)
	if err != nil {
		l.g.log.Warn("control link connect failed", "error", err)
		return
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK ||
		!strings.HasPrefix(resp.Header.Get("Content-Type"), "text/event-stream") {
		l.g.log.Warn("control link rejected", "status", resp.StatusCode)
		return
	}

	err = readSSE(resp.Body, func(eventName string, data []byte) {
		switch eventName {
		case ctlapi.LinkEventRegistry:
			var rf ctlapi.RegistryFrame
			if jerr := json.Unmarshal(data, &rf); jerr != nil {
				l.g.log.Warn("malformed registry frame ignored", "error", jerr)
				return
			}
			l.g.log.Info("registry change notified by daemon",
				"kind", rf.Kind, "rev", rf.Rev)
			// The frame is a notification, not a snapshot: re-read and adopt
			// iff generation >= applied (canonical.md §5c #2). The local
			// watcher would eventually catch the same change; the link is
			// just the faster channel.
			l.g.onRegistryChange(registry.DocKind(rf.Kind))
		default:
			l.g.log.Debug("ignoring unknown link event", "event", eventName)
		}
	})
	if err != nil && ctx.Err() == nil {
		l.g.log.Warn("control link stream ended", "error", err)
	}
}

// clear drops the registration state.
func (l *ctlLink) clear(reason string) {
	l.mu.Lock()
	hadSession := l.sessionID != ""
	l.sessionID = ""
	l.mu.Unlock()
	if hadSession {
		l.g.log.Info("daemon session ended", "reason", reason)
	}
	if hadSession {
		// Fall back to the static three-layer scope (and, on the widowed-
		// overlay path, WIDEN back from the discarded overlay — a session
		// grant dies with its session by design, docs/architecture.md §2).
		l.g.onSessionChanged()
	}
}

// reportDown logs a dial/registration failure: Info the first time, Debug
// on repeats (a permanently absent daemon is normal operation and must not
// spam the log every retry interval).
func (l *ctlLink) reportDown(err error) {
	if l.loggedDown {
		l.g.log.Debug("daemon still unreachable", "error", err)
		return
	}
	l.loggedDown = true
	l.g.log.Info("daemon unreachable; running standalone", "error", err, "retry", l.retry.String())
}

// setHeaders stamps the control-plane version and actor headers.
func (l *ctlLink) setHeaders(req *http.Request, actor string) {
	req.Header.Set(api.HeaderAPIVersion, api.APIVersion)
	req.Header.Set("X-Agenthub-Actor", actor)
}

// post performs one enveloped POST against the control socket.
func (l *ctlLink) post(ctx context.Context, path, actor string, in, out any) error {
	body, err := json.Marshal(in)
	if err != nil {
		return fmt.Errorf("gateway: encoding %s body: %w", path, err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		"http://agenthub"+path, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("gateway: building %s request: %w", path, err)
	}
	l.setHeaders(req, actor)
	req.Header.Set("Content-Type", "application/json")
	resp, err := l.hc.Do(req)
	if err != nil {
		return fmt.Errorf("gateway: %s: %w", path, err)
	}
	defer func() { _ = resp.Body.Close() }()

	var env struct {
		OK    bool            `json:"ok"`
		Data  json.RawMessage `json:"data"`
		Error *struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	// Fail-closed: anything that is not a well-formed success envelope is an
	// error (a torn body must never read as success).
	if err := json.NewDecoder(resp.Body).Decode(&env); err != nil {
		return fmt.Errorf("gateway: %s: status %d with undecodable body: %w", path, resp.StatusCode, err)
	}
	if env.Error != nil {
		return fmt.Errorf("gateway: %s: %s: %s", path, env.Error.Code, env.Error.Message)
	}
	if !env.OK || resp.StatusCode >= 400 {
		return fmt.Errorf("gateway: %s: status %d without error body", path, resp.StatusCode)
	}
	if out != nil {
		if len(env.Data) == 0 {
			return fmt.Errorf("gateway: %s: success envelope missing data", path)
		}
		if err := json.Unmarshal(env.Data, out); err != nil {
			return fmt.Errorf("gateway: %s: decoding data: %w", path, err)
		}
	}
	return nil
}

// readSSE parses a minimal SSE stream (event/data fields, ':' comments),
// invoking handle once per dispatched event. It returns when the stream
// ends. Frame size is bounded by the scanner buffer — overlay frames are
// small by construction.
func readSSE(r io.Reader, handle func(event string, data []byte)) error {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), 1<<20)
	eventName := ""
	var data []byte
	for sc.Scan() {
		line := sc.Text()
		switch {
		case line == "":
			if len(data) > 0 {
				handle(eventName, data)
			}
			eventName, data = "", nil
		case strings.HasPrefix(line, ":"):
			// keep-alive comment
		case strings.HasPrefix(line, "event:"):
			eventName = strings.TrimSpace(strings.TrimPrefix(line, "event:"))
		case strings.HasPrefix(line, "data:"):
			chunk := strings.TrimPrefix(line, "data:")
			chunk = strings.TrimPrefix(chunk, " ")
			if len(data) > 0 {
				data = append(data, '\n')
			}
			data = append(data, chunk...)
		default:
			// id: and unknown fields are irrelevant to the link protocol.
		}
	}
	return sc.Err()
}
