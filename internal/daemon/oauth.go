package daemon

import (
	"context"
	"errors"
	"log/slog"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"github.com/dinstein/agent-hub/internal/logx"
	"github.com/dinstein/agent-hub/internal/oauthflow"
	"github.com/dinstein/agent-hub/internal/registry"
	"github.com/dinstein/agent-hub/internal/secrets"
)

// This file is the daemon's OAuth refresh coordination (docs/modules/oauth.md /
// 6.4): tokens are renewed 60s BEFORE they expire, exactly once per server
// at a time, and a failed renewal backs off instead of hammering the
// authorization server.
//
// Three decisions worth stating, because each has a wrong-looking obvious
// alternative:
//
//  1. Refreshes run through oauthflow.Coordinator on its OFFLINE path (the
//     <server>.refresh.lock sibling file lock + a post-lock re-read of
//     expires_at), not the in-process-singleflight-only online path. The
//     online path is sound only while the daemon is the sole vault writer,
//     and `agenthub auth login/refresh` writes the vault directly today.
//     Cost of the lock when it was not needed: one syscall. Cost of
//     skipping it when it was: a one-time refresh token spent twice, which
//     locks the user out until a human re-authorizes.
//
//  2. On top of that, an in-process singleflight Group still dedups, so a
//     future control-plane `auth refresh` RPC lands on the same gate as the
//     proactive timer instead of racing it.
//
//  3. Tokens with NO expiry (ExpiresAt == 0, which several providers issue)
//     are never proactively refreshed. Design.md 7.7 is explicit that "no
//     expires_in" means "never expires", not "expired": those servers are
//     covered by the 401/403 passive refresh in internal/downstream.

// Refresh scheduling bounds.
const (
	// minRefreshScan is the floor on the scan loop's sleep, so a token that
	// is already due cannot spin the loop.
	minRefreshScan = 2 * time.Second
	// maxRefreshScan is the ceiling: even with nothing due, rescan this
	// often so credentials written by `auth login` while the daemon runs are
	// picked up without a restart.
	maxRefreshScan = 60 * time.Second
	// fastRetries is how many consecutive failures use the 15s retry of
	// docs/modules/oauth.md before switching to the OAuth slow ladder. A blip
	// deserves a prompt retry; twenty failures a minute against a provider
	// is abuse, and a persistently failing refresh is waiting on a human
	// (oauthflow.SlowBackoff: 5m/15m/1h/4h/24h).
	fastRetries = 3
)

// refresherConfig configures the proactive refresh loop.
type refresherConfig struct {
	// Store is the registry the server list comes from. Required.
	Store *registry.Store
	// Secrets is the vault. Required.
	Secrets secrets.Store
	// LockDir holds the <server>.refresh.lock files (the secrets dir, so
	// the lock is a sibling of the vault it guards). Required.
	LockDir string
	// Log is the daemon logger. Required.
	Log *slog.Logger
	// AllowLoopback relaxes the OAuth client's SSRF screen to literal
	// loopback authorization servers (self-hosted providers, tests).
	AllowLoopback bool
	// Now overrides time.Now (tests).
	Now func() time.Time
	// MaxScan overrides maxRefreshScan (tests shrink it).
	MaxScan time.Duration
	// MinScan overrides minRefreshScan (tests shrink it).
	MinScan time.Duration
	// OnCycle, when set, is called after every scan with the number of
	// refreshes attempted in it. Tests synchronize on it.
	OnCycle func(attempted int)
}

// refresher owns the proactive-refresh loop of one daemon.
type refresher struct {
	cfg   refresherConfig
	store *oauthflow.Store
	coord *oauthflow.Coordinator
	group oauthflow.Group[string]

	// mu guards the two maps below. The scan loop is single-goroutine
	// today, but the singleflight above exists precisely so another caller
	// (a control-plane `auth refresh`) can join a refresh in flight — and
	// that caller would touch these maps from its own goroutine.
	mu sync.Mutex
	// fails counts consecutive failures per server; it drives the backoff
	// ladder and is reset by any success.
	fails map[string]int
	// retryAt is the backoff state of a failing server.
	retryAt map[string]backoffState
}

// backoffState is one server's suppression window plus the expiry that was
// stored when it was recorded. The second field is what lets a fresh
// `auth login` cancel a long backoff: new credentials advance expires_at,
// and a suppression earned by the OLD ones must not outlive them.
type backoffState struct {
	until      time.Time
	observedAt int64 // State.ExpiresAt at the time of the failure
}

// hold reports the instant before which id must not be retried, given the
// expiry currently in the vault. A vault entry newer than the one that
// earned the backoff clears it.
func (r *refresher) hold(id string, currentExpiresAt int64) (time.Time, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	b, ok := r.retryAt[id]
	if !ok {
		return time.Time{}, false
	}
	if currentExpiresAt > b.observedAt {
		delete(r.retryAt, id)
		delete(r.fails, id)
		return time.Time{}, false
	}
	return b.until, true
}

// noteSuccess clears a server's failure state.
func (r *refresher) noteSuccess(id string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.fails, id)
	delete(r.retryAt, id)
}

// noteFailure records a failure and returns the wait it earned.
// observedAt is the expiry the failing state carried (see backoffState).
func (r *refresher) noteFailure(id string, now time.Time, observedAt int64, park bool) (int, time.Duration) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if park {
		// Unrecoverable without a human: jump straight to the slowest rung
		// of the ladder rather than retrying, because `agenthub auth login`
		// is the only fix. (A rescan still notices a fresh login promptly —
		// this only suppresses the pointless token requests.)
		r.fails[id] = fastRetries + len(oauthflow.SlowBackoffLadder)
	} else {
		r.fails[id]++
	}
	n := r.fails[id]
	d := r.backoff(n)
	r.retryAt[id] = backoffState{until: now.Add(d), observedAt: observedAt}
	return n, d
}

// failCount reports a server's consecutive failure count (tests, logs).
func (r *refresher) failCount(id string) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.fails[id]
}

// newRefresher builds the loop. It never fails: a broken vault or registry
// surfaces per cycle as a logged warning, because losing proactive refresh
// must never take the coordination plane down with it.
func newRefresher(cfg refresherConfig) *refresher {
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.MaxScan <= 0 {
		cfg.MaxScan = maxRefreshScan
	}
	if cfg.MinScan <= 0 {
		cfg.MinScan = minRefreshScan
	}
	store := oauthflow.NewStore(cfg.Secrets)
	client := oauthflow.NewClient(oauthflow.Config{AllowLoopback: cfg.AllowLoopback})
	return &refresher{
		cfg:   cfg,
		store: store,
		coord: oauthflow.NewCoordinator(oauthflow.CoordinatorConfig{
			Store:   store,
			Client:  client,
			LockDir: cfg.LockDir,
			Now:     cfg.Now,
			// Online is deliberately nil — see the file comment, decision 1.
		}),
		fails:   map[string]int{},
		retryAt: map[string]backoffState{},
	}
}

// run drives the scan loop until ctx is done.
func (r *refresher) run(ctx context.Context) {
	for {
		wait := r.cycle(ctx)
		timer := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}
	}
}

// cycle refreshes everything that is due and returns how long to sleep
// before the next scan: the time until the earliest upcoming deadline,
// clamped into [MinScan, MaxScan].
func (r *refresher) cycle(ctx context.Context) time.Duration {
	now := r.cfg.Now()
	next := now.Add(r.cfg.MaxScan)
	attempted := 0

	for _, id := range r.servers() {
		st, err := r.store.LoadState(ctx, id)
		if err != nil {
			// ErrNoState is the normal case for a server that was never
			// authorized (or uses a hand-pasted token); nothing to do and
			// nothing to report.
			if !errors.Is(err, oauthflow.ErrNoState) {
				r.cfg.Log.Warn("oauth state unreadable; skipping proactive refresh",
					logx.Server(id), "error", err)
			}
			continue
		}
		at, scheduled := st.RefreshAt()
		if !scheduled {
			continue // no expiry advertised: the 401 path owns this server
		}
		if now.Before(at) {
			next = earliest(next, at)
			continue
		}
		// The backoff is consulted only for servers that are actually due:
		// a suppression window must never hide the fact that a token needs
		// renewing, only slow down how often we try.
		if hold, ok := r.hold(id, st.ExpiresAt); ok && now.Before(hold) {
			next = earliest(next, hold)
			continue
		}
		attempted++
		r.refreshOne(ctx, id, now, st.ExpiresAt)
		if hold, ok := r.hold(id, st.ExpiresAt); ok {
			next = earliest(next, hold)
		}
	}

	if r.cfg.OnCycle != nil {
		r.cfg.OnCycle(attempted)
	}
	wait := next.Sub(r.cfg.Now())
	if wait < r.cfg.MinScan {
		wait = r.cfg.MinScan
	}
	if wait > r.cfg.MaxScan {
		wait = r.cfg.MaxScan
	}
	return wait
}

// refreshOne renews one server's token under the singleflight gate and
// records the outcome for the backoff ladder.
func (r *refresher) refreshOne(ctx context.Context, id string, now time.Time, observedExpiresAt int64) {
	_, shared, err := r.group.Do(ctx, id, func(ctx context.Context) (string, error) {
		_, tok, rerr := r.coord.Refresh(ctx, id)
		return tok, rerr
	})
	switch {
	case err == nil, errors.Is(err, oauthflow.ErrRefreshSuperseded):
		// Superseded means another writer already stored a fresh
		// credential — the goal state, reached by someone else.
		r.noteSuccess(id)
		r.cfg.Log.Info("access token refreshed", logx.Server(id), "shared", shared)
	case errors.Is(err, oauthflow.ErrNoRefreshToken), errors.Is(err, oauthflow.ErrNoState):
		r.noteFailure(id, now, observedExpiresAt, true)
		r.cfg.Log.Warn("token cannot be refreshed without a new login",
			logx.Server(id), "error", err)
	default:
		n, d := r.noteFailure(id, now, observedExpiresAt, false)
		r.cfg.Log.Warn("proactive token refresh failed; keeping the current token",
			logx.Server(id), "attempt", n, "retry_in", d, "error", err)
	}
}

// backoff is the retry wait after n consecutive failures: the flat 15s of
// docs/modules/oauth.md for the first few, then the OAuth slow ladder.
func (r *refresher) backoff(n int) time.Duration {
	if n <= fastRetries {
		return oauthflow.RefreshRetryBackoff
	}
	return oauthflow.SlowBackoff(n - fastRetries - 1)
}

// servers lists the enabled HTTP-transport servers, sorted, so a cycle is
// deterministic and a failing server cannot starve the others.
func (r *refresher) servers() []string {
	snap := r.cfg.Store.Snapshot()
	ids := make([]string, 0, len(snap.Servers.V.Servers))
	for id, doc := range snap.Servers.V.Servers {
		if !doc.V.Enabled || !doc.V.IsHTTP() {
			continue
		}
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids
}

func earliest(a, b time.Time) time.Time {
	if b.Before(a) {
		return b
	}
	return a
}

// startRefresher wires the proactive refresh loop into a running daemon.
// It returns nil (and logs) when the vault directory cannot be resolved:
// no refresh coordination is a degradation, never a reason to refuse to
// coordinate anything else.
func startRefresher(ctx context.Context, cfg Config, store *registry.Store, dataDir string, log *slog.Logger) *refresher {
	sec := cfg.Secrets
	if sec == nil {
		sec = secrets.NewChain(secrets.ChainConfig{Dir: filepath.Join(dataDir, "secrets")})
	}
	r := newRefresher(refresherConfig{
		Store:         store,
		Secrets:       sec,
		LockDir:       filepath.Join(dataDir, "secrets"),
		Log:           log,
		AllowLoopback: cfg.OAuthAllowLoopback,
		MaxScan:       cfg.RefreshScanInterval,
	})
	go r.run(ctx)
	return r
}

// Store exposes the vault face the control plane reads for `auth status`
// and clears for `auth logout`. It is the refresher's own store rather than
// a second one built beside it: two stores over one vault would be two
// caches of the same rotating credential.
func (r *refresher) Store() *oauthflow.Store { return r.store }

// Coordinator exposes the refresh singleflight so a control-plane refresh
// JOINS an in-flight one instead of racing it. A one-time refresh token
// spent twice is unrecoverable, which is exactly what a second coordinator
// would eventually cause.
func (r *refresher) Coordinator() *oauthflow.Coordinator { return r.coord }
