package transport

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/dinstein/agent-hub/internal/mcp"
)

// cancelForwardTimeout bounds the best-effort notifications/cancelled POST
// sent when a call's context dies. It runs on the transport lifetime
// context, not the dead call context.
const cancelForwardTimeout = 5 * time.Second

// deleteTimeout bounds the session-terminating DELETE issued by Close.
const deleteTimeout = 3 * time.Second

// streamableHTTP is the MCP 2025-11-25 Streamable HTTP client transport.
//
// Shape of the binding (canonical.md §5b — read side):
//
//   - every client→server message is POSTed to the single endpoint,
//   - the POST answer is either application/json (one response) or
//     text/event-stream (a stream that carries the response plus any
//     server-initiated reverse RPCs and notifications emitted while the
//     call is in flight),
//   - Mcp-Session-Id returned by initialize is echoed on every later
//     request, and DELETEd on Close,
//   - an optional GET opens an independent server→client SSE stream for
//     notifications that belong to no call,
//   - Last-Event-ID resumes a POST stream that died mid-call (best effort:
//     one attempt, and never after 410 Gone).
//
// Failure model: unlike stdio there is no single long-lived byte stream, so
// a broken request does NOT poison the transport — only Close does. Each
// call is classified on its own (see httpError / requestError), which is
// what lets internal/downstream's breaker see per-request health.
type streamableHTTP struct {
	httpBase

	notifyStream  bool
	disableResume bool
	retryBase     time.Duration

	// ctx is the transport lifetime; cancelled by Close. It parents the
	// GET stream and every out-of-call POST (peer replies, cancelled).
	ctx    context.Context
	cancel context.CancelFunc

	nextID atomic.Int64

	mu        sync.Mutex
	sessionID string
	// sessionLost records that a session id this transport HELD was
	// invalidated by the server. Sticky, and not derivable from sessionID
	// being empty: a transport that never had one looks the same.
	// lostSession is the accessor; reclassifySessionLoss and streamLoop read it.
	sessionLost  bool
	protoVersion string
	// reqMeta, when non-nil, switches the transport into 2026-07-28
	// stateless mode: every outgoing message carries it as _meta, and every
	// POST carries the Mcp-Method (and, when applicable, Mcp-Name) header.
	// Set once by Handshake (via setNegotiated), before concurrent calls.
	reqMeta     *mcp.RequestMeta
	lastEventID string
	peer        PeerHandler
	onChange    func(ChangeMask)
	closed      bool
	streamOn    bool
	moved       bool // 410 seen: never dial this endpoint again

	peerSem   chan struct{}
	wg        sync.WaitGroup
	closeOnce sync.Once
}

// DialStreamableHTTP returns a Streamable HTTP transport for cfg. No
// connection is made here: the first Call (normally initialize) opens one.
// Configuration errors are ClassFatal.
//
// SSRF screening is the caller's to inject via HTTPConfig.DialContext —
// this package is standard-library only and cannot import
// internal/guard/netguard (canonical.md §2 rule 2).
func DialStreamableHTTP(cfg HTTPConfig) (Transport, error) {
	base, err := newHTTPBase(cfg)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithCancel(context.Background())
	return &streamableHTTP{
		httpBase:      base,
		notifyStream:  cfg.NotificationStream,
		disableResume: cfg.DisableResume,
		retryBase:     cfg.retryBase,
		ctx:           ctx,
		cancel:        cancel,
		peerSem:       make(chan struct{}, maxPeerWorkers),
	}, nil
}

// setNegotiated implements negotiatedSetter: Handshake records the outcome
// here. It reuses the protoVersion slot the legacy path fills from
// afterInitialize, so the MCP-Protocol-Version header stays correct on both.
// On the 2026 path it also opens the subscriptions/listen stream — the
// replacement for the GET notification stream afterInitialize would have
// opened, gated by the same NotificationStream config.
func (t *streamableHTTP) setNegotiated(version string, meta *mcp.RequestMeta) {
	t.mu.Lock()
	t.protoVersion = version
	t.reqMeta = meta
	t.mu.Unlock()
	if meta != nil && t.notifyStream {
		t.startBackgroundStream(t.openListenStream)
	}
}

func (t *streamableHTTP) currentMeta() *mcp.RequestMeta {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.reqMeta
}

// Call implements Transport.
func (t *streamableHTTP) Call(ctx context.Context, method string, params any) (json.RawMessage, error) {
	raw, err := marshalParams(params)
	if err != nil {
		return nil, &Error{Class: ClassFatal, Err: err}
	}
	if raw, err = injectMeta(raw, t.currentMeta()); err != nil {
		return nil, &Error{Class: ClassFatal, Err: err}
	}
	if err := t.stateErr(); err != nil {
		return nil, err
	}
	id := mcp.NewIntID(t.nextID.Add(1))
	body, err := encodeMessage(mcp.NewRequest(id, method, raw), t.maxFrame)
	if err != nil {
		return nil, err
	}

	resp, terr := t.post(ctx, body, method, nameForHeader(raw), versionForHeader(raw))
	if terr != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			t.sendCancelled(id, ctxErr)
			return nil, ctxErr
		}
		return nil, terr
	}

	var result json.RawMessage
	mt := responseMediaType(resp)
	switch mt {
	case mediaJSON:
		result, err = t.readJSONResponse(resp, id)
	case mediaSSE:
		result, err = t.awaitStreamResponse(ctx, resp, id, true)
	default:
		drainClose(resp)
		err = &Error{Class: ClassUnavailable, Err: fmt.Errorf(
			"%w: request %q answered with content-type %q (want %s or %s)",
			ErrHTTPProtocol, method, mt, mediaJSON, mediaSSE)}
	}
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			t.sendCancelled(id, ctxErr)
			return nil, ctxErr
		}
		return nil, err
	}
	if method == mcp.MethodInitialize {
		t.afterInitialize(result)
	}
	return result, nil
}

// Notify implements Transport. A notification carries no id, so the server
// answers 202 Accepted with no body; any 2xx is accepted and drained.
func (t *streamableHTTP) Notify(ctx context.Context, method string, params any) error {
	raw, err := marshalParams(params)
	if err != nil {
		return &Error{Class: ClassFatal, Err: err}
	}
	if err := t.stateErr(); err != nil {
		return err
	}
	if raw, err = injectMeta(raw, t.currentMeta()); err != nil {
		return &Error{Class: ClassFatal, Err: err}
	}
	body, err := encodeMessage(mcp.NewNotification(method, raw), t.maxFrame)
	if err != nil {
		return err
	}
	resp, terr := t.post(ctx, body, method, "", versionForHeader(raw))
	if terr != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return ctxErr
		}
		return terr
	}
	drainClose(resp)
	return nil
}

// OnPeerRequest implements Transport. Reverse RPCs arriving on any stream
// are answered by POSTing the response back to the endpoint, so — unlike
// stdio — the handler runs off the reader goroutine and MAY call back into
// this transport. Concurrency is bounded by maxPeerWorkers.
func (t *streamableHTTP) OnPeerRequest(h PeerHandler) {
	t.mu.Lock()
	t.peer = h
	t.mu.Unlock()
}

// OnListChanged implements Transport.
func (t *streamableHTTP) OnListChanged(fn func(mask ChangeMask)) {
	t.mu.Lock()
	t.onChange = fn
	t.mu.Unlock()
}

// Stderr implements Transport: no child process, no stderr.
func (t *streamableHTTP) Stderr() string { return "" }

// Close implements Transport. It DELETEs the session (best effort, on a
// fresh context so it survives the lifetime cancellation), cancels every
// in-flight stream, and waits for the background goroutines. Idempotent.
func (t *streamableHTTP) Close() error {
	t.closeOnce.Do(func() {
		t.mu.Lock()
		t.closed = true
		sid, moved := t.sessionID, t.moved
		t.mu.Unlock()

		if sid != "" && !moved {
			t.deleteSession(sid)
		}
		t.cancel()
		t.wg.Wait()
	})
	return nil
}

// stateErr reports the terminal states: closed, or an endpoint that
// answered 410 Gone (never contacted again).
func (t *streamableHTTP) stateErr() *Error {
	t.mu.Lock()
	defer t.mu.Unlock()
	switch {
	case t.closed:
		return &Error{Class: ClassUnavailable, Err: ErrClosed}
	case t.moved:
		return &Error{Class: ClassUnavailable, Err: fmt.Errorf("%w: %s", ErrEndpointMoved, t.endpoint.Redacted())}
	default:
		return nil
	}
}

// post sends one message and returns the 2xx response with its body still
// open. Non-2xx bodies are drained and turned into typed errors here.
// method and name feed the Mcp-Method / Mcp-Name headers MCP 2026-07-28
// requires on every POST; both are ignored in pre-2026 mode, and an empty
// method (a JSON-RPC response being POSTed back) suppresses them.
// declared is the protocol version the body's _meta carries, and wins over
// transport state for the MCP-Protocol-Version header: the two MUST agree.
func (t *streamableHTTP) post(ctx context.Context, body []byte, method, name, declared string) (*http.Response, *Error) {
	req, err := t.newRequest(ctx, http.MethodPost, t.endpoint, body)
	if err != nil {
		return nil, &Error{Class: ClassFatal, Err: fmt.Errorf("build request: %w", err)}
	}
	req.Header.Set(headerContentType, mediaJSON)
	req.Header.Set(headerAccept, mediaJSON+", "+mediaSSE)
	// server/discover is itself a 2026-shaped request — it goes out before
	// any version is negotiated, and a strict stateless server rejects a
	// POST without Mcp-Method (-32020). Everything else gains the headers
	// only once 2026 is negotiated, keeping pre-2026 traffic byte-identical.
	if method != "" && (t.currentMeta() != nil || method == mcp.MethodDiscover) {
		req.Header.Set(headerMcpMethod, method)
		if name != "" {
			req.Header.Set(headerMcpName, name)
		}
	}
	t.applyProtocolHeaders(req, declared)

	resp, err := t.client.Do(req)
	if err != nil {
		return nil, requestError(err)
	}
	t.captureSession(resp)
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		terr := httpError(resp)
		t.noteTerminalStatus(resp.StatusCode, terr.RPCCode)
		return nil, t.reclassifySessionLoss(req, resp.StatusCode, terr)
	}
	return resp, nil
}

// reclassifySessionLoss rescues the one error that would otherwise strand a
// transport whose session the server has already dropped.
//
// The sequence is the specification's own, and every step of it is right: the
// server drops a session, answers 404, this transport clears its id
// (noteTerminalStatus), and the NEXT legacy request therefore goes out with
// no Mcp-Session-Id at all. What comes back for that one is a server telling
// us we need a session — and a conformant ≤ 2025-11-25 server says so with
// 400 Bad Request, which the status table classifies ClassFatal: "our request
// was rejected on its merits". True in general, and exactly wrong here.
// ClassFatal neither trips the breaker nor triggers a respawn, so the
// connection sits with no session and nothing that will ever mint one, while
// the very thing it needs — a fresh initialize — is what a respawn does.
//
// Recovery works today only because agenthub's own exposure face answers 404
// where the specification says 400, and 404 is ClassUnavailable. That is the
// server-side defect this commit's sibling fixes, and fixing it without this
// would turn hub-to-hub session expiry from slow self-heal into a dead
// connection.
//
// The predicate is narrow on purpose, and each clause carries its weight:
//
//   - we sent NO session header, so this cannot be the server rejecting an
//     id it does not like (that answer is 404 and already recoverable);
//   - we HELD one and lost it, so a server that never issued a session and
//     simply dislikes this request keeps its honest ClassFatal;
//   - the status is 400, the one a conformant server uses for it.
//
// FAIL-CLOSED direction: when in doubt the error keeps its original class.
// Being wrong here costs a respawn that was not needed; leaving the case out
// costs a connection that never comes back.
// lostSession reports whether a session id this transport HELD was
// invalidated by the peer. It is the same fact reclassifySessionLoss reads,
// and streamLoop needs it for a different purpose: to say which of the two
// reasons a stream ended.
func (t *streamableHTTP) lostSession() bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.sessionLost
}

func (t *streamableHTTP) reclassifySessionLoss(req *http.Request, code int, terr *Error) *Error {
	if code != http.StatusBadRequest || req.Header.Get(headerSessionID) != "" {
		return terr
	}
	if !t.lostSession() {
		return terr
	}
	return &Error{
		Class: ClassUnavailable, StatusCode: code, RPCCode: terr.RPCCode,
		Err: fmt.Errorf("%w: sent without a session id after the server dropped one: %w",
			ErrSessionExpired, terr.Err),
	}
}

// noteTerminalStatus records the two statuses that change transport state:
// 410 poisons the endpoint permanently, 404 invalidates the session id so
// a caller-driven re-initialize can mint a new one.
//
// rpcCode is the JSON-RPC code the rejected body carried, or 0. A 404 that
// carries one is an unimplemented METHOD, not a dropped session, and
// throwing away a live session id over it would turn "I do not serve that"
// into a reconnect. Callers that have no body to classify pass 0 and get the
// status-only behaviour.
func (t *streamableHTTP) noteTerminalStatus(code, rpcCode int) {
	t.mu.Lock()
	defer t.mu.Unlock()
	switch code {
	case http.StatusGone:
		t.moved = true
	case http.StatusNotFound:
		if rpcCode != 0 {
			return
		}
		// sessionLost is sticky and separate from the id being empty: a
		// transport that never had a session looks identical otherwise, and
		// reclassifySessionLoss must tell the two apart.
		if t.sessionID != "" {
			t.sessionLost = true
		}
		t.sessionID = ""
	}
}

// applyProtocolHeaders sets the session and protocol-version headers.
// Before initialize completes the declared version is used; afterwards the
// negotiated one, as the 2025-06-18+ binding requires.
//
// declared overrides both when the body being sent carries its own protocol
// _meta: MCP 2026-07-28 requires the header to equal that value exactly, and
// a POST whose header and body disagree MUST be rejected with -32020. Empty
// means the body declared nothing, which leaves transport state in charge.
func (t *streamableHTTP) applyProtocolHeaders(req *http.Request, declared string) {
	t.mu.Lock()
	sid, pv := t.sessionID, t.protoVersion
	t.mu.Unlock()
	if sid != "" {
		req.Header.Set(headerSessionID, sid)
	}
	switch {
	case declared != "":
		pv = declared
	case pv == "":
		pv = mcp.ProtocolVersion
	}
	req.Header.Set(headerProtocolVersion, pv)
}

// captureSession records the Mcp-Session-Id a response carried. The spec
// assigns it on the initialize response; accepting it from any response is
// strictly more forgiving and never loses one.
func (t *streamableHTTP) captureSession(resp *http.Response) {
	sid := resp.Header.Get(headerSessionID)
	if sid == "" {
		return
	}
	t.mu.Lock()
	t.sessionID = sid
	t.mu.Unlock()
}

// afterInitialize records the negotiated protocol version (echoed on every
// later request) and opens the optional notification stream. Validation of
// the version is Initialize's job, not the transport's — an unparsable
// result simply leaves the declared version in place.
func (t *streamableHTTP) afterInitialize(result json.RawMessage) {
	var res struct {
		ProtocolVersion string `json:"protocolVersion"`
	}
	if err := json.Unmarshal(result, &res); err == nil && res.ProtocolVersion != "" {
		t.mu.Lock()
		t.protoVersion = res.ProtocolVersion
		t.mu.Unlock()
	}
	if t.notifyStream {
		t.startNotificationStream()
	}
}

// readJSONResponse handles an application/json answer: one message (or, for
// a 2025-03-26 server, a batch) containing our response.
func (t *streamableHTTP) readJSONResponse(resp *http.Response, id mcp.ID) (json.RawMessage, error) {
	defer drainClose(resp)
	data, err := readBounded(resp.Body, t.maxFrame)
	if err != nil {
		return nil, &Error{Class: ClassUnavailable, Err: fmt.Errorf("read response body: %w", err)}
	}
	msgs, err := decodeMessages(data)
	if err != nil {
		return nil, &Error{Class: ClassUnavailable, Err: err}
	}
	for _, msg := range msgs {
		switch m := msg.(type) {
		case *mcp.Response:
			if m.ID.Key() != id.Key() {
				continue
			}
			if m.Error != nil {
				return nil, &Error{Class: ClassFatal, Err: m.Error}
			}
			return m.Result, nil
		case *mcp.Request:
			t.dispatchPeer(m)
		case *mcp.Notification:
			t.dispatchNotification(m)
		}
	}
	return nil, &Error{Class: ClassUnavailable, Err: fmt.Errorf(
		"%w: json answer carried no response for request %s", ErrHTTPProtocol, id)}
}

// awaitStreamResponse consumes a text/event-stream answer until it carries
// the response to id. Reverse RPCs and notifications seen on the way are
// dispatched. allowResume permits exactly one Last-Event-ID resumption of a
// stream that ended early.
func (t *streamableHTTP) awaitStreamResponse(ctx context.Context, resp *http.Response, id mcp.ID, allowResume bool) (json.RawMessage, error) {
	defer drainClose(resp)
	sc := newSSEScanner(resp.Body, t.maxFrame)
	for {
		ev, err := sc.Next()
		if err != nil {
			t.rememberEventID(sc.lastEventID())
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			if errors.Is(err, io.EOF) && allowResume && !t.disableResume && sc.lastEventID() != "" {
				if waitBeforeResume(ctx, sc.retryHint()) {
					if resumed, rerr := t.resumeStream(ctx, sc.lastEventID()); rerr == nil {
						return t.awaitStreamResponse(ctx, resumed, id, false)
					}
				}
			}
			return nil, &Error{Class: ClassUnavailable, Err: fmt.Errorf(
				"stream ended before the response to request %s: %w", id, err)}
		}
		if ev.id != "" {
			t.rememberEventID(ev.id)
		}
		if ev.name != "message" || len(ev.data) == 0 {
			continue // keep-alives and vendor event types are not messages
		}
		msg, perr := mcp.ParseMessage(ev.data)
		if perr != nil {
			return nil, &Error{Class: ClassUnavailable, Err: perr}
		}
		switch m := msg.(type) {
		case *mcp.Response:
			if m.ID.Key() != id.Key() {
				continue // a late reply to an abandoned call
			}
			if m.Error != nil {
				return nil, &Error{Class: ClassFatal, Err: m.Error}
			}
			return m.Result, nil
		case *mcp.Request:
			t.dispatchPeer(m)
		case *mcp.Notification:
			t.dispatchNotification(m)
		}
	}
}

// waitBeforeResume holds off a per-call resumption for as long as the server
// asked, and reports whether resuming is still allowed.
//
// MCP 2025-11-25 makes waiting out a `retry` hint a MUST (see
// sseScanner.retryHint). This client cannot always obey it: the resume runs
// under the caller's context, so sleeping out a 30-second hint on a call with
// ten seconds left would turn an answer into a deadline. The rule it keeps
// instead is the one the MUST exists to protect — NEVER RECONNECT SOONER THAN
// ASKED — and it keeps it in the only other direction available: when the
// wait does not fit, the resume is abandoned and the caller reports the
// stream break it already had. Coming back early is the one option ruled out.
//
// No hint means no obligation, and resumption proceeds immediately as before.
func waitBeforeResume(ctx context.Context, hint time.Duration) bool {
	if hint <= 0 {
		return true
	}
	if deadline, ok := ctx.Deadline(); ok && time.Until(deadline) <= hint {
		return false
	}
	timer := time.NewTimer(hint)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}

// resumeStream reopens the stream at lastEventID. Best effort by contract:
// any failure returns an error and the caller reports the original break.
func (t *streamableHTTP) resumeStream(ctx context.Context, lastID string) (*http.Response, error) {
	if err := t.stateErr(); err != nil {
		return nil, err
	}
	req, err := t.newRequest(ctx, http.MethodGet, t.endpoint, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set(headerAccept, mediaSSE)
	req.Header.Set(headerLastEventID, lastID)
	// Resume is a ≤ 2025-11-25 mechanism and carries no body to declare a
	// version; transport state is the only source.
	t.applyProtocolHeaders(req, "")

	resp, err := t.client.Do(req)
	if err != nil {
		return nil, err
	}
	t.captureSession(resp)
	if resp.StatusCode != http.StatusOK {
		terr := httpError(resp)
		t.noteTerminalStatus(resp.StatusCode, terr.RPCCode)
		return nil, terr
	}
	if responseMediaType(resp) != mediaSSE {
		drainClose(resp)
		return nil, fmt.Errorf("%w: resume answered with content-type %q", ErrHTTPProtocol, responseMediaType(resp))
	}
	return resp, nil
}

func (t *streamableHTTP) rememberEventID(id string) {
	if id == "" {
		return
	}
	t.mu.Lock()
	t.lastEventID = id
	t.mu.Unlock()
}

// startNotificationStream opens the optional server→client GET stream once
// (≤ 2025-11-25). It carries notifications (and reverse RPCs) that belong
// to no call.
func (t *streamableHTTP) startNotificationStream() {
	t.startBackgroundStream(t.openNotificationStream)
}

// startBackgroundStream runs one long-lived out-of-call stream (the legacy
// GET stream or the 2026 subscriptions/listen stream, chosen by the opener)
// once per transport.
func (t *streamableHTTP) startBackgroundStream(open func() (*http.Response, bool, error)) {
	t.mu.Lock()
	if t.streamOn || t.closed {
		t.mu.Unlock()
		return
	}
	t.streamOn = true
	// wg.Add happens under the same lock that publishes closed, so no Add
	// can race Close's Wait.
	t.wg.Add(1)
	t.mu.Unlock()

	go func() {
		defer t.wg.Done()
		t.streamLoop(open)
	}()
}

// streamLoop keeps the out-of-call stream open until Close, reconnecting
// with bounded backoff (and, on the legacy GET stream, carrying
// Last-Event-ID across breaks).
//
// It gives up permanently when the opener says so, which is the whole 4xx
// class minus 408/429, plus 501 and 410 — streamRefusedPermanently says why
// the class and not a list. This comment used to name the six statuses that
// predicate replaced, and so contradicted the code twenty lines below it.
//
// KNOWN GAP — this loop has no session-recovery path. If the peer expires
// the session, the reopen carries a stale id (404) or, once a POST has
// cleared it, none at all (400), and both are permanent refusals. The stream
// then stays down for the life of the transport although the server does
// offer it. Nothing polls it back up: the gateway sets no PingInterval,
// deliberately. Recovery waits for the next real tools/call, whose own
// rejection reclassifySessionLoss upgrades to ClassUnavailable and which
// respawns the connection — and for an idle hosted downstream that can be a
// long time, which is the very case opening this stream exists to fix.
//
// Not fixed here because the obvious fix is a policy decision rather than a
// patch: retrying a session-loss refusal reintroduces exactly the unbounded
// five-second heartbeat streamRefusedPermanently was written to remove,
// unless something bounds it — a retry budget, or a signal from the POST
// path that a session exists again. What IS fixed here is the half that
// costs nothing: the give-up line no longer blames the server for a session
// this client lost.
func (t *streamableHTTP) streamLoop(open func() (*http.Response, bool, error)) {
	attempt := 0
	for {
		if t.ctx.Err() != nil || t.stateErr() != nil {
			return
		}
		resp, permanent, err := open()
		if err != nil {
			if permanent {
				// Say so once. This is the only place that learns the
				// stream is over, and it used to drop the error on the
				// floor — leaving an operator asking why a
				// tools/list_changed never arrives with nothing at all to
				// read, since the goroutine then exits silently.
				//
				// Two causes, and naming the wrong one sends that operator
				// to the wrong system. A server that does not offer the
				// stream is a fact about the server; a session this client
				// lost is a fact about this client, and the same server
				// would serve the stream again to a connection that had
				// one. See the KNOWN GAP above for why it does not get one.
				if t.lostSession() {
					t.log.Info("server-initiated notification stream ended with the session; "+
						"list changes will not be pushed again until this connection is rebuilt",
						"reason", err)
					return
				}
				t.log.Info("no server-initiated notification stream; "+
					"list changes will not be pushed on this connection", "reason", err)
				return
			}
			if !t.sleep(backoffFor(t.retryBase, attempt)) {
				return
			}
			attempt++
			continue
		}
		attempt = 0
		// The server's own pacing wins when it asked for more than our
		// backoff would have taken: 2025-11-25 makes waiting out a `retry`
		// hint a MUST, and this loop has no caller deadline to trade it
		// against, so the full hint is honoured however long it is. It
		// interrupts on Close like any other wait.
		if !t.sleep(max(backoffFor(t.retryBase, 0), t.consumeStream(resp))) {
			return
		}
	}
}

// openListenStream performs one subscriptions/listen POST (MCP 2026-07-28):
// the long-lived SSE answer replaces the legacy GET notification stream.
// The stream's notifications dispatch by method name exactly like the GET
// stream's did; the per-event subscriptionId in _meta is correlation data
// this client does not need until it subscribes to individual resources.
// permanent=true means "never retry this endpoint".
//
// The filter is an allow list the server MUST honour exactly, so it names
// the three list-changed types and nothing else. ResourceSubscriptions stays
// nil rather than empty: this client subscribes to no individual resource,
// and asking for none is not the same as asking for an empty set.
func (t *streamableHTTP) openListenStream() (resp *http.Response, permanent bool, err error) {
	params := mcp.SubscriptionsListenParams{
		Notifications: mcp.SubscriptionFilter{
			ToolsListChanged:     true,
			PromptsListChanged:   true,
			ResourcesListChanged: true,
		},
		Meta: t.currentMeta(),
	}
	raw, merr := json.Marshal(params)
	if merr != nil {
		return nil, true, merr
	}
	body, berr := encodeMessage(mcp.NewRequest(mcp.NewIntID(t.nextID.Add(1)), mcp.MethodSubscriptionsListen, raw), t.maxFrame)
	if berr != nil {
		return nil, true, berr
	}
	req, err := t.newRequest(t.ctx, http.MethodPost, t.endpoint, body)
	if err != nil {
		return nil, true, err
	}
	req.Header.Set(headerContentType, mediaJSON)
	req.Header.Set(headerAccept, mediaSSE)
	req.Header.Set(headerMcpMethod, mcp.MethodSubscriptionsListen)
	t.applyProtocolHeaders(req, versionForHeader(raw))

	resp, err = t.client.Do(req)
	if err != nil {
		return nil, t.ctx.Err() != nil, err
	}
	if resp.StatusCode != http.StatusOK {
		code := resp.StatusCode
		terr := httpError(resp)
		t.noteTerminalStatus(code, terr.RPCCode)
		if streamRefusedPermanently(code) {
			return nil, true, terr
		}
		return nil, false, terr
	}
	if responseMediaType(resp) != mediaSSE {
		// A JSON answer is the server answering the listen request with a
		// single response (usually an error): it does not offer the stream.
		// Give up rather than hammer it.
		drainClose(resp)
		return nil, true, fmt.Errorf("%w: subscriptions/listen answered content-type %q",
			ErrHTTPProtocol, responseMediaType(resp))
	}
	return resp, false, nil
}

// openNotificationStream performs one GET. permanent=true means "never
// retry this endpoint".
func (t *streamableHTTP) openNotificationStream() (resp *http.Response, permanent bool, err error) {
	req, err := t.newRequest(t.ctx, http.MethodGet, t.endpoint, nil)
	if err != nil {
		return nil, true, err
	}
	req.Header.Set(headerAccept, mediaSSE)
	t.mu.Lock()
	lastID := t.lastEventID
	t.mu.Unlock()
	if lastID != "" {
		req.Header.Set(headerLastEventID, lastID)
	}
	// The GET stream is the ≤ 2025-11-25 shape; it has no body and so
	// declares no version of its own.
	t.applyProtocolHeaders(req, "")

	resp, err = t.client.Do(req)
	if err != nil {
		return nil, t.ctx.Err() != nil, err
	}
	t.captureSession(resp)
	if resp.StatusCode != http.StatusOK {
		code := resp.StatusCode
		terr := httpError(resp)
		t.noteTerminalStatus(code, terr.RPCCode)
		if streamRefusedPermanently(code) {
			return nil, true, terr
		}
		return nil, false, terr
	}
	if responseMediaType(resp) != mediaSSE {
		drainClose(resp)
		return nil, true, fmt.Errorf("%w: GET stream content-type %q", ErrHTTPProtocol, responseMediaType(resp))
	}
	return resp, false, nil
}

// streamRefusedPermanently reports whether a status answering an attempt to
// open the out-of-call stream means "never ask again" rather than "not right
// now".
//
// The rule is the whole 4xx class, minus the two that are explicitly about
// timing. A 4xx says the request itself is wrong, and this request never
// changes: it is the same GET (or the same subscriptions/listen POST) on
// every attempt, so a server that refused it once refuses it forever, and
// retrying is a loop that cannot terminate. That is not hypothetical — the
// backoff caps at maxRetryBackoff, so an unlisted 4xx becomes one request
// every five seconds for the life of the connection, per downstream. The
// named list this replaced covered 405/501/410/404/401/403 and let 400
// through, which is the status a server that simply does not understand the
// GET is most likely to pick.
//
//	408 Request Timeout and 429 Too Many Requests are the exceptions,
//	because both are the server saying "later", which is what a retry is
//	for.
//
// 501 Not Implemented is the one 5xx here: it is a statement about the
// endpoint rather than about its current health.
//
// FAIL-OPEN by design, and cheaply: a status not on this list keeps
// retrying, which costs one request per backoff step and never loses data.
// Being wrong the other way — giving up on a transient failure — silently
// ends list-change delivery for the life of the connection.
func streamRefusedPermanently(code int) bool {
	switch code {
	case http.StatusRequestTimeout, http.StatusTooManyRequests:
		return false
	case http.StatusNotImplemented:
		return true
	}
	return code >= 400 && code < 500
}

// consumeStream drains a server→client stream, dispatching notifications
// and reverse RPCs. Responses on this stream match no in-flight POST and
// are dropped. It returns when the stream ends or the transport closes.
//
// The returned duration is the `retry` hint the server left behind, 0 for
// none. streamLoop owes it a wait at least that long before reopening; see
// sseScanner.retryHint for the rule and waitBeforeResume for the per-call
// half of it. A stream this client abandons over a framing error returns no
// hint: the peer is not keeping its side of the protocol, so its pacing
// request is not the thing to honour.
func (t *streamableHTTP) consumeStream(resp *http.Response) time.Duration {
	defer drainClose(resp)
	sc := newSSEScanner(resp.Body, t.maxFrame)
	for {
		ev, err := sc.Next()
		if err != nil {
			t.rememberEventID(sc.lastEventID())
			return sc.retryHint()
		}
		if ev.id != "" {
			t.rememberEventID(ev.id)
		}
		if ev.name != "message" || len(ev.data) == 0 {
			continue
		}
		msg, perr := mcp.ParseMessage(ev.data)
		if perr != nil {
			return 0 // a peer that emits garbage cannot keep framing intact
		}
		switch m := msg.(type) {
		case *mcp.Request:
			t.dispatchPeer(m)
		case *mcp.Notification:
			t.dispatchNotification(m)
		case *mcp.Response:
			// No POST is waiting on this stream; drop it.
		}
	}
}

// sleep waits d unless the transport closes first. It reports whether the
// wait completed.
func (t *streamableHTTP) sleep(d time.Duration) bool {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-t.ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}

func (t *streamableHTTP) dispatchNotification(n *mcp.Notification) {
	if n.Method == mcp.NotificationSubscriptionsAcknowledged {
		t.noteAcknowledged(n)
		return
	}
	mask, ok := changeMaskFor(n.Method)
	if !ok {
		return // unknown notifications are ignored, never fatal
	}
	t.mu.Lock()
	fn := t.onChange
	t.mu.Unlock()
	if fn != nil {
		fn(mask)
	}
}

// noteAcknowledged reads the first message of a subscriptions/listen stream
// (MCP 2026-07-28), which reports the subset of the requested filter the
// server will honour. Types it declined are omitted rather than refused, so
// a stream that will never deliver a tools/list_changed looks exactly like a
// server whose tools never change — this log line is the only difference an
// operator can see, and it is why the notification is worth reading at all.
//
// Never fatal: the subscription is already open and useful for whatever the
// server did acknowledge. A malformed payload is dropped like any other
// unparsable notification.
func (t *streamableHTTP) noteAcknowledged(n *mcp.Notification) {
	var p mcp.SubscriptionsAcknowledgedParams
	if len(n.Params) == 0 || json.Unmarshal(n.Params, &p) != nil {
		return
	}
	// openListenStream asks for exactly these three, so anything false here
	// was declined.
	var declined []string
	for name, honoured := range map[string]bool{
		"toolsListChanged":     p.Notifications.ToolsListChanged,
		"promptsListChanged":   p.Notifications.PromptsListChanged,
		"resourcesListChanged": p.Notifications.ResourcesListChanged,
	} {
		if !honoured {
			declined = append(declined, name)
		}
	}
	if len(declined) == 0 {
		return
	}
	slices.Sort(declined) // map order is not a diagnostic
	t.log.Info("server declined subscription types; those changes will never be announced",
		"declined", strings.Join(declined, ","))
}

// dispatchPeer answers a server-initiated request on its own goroutine and
// POSTs the reply back to the endpoint. Acquiring the semaphore blocks the
// stream reader once maxPeerWorkers replies are in flight: bounded
// backpressure beats an unbounded goroutine fan-out driven by the peer.
func (t *streamableHTTP) dispatchPeer(req *mcp.Request) {
	select {
	case t.peerSem <- struct{}{}:
	case <-t.ctx.Done():
		return
	}
	t.mu.Lock()
	if t.closed {
		t.mu.Unlock()
		<-t.peerSem
		return
	}
	h := t.peer
	// wg.Add under the lock that publishes closed: Close never Waits on a
	// counter that is about to be incremented.
	t.wg.Add(1)
	t.mu.Unlock()

	go func() {
		defer t.wg.Done()
		defer func() { <-t.peerSem }()
		t.answerPeer(h, req)
	}()
}

func (t *streamableHTTP) answerPeer(h PeerHandler, req *mcp.Request) {
	ctx, cancel := context.WithTimeout(t.ctx, peerReplyTimeout)
	defer cancel()

	resp := invokePeer(ctx, h, req)
	body, err := encodeMessage(resp, t.maxFrame)
	if err != nil {
		// The handler produced an answer too large for one frame; replace
		// it with an in-band error so the server is not left waiting.
		body, err = encodeMessage(mcp.NewErrorResponse(req.ID, &mcp.Error{
			Code:    mcp.CodeInternalError,
			Message: "peer response exceeds frame size limit",
		}), t.maxFrame)
		if err != nil {
			return
		}
	}
	// A JSON-RPC response POSTed back carries no params and so declares no
	// version of its own; it is also a ≤ 2025-11-25 reverse-RPC mechanism.
	httpResp, terr := t.post(ctx, body, "", "", "")
	if terr != nil {
		return // best effort: the next call will surface the real state
	}
	drainClose(httpResp)
}

// sendCancelled forwards notifications/cancelled for an abandoned call.
// Best effort by contract: it runs on the transport lifetime context (the
// call context is already dead) and every failure is swallowed.
func (t *streamableHTTP) sendCancelled(id mcp.ID, cause error) {
	if err := t.stateErr(); err != nil {
		return
	}
	reason := ""
	if cause != nil {
		reason = cause.Error()
	}
	raw, err := json.Marshal(mcp.CancelledParams{RequestID: id, Reason: reason})
	if err != nil {
		return
	}
	if raw, err = injectMeta(raw, t.currentMeta()); err != nil {
		return
	}
	body, err := encodeMessage(mcp.NewNotification(mcp.NotificationCancelled, raw), t.maxFrame)
	if err != nil {
		return
	}
	ctx, cancel := context.WithTimeout(t.ctx, cancelForwardTimeout)
	defer cancel()
	resp, terr := t.post(ctx, body, mcp.NotificationCancelled, "", versionForHeader(raw))
	if terr == nil {
		drainClose(resp)
	}
}

// deleteSession terminates the session server-side. Best effort: a 405
// (server does not allow client termination) is a normal outcome.
func (t *streamableHTTP) deleteSession(sid string) {
	ctx, cancel := context.WithTimeout(context.Background(), deleteTimeout)
	defer cancel()
	req, err := t.newRequest(ctx, http.MethodDelete, t.endpoint, nil)
	if err != nil {
		return
	}
	req.Header.Set(headerSessionID, sid)
	t.mu.Lock()
	pv := t.protoVersion
	t.mu.Unlock()
	if pv == "" {
		pv = mcp.ProtocolVersion
	}
	req.Header.Set(headerProtocolVersion, pv)

	resp, err := t.client.Do(req)
	if err != nil {
		return
	}
	drainClose(resp)
}
