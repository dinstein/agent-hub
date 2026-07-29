package transport

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sync"
	"sync/atomic"

	"github.com/dinstein/agent-hub/internal/mcp"
)

// conn implements Transport over a raw byte stream (child stdin/stdout for
// stdio; in-memory pipes in tests). It owns the single read loop that
// dispatches responses, answers peer requests inline, and fires
// list_changed callbacks.
//
// Failure model: any read-side error (EOF, malformed frame, oversized
// frame) or write-side I/O error moves the conn into a terminal failed
// state — failErr is set once and never cleared, every pending call is
// released with it, and later Call/Notify return it immediately. The only
// non-terminal error is an oversized *outgoing* frame, which is rejected
// before any byte hits the wire.
type conn struct {
	fw *mcp.FrameWriter
	fr *mcp.FrameReader
	wc io.Closer // write side (child stdin); closed on Close

	// ctx is the connection lifetime context handed to peer handlers;
	// cancelled when the conn fails or closes.
	ctx    context.Context
	cancel context.CancelFunc

	nextID atomic.Int64

	mu       sync.Mutex
	pending  map[string]chan *mcp.Response
	peer     PeerHandler
	onChange func(ChangeMask)
	failErr  *Error // terminal state; set once under mu

	readDone  chan struct{}
	closeOnce sync.Once

	// stderrFn / shutdown are optional hooks installed by the stdio
	// transport before start(): stderr tail snapshot and process reaping.
	stderrFn func() string
	shutdown func()
}

// newConn builds a conn over w/r with the given frame size bound. The
// caller must invoke start() exactly once after installing optional hooks.
func newConn(w io.WriteCloser, r io.Reader, maxFrame int) *conn {
	ctx, cancel := context.WithCancel(context.Background())
	return &conn{
		fw:       mcp.NewFrameWriter(w),
		fr:       mcp.NewFrameReaderSize(r, maxFrame),
		wc:       w,
		ctx:      ctx,
		cancel:   cancel,
		pending:  make(map[string]chan *mcp.Response),
		readDone: make(chan struct{}),
	}
}

func (c *conn) start() { go c.readLoop() }

// fail moves the conn into the terminal failed state (first caller wins)
// and releases every pending call by closing its channel; Call translates
// a closed channel into the stored failErr.
func (c *conn) fail(err *Error) {
	c.mu.Lock()
	if c.failErr == nil {
		c.failErr = err
	}
	for key, ch := range c.pending {
		delete(c.pending, key)
		close(ch)
	}
	c.mu.Unlock()
	c.cancel()
}

func (c *conn) failedErr() *Error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.failErr
}

func (c *conn) readLoop() {
	defer close(c.readDone)
	for {
		line, err := c.fr.Next()
		if err != nil {
			// EOF, read error, or oversized frame: the stream is dead or
			// undefined mid-frame. All pending calls end ClassUnavailable
			// (process exit / stdout close contract).
			c.fail(&Error{Class: ClassUnavailable, Err: fmt.Errorf("downstream read: %w", err)})
			return
		}
		msg, perr := mcp.ParseMessage(line)
		if perr != nil {
			// Malformed frame: typed protocol error. We choose to close
			// the connection — a peer that emits garbage cannot be trusted
			// to keep framing intact — but the process never crashes.
			c.fail(&Error{Class: ClassUnavailable, Err: perr})
			return
		}
		switch m := msg.(type) {
		case *mcp.Response:
			c.deliver(m)
		case *mcp.Request:
			c.handlePeer(m)
		case *mcp.Notification:
			c.handleNotification(m)
		}
	}
}

func (c *conn) deliver(resp *mcp.Response) {
	c.mu.Lock()
	ch, ok := c.pending[resp.ID.Key()]
	if ok {
		delete(c.pending, resp.ID.Key())
	}
	c.mu.Unlock()
	if ok {
		ch <- resp // buffered(1); never blocks the read loop
	}
	// Unmatched responses (cancelled calls racing a late reply) are dropped.
}

// handlePeer answers a server-initiated reverse RPC inline on the read
// loop. The response id is forced to the request id regardless of what the
// handler set.
func (c *conn) handlePeer(req *mcp.Request) {
	c.mu.Lock()
	h := c.peer
	c.mu.Unlock()

	var resp *mcp.Response
	if h == nil {
		resp = mcp.NewErrorResponse(req.ID, &mcp.Error{
			Code:    mcp.CodeMethodNotFound,
			Message: fmt.Sprintf("no peer handler for %q", req.Method),
		})
	} else {
		r, err := h(c.ctx, req)
		switch {
		case err != nil:
			resp = mcp.NewErrorResponse(req.ID, &mcp.Error{
				Code:    mcp.CodeInternalError,
				Message: err.Error(),
			})
		case r == nil:
			resp = mcp.NewErrorResponse(req.ID, &mcp.Error{
				Code:    mcp.CodeInternalError,
				Message: "peer handler returned no response",
			})
		default:
			resp = r
			resp.JSONRPC = mcp.Version
			resp.ID = req.ID
		}
	}
	if err := c.fw.WriteFrame(resp); err != nil {
		if errors.Is(err, mcp.ErrFrameTooLarge) {
			// Replace the oversized reply with an in-band error; the
			// stream itself is still intact.
			_ = c.fw.WriteFrame(mcp.NewErrorResponse(req.ID, &mcp.Error{
				Code:    mcp.CodeInternalError,
				Message: "peer response exceeds frame size limit",
			}))
			return
		}
		c.fail(&Error{Class: ClassUnavailable, Err: fmt.Errorf("write peer response: %w", err)})
	}
}

func (c *conn) handleNotification(n *mcp.Notification) {
	mask, ok := changeMaskFor(n.Method)
	if !ok {
		return // unknown notifications are ignored, never fatal
	}
	c.mu.Lock()
	fn := c.onChange
	c.mu.Unlock()
	if fn != nil {
		fn(mask)
	}
}

// Call implements Transport.
func (c *conn) Call(ctx context.Context, method string, params any) (json.RawMessage, error) {
	raw, err := marshalParams(params)
	if err != nil {
		return nil, &Error{Class: ClassFatal, Err: err}
	}
	id := mcp.NewIntID(c.nextID.Add(1))
	ch := make(chan *mcp.Response, 1)

	c.mu.Lock()
	if c.failErr != nil {
		fe := c.failErr
		c.mu.Unlock()
		return nil, fe
	}
	c.pending[id.Key()] = ch
	c.mu.Unlock()

	if err := c.fw.WriteFrame(mcp.NewRequest(id, method, raw)); err != nil {
		c.removePending(id)
		if errors.Is(err, mcp.ErrFrameTooLarge) {
			// Nothing was written; the connection is still healthy, and
			// retrying the same oversized payload can never succeed.
			return nil, &Error{Class: ClassFatal, Err: err}
		}
		fe := &Error{Class: ClassUnavailable, Err: err}
		c.fail(fe)
		return nil, fe
	}

	select {
	case <-ctx.Done():
		c.removePending(id)
		// Best-effort cancellation forwarding before surfacing ctx error.
		c.sendCancelled(id, ctx)
		return nil, ctx.Err()
	case resp, ok := <-ch:
		if !ok {
			fe := c.failedErr()
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
func (c *conn) Notify(_ context.Context, method string, params any) error {
	raw, err := marshalParams(params)
	if err != nil {
		return &Error{Class: ClassFatal, Err: err}
	}
	if fe := c.failedErr(); fe != nil {
		return fe
	}
	if err := c.fw.WriteFrame(mcp.NewNotification(method, raw)); err != nil {
		if errors.Is(err, mcp.ErrFrameTooLarge) {
			return &Error{Class: ClassFatal, Err: err}
		}
		fe := &Error{Class: ClassUnavailable, Err: err}
		c.fail(fe)
		return fe
	}
	return nil
}

// sendCancelled forwards notifications/cancelled for an abandoned call.
// Best-effort by contract: write errors are ignored (a dead pipe is about
// to be reported by the read loop anyway).
func (c *conn) sendCancelled(id mcp.ID, ctx context.Context) {
	if c.failedErr() != nil {
		return
	}
	reason := ""
	if err := ctx.Err(); err != nil {
		reason = err.Error()
	}
	raw, err := json.Marshal(mcp.CancelledParams{RequestID: id, Reason: reason})
	if err != nil {
		return
	}
	_ = c.fw.WriteFrame(mcp.NewNotification(mcp.NotificationCancelled, raw))
}

func (c *conn) removePending(id mcp.ID) {
	c.mu.Lock()
	delete(c.pending, id.Key())
	c.mu.Unlock()
}

// OnPeerRequest implements Transport.
func (c *conn) OnPeerRequest(h PeerHandler) {
	c.mu.Lock()
	c.peer = h
	c.mu.Unlock()
}

// OnListChanged implements Transport.
func (c *conn) OnListChanged(fn func(mask ChangeMask)) {
	c.mu.Lock()
	c.onChange = fn
	c.mu.Unlock()
}

// Stderr implements Transport.
func (c *conn) Stderr() string {
	if c.stderrFn == nil {
		return ""
	}
	return c.stderrFn()
}

// Close implements Transport. It fails pending calls, closes the write
// side (EOF for a well-behaved child), then runs the shutdown hook (which
// for stdio waits for the process with a kill escalation).
func (c *conn) Close() error {
	c.closeOnce.Do(func() {
		c.fail(&Error{Class: ClassUnavailable, Err: ErrClosed})
		_ = c.wc.Close()
		if c.shutdown != nil {
			c.shutdown()
		}
	})
	return nil
}

// marshalParams turns Call/Notify params into raw JSON. nil stays nil
// (params member omitted); json.RawMessage passes through verbatim.
func marshalParams(params any) (json.RawMessage, error) {
	switch p := params.(type) {
	case nil:
		return nil, nil
	case json.RawMessage:
		return p, nil
	default:
		raw, err := json.Marshal(params)
		if err != nil {
			return nil, fmt.Errorf("encode params: %w", err)
		}
		return raw, nil
	}
}
