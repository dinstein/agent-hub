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
	"github.com/dinstein/agent-hub/internal/mrtr"
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

	// reconnects counts AUTOMATIC reconnects and is the exponent of the
	// respawn backoff ladder. A successful respawn does not by itself reset
	// it: a server that dies right after every respawn must keep climbing
	// instead of hammering the launcher at the base delay forever
	// ("reconnect preserves retryCount, so backoff cannot be defeated").
	// What does reset it is `served` below, and an explicit Reconnect.
	reconnects atomic.Uint64

	// served records whether the CURRENT connection has completed at least
	// one round trip since it was built, and it is what lets the ladder tell
	// apart the two deaths that otherwise look identical to it:
	//
	//   - the crash loop the ladder exists for — dial, handshake, die before
	//     serving anything. `served` is false at every respawn, so the
	//     exponent climbs exactly as before.
	//   - a long-lived HTTP/SSE stream reaped for idleness between calls.
	//     Every respawn is preceded by a connection that worked, so the
	//     ladder starts over and the next death costs one dial, not a
	//     half-minute of backoff in front of it.
	//
	// Without the distinction the second case climbs to the 30s MaxDelay
	// within nine deaths and stays there for the life of the process, and a
	// downstream behind an ingress with a three-minute idle timeout charges
	// every later call ~22s of sleep before it may even redial — a penalty
	// for flapping, levied on a server that has never once failed to answer.
	//
	// "Completed a round trip" is the same liveness rule the rest of this
	// package applies (see isAnswered): a JSON-RPC error RESPONSE counts,
	// because the connection carried it. A handshake does not — that is what
	// the crash loop already does successfully every time.
	served atomic.Bool

	// inputID numbers the synthesized MRTR peer requests (never on the wire).
	inputID   atomic.Int64
	reconnect RetryConfig

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

	// One bound logger for the whole connection, built before the struct so
	// the breaker writes under the same identity as every other line this
	// server produces.
	srvLog := log.With(logx.FieldServer, spec.ID)

	lifeCtx, stop := context.WithCancel(context.Background())
	s := &Server{
		spec:           spec,
		log:            srvLog,
		dial:           dial,
		br:             newBreaker(deps.Breaker, srvLog),
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

// maxInputRounds bounds the MRTR retry loop. Each round is one full
// input-collection pass (potentially involving a human), so a server that
// keeps answering input_required past this is not converging — the call
// fails rather than looping at the server's pleasure.
const maxInputRounds = 4

// Call invokes tool with args on this server. It is safe for concurrent
// use; calls execute serially in arrival order. Cancellation of ctx returns
// immediately — forwarding notifications/cancelled downstream is handled by
// the transport layer.
//
// A 2026-07-28 server may answer input_required (MRTR): the retry loop
// lives HERE, below the pipeline, so callers only ever see a complete
// result — gates ran once on the original call and are never re-entered by
// a retry. Each round re-enters the owner queue and the breaker gate
// separately, so a slow input collection (a human, on the other end of
// roots/elicitation) never blocks other calls to this server.
func (s *Server) Call(ctx context.Context, tool string, args json.RawMessage) (*mcp.CallResult, error) {
	p := mcp.CallToolParams{Name: tool, Arguments: args}
	for round := 0; ; round++ {
		params, err := json.Marshal(p)
		if err != nil {
			return nil, fmt.Errorf("downstream %q: encode tools/call params: %w", s.spec.ID, err)
		}
		raw, err := s.enqueue(ctx, kindCall, mcp.MethodToolsCall, params)
		if err != nil {
			return nil, err
		}
		ir, isInputRequired, err := decodeInputRequired(raw)
		if err != nil {
			return nil, fmt.Errorf("downstream %q: decode tools/call result: %w", s.spec.ID, err)
		}
		if !isInputRequired {
			var res mcp.CallResult
			if err := json.Unmarshal(raw, &res); err != nil {
				return nil, fmt.Errorf("downstream %q: decode tools/call result: %w", s.spec.ID, err)
			}
			return &res, nil
		}
		if round >= maxInputRounds {
			return nil, fmt.Errorf("downstream %q: tools/call %q still input_required after %d rounds",
				s.spec.ID, tool, maxInputRounds)
		}
		responses, err := s.resolveInputs(ctx, ir.InputRequests)
		if err != nil {
			return nil, fmt.Errorf("downstream %q: tools/call %q input round %d: %w",
				s.spec.ID, tool, round+1, err)
		}
		// The retry re-issues the ORIGINAL params plus the collected
		// responses and the requestState echoed verbatim; the transport
		// assigns the new JSON-RPC id the spec requires.
		p.InputResponses = responses
		p.RequestState = ir.RequestState
	}
}

// decodeInputRequired peeks at a tools/call result's resultType. Servers
// speaking ≤ 2025-11-25 omit the field, which reads as complete.
func decodeInputRequired(raw json.RawMessage) (*mcp.InputRequiredResult, bool, error) {
	var probe struct {
		ResultType string `json:"resultType"`
	}
	if err := json.Unmarshal(raw, &probe); err != nil {
		return nil, false, err
	}
	if probe.ResultType != mcp.ResultTypeInputRequired {
		return nil, false, nil
	}
	var ir mcp.InputRequiredResult
	if err := json.Unmarshal(raw, &ir); err != nil {
		return nil, false, err
	}
	return &ir, true, nil
}

// resolveInputs answers one MRTR round through the same peer-handler seam
// the legacy reverse-RPC path uses, so both protocol generations serve
// roots/list — and reject everything unimplemented — identically. The
// synthesized request ids never touch the wire; they exist because the
// PeerHandler contract answers a *mcp.Request.
func (s *Server) resolveInputs(ctx context.Context, reqs mcp.InputRequests) (mcp.InputResponses, error) {
	return mrtr.Resolve(ctx, reqs, func(ctx context.Context, method string, params json.RawMessage) (json.RawMessage, error) {
		resp, err := s.forwardPeer(ctx, mcp.NewRequest(mcp.NewIntID(s.inputID.Add(1)), method, params))
		if err != nil {
			return nil, err
		}
		if resp == nil {
			return nil, fmt.Errorf("peer handler returned no response for %q", method)
		}
		if resp.Error != nil {
			return nil, resp.Error
		}
		return resp.Result, nil
	})
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
	if _, err := s.respawn(cctx, causeManual, nil); err != nil {
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
		s.served.Store(true)
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
			s.served.Store(true)
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
			cause := causeDeadConn
			if req.probe {
				cause = causeProbe
			}
			ntr, rerr := s.respawn(req.ctx, cause, err)
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

// respawnCause names what made a respawn happen. It exists because for as
// long as every respawn logged "respawned after failed half-open probe",
// the two automatic causes were indistinguishable in the log — and the
// message named the rarer one. Four days of gateway logs held 163 respawns
// under that message and not one of them was a half-open probe; they were
// all causeDeadConn, which points at the network between here and the
// downstream rather than at the downstream itself. Which cause fires decides
// where the fix goes, so it is a field.
type respawnCause string

const (
	// causeProbe is the half-open probe of an open circuit failing on the
	// connection it was sent to test.
	causeProbe respawnCause = "half-open-probe"
	// causeDeadConn is an ordinary call the transport rejected before the
	// wire because the connection had already died — a long-lived HTTP/SSE
	// stream reaped between calls is the shape that produces it.
	causeDeadConn respawnCause = "dead-connection"
	// causeManual is Reconnect(), a human asking for a fresh connection.
	// There is no failure behind it, so it carries no trigger error.
	causeManual respawnCause = "manual"
)

// respawn rebuilds the connection through the dial factory: wait out the
// reconnect backoff, close the dead transport, dial + initialize +
// tools/list, swap the connection state. A failure is always reported as
// ClassUnavailable so the breaker reopens even if the underlying cause
// classified differently (a server that dies during its own handshake is
// not healthy).
//
// The backoff exponent is Server.reconnects, which survives a successful
// respawn on purpose: a flapping server climbs the ladder instead of
// re-dialing at the base delay forever. It is reset by a connection that
// PROVED itself before dying (Server.served) and by Reconnect (a user
// action) — never by the respawn itself.
// cause and trigger are the log's account of why this happened: what kind of
// failure reached the respawn decision, and the error that carried it
// (trigger is nil for causeManual). They are recorded on the way out rather
// than on the way in, so one respawn is one line whichever way it ends.
func (s *Server) respawn(ctx context.Context, cause respawnCause, trigger error) (transport.Transport, error) {
	if s.served.Swap(false) {
		// The connection being replaced had answered at least once, so the
		// failure history in front of it is not a crash loop and must not be
		// charged to the connection about to be built. Swap rather than Load:
		// that new connection has proven nothing yet.
		s.reconnects.Store(0)
	}
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
		s.log.Warn("respawn failed", append(respawnFields(cause, trigger, n), "error", err)...)
		s.health.failure(time.Now(), err, hardConnError(err))
		return nil, &transport.Error{Class: transport.ClassUnavailable, Err: fmt.Errorf("respawn: %w", err)}
	}
	s.attach(tr, initRes, tools)
	s.health.success(time.Now())
	s.log.Info("respawned", respawnFields(cause, trigger, n)...)
	return tr, nil
}

// respawnFields renders the cause of one respawn for the log. The trigger is
// flattened to its text and omitted when absent: a "trigger":null on every
// manual reconnect would read as a failure nobody could name.
func respawnFields(cause respawnCause, trigger error, n uint64) []any {
	fields := []any{"cause", string(cause), "reconnects", n}
	if trigger != nil {
		fields = append(fields, "trigger", trigger.Error())
	}
	return fields
}

// dialAndInit performs dial + MCP handshake + initial tools/list. On
// handshake failure the transport's stderr tail is embedded in the error
// (that tail is usually the only diagnostic a crashing child leaves).
func (s *Server) dialAndInit(ctx context.Context) (transport.Transport, *mcp.InitializeResult, []mcp.ToolDef, error) {
	tr, err := s.dial(ctx, s.spec)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("downstream %q: dial: %w", s.spec.ID, err)
	}
	hres, err := transport.Handshake(ctx, tr, clientInfo)
	if err != nil {
		err = fmt.Errorf("downstream %q: handshake: %w", s.spec.ID, withStderr(err, tr))
		_ = tr.Close()
		return nil, nil, nil, err
	}
	// The stored *mcp.InitializeResult is the normalized handshake outcome:
	// on the 2026-07-28 discover path no initialize ever ran, but the fields
	// mean the same thing to every consumer (status, doctor, server test).
	initRes := &mcp.InitializeResult{
		ProtocolVersion: hres.Version,
		Capabilities:    hres.Capabilities,
		ServerInfo:      hres.ServerInfo,
		Instructions:    hres.Instructions,
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
