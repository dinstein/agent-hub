package gateway

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"time"

	"github.com/dinstein/agent-hub/internal/downstream"
	"github.com/dinstein/agent-hub/internal/logx"
	"github.com/dinstein/agent-hub/internal/oauthflow"
)

// This file is the GATEWAY's proactive refresh: the half of
// docs/status/oauth.md that used to exist only in the daemon.
//
// "Gateway", not "stdio gateway". The daemon's data plane assembles gateways
// of its own (internal/daemon/httpdata.go), and they reach this file by the
// same route — Config.Auth left nil, so newGateway builds the chain. The
// distinction that matters here is not the transport but the OWNERSHIP: the
// daemon renews on a timer because it is long-lived and owns every server at
// once, while a gateway owns whatever its client dialed and has no timer, so
// it renews at the only moment it is guaranteed to be looking: when a
// connection asks for the credential. `expires_at` is read
// from the vault, and if the token is inside the refresh grace it is renewed
// before the request goes out.
//
// Why this is not merely a nicety over the 401/403 path in
// internal/downstream: that path needs the downstream to REJECT the token,
// and some servers do not. A real one answers `initialize`, `tools/list` and
// `tools/call` with 200 for a bogus or expired bearer, returning
// `isError: true` and the text "Invalid token" inside a perfectly ordinary
// tool result; it 401s only when the Authorization header is missing
// entirely. Against such a server the passive path can never fire, and before
// this the credential stayed dead until the client was restarted. Reading the
// downstream's result to find out is not an option and never was — the gate
// chain decides from configuration alone and nothing in it inspects what a
// call carries back (AGENTS.md).
//
// The retry schedule is the deadline. On a failed renewal the source reports
// a deadline one backoff rung away, so the round tripper keeps the credential
// it has until then and the next request is the retry. There is no separate
// timer to own and no way for a failing provider to be hit once per request.

// proactiveSource is the gateway's TokenSource: a scoped vault source that
// renews the credential before handing it out, and reports when the value it
// handed out stops being worth sending.
//
// It embeds the TokenSource rather than replacing it, and that division is
// load-bearing: the REFRESH is keyed on the server (the refresh token is the
// server's, and a per-scope lock would let two derivations spend it
// concurrently), while the READ stays the embedded source's scoped
// specific-wins lookup. A derived instance with its own stored credential
// must keep getting its own, not the shared one this refresh renewed.
type proactiveSource struct {
	downstream.TokenSource

	coord     *oauthflow.Coordinator
	serverID  string
	scopeName string
	log       *slog.Logger

	// epoch reports this server's credential epoch (credwatch.go), or is nil
	// in an assembly with no announcement plane. It is what keeps a decision
	// taken about one credential from outliving it: every schedule below —
	// including the long hold a refused grant earns — describes the bytes in
	// the vault at the time it was taken, and a login replaces those bytes.
	// Without this, `auth login` fixed the credential while the renewer that
	// gave up on the old one stayed parked for its whole backoff.
	epoch func() uint64

	// now overrides time.Now (tests).
	now func() time.Time

	mu sync.Mutex
	// seenEpoch is the epoch the current schedule was decided under.
	seenEpoch uint64
	// notAfter is what NotAfter reports and, equivalently, when this source
	// will next consult the coordinator. Zero means "never again": either
	// there is no OAuth state at all (a hand-pasted token) or the provider
	// advertised no expiry, and in both cases the passive 401/403 path owns
	// the server exactly as it did before.
	notAfter time.Time
	// checked latches the zero above as a decision rather than as "not yet
	// looked". Without it a server with no OAuth state would re-read the
	// vault on every single request.
	checked bool
	// fails counts consecutive renewal failures; it drives the backoff and is
	// reset by any success.
	fails int
}

var (
	_ downstream.TokenSource = (*proactiveSource)(nil)
	// The deadline face is unexported in internal/downstream on purpose (it
	// is a seam, not an API). This is the compile-time proof that the method
	// set still matches it; a rename on either side breaks here rather than
	// silently reverting this whole file to the passive-only behaviour.
	_ interface{ NotAfter() time.Time } = (*proactiveSource)(nil)
)

// newProactiveSource wraps base with the proactive renewal of this file.
// epoch may be nil; then a schedule stands until it expires on its own.
func newProactiveSource(base downstream.TokenSource, coord *oauthflow.Coordinator,
	serverID, scopeName string, epoch func() uint64, log *slog.Logger) *proactiveSource {
	return &proactiveSource{
		TokenSource: base, coord: coord, serverID: serverID, scopeName: scopeName,
		epoch: epoch, log: log,
	}
}

// currentEpoch reads the credential epoch, or 0 without an announcement plane.
func (p *proactiveSource) currentEpoch() uint64 {
	if p.epoch == nil {
		return 0
	}
	return p.epoch()
}

func (p *proactiveSource) clock() time.Time {
	if p.now != nil {
		return p.now()
	}
	return time.Now()
}

// NotAfter implements the credentialDeadline face of internal/downstream.
func (p *proactiveSource) NotAfter() time.Time {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.notAfter
}

// Token renews first when the stored token is due, then hands over whatever
// the vault holds.
//
// Failure direction: FAIL-SOFT, and deliberately so. A renewal that cannot
// run must not fail the request — the stored credential may well still be
// accepted (the grace means we try before expiry), and on a server that never
// had OAuth state at all there is nothing to renew and a hand-pasted token to
// deliver. Every error here therefore ends in the embedded source's read,
// with the downstream's own answer as the diagnostic.
func (p *proactiveSource) Token(ctx context.Context) (string, bool, error) {
	p.ensureFresh(ctx)
	return p.TokenSource.Token(ctx)
}

// due reports whether the coordinator should be consulted now.
func (p *proactiveSource) due(now time.Time) bool {
	// Read the epoch outside the lock: it has one of its own, and nothing
	// under it calls back into this source.
	cur := p.currentEpoch()
	p.mu.Lock()
	defer p.mu.Unlock()
	if cur != p.seenEpoch {
		// Somebody stored new credentials for this server. Every decision
		// below was taken about the ones they replaced.
		return true
	}
	if p.checked && p.notAfter.IsZero() {
		return false // decided: this server has no proactive schedule
	}
	return p.notAfter.IsZero() || !now.Before(p.notAfter)
}

// ensureFresh renews the credential if it is within the refresh grace, and
// records when to look again.
func (p *proactiveSource) ensureFresh(ctx context.Context) {
	now := p.clock()
	if !p.due(now) {
		return
	}

	st, _, renewed, err := p.coord.EnsureFresh(ctx, p.serverID)
	// Superseded means another process stored a fresh credential first; the
	// state it returns is that one, so this is the goal state reached by
	// someone else, not a failure.
	superseded := errors.Is(err, oauthflow.ErrRefreshSuperseded)
	if err != nil && !superseded {
		p.renewalFailed(now, err)
		return
	}
	if renewed {
		// Logged only on a real renewal: EnsureFresh is called on every cache
		// miss and almost always finds the stored token still good, so a line
		// here would drown the one an operator greps for.
		p.log.Info("access token refreshed", logx.Server(p.serverID),
			"trigger", oauthflow.TriggerExpiry, "superseded", superseded, "scope", p.scopeName)
	}
	p.scheduleFrom(st, now)
}

// scheduleFrom records the next look from the state now in the vault.
func (p *proactiveSource) scheduleFrom(st *oauthflow.State, now time.Time) {
	at, scheduled := st.RefreshAt()
	cur := p.currentEpoch()
	p.mu.Lock()
	defer p.mu.Unlock()
	p.fails = 0
	p.checked = true
	p.seenEpoch = cur
	if !scheduled {
		// No expiry advertised means "never expires", not "expired"
		// (docs/status/oauth.md). The 401/403 path owns this server.
		p.notAfter = time.Time{}
		return
	}
	// Floored, so a provider issuing tokens shorter than the grace cannot
	// turn every request into a renewal. The floor costs at most one late
	// refresh of that length; without it a misconfigured downstream becomes a
	// loop against the authorization server.
	if floor := now.Add(oauthflow.RefreshRetryBackoff); at.Before(floor) {
		at = floor
	}
	p.notAfter = at
}

// renewalFailed records a failed renewal and holds off for one backoff rung.
func (p *proactiveSource) renewalFailed(now time.Time, err error) {
	// The same two predicates the daemon's scan uses, for the same reason:
	// this is the second of the two places that must agree about which
	// failures a human has to repair.
	dead := oauthflow.NeedsLogin(err)
	unmanaged := oauthflow.IsUnmanaged(err)
	cur := p.currentEpoch()

	p.mu.Lock()
	p.checked = true
	p.seenEpoch = cur
	switch {
	case unmanaged:
		// Not an OAuth server at all: a hand-pasted token, or one that was
		// never authorized. Nothing to schedule, ever — and latching it is
		// what keeps this off the per-request path.
		p.notAfter = time.Time{}
	case dead:
		// Only `agenthub auth login` fixes this. Jump to the slowest rung
		// rather than asking the provider again on a schedule.
		//
		// The rung is a ceiling, not a wait: a login announces itself
		// (credwatch.go) and due() re-consults on the epoch bump, so the 24
		// hours are what an installation with no announcement plane falls
		// back to. And a REVOKED grant does not reach the provider at all
		// even when the rung expires — the coordinator answers it from the
		// vault — so what this rung bounds here is a vault read.
		p.fails = oauthflow.FastRetries + len(oauthflow.SlowBackoffLadder)
		p.notAfter = now.Add(oauthflow.RetryBackoff(p.fails))
	default:
		p.fails++
		p.notAfter = now.Add(oauthflow.RetryBackoff(p.fails))
	}
	n, retryIn, silent := p.fails, p.notAfter, unmanaged
	p.mu.Unlock()

	if silent {
		return // a server with no OAuth state is the normal case, not an event
	}
	if dead {
		p.log.Warn("token cannot be refreshed without a new login",
			logx.Server(p.serverID), "trigger", oauthflow.TriggerExpiry,
			"scope", p.scopeName, "error", err)
		return
	}
	p.log.Warn("access token refresh failed", logx.Server(p.serverID),
		"trigger", oauthflow.TriggerExpiry, "scope", p.scopeName,
		"attempt", n, "retry_in", retryIn.Sub(now), "error", err)
}
