package gateway

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sync"
	"sync/atomic"

	"github.com/dinstein/agent-hub/internal/mcp"
)

// This file is the seam the daemon's HTTP data plane hangs off
// (docs/conventions.md#package-layout). It exposes the SAME gateway assembly Run serves over
// stdin/stdout,
// connected over an in-memory pipe pair instead.
//
// Why reuse rather than assemble a second data plane: docs/conventions.md#package-layout freezes
// "one execute pipeline", and the cheapest way to keep that true is to have
// no second assembly at all. A message arriving over HTTP is written into the
// same frame reader an stdio client writes into, so it meets the same
// discovery surface, the same router, the same pipeline.Execute call site
// (upstream.go execTool) and the same defend_and_shape hook. There is no
// shortcut for the HTTP face to take because there is no second path to take
// it on.

// ErrConnClosed is returned once a Conn's gateway has stopped serving.
var ErrConnClosed = errors.New("gateway: connection closed")

// Conn is one in-process MCP connection to an assembled gateway.
//
// It is a CLIENT of the gateway, not a variant of it: Do writes a request
// frame and waits for the matching response frame, exactly as a stdio client
// would. Safe for concurrent use.
type Conn struct {
	g  *gateway
	fw *mcp.FrameWriter

	// inW is the client→gateway pipe; closing it is the EOF that ends the
	// gateway's read loop (the same termination an stdio client causes).
	inW  *io.PipeWriter
	outW *io.PipeWriter
	outR *io.PipeReader

	stop     context.CancelFunc
	runDone  chan struct{}
	readDone chan struct{}
	// reverse answers gateway→client RPCs off the read loop; waited on at
	// close so a late write can never race the pipe teardown.
	reverse sync.WaitGroup

	nextID atomic.Int64

	mu      sync.Mutex
	pending map[string]chan *mcp.Response
	// subs are the live consumers of the gateway→client direction
	// (subscribe.go). Guarded by the same mutex as pending, because both are
	// touched by the read loop on every frame.
	subs   map[*Subscription]struct{}
	closed bool

	closeOnce sync.Once
}

// Open assembles a gateway from cfg and connects to it in-process. cfg.In and
// cfg.Out are supplied by this function and must be left unset.
//
// The returned Conn owns the gateway's lifetime: Close is the only way to
// stop it, and it must be called or the downstream connections leak.
func Open(cfg Config) (*Conn, error) {
	if cfg.In != nil || cfg.Out != nil {
		return nil, errors.New("gateway: Open supplies Config.In and Config.Out itself")
	}
	inR, inW := io.Pipe()
	outR, outW := io.Pipe()
	cfg.In, cfg.Out = inR, outW

	g, err := newGateway(cfg)
	if err != nil {
		_ = inW.Close()
		_ = outW.Close()
		return nil, err
	}

	ctx, stop := context.WithCancel(context.Background())
	c := &Conn{
		g:        g,
		fw:       mcp.NewFrameWriter(inW),
		inW:      inW,
		outW:     outW,
		outR:     outR,
		stop:     stop,
		runDone:  make(chan struct{}),
		readDone: make(chan struct{}),
		pending:  make(map[string]chan *mcp.Response),
	}
	go func() {
		defer close(c.runDone)
		defer g.shutdown()
		if err := g.run(ctx); err != nil {
			g.log.Warn("in-process gateway stopped", "error", err)
		}
	}()
	go c.readLoop()
	return c, nil
}

// Do sends one request and returns its answer. The caller's request id is
// preserved in the response, but a DIFFERENT id travels on the wire: several
// upstream sessions share one Conn, and their id spaces are not coordinated —
// echoing a caller-chosen id would let one session collect another's answer.
//
// It never returns nil: a transport failure, a cancellation or a closed
// gateway becomes a JSON-RPC error response, because the caller (the HTTP
// face) owes its client exactly one answer either way.
func (c *Conn) Do(ctx context.Context, req *mcp.Request) *mcp.Response {
	if req == nil {
		return mcp.NewErrorResponse(mcp.ID{}, &mcp.Error{
			Code: mcp.CodeInvalidRequest, Message: "empty request",
		})
	}
	wireID := mcp.NewStringID(fmt.Sprintf("hb-%d", c.nextID.Add(1)))
	ch := make(chan *mcp.Response, 1)

	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return errorResponse(req.ID, mcp.CodeInternalError, ErrConnClosed.Error())
	}
	c.pending[wireID.Key()] = ch
	c.mu.Unlock()
	defer func() {
		c.mu.Lock()
		delete(c.pending, wireID.Key())
		c.mu.Unlock()
	}()

	frame := mcp.NewRequest(wireID, req.Method, req.Params)
	if err := c.fw.WriteFrame(frame); err != nil {
		return errorResponse(req.ID, mcp.CodeInternalError, "agenthub could not reach its gateway: "+err.Error())
	}

	select {
	case <-ctx.Done():
		// The HTTP client went away. The gateway keeps the call in flight;
		// its own cancellation path is notifications/cancelled, which this
		// face has no channel to receive, so the work is left to finish and
		// its result dropped rather than half-cancelled.
		return errorResponse(req.ID, mcp.CodeInternalError, "request cancelled")
	case <-c.runDone:
		return errorResponse(req.ID, mcp.CodeInternalError, ErrConnClosed.Error())
	case res := <-ch:
		out := *res
		out.ID = req.ID
		return &out
	}
}

// Notify forwards one notification. Notifications are never answered, so a
// write failure is reported to the caller and nothing else happens.
func (c *Conn) Notify(n *mcp.Notification) error {
	if n == nil {
		return nil
	}
	c.mu.Lock()
	closed := c.closed
	c.mu.Unlock()
	if closed {
		return ErrConnClosed
	}
	return c.fw.WriteFrame(n)
}

// Counters exposes the gate/stage invocation counts of this connection's
// pipeline (pipeline.Counters). It is the diagnostics and metrics seam, and
// it is what proves the HTTP path advances every gate exactly like the stdio
// path — the counters belong to the one pipeline both share.
func (c *Conn) Counters() map[string]uint64 { return c.g.pipe.Counters() }

// Close stops the gateway and releases every pipe. Idempotent.
func (c *Conn) Close() {
	c.closeOnce.Do(func() {
		c.mu.Lock()
		c.closed = true
		c.mu.Unlock()

		// EOF first: the gateway's read loop exits the way an stdio client
		// disconnecting makes it exit, so shutdown takes the ordinary path.
		_ = c.inW.Close()
		<-c.runDone
		c.stop()
		// Only now unblock the reader: the gateway may still have been
		// writing while it drained.
		_ = c.outW.Close()
		<-c.readDone
		// After readDone, because no further notification can be offered
		// once the read loop has exited: a subscriber waking to "closed"
		// has therefore seen everything this connection ever produced.
		c.closeSubscriptions()
		c.reverse.Wait()
		_ = c.outR.Close()
	})
}

// readLoop demultiplexes the gateway→client direction.
func (c *Conn) readLoop() {
	defer close(c.readDone)
	fr := mcp.NewFrameReader(c.outR)
	for {
		line, err := fr.Next()
		if err != nil {
			return
		}
		msg, perr := mcp.ParseMessage(line)
		if perr != nil {
			c.g.log.Warn("in-process gateway emitted a malformed frame", "error", perr)
			continue
		}
		switch m := msg.(type) {
		case *mcp.Response:
			c.deliver(m)
		case *mcp.Notification:
			// Server-initiated notifications (tools/list_changed) go to
			// whoever subscribed (subscribe.go). Nobody subscribed is still
			// a drop, and still the common case: a client that opened no
			// stream is not owed one.
			c.fanout(m)
		case *mcp.Request:
			// Reverse RPC (roots/list). Answered OFF the read loop: the pipes
			// are unbuffered, so replying inline can deadlock against the
			// gateway writing in the other direction.
			c.reverse.Add(1)
			go func() {
				defer c.reverse.Done()
				c.refuseReverse(m)
			}()
		}
	}
}

// refuseReverse answers a gateway→client request. There is no client to ask:
// one Conn serves many HTTP sessions and the face carries no server→client
// channel. MethodNotFound is the accurate answer and the gateway treats it as
// "this client offers no roots" (fail-closed: an empty root set narrows
// project-layer scope rather than widening it).
func (c *Conn) refuseReverse(req *mcp.Request) {
	res := mcp.NewErrorResponse(req.ID, &mcp.Error{
		Code:    mcp.CodeMethodNotFound,
		Message: fmt.Sprintf("the agenthub HTTP face cannot ask its client %q", req.Method),
	})
	if err := c.fw.WriteFrame(res); err != nil {
		c.g.log.Debug("reverse RPC refusal not delivered", "method", req.Method, "error", err)
	}
}

// deliver hands a response to its waiter; unmatched ids are dropped (a late
// answer to an abandoned request).
func (c *Conn) deliver(res *mcp.Response) {
	c.mu.Lock()
	ch, ok := c.pending[res.ID.Key()]
	if ok {
		delete(c.pending, res.ID.Key())
	}
	c.mu.Unlock()
	if ok {
		ch <- res // buffered(1): never blocks the read loop
	}
}

// errorResponse builds one JSON-RPC error answer for the caller's id.
func errorResponse(id mcp.ID, code int, msg string) *mcp.Response {
	return mcp.NewErrorResponse(id, &mcp.Error{Code: code, Message: msg})
}
