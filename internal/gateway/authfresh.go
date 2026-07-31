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

// This file is the standalone gateway's PROACTIVE refresh: the half of
// docs/modules/oauth.md that used to exist only in the daemon.
//
// The daemon renews on a timer because it is a long-lived process that owns
// every server at once. A stdio gateway owns whatever its client dialed and
// has no timer, so it renews at the only moment it is guaranteed to be
// looking: when a connection asks for the credential. `expires_at` is read
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

	// now overrides time.Now (tests).
	now func() time.Time

	mu sync.Mutex
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
func newProactiveSource(base downstream.TokenSource, coord *oauthflow.Coordinator,
	serverID, scopeName string, log *slog.Logger) *proactiveSource {
	return &proactiveSource{
		TokenSource: base, coord: coord, serverID: serverID, scopeName: scopeName, log: log,
	}
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
	p.mu.Lock()
	defer p.mu.Unlock()
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
	p.mu.Lock()
	defer p.mu.Unlock()
	p.fails = 0
	p.checked = true
	if !scheduled {
		// No expiry advertised means "never expires", not "expired"
		// (docs/modules/oauth.md). The 401/403 path owns this server.
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
	dead := errors.Is(err, oauthflow.ErrNoRefreshToken)
	unmanaged := errors.Is(err, oauthflow.ErrNoState)

	p.mu.Lock()
	p.checked = true
	switch {
	case unmanaged:
		// Not an OAuth server at all: a hand-pasted token, or one that was
		// never authorized. Nothing to schedule, ever — and latching it is
		// what keeps this off the per-request path.
		p.notAfter = time.Time{}
	case dead:
		// Only `agenthub auth login` fixes this. Jump to the slowest rung
		// rather than asking the provider again on a schedule.
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
