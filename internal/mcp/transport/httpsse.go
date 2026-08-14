package transport

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sync"
	"sync/atomic"

	"github.com/dinstein/agent-hub/internal/mcp"
)

// httpSSE is the legacy two-channel HTTP transport of MCP 2024-11-05 /
// 2025-03-26: a long-lived GET carries every server→client message as SSE,
// and client→server messages are POSTed to an address the server hands out
// in its first event.
//
// DEPRECATED-UPSTREAM(http+sse, earliest-removal: none)
//
// Deprecated upstream on 2025-03-26 and superseded by Streamable HTTP, but
// kept on the READ side indefinitely and deliberately: a proxy's value is
// being able to attach to old servers that expose nothing else
// (docs/conventions.md#mcp-protocol-scope, ruling #29). It is never offered on the exposure side.
// 2025-03-26 is the date it was deprecated, NOT a date it may be removed —
// the two were once in one field here, and the marker exists so a sweep can
// read the removal date without reading the prose.
//
// Failure model: unlike streamable-http there IS a single long-lived byte
// stream, so this transport behaves like stdio — any stream error is
// terminal, failErr is set once, and every pending call is released with
// ClassUnavailable. There is no resumption: the legacy binding defines
// none, and silently re-running a non-idempotent tools/call is worse than
// a clean reconnect driven by internal/downstream.
type httpSSE struct {
	httpBase

	// ctx is the transport lifetime: it parents the GET stream, so
	// cancelling it tears the stream down. Cancelled by fail and Close.
	ctx    context.Context
	cancel context.CancelFunc

	nextID atomic.Int64

	mu       sync.Mutex
	postURL  *url.URL
	pending  map[string]chan *mcp.Response
	peer     PeerHandler
	onChange func(ChangeMask)
	failErr  *Error // terminal state; set once under mu
	closed   bool

	ready     chan struct{} // closed when the endpoint event arrives
	readyOnce sync.Once
	readDone  chan struct{}

	peerSem   chan struct{}
	wg        sync.WaitGroup
	closeOnce sync.Once
}

// DialHTTPSSE opens the legacy HTTP+SSE transport and blocks until the
// server's endpoint event names the POST address (or ctx expires). ctx
// bounds the handshake only; the stream itself lives until Close.
//
// DEPRECATED-UPSTREAM(http+sse, earliest-removal: none) — see the file
// comment: deprecated 2025-03-26, kept on the read side indefinitely.
func DialHTTPSSE(ctx context.Context, cfg HTTPConfig) (Transport, error) {
	base, err := newHTTPBase(cfg)
	if err != nil {
		return nil, err
	}
	lifetime, cancel := context.WithCancel(context.Background())
	t := &httpSSE{
		httpBase: base,
		ctx:      lifetime,
		cancel:   cancel,
		pending:  make(map[string]chan *mcp.Response),
		ready:    make(chan struct{}),
		readDone: make(chan struct{}),
		peerSem:  make(chan struct{}, maxPeerWorkers),
	}

	resp, terr := t.openStream(ctx)
	if terr != nil {
		cancel()
		return nil, terr
	}
	go t.readLoop(resp.Body)

	select {
	case <-t.ready:
		// fail() also unblocks ready, so a closed channel is not by itself
		// proof of a successful handshake.
		if fe := t.failedErr(); fe != nil {
			return nil, fe
		}
		return t, nil
	case <-t.ctx.Done():
		// The read loop failed (bad event, stream died) before handshake.
		if fe := t.failedErr(); fe != nil {
			return nil, fe
		}
		return nil, &Error{Class: ClassUnavailable, Err: ErrClosed}
	case <-ctx.Done():
		_ = t.Close()
		return nil, ctx.Err()
	}
}

// openStream performs the GET. The request is bound to the transport
// lifetime (not to the handshake ctx), so the stream survives Dial; the
// handshake deadline is enforced by racing Do against dialCtx.
func (t *httpSSE) openStream(dialCtx context.Context) (*http.Response, *Error) {
	req, err := t.newRequest(t.ctx, http.MethodGet, t.endpoint, nil)
	if err != nil {
		return nil, &Error{Class: ClassFatal, Err: fmt.Errorf("build request: %w", err)}
	}
	req.Header.Set(headerAccept, mediaSSE)
	// No MCP-Protocol-Version header: it was introduced in 2025-06-18,
	// after this transport was already deprecated, and legacy servers must
	// not be handed a header their binding never defined.

	type result struct {
		resp *http.Response
		err  error
	}
	ch := make(chan result, 1)
	go func() {
		resp, err := t.client.Do(req)
		ch <- result{resp, err}
	}()

	var r result
	select {
	case <-dialCtx.Done():
		t.cancel() // abort the in-flight GET
		if late := <-ch; late.resp != nil {
			drainClose(late.resp) // reap it so no body leaks
		}
		return nil, &Error{Class: ClassUnavailable, Err: dialCtx.Err()}
	case r = <-ch:
	}
	if r.err != nil {
		return nil, requestError(r.err)
	}
	if r.resp.StatusCode != http.StatusOK {
		return nil, httpError(r.resp)
	}
	if mt := responseMediaType(r.resp); mt != mediaSSE {
		drainClose(r.resp)
		return nil, &Error{Class: ClassUnavailable, Err: fmt.Errorf(
			"%w: sse endpoint answered content-type %q (want %s)", ErrHTTPProtocol, mt, mediaSSE)}
	}
	return r.resp, nil
}

// readLoop is the single consumer of the SSE stream: it resolves the POST
// endpoint, delivers responses to pending calls, answers reverse RPCs and
// fires list_changed callbacks.
func (t *httpSSE) readLoop(body io.ReadCloser) {
	defer close(t.readDone)
	defer func() { _ = body.Close() }()

	sc := newSSEScanner(body, t.maxFrame)
	for {
		ev, err := sc.Next()
		if err != nil {
			t.fail(&Error{Class: ClassUnavailable, Err: fmt.Errorf("sse stream: %w", err)})
			return
		}
		switch ev.name {
		case "endpoint":
			if terr := t.setPostURL(string(ev.data)); terr != nil {
				t.fail(terr)
				return
			}
		case "message":
			if len(ev.data) == 0 {
				continue
			}
			msg, perr := mcp.ParseMessage(ev.data)
			if perr != nil {
				// A peer that emits garbage cannot be trusted to keep
				// framing intact; the process never crashes.
				t.fail(&Error{Class: ClassUnavailable, Err: perr})
				return
			}
			switch m := msg.(type) {
			case *mcp.Response:
				t.deliver(m)
			case *mcp.Request:
				t.dispatchPeer(m)
			case *mcp.Notification:
				t.dispatchNotification(m)
			}
		default:
			// Unknown event types (keep-alives, vendor extensions) are
			// ignored, never fatal.
		}
	}
}

// setPostURL resolves the endpoint event payload against the stream URL.
//
// Failure direction: FAIL-CLOSED on cross-origin. The payload is
// server-controlled, and the caller's headers (Authorization) ride on every
// POST, so an endpoint pointing at another host would be credential
// exfiltration dressed up as a protocol event.
func (t *httpSSE) setPostURL(raw string) *Error {
	ref, err := url.Parse(raw)
	if err != nil {
		return &Error{Class: ClassUnavailable, Err: fmt.Errorf(
			"%w: endpoint event %q is not a url: %v", ErrHTTPProtocol, raw, err)}
	}
	abs := t.endpoint.ResolveReference(ref)
	if !sameOrigin(t.endpoint, abs) {
		return &Error{Class: ClassUnavailable, Err: fmt.Errorf(
			"%w: endpoint event points at %s, which is not the stream origin %s",
			ErrHTTPProtocol, abs.Redacted(), t.endpoint.Redacted())}
	}
	t.mu.Lock()
	t.postURL = abs
	t.mu.Unlock()
	// The POST address is the one piece of this binding nobody outside the
	// transport ever sees: it is not in the registry, not in the dial, and
	// otherwise appears only in a wire trace nobody had switched on before
	// the failure. Recording it once per connection is what makes a later
	// 405 or 404 answerable without asking for a reproduction.
	//
	// Written BEFORE ready is closed, which is what makes it deterministic:
	// closing ready releases the dial, and a caller that returns first would
	// see a connected transport whose negotiation had not been recorded yet.
	if sameURL(abs, t.endpoint) {
		t.log.Warn("sse endpoint event names the stream url as the post address",
			"post_url", logURL(abs), "post_query", abs.RawQuery != "")
	} else {
		t.log.Debug("sse endpoint event resolved",
			"post_url", logURL(abs), "post_query", abs.RawQuery != "")
	}

	t.readyOnce.Do(func() { close(t.ready) })
	return nil
}

// logURL renders a URL for a log record: scheme, host and path only.
//
// The query is dropped rather than redacted because this binding routinely
// carries the session id there (`?sessionId=…`), which is a capability — and
// logx's scrubber matches on key NAMES it knows, so `sessionId` would pass
// straight through it. What the record is for is which PATH the downstream
// handed out; post_query reports that a query existed without reproducing it.
func logURL(u *url.URL) string {
	if u == nil {
		return ""
	}
	trimmed := *u
	trimmed.RawQuery = ""
	trimmed.Fragment = ""
	return trimmed.Redacted()
}

// Call implements Transport. The POST only acknowledges receipt (202); the
// response itself arrives on the SSE stream and is matched by id.
func (t *httpSSE) Call(ctx context.Context, method string, params any) (json.RawMessage, error) {
	raw, err := marshalParams(params)
	if err != nil {
		return nil, &Error{Class: ClassFatal, Err: err}
	}
	if fe := t.failedErr(); fe != nil {
		return nil, presend(fe)
	}
	id := mcp.NewIntID(t.nextID.Add(1))
	body, err := encodeMessage(mcp.NewRequest(id, method, raw), t.maxFrame)
	if err != nil {
		return nil, err
	}

	ch := make(chan *mcp.Response, 1)
	t.mu.Lock()
	if t.failErr != nil {
		fe := t.failErr
		t.mu.Unlock()
		return nil, presend(fe)
	}
	t.pending[id.Key()] = ch
	t.mu.Unlock()

	if terr := t.postMessage(ctx, body); terr != nil {
		t.removePending(id)
		if ctxErr := ctx.Err(); ctxErr != nil {
			t.sendCancelled(id, ctxErr)
			return nil, ctxErr
		}
		return nil, terr
	}

	select {
	case <-ctx.Done():
		t.removePending(id)
		t.sendCancelled(id, ctx.Err())
		return nil, ctx.Err()
	case resp, ok := <-ch:
		if !ok {
			fe := t.failedErr()
			if fe == nil { // cannot happen: channels close only after failErr is set
				fe = &Error{Class: ClassUnavailable, Err: ErrClosed}
			}
			return nil, fe
		}
		if resp.Error != nil {
			return nil, &Error{Class: ClassFatal, Err: resp.Error}
		}
		return resp.Result, nil
	}
}

// Notify implements Transport.
func (t *httpSSE) Notify(ctx context.Context, method string, params any) error {
	raw, err := marshalParams(params)
	if err != nil {
		return &Error{Class: ClassFatal, Err: err}
	}
	if fe := t.failedErr(); fe != nil {
		return fe
	}
	body, err := encodeMessage(mcp.NewNotification(method, raw), t.maxFrame)
	if err != nil {
		return err
	}
	if terr := t.postMessage(ctx, body); terr != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return ctxErr
		}
		return terr
	}
	return nil
}

// postMessage sends one message to the server-supplied POST address.
func (t *httpSSE) postMessage(ctx context.Context, body []byte) *Error {
	t.mu.Lock()
	u := t.postURL
	t.mu.Unlock()
	if u == nil {
		return &Error{Class: ClassUnavailable, Err: fmt.Errorf(
			"%w: no endpoint event received yet", ErrHTTPProtocol)}
	}
	req, err := t.newRequest(ctx, http.MethodPost, u, body)
	if err != nil {
		return &Error{Class: ClassFatal, Err: fmt.Errorf("build request: %w", err)}
	}
	req.Header.Set(headerContentType, mediaJSON)
	req.Header.Set(headerAccept, mediaJSON)

	resp, err := t.client.Do(req)
	if err != nil {
		return requestError(err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return t.postError(resp, u)
	}
	// The legacy binding answers 202 Accepted with no meaningful body; the
	// real answer comes back on the stream.
	drainClose(resp)
	return nil
}

// postError classifies a failed POST, naming the one cause the status alone
// hides: 405 on the stream URL itself.
//
// The POST address is not ours to choose — it is whatever the server's
// endpoint event named — so a server that names its own GET-only stream
// produces a bare "POST <stream url>: http 405", which reads like a client
// bug and sends the operator to check their configuration, where the URL is
// of course correct. Naming the endpoint event turns it into what it is: the
// downstream handed out a POST address it does not serve, typically after a
// rolling restart put an older or mismatched instance behind the same host.
func (t *httpSSE) postError(resp *http.Response, post *url.URL) *Error {
	err := httpError(resp)
	if resp.StatusCode == http.StatusMethodNotAllowed && sameURL(post, t.endpoint) {
		err.Err = fmt.Errorf("%w (the server's endpoint event named the SSE stream url itself as the "+
			"POST address, and that url only answers GET)", err.Err)
	}
	return err
}

// sameURL compares two absolute URLs by everything that decides where a
// request lands. Fragments are not sent, so they are not part of it.
func sameURL(a, b *url.URL) bool {
	if a == nil || b == nil {
		return false
	}
	return a.Scheme == b.Scheme && a.Host == b.Host && a.Path == b.Path && a.RawQuery == b.RawQuery
}

func (t *httpSSE) deliver(resp *mcp.Response) {
	t.mu.Lock()
	ch, ok := t.pending[resp.ID.Key()]
	if ok {
		delete(t.pending, resp.ID.Key())
	}
	t.mu.Unlock()
	if ok {
		ch <- resp // buffered(1); never blocks the read loop
	}
	// Unmatched responses (a cancelled call racing a late reply) are dropped.
}

func (t *httpSSE) dispatchNotification(n *mcp.Notification) {
	mask, ok := changeMaskFor(n.Method)
	if !ok {
		return
	}
	t.mu.Lock()
	fn := t.onChange
	t.mu.Unlock()
	if fn != nil {
		fn(mask)
	}
}

// dispatchPeer answers a reverse RPC off the read loop and POSTs the reply,
// because in this binding the reply travels on the other channel. Bounded
// by maxPeerWorkers: the read loop stalls rather than fanning out without
// limit under a flooding peer.
func (t *httpSSE) dispatchPeer(req *mcp.Request) {
	select {
	case t.peerSem <- struct{}{}:
	case <-t.ctx.Done():
		return
	}
	t.mu.Lock()
	if t.closed || t.failErr != nil {
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

		ctx, cancel := context.WithTimeout(t.ctx, peerReplyTimeout)
		defer cancel()

		resp := invokePeer(ctx, h, req)
		body, err := encodeMessage(resp, t.maxFrame)
		if err != nil {
			body, err = encodeMessage(mcp.NewErrorResponse(req.ID, &mcp.Error{
				Code:    mcp.CodeInternalError,
				Message: "peer response exceeds frame size limit",
			}), t.maxFrame)
			if err != nil {
				return
			}
		}
		_ = t.postMessage(ctx, body) // best effort
	}()
}

// sendCancelled forwards notifications/cancelled for an abandoned call.
// Best effort: it runs on the transport lifetime context and swallows
// every failure.
func (t *httpSSE) sendCancelled(id mcp.ID, cause error) {
	if t.failedErr() != nil {
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
	body, err := encodeMessage(mcp.NewNotification(mcp.NotificationCancelled, raw), t.maxFrame)
	if err != nil {
		return
	}
	ctx, cancel := context.WithTimeout(t.ctx, cancelForwardTimeout)
	defer cancel()
	_ = t.postMessage(ctx, body)
}

// fail moves the transport into the terminal failed state (first caller
// wins) and releases every pending call by closing its channel; Call
// translates a closed channel into the stored failErr.
func (t *httpSSE) fail(err *Error) {
	t.mu.Lock()
	if t.failErr == nil {
		t.failErr = err
	}
	for key, ch := range t.pending {
		delete(t.pending, key)
		close(ch)
	}
	t.mu.Unlock()
	t.cancel()
	// Unblock a handshake that never saw its endpoint event.
	t.readyOnce.Do(func() { close(t.ready) })
}

// presend re-wraps a stored terminal error for a call that was rejected
// BEFORE anything went on the wire, marking it with ErrDeadConnection so
// downstream may safely rebuild the connection and replay the request.
//
// It returns a COPY: the stored failErr is shared with post-send waiters
// (whose requests may well have executed), and marking that value in place
// would tell them a replay is safe when it is not.
func presend(fe *Error) *Error {
	if fe == nil {
		return nil
	}
	return &Error{
		Class:      fe.Class,
		RetryAfter: fe.RetryAfter,
		Err:        fmt.Errorf("%w: %w", ErrDeadConnection, fe.Err),
	}
}

func (t *httpSSE) failedErr() *Error {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.failErr
}

func (t *httpSSE) removePending(id mcp.ID) {
	t.mu.Lock()
	delete(t.pending, id.Key())
	t.mu.Unlock()
}

// OnPeerRequest implements Transport.
func (t *httpSSE) OnPeerRequest(h PeerHandler) {
	t.mu.Lock()
	t.peer = h
	t.mu.Unlock()
}

// OnListChanged implements Transport.
func (t *httpSSE) OnListChanged(fn func(mask ChangeMask)) {
	t.mu.Lock()
	t.onChange = fn
	t.mu.Unlock()
}

// Stderr implements Transport: no child process, no stderr.
func (t *httpSSE) Stderr() string { return "" }

// Close implements Transport. Pending calls fail with ClassUnavailable
// wrapping ErrClosed, the stream is torn down, and every background
// goroutine is joined. Idempotent.
func (t *httpSSE) Close() error {
	t.closeOnce.Do(func() {
		t.mu.Lock()
		t.closed = true
		t.mu.Unlock()

		t.fail(&Error{Class: ClassUnavailable, Err: ErrClosed})
		<-t.readDone
		t.wg.Wait()
	})
	return nil
}
