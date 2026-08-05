package transport

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
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

	mu           sync.Mutex
	sessionID    string
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
		t.noteTerminalStatus(resp.StatusCode)
		return nil, terr
	}
	return resp, nil
}

// noteTerminalStatus records the two statuses that change transport state:
// 410 poisons the endpoint permanently, 404 invalidates the session id so
// a caller-driven re-initialize can mint a new one.
func (t *streamableHTTP) noteTerminalStatus(code int) {
	t.mu.Lock()
	defer t.mu.Unlock()
	switch code {
	case http.StatusGone:
		t.moved = true
	case http.StatusNotFound:
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
				if resumed, rerr := t.resumeStream(ctx, sc.lastEventID()); rerr == nil {
					return t.awaitStreamResponse(ctx, resumed, id, false)
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
		t.noteTerminalStatus(resp.StatusCode)
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
// It gives up permanently when the opener says so — the statuses that mean
// "this server does not offer the stream" (405/501) or "stop asking"
// (410, 404, 401, 403); everything else is treated as transient.
func (t *streamableHTTP) streamLoop(open func() (*http.Response, bool, error)) {
	attempt := 0
	for {
		if t.ctx.Err() != nil || t.stateErr() != nil {
			return
		}
		resp, permanent, err := open()
		if err != nil {
			if permanent {
				return
			}
			if !t.sleep(backoffFor(t.retryBase, attempt)) {
				return
			}
			attempt++
			continue
		}
		attempt = 0
		t.consumeStream(resp)
		if !t.sleep(backoffFor(t.retryBase, 0)) {
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
func (t *streamableHTTP) openListenStream() (resp *http.Response, permanent bool, err error) {
	params := mcp.SubscriptionsListenParams{
		Events: []string{
			mcp.SubscriptionEventToolsListChanged,
			mcp.SubscriptionEventPromptsListChanged,
			mcp.SubscriptionEventResourcesListChanged,
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
		t.noteTerminalStatus(code)
		switch code {
		case http.StatusMethodNotAllowed, http.StatusNotImplemented,
			http.StatusGone, http.StatusNotFound,
			http.StatusUnauthorized, http.StatusForbidden:
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
		t.noteTerminalStatus(code)
		switch code {
		case http.StatusMethodNotAllowed, http.StatusNotImplemented,
			http.StatusGone, http.StatusNotFound,
			http.StatusUnauthorized, http.StatusForbidden:
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

// consumeStream drains a server→client stream, dispatching notifications
// and reverse RPCs. Responses on this stream match no in-flight POST and
// are dropped. It returns when the stream ends or the transport closes.
func (t *streamableHTTP) consumeStream(resp *http.Response) {
	defer drainClose(resp)
	sc := newSSEScanner(resp.Body, t.maxFrame)
	for {
		ev, err := sc.Next()
		if err != nil {
			t.rememberEventID(sc.lastEventID())
			return
		}
		if ev.id != "" {
			t.rememberEventID(ev.id)
		}
		if ev.name != "message" || len(ev.data) == 0 {
			continue
		}
		msg, perr := mcp.ParseMessage(ev.data)
		if perr != nil {
			return // a peer that emits garbage cannot keep framing intact
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
