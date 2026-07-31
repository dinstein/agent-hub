package downstream

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"sync"
	"syscall"
	"time"

	"github.com/dinstein/agent-hub/internal/mcp"
	"github.com/dinstein/agent-hub/internal/mcp/transport"
)

// ConnState is the observed connection condition of one downstream server.
// The values are the wire strings of ctlapi.ConnState — this package must
// not import the control plane, so the two are kept in sync by the string
// values alone, and by TestDownstreamConnStatesAreKnownHere on the ctlapi
// side. That test is what makes this comment true; it did not exist for as
// long as the comment claimed it.
//
// Adding a state here means adding it there too, in the same change: an
// unrecognized value ranks above every known one in the control plane's
// worst-wins fold, so a new state arrives as "unknown connection state" on a
// server that is working.
type ConnState string

// Connection states reported by Health.
const (
	// ConnConnecting is the state before the first probe verdict exists.
	ConnConnecting ConnState = "connecting"
	// ConnConnected means the last probe proved the server answers.
	ConnConnected ConnState = "connected"
	// ConnError means the server is considered down (see HealthFailureStreak
	// for how many transient failures that takes, and hardConnError for the
	// failures that flip it immediately).
	ConnError ConnState = "error"
)

// HealthFailureStreak is how many CONSECUTIVE transient ping failures flip
// the health state to ConnError ("3 transient failures before flipping to Error").
// A single dropped frame or a momentarily wedged server must not paint a
// working connection red.
const HealthFailureStreak = 3

// DefaultPingInterval is the recommended health-probe period for an
// assembly that holds a connection pool (the daemon). It is NOT applied
// implicitly: Deps.PingInterval == 0 means "no background probing", because
// a short-lived stdio gateway with one server gains nothing from a prober
// and should not pay for one.
const DefaultPingInterval = 30 * time.Second

// pingTimeout bounds a single MCP ping round trip. A probe that has not
// answered within this window counts as one transient failure — it must not
// pile up behind the owner queue of a wedged server.
const pingTimeout = 10 * time.Second

// Health is the probe-derived condition of a connection. It is a value
// snapshot: reading it never races with the prober.
type Health struct {
	// State is the current connection state.
	State ConnState
	// Detail elaborates a non-connected state (last probe error). Empty
	// while healthy.
	Detail string
	// Failures is the current consecutive transient failure streak (reset
	// by any successful probe).
	Failures int
	// Since is when State was last entered.
	Since time.Time
	// LastProbe is when the last probe completed (zero = never probed).
	LastProbe time.Time
}

// healthTracker owns the probe verdict state machine. It is written only by
// the probe path and read under its mutex by Health().
//
// It logs STATE CHANGES, never individual verdicts, and that distinction is
// the point. A server that is down for an hour produces one line saying so
// and one saying it came back; a line per failing probe would produce a
// hundred and twenty identical warnings instead — the shape that teaches a
// reader to filter the message out, and the transition with it.
type healthTracker struct {
	mu  sync.Mutex
	h   Health
	log *slog.Logger
}

// newHealthTracker builds the tracker for one connection. log is the
// server's bound logger (it carries the server and, for a derivation, the
// instance); nil means no logging.
func newHealthTracker(now time.Time, log *slog.Logger) *healthTracker {
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	return &healthTracker{h: Health{State: ConnConnecting, Since: now}, log: log}
}

func (t *healthTracker) snapshot() Health {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.h
}

// success records a probe the server answered — including an answer that is
// a JSON-RPC error (method-not-found for ping on an old server still proves
// the connection carries traffic). The failure streak resets.
func (t *healthTracker) success(now time.Time) {
	t.mu.Lock()
	before, failures := t.h.State, t.h.Failures
	if t.h.State != ConnConnected {
		t.h.State = ConnConnected
		t.h.Since = now
	}
	t.h.Detail = ""
	t.h.Failures = 0
	t.h.LastProbe = now
	t.mu.Unlock()

	// Every state line in this package carries the pair, so one grep on
	// from/to follows a connection through an outage whichever way it moved.
	pair := []any{"from", string(before), "to", string(ConnConnected)}
	switch before {
	case ConnConnected:
	case ConnError:
		// The one recovery line. Nothing else announces it: the call path
		// only ever reports what fails, so without this a log shows a server
		// going down and never coming back.
		t.log.Info("downstream healthy again", append(pair, "failures_cleared", failures)...)
	default:
		// connecting → connected is the handshake, which the assembling
		// layer already reports in its own words ("downstream connected",
		// with the tool count). Debug, so the two do not read as two events.
		t.log.Debug("downstream answered its first probe", pair...)
	}
}

// failure records one probe failure. hard failures (connection refused and
// friends) flip the state at once; everything else needs
// HealthFailureStreak consecutive failures.
func (t *healthTracker) failure(now time.Time, err error, hard bool) {
	t.mu.Lock()
	before := t.h.State
	t.h.Failures++
	t.h.Detail = err.Error()
	t.h.LastProbe = now
	flip := hard || t.h.Failures >= HealthFailureStreak
	if flip && t.h.State != ConnError {
		t.h.State = ConnError
		t.h.Since = now
	}
	after, failures := t.h.State, t.h.Failures
	t.mu.Unlock()

	if before == after {
		return
	}
	// `hard` is on the line because it is the difference between "this server
	// refused the connection outright" and "three probes in a row timed out",
	// and the two send a reader to different places.
	t.log.Warn("downstream is not answering", "from", string(before), "to", string(after),
		"failures", failures, "hard", hard, "error", err)
}

// Health returns the current probe-derived connection condition. Servers
// that were never probed report ConnConnecting.
func (s *Server) Health() Health { return s.health.snapshot() }

// Ping performs one MCP ping round trip through the owner queue and folds
// the outcome into Health. It bypasses the circuit breaker on purpose: the
// breaker gates TOOL CALLS, and a health probe that the breaker could
// refuse would never be able to observe recovery.
//
// A JSON-RPC error answer (e.g. method-not-found on a server that predates
// ping) counts as SUCCESS: the round trip completed, which is the only
// thing a liveness probe is entitled to conclude.
func (s *Server) Ping(ctx context.Context) error {
	pctx, cancel := context.WithTimeout(ctx, pingTimeout)
	defer cancel()
	_, err := s.enqueue(pctx, kindPing, mcp.MethodPing, nil)
	now := time.Now()
	switch {
	case err == nil, isAnswered(err):
		// The two ways a probe succeeds record the same verdict because they
		// prove the same thing: a JSON-RPC error RESPONSE means the round trip
		// completed, and completing is all a liveness probe is entitled to
		// conclude.
		s.health.success(now)
		s.served.Store(true)
		return nil
	case ctx.Err() != nil:
		// The CALLER went away — neither proof of health nor of failure.
		return err
	default:
		// The tracker owns the reporting: it is the only thing that knows
		// whether this verdict MOVED the state. There used to be a warning
		// here instead, fired on every hard failure — so a server with the
		// plug pulled logged one every probe interval for as long as it
		// stayed unplugged, and never logged its recovery at all.
		s.health.failure(now, err, hardConnError(err))
		return err
	}
}

// runProbe is the background health prober: one ping every interval for the
// lifetime of the server. It is started by Connect only when
// Deps.PingInterval > 0 — a gateway with a single short-lived stdio server
// does not need it, a daemon holding a pool does.
func (s *Server) runProbe(interval time.Duration) {
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-s.lifeCtx.Done():
			return
		case <-t.C:
			// The probe rides the server lifetime, not a caller's context:
			// a probe must never be cancelled by whoever happened to trigger
			// the tick.
			_ = s.Ping(s.lifeCtx)
		}
	}
}

// isAnswered reports whether err is an ordinary JSON-RPC error response,
// i.e. the server answered. Those prove liveness (same rule the breaker
// applies to call outcomes).
func isAnswered(err error) bool {
	cls, _ := classify(err)
	return cls == transport.ClassFatal && !errors.Is(err, context.Canceled) &&
		!errors.Is(err, context.DeadlineExceeded) && !errors.Is(err, ErrServerClosed)
}

// hardConnError reports whether err is a connection-level failure that no
// amount of waiting will fix within the probe cadence — the
// "hard errors like connection refused flip immediately" set:
//
//   - the OS refused, could not route to, or had no network for the peer,
//   - the MCP endpoint answered 410 Gone (moved for good — see
//     ErrEndpointMoved),
//   - the transport is closed / the child process is gone.
//
// Everything else (timeouts, resets, malformed frames, 5xx) is transient
// and must accumulate to HealthFailureStreak first. Failure direction:
// misclassifying a hard error as transient only delays a red light by two
// probes; the reverse would flap a working server to red on one hiccup.
func hardConnError(err error) bool {
	switch {
	case errors.Is(err, syscall.ECONNREFUSED),
		errors.Is(err, syscall.EHOSTUNREACH),
		errors.Is(err, syscall.ENETUNREACH),
		errors.Is(err, syscall.ENETDOWN),
		errors.Is(err, ErrEndpointMoved),
		errors.Is(err, transport.ErrClosed),
		errors.Is(err, os.ErrProcessDone),
		errors.Is(err, io.ErrClosedPipe):
		return true
	}
	return false
}
