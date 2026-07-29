package downstream

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/dinstein/agent-hub/internal/logx"
	"github.com/dinstein/agent-hub/internal/mcp"
	"github.com/dinstein/agent-hub/internal/mcp/transport"
)

// clientInfo is what agenthub declares as clientInfo when initializing a
// downstream connection.
var clientInfo = mcp.Implementation{Name: "agenthub", Version: "0.0.0-dev"}

// callKind discriminates owner-queue requests.
type callKind int

const (
	kindCall    callKind = iota // breaker-gated JSON-RPC call
	kindRefresh                 // tools/list re-query on the live connection
	kindPing                    // MCP ping health probe (breaker-exempt)
)

// callReq is one unit of work for the owner goroutine.
type callReq struct {
	kind   callKind
	ctx    context.Context
	method string
	params json.RawMessage
	probe  bool          // this call is the single half-open probe
	reply  chan callResp // buffered(1): the owner never blocks on reply
}

type callResp struct {
	raw json.RawMessage
	err error
}

// Server is one connected downstream MCP server. All calls are serialized
// through the owner goroutine;
// see the package doc for the invariants.
type Server struct {
	spec  Spec
	log   *slog.Logger
	dial  DialFunc // also the respawn factory
	br    *breaker
	retry RetryConfig

	connectTimeout time.Duration

	// lifeCtx is the server lifetime; stop is called exactly once by Close.
	lifeCtx context.Context
	stop    context.CancelFunc

	calls     chan callReq  // capacity 1; consumed only by the owner
	refreshCh chan struct{} // capacity 1; coalesces list_changed bursts

	// listMerge folds concurrent tools/list refreshes into one round trip
	// (see refresh.go).
	listMerge listMerge

	// health is the ping-probe verdict state machine (see probe.go).
	health *healthTracker

	// trace is the per-server JSON-RPC frame log; nil = not wired (the
	// nil *ServerLog methods are no-ops, so there is no nil check below).
	trace *ServerLog
	// traceInst is this instance's DeriveKey, stamped on every frame it
	// writes. Derived instances of one server share one log file, so without
	// it their frames interleave into what reads like a single conversation.
	// "" is the base connection.
	traceInst string

	// reconnects counts AUTOMATIC reconnects and is deliberately NEVER
	// reset by a successful reconnect: a server that dies right after every
	// respawn must keep climbing the backoff ladder instead of hammering
	// the launcher at the base delay forever ("reconnect preserves retryCount,
	// so backoff cannot be defeated"). Only Reconnect — an explicit user action —
	// zeroes it.
	reconnects atomic.Uint64
	reconnect  RetryConfig

	// mu guards the mutable connection state below. The owner goroutine is
	// the only writer of tr/tools after Connect returns; Close and the
	// accessors read them.
	mu       sync.Mutex
	tr       transport.Transport
	tools    []mcp.ToolDef
	initRes  *mcp.InitializeResult
	onChange func(transport.ChangeMask)
	peer     transport.PeerHandler

	ownerDone chan struct{}
	closeOnce sync.Once
}

// Connect dials, initializes and lists tools of one downstream server, then
// starts its owner goroutine. The whole first connection is bounded by
// deps.ConnectTimeout (default 120s: cold launcher caches are slow). A dead
// process / closed stdout during connect surfaces as a ClassUnavailable
// error; initialization failures embed the child's stderr tail.
func Connect(ctx context.Context, spec Spec, deps Deps) (*Server, error) {
	if spec.ID == "" {
		return nil, fmt.Errorf("downstream: spec.ID must not be empty")
	}
	if spec.Kind == "" {
		spec.Kind = transport.Stdio
	}
	dial := deps.dialer()
	log := deps.Log
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	timeout := deps.ConnectTimeout
	if timeout <= 0 {
		timeout = DefaultConnectTimeout
	}

	lifeCtx, stop := context.WithCancel(context.Background())
	s := &Server{
		spec:           spec,
		log:            log.With(logx.FieldServer, spec.ID),
		dial:           dial,
		br:             newBreaker(deps.Breaker),
		retry:          deps.Retry.withDefaults(),
		reconnect:      deps.Reconnect.withReconnectDefaults(),
		connectTimeout: timeout,
		lifeCtx:        lifeCtx,
		stop:           stop,
		calls:          make(chan callReq, 1),
		refreshCh:      make(chan struct{}, 1),
		health:         newHealthTracker(time.Now()),
		trace:          deps.traceFor(spec),
		traceInst:      string(spec.DeriveKey),
		ownerDone:      make(chan struct{}),
	}

	cctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	tr, initRes, tools, err := s.dialAndInit(cctx)
	if err != nil {
		stop()
		close(s.ownerDone)
		return nil, err
	}
	s.attach(tr, initRes, tools)
	s.health.success(time.Now()) // the handshake IS the first liveness proof
	go s.owner()
	if deps.PingInterval > 0 {
		go s.runProbe(deps.PingInterval)
	}
	return s, nil
}

// ID returns the server's registry identifier.
func (s *Server) ID() string { return s.spec.ID }

// InitializeResult returns the handshake result of the current connection.
func (s *Server) InitializeResult() *mcp.InitializeResult {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.initRes
}

// Tools returns the cached tools/list result (populated at connect, updated
// by RefreshTools and by tools/list_changed notifications). The returned
// slice is a copy; ToolDef payloads are shared and must not be mutated.
func (s *Server) Tools() []mcp.ToolDef {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]mcp.ToolDef, len(s.tools))
	copy(out, s.tools)
	return out
}

// OnListChanged registers fn to be forwarded every list_changed
// notification from the current (and any respawned) connection. fn runs on
// the transport read loop and must not block. Independently of fn, a tools
// list_changed schedules a coalesced tools/list refresh on the owner.
func (s *Server) OnListChanged(fn func(mask transport.ChangeMask)) {
	s.mu.Lock()
	s.onChange = fn
	s.mu.Unlock()
}

// OnPeerRequest registers the handler for server-initiated reverse RPCs
// (roots/list). The registration survives respawn. Same contract as
// transport.PeerHandler: runs on the read loop, must not block.
func (s *Server) OnPeerRequest(h transport.PeerHandler) {
	s.mu.Lock()
	s.peer = h
	tr := s.tr
	s.mu.Unlock()
	if tr != nil {
		tr.OnPeerRequest(s.forwardPeer)
	}
}

// Call invokes tool with args on this server. It is safe for concurrent
// use; calls execute serially in arrival order. Cancellation of ctx returns
// immediately — forwarding notifications/cancelled downstream is handled by
// the transport layer.
func (s *Server) Call(ctx context.Context, tool string, args json.RawMessage) (*mcp.CallResult, error) {
	params, err := json.Marshal(mcp.CallToolParams{Name: tool, Arguments: args})
	if err != nil {
		return nil, fmt.Errorf("downstream %q: encode tools/call params: %w", s.spec.ID, err)
	}
	raw, err := s.enqueue(ctx, kindCall, mcp.MethodToolsCall, params)
	if err != nil {
		return nil, err
	}
	var res mcp.CallResult
	if err := json.Unmarshal(raw, &res); err != nil {
		return nil, fmt.Errorf("downstream %q: decode tools/call result: %w", s.spec.ID, err)
	}
	return &res, nil
}

// RefreshTools re-queries tools/list on the live connection and replaces
// the cached tool list. It never respawns the process and bypasses the
// circuit breaker (it is an explicit maintenance operation, not a
// breaker-gated tool call), but still serializes through the owner queue.
//
// Concurrent callers are MERGED: the first becomes the leader and performs
// the round trip, the rest wait for its outcome (on a slow
// stdio server several discovery paths otherwise queue behind each other).
// See refresh.go for the exact waiter semantics.
func (s *Server) RefreshTools(ctx context.Context) error {
	return s.refreshMerged(ctx)
}

// Reconnect rebuilds the connection on explicit user request. Unlike the
// automatic half-open respawn it resets the reconnect ladder to zero: a
// human pressing "reconnect" is stating that the previous failure history is
// no longer relevant ("only an explicit user reconnect resets everything"). It also
// resets the circuit breaker, since a closed breaker over a fresh connection
// is the state the user asked for.
func (s *Server) Reconnect(ctx context.Context) error {
	// Zeroed twice on purpose: before, so this attempt itself waits out no
	// backoff, and after, so the manual reconnect is not counted as an
	// automatic one (Reconnects() means "automatic reconnects since the last
	// manual one").
	s.reconnects.Store(0)
	cctx, cancel := context.WithTimeout(ctx, s.connectTimeout)
	defer cancel()
	if _, err := s.respawn(cctx); err != nil {
		return err
	}
	s.reconnects.Store(0)
	s.br.recordSuccess()
	s.health.success(time.Now())
	return nil
}

// Reconnects reports how many AUTOMATIC reconnects this server has
// performed since the last manual Reconnect. It is the backoff ladder's
// exponent and is exported for the health/diagnostic surfaces.
func (s *Server) Reconnects() uint64 { return s.reconnects.Load() }

// Close tears the server down: the owner goroutine exits, the transport is
// closed (child reaped by the transport layer), pending and future calls
// fail. Close is idempotent and blocks until the owner has exited.
func (s *Server) Close() {
	s.closeOnce.Do(func() {
		s.stop()
		s.mu.Lock()
		tr := s.tr
		s.mu.Unlock()
		if tr != nil {
			_ = tr.Close()
		}
		<-s.ownerDone
	})
}

// enqueue runs the breaker gate, posts the request to the owner queue, and
// waits for the reply or cancellation. The breaker verdict precedes the
// channel send by design: cooldown failures never occupy a queue slot.
func (s *Server) enqueue(ctx context.Context, kind callKind, method string, params json.RawMessage) (json.RawMessage, error) {
	var probe bool
	if kind == kindCall {
		p, err := s.br.allow()
		if err != nil {
			return nil, fmt.Errorf("downstream %q: %w", s.spec.ID, err)
		}
		probe = p
	}
	req := callReq{kind: kind, ctx: ctx, method: method, params: params, probe: probe, reply: make(chan callResp, 1)}
	select {
	case s.calls <- req:
	case <-ctx.Done():
		if probe {
			s.br.releaseProbe()
		}
		return nil, context.Cause(ctx)
	case <-s.lifeCtx.Done():
		if probe {
			s.br.releaseProbe()
		}
		return nil, ErrServerClosed
	}
	select {
	case r := <-req.reply:
		return r.raw, r.err
	case <-ctx.Done():
		// Do not wait for the owner: it is executing with req.ctx and will
		// unwind on its own; the transport forwards the cancellation.
		return nil, context.Cause(ctx)
	case <-s.lifeCtx.Done():
		return nil, ErrServerClosed
	}
}

// owner is the single consumer of the calls queue. It also drains the
// coalesced refresh channel fed by tools/list_changed notifications.
func (s *Server) owner() {
	defer close(s.ownerDone)
	for {
		select {
		case <-s.lifeCtx.Done():
			return
		case <-s.refreshCh:
			s.autoRefresh()
		case req := <-s.calls:
			s.serve(req)
		}
	}
}

// serve executes one queued request and records the breaker outcome.
func (s *Server) serve(req callReq) {
	switch req.kind {
	case kindRefresh:
		req.reply <- callResp{err: s.refreshOnce(req.ctx)}
		return
	case kindPing:
		// Breaker-exempt by construction (see Server.Ping): the probe reports
		// through the health tracker, not through the breaker.
		raw, err := s.roundTrip(req.ctx, req.method, req.params)
		req.reply <- callResp{raw: raw, err: err}
		return
	}
	raw, err := s.execute(req)
	switch {
	case err == nil:
		s.br.recordSuccess()
	case req.ctx.Err() != nil:
		// Cancelled by the caller: neither proof of health nor of failure.
		if req.probe {
			s.br.releaseProbe()
		}
	default:
		if cls, _ := classify(err); cls == transport.ClassUnavailable {
			s.br.recordFailure()
		} else {
			// The server answered (error response or rate limit): liveness
			// proven, reset the failure streak.
			s.br.recordSuccess()
		}
	}
	req.reply <- callResp{raw: raw, err: err}
}

// execute performs the transport round trip with the frozen retry and
// half-open respawn semantics:
//
//   - ClassRetry (never reached the server, or 429) is retried up to
//     RetryConfig.MaxAttempts total attempts, honoring RetryAfter + jitter.
//   - ClassFatal and post-send I/O errors are never retried (tools/call is
//     not idempotent).
//   - When the call is the half-open probe and fails with a health failure,
//     the connection is rebuilt once via the dial factory and the probe is
//     retried on the fresh connection. If the old connection was already
//     terminally failed the request never reached the server; the residual
//     double-execution window (process died mid-call) is the accepted
//     tradeoff of probe semantics (docs/architecture.md §2).
//   - ANY call (not just the probe) is likewise rebuilt once when the
//     failure is a pre-send dead connection (transport.ErrDeadConnection):
//     a long-lived SSE stream that died between calls would otherwise make
//     every later call fail forever with no one to reconnect it. This does
//     not widen the double-execution window — the marker is only ever
//     attached where nothing reached the wire (see deadConnection).
func (s *Server) execute(req callReq) (json.RawMessage, error) {
	tr := s.transport()
	attempt := 1
	respawned := false
	for {
		raw, err := s.callTransport(req.ctx, tr, req.method, req.params)
		if err == nil {
			return raw, nil
		}
		if req.ctx.Err() != nil {
			return nil, err
		}
		if endpointMoved(err) {
			// Terminal: neither retry nor respawn can fix a URL that is gone
			// (see moved.go). Report with the remediation hint instead of
			// burning the reconnect ladder on a dead endpoint.
			return nil, s.withMovedHint(err)
		}
		cls, retryAfter := classify(err)
		switch {
		case cls == transport.ClassRetry && attempt < s.retry.MaxAttempts:
			if werr := s.sleep(req.ctx, backoff(s.retry, attempt, retryAfter)); werr != nil {
				return nil, werr
			}
			attempt++
			continue
		case cls == transport.ClassUnavailable && !respawned && (req.probe || deadConnection(err)):
			ntr, rerr := s.respawn(req.ctx)
			if rerr != nil {
				return nil, rerr
			}
			respawned = true
			tr = ntr
			continue
		}
		return nil, err
	}
}

// roundTrip performs one transport call on the CURRENT connection with
// tracing. Used by the ping probe and the refresh path (neither retries nor
// respawns).
func (s *Server) roundTrip(ctx context.Context, method string, params json.RawMessage) (json.RawMessage, error) {
	return s.callTransport(ctx, s.transport(), method, params)
}

// callTransport is the single place a JSON-RPC frame crosses the downstream
// boundary, which is why it is also the single place the per-server trace
// log is fed (serverlog.go). Tracing is off by default and never blocks.
func (s *Server) callTransport(ctx context.Context, tr transport.Transport, method string, params json.RawMessage) (json.RawMessage, error) {
	if tr == nil {
		return nil, &transport.Error{Class: transport.ClassUnavailable, Err: transport.ErrClosed}
	}
	if !s.trace.Enabled() {
		return tr.Call(ctx, method, params)
	}
	s.trace.out(s.traceInst, method, params)
	start := time.Now()
	raw, err := tr.Call(ctx, method, params)
	s.trace.in(s.traceInst, method, raw, err, time.Since(start))
	return raw, err
}

// respawn rebuilds the connection through the dial factory: wait out the
// reconnect backoff, close the dead transport, dial + initialize +
// tools/list, swap the connection state. A failure is always reported as
// ClassUnavailable so the breaker reopens even if the underlying cause
// classified differently (a server that dies during its own handshake is
// not healthy).
//
// The backoff exponent is Server.reconnects, which survives successful
// reconnects on purpose: a flapping server climbs the ladder instead of
// re-dialing at the base delay forever. Only Reconnect (a user action)
// resets it.
func (s *Server) respawn(ctx context.Context) (transport.Transport, error) {
	n := s.reconnects.Add(1)
	if n > 1 {
		// n == 1 is the first respawn since the last manual reset: no wait,
		// so a single transient death recovers instantly.
		if werr := s.sleep(ctx, backoff(s.reconnect, int(min(n-1, 16)), 0)); werr != nil {
			return nil, werr
		}
	}
	s.mu.Lock()
	old := s.tr
	s.mu.Unlock()
	if old != nil {
		_ = old.Close()
	}
	cctx, cancel := context.WithTimeout(ctx, s.connectTimeout)
	defer cancel()
	tr, initRes, tools, err := s.dialAndInit(cctx)
	if err != nil {
		s.log.Warn("respawn failed", "error", err, "reconnects", n)
		s.health.failure(time.Now(), err, hardConnError(err))
		return nil, &transport.Error{Class: transport.ClassUnavailable, Err: fmt.Errorf("respawn: %w", err)}
	}
	s.attach(tr, initRes, tools)
	s.health.success(time.Now())
	s.log.Info("respawned after failed half-open probe", "reconnects", n)
	return tr, nil
}

// dialAndInit performs dial + MCP handshake + initial tools/list. On
// handshake failure the transport's stderr tail is embedded in the error
// (that tail is usually the only diagnostic a crashing child leaves).
func (s *Server) dialAndInit(ctx context.Context) (transport.Transport, *mcp.InitializeResult, []mcp.ToolDef, error) {
	tr, err := s.dial(ctx, s.spec)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("downstream %q: dial: %w", s.spec.ID, err)
	}
	initRes, err := transport.Initialize(ctx, tr, clientInfo)
	if err != nil {
		err = fmt.Errorf("downstream %q: initialize: %w", s.spec.ID, withStderr(err, tr))
		_ = tr.Close()
		return nil, nil, nil, err
	}
	raw, err := s.callTransport(ctx, tr, mcp.MethodToolsList, nil)
	if err != nil {
		err = fmt.Errorf("downstream %q: initial tools/list: %w", s.spec.ID, withStderr(err, tr))
		_ = tr.Close()
		return nil, nil, nil, err
	}
	var lr mcp.ListToolsResult
	if uerr := json.Unmarshal(raw, &lr); uerr != nil {
		_ = tr.Close()
		return nil, nil, nil, fmt.Errorf("downstream %q: decode tools/list: %w", s.spec.ID, uerr)
	}
	return tr, initRes, lr.Tools, nil
}

// attach swaps in a connected transport and re-registers the notification
// and peer-request hooks (registrations survive respawn).
func (s *Server) attach(tr transport.Transport, initRes *mcp.InitializeResult, tools []mcp.ToolDef) {
	s.mu.Lock()
	s.tr = tr
	s.initRes = initRes
	s.tools = tools
	hasPeer := s.peer != nil
	s.mu.Unlock()
	tr.OnListChanged(s.handleListChanged)
	if hasPeer {
		tr.OnPeerRequest(s.forwardPeer)
	}
}

// handleListChanged runs on the transport read loop: schedule a coalesced
// refresh for tool changes (non-blocking; a full channel means one is
// already pending — bursts merge into a single refresh) and forward the
// mask to the user callback.
func (s *Server) handleListChanged(mask transport.ChangeMask) {
	if mask.Has(transport.ChangeTools) {
		select {
		case s.refreshCh <- struct{}{}:
		default:
		}
	}
	s.mu.Lock()
	fn := s.onChange
	s.mu.Unlock()
	if fn != nil {
		fn(mask)
	}
}

// forwardPeer relays a reverse RPC to the currently registered handler.
func (s *Server) forwardPeer(ctx context.Context, req *mcp.Request) (*mcp.Response, error) {
	s.mu.Lock()
	h := s.peer
	s.mu.Unlock()
	if h == nil {
		return nil, fmt.Errorf("downstream %q: no peer handler", s.spec.ID)
	}
	return h(ctx, req)
}

// autoRefresh is the owner-side reaction to tools/list_changed.
func (s *Server) autoRefresh() {
	ctx, cancel := context.WithTimeout(s.lifeCtx, refreshTimeout)
	defer cancel()
	if err := s.refreshOnce(ctx); err != nil {
		// Non-fatal: the cached list stays; a later notification or an
		// explicit RefreshTools will retry.
		s.log.Warn("tools refresh after list_changed failed", "error", err)
	}
}

// refreshOnce re-queries tools/list on the current connection and replaces
// the cache. Owner goroutine only. Never respawns.
func (s *Server) refreshOnce(ctx context.Context) error {
	raw, err := s.roundTrip(ctx, mcp.MethodToolsList, nil)
	if err != nil {
		return fmt.Errorf("downstream %q: refresh tools: %w", s.spec.ID, err)
	}
	var lr mcp.ListToolsResult
	if err := json.Unmarshal(raw, &lr); err != nil {
		return fmt.Errorf("downstream %q: decode tools/list: %w", s.spec.ID, err)
	}
	s.mu.Lock()
	s.tools = lr.Tools
	s.mu.Unlock()
	return nil
}

func (s *Server) transport() transport.Transport {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.tr
}

// sleep waits d for a retry, aborting on caller cancellation or Close.
func (s *Server) sleep(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return context.Cause(ctx)
	case <-s.lifeCtx.Done():
		return ErrServerClosed
	case <-t.C:
		return nil
	}
}

// withStderr embeds the child's last StderrRingLines stderr LINES in err
// (unchanged when there are none). The original error stays unwrappable.
//
// This is the fix for "a startup crash leaves nothing but deadline exceeded": a
// child that dies during its own handshake leaves nothing but its stderr,
// and a raw 4 KiB byte window is unreadable in a one-line error. See
// stderrlines.go for why the line ring is a projection of the byte window
// rather than a second capture.
func withStderr(err error, tr transport.Transport) error {
	lines := formatStderrTail(stderrTail(tr, StderrRingLines))
	if lines == "" {
		return err
	}
	return fmt.Errorf("%w (stderr tail: %s)", err, lines)
}
