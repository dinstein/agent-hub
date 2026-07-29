package fakemcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"sync"
	"sync/atomic"

	"github.com/dinstein/agent-hub/internal/mcp"
	"github.com/dinstein/agent-hub/internal/mcp/transport"
)

// stderrTailWindow mirrors the stdio transport's 4 KiB stderr tail so the
// in-process driver reports Stderr() with the same truncation semantics.
const stderrTailWindow = 4 << 10

// Connect starts an in-process fake server running script and returns a
// transport.Transport connected to it — no child process involved, so
// fault-injection tests stay fast and deterministic.
//
// The wire is a pair of OS pipes (kernel-buffered, unlike io.Pipe), which
// preserves the real transport's non-blocking best-effort writes such as
// notifications/cancelled while the server is deliberately sleeping.
//
// The returned transport mirrors the semantics of the internal stdio
// transport (which has no exported in-memory constructor): pending-call
// dispatch by id, ClassUnavailable on stream failure with the mcp sentinels
// preserved for errors.Is, ClassFatal for JSON-RPC error responses and
// oversized outgoing frames, best-effort cancellation forwarding, inline
// peer-request replies, list_changed callbacks, a 4 KiB stderr tail, and an
// idempotent Close.
func Connect(script *Script) (transport.Transport, error) {
	c2sR, c2sW, err := os.Pipe() // client → server
	if err != nil {
		return nil, fmt.Errorf("fakemcp: pipe: %w", err)
	}
	s2cR, s2cW, err := os.Pipe() // server → client
	if err != nil {
		_ = c2sR.Close()
		_ = c2sW.Close()
		return nil, fmt.Errorf("fakemcp: pipe: %w", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	p := &pipeTransport{
		fw:        mcp.NewFrameWriter(c2sW),
		fr:        mcp.NewFrameReader(s2cR),
		wc:        c2sW,
		rc:        s2cR,
		cancel:    cancel,
		stderr:    &tailBuffer{max: stderrTailWindow},
		pending:   make(map[string]chan *mcp.Response),
		serveDone: make(chan struct{}),
		readDone:  make(chan struct{}),
	}
	go func() {
		defer close(p.serveDone)
		_ = Serve(ctx, c2sR, s2cW, p.stderr, script)
		// Closing the write side delivers EOF to the client — the same
		// observable as a child process crashing / closing stdout.
		_ = s2cW.Close()
		_ = c2sR.Close()
	}()
	go p.readLoop()
	return p, nil
}

// pipeTransport is the client half of Connect. Failure model matches the
// stdio transport: any read-side error is terminal (failErr set once, all
// pending calls released), Close is idempotent.
type pipeTransport struct {
	fw     *mcp.FrameWriter
	fr     *mcp.FrameReader
	wc     io.Closer // client write end (server stdin)
	rc     io.Closer // client read end (server stdout)
	cancel context.CancelFunc
	stderr *tailBuffer

	nextID atomic.Int64

	mu       sync.Mutex
	pending  map[string]chan *mcp.Response
	peer     transport.PeerHandler
	onChange func(transport.ChangeMask)
	failErr  *transport.Error

	serveDone chan struct{}
	readDone  chan struct{}
	closeOnce sync.Once
}

func (p *pipeTransport) fail(err *transport.Error) {
	p.mu.Lock()
	if p.failErr == nil {
		p.failErr = err
	}
	for key, ch := range p.pending {
		delete(p.pending, key)
		close(ch)
	}
	p.mu.Unlock()
}

func (p *pipeTransport) failedErr() *transport.Error {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.failErr
}

func (p *pipeTransport) readLoop() {
	defer close(p.readDone)
	for {
		line, err := p.fr.Next()
		if err != nil {
			p.fail(&transport.Error{Class: transport.ClassUnavailable, Err: fmt.Errorf("fakemcp read: %w", err)})
			return
		}
		msg, perr := mcp.ParseMessage(line)
		if perr != nil {
			p.fail(&transport.Error{Class: transport.ClassUnavailable, Err: perr})
			return
		}
		switch m := msg.(type) {
		case *mcp.Response:
			p.deliver(m)
		case *mcp.Request:
			p.handlePeer(m)
		case *mcp.Notification:
			p.handleNotification(m)
		}
	}
}

func (p *pipeTransport) deliver(resp *mcp.Response) {
	p.mu.Lock()
	ch, ok := p.pending[resp.ID.Key()]
	if ok {
		delete(p.pending, resp.ID.Key())
	}
	p.mu.Unlock()
	if ok {
		ch <- resp // buffered(1); never blocks the read loop
	}
	// Unmatched responses (wrong-id fault, late replies) are dropped.
}

func (p *pipeTransport) handlePeer(req *mcp.Request) {
	p.mu.Lock()
	h := p.peer
	p.mu.Unlock()
	var resp *mcp.Response
	if h == nil {
		resp = mcp.NewErrorResponse(req.ID, &mcp.Error{
			Code:    mcp.CodeMethodNotFound,
			Message: fmt.Sprintf("no peer handler for %q", req.Method),
		})
	} else {
		r, err := h(context.Background(), req)
		switch {
		case err != nil:
			resp = mcp.NewErrorResponse(req.ID, &mcp.Error{Code: mcp.CodeInternalError, Message: err.Error()})
		case r == nil:
			resp = mcp.NewErrorResponse(req.ID, &mcp.Error{Code: mcp.CodeInternalError, Message: "peer handler returned no response"})
		default:
			resp = r
			resp.JSONRPC = mcp.Version
			resp.ID = req.ID
		}
	}
	// Best-effort in a test harness; a dead pipe surfaces via the read loop.
	_ = p.fw.WriteFrame(resp)
}

func (p *pipeTransport) handleNotification(n *mcp.Notification) {
	var mask transport.ChangeMask
	switch n.Method {
	case mcp.NotificationToolsListChanged:
		mask = transport.ChangeTools
	case mcp.NotificationResourcesListChanged:
		mask = transport.ChangeResources
	case mcp.NotificationPromptsListChanged:
		mask = transport.ChangePrompts
	default:
		return
	}
	p.mu.Lock()
	fn := p.onChange
	p.mu.Unlock()
	if fn != nil {
		fn(mask)
	}
}

// Call implements transport.Transport.
func (p *pipeTransport) Call(ctx context.Context, method string, params any) (json.RawMessage, error) {
	raw, err := marshalCallParams(params)
	if err != nil {
		return nil, &transport.Error{Class: transport.ClassFatal, Err: err}
	}
	id := mcp.NewIntID(p.nextID.Add(1))
	ch := make(chan *mcp.Response, 1)

	p.mu.Lock()
	if p.failErr != nil {
		fe := p.failErr
		p.mu.Unlock()
		return nil, fe
	}
	p.pending[id.Key()] = ch
	p.mu.Unlock()

	if err := p.fw.WriteFrame(mcp.NewRequest(id, method, raw)); err != nil {
		p.removePending(id)
		if errors.Is(err, mcp.ErrFrameTooLarge) {
			// Nothing hit the wire; the connection is still usable.
			return nil, &transport.Error{Class: transport.ClassFatal, Err: err}
		}
		fe := &transport.Error{Class: transport.ClassUnavailable, Err: err}
		p.fail(fe)
		return nil, fe
	}

	select {
	case <-ctx.Done():
		p.removePending(id)
		p.sendCancelled(ctx, id)
		return nil, ctx.Err()
	case resp, ok := <-ch:
		if !ok {
			fe := p.failedErr()
			if fe == nil { // cannot happen: channels close only after failErr is set
				fe = &transport.Error{Class: transport.ClassUnavailable, Err: transport.ErrClosed}
			}
			return nil, fe
		}
		if resp.Error != nil {
			return nil, &transport.Error{Class: transport.ClassFatal, Err: resp.Error}
		}
		return resp.Result, nil
	}
}

// Notify implements transport.Transport.
func (p *pipeTransport) Notify(_ context.Context, method string, params any) error {
	raw, err := marshalCallParams(params)
	if err != nil {
		return &transport.Error{Class: transport.ClassFatal, Err: err}
	}
	if fe := p.failedErr(); fe != nil {
		return fe
	}
	if err := p.fw.WriteFrame(mcp.NewNotification(method, raw)); err != nil {
		if errors.Is(err, mcp.ErrFrameTooLarge) {
			return &transport.Error{Class: transport.ClassFatal, Err: err}
		}
		fe := &transport.Error{Class: transport.ClassUnavailable, Err: err}
		p.fail(fe)
		return fe
	}
	return nil
}

// sendCancelled forwards notifications/cancelled for an abandoned call
// (best-effort; the OS pipe buffer absorbs it even while the server
// sleeps).
func (p *pipeTransport) sendCancelled(ctx context.Context, id mcp.ID) {
	if p.failedErr() != nil {
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
	_ = p.fw.WriteFrame(mcp.NewNotification(mcp.NotificationCancelled, raw))
}

func (p *pipeTransport) removePending(id mcp.ID) {
	p.mu.Lock()
	delete(p.pending, id.Key())
	p.mu.Unlock()
}

// OnPeerRequest implements transport.Transport.
func (p *pipeTransport) OnPeerRequest(h transport.PeerHandler) {
	p.mu.Lock()
	p.peer = h
	p.mu.Unlock()
}

// OnListChanged implements transport.Transport.
func (p *pipeTransport) OnListChanged(fn func(mask transport.ChangeMask)) {
	p.mu.Lock()
	p.onChange = fn
	p.mu.Unlock()
}

// Stderr implements transport.Transport: the last 4 KiB the script wrote
// to its stderr sink.
func (p *pipeTransport) Stderr() string { return p.stderr.String() }

// Close implements transport.Transport. Closing the read end first
// unblocks a server stuck writing an oversized frame (EPIPE) and the
// client read loop, so Close never deadlocks; it then waits for both
// goroutines so no test leaks them.
func (p *pipeTransport) Close() error {
	p.closeOnce.Do(func() {
		p.fail(&transport.Error{Class: transport.ClassUnavailable, Err: transport.ErrClosed})
		p.cancel()       // aborts scripted sleeps/storms
		_ = p.wc.Close() // EOF on the server's stdin
		_ = p.rc.Close() // EPIPE for blocked server writes; unblocks readLoop
		<-p.serveDone
		<-p.readDone
	})
	return nil
}

// marshalCallParams mirrors the stdio transport's params handling: nil
// stays nil (params omitted), json.RawMessage passes through verbatim.
func marshalCallParams(params any) (json.RawMessage, error) {
	switch v := params.(type) {
	case nil:
		return nil, nil
	case json.RawMessage:
		return v, nil
	default:
		raw, err := json.Marshal(params)
		if err != nil {
			return nil, fmt.Errorf("encode params: %w", err)
		}
		return raw, nil
	}
}

// tailBuffer retains the last max bytes written (in-process stand-in for
// the stdio transport's stderr tail window). Safe for concurrent use.
type tailBuffer struct {
	mu   sync.Mutex
	max  int
	data []byte
}

// Write implements io.Writer; it never fails.
func (b *tailBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if len(p) >= b.max {
		b.data = append(b.data[:0], p[len(p)-b.max:]...)
		return len(p), nil
	}
	b.data = append(b.data, p...)
	if over := len(b.data) - b.max; over > 0 {
		b.data = append(b.data[:0], b.data[over:]...)
	}
	return len(p), nil
}

// String returns a snapshot of the retained tail.
func (b *tailBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return string(b.data)
}
