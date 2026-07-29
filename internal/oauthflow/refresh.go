package oauthflow

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Refresher renews one server's access token and persists the result.
//
// The implementation in this package is *Coordinator, which picks the
// correct serialization for the process: in-process singleflight when the
// daemon owns refreshes, the <server>.refresh.lock file lock when offline.
// The interface exists so the gateway can be handed a remote refresher (an
// RPC to the daemon) without knowing the difference.
type Refresher interface {
	// Refresh renews serverID's token. It returns the state and access
	// token now in the vault — which, on ErrRefreshSuperseded, are another
	// writer's, not this call's.
	Refresh(ctx context.Context, serverID string) (*State, string, error)
}

// Lock acquisition bounds.
const (
	// refreshLockTimeout bounds waiting for the sibling file lock. It is
	// generous because the holder is doing a network round trip; the
	// failure it guards against is a crashed holder, and flock releases on
	// process exit, so a genuine deadlock is impossible.
	refreshLockTimeout = 30 * time.Second
	// refreshLockPoll is the retry interval on a busy lock.
	refreshLockPoll = 10 * time.Millisecond
)

// SlowBackoffLadder is the OAuth-specific retry ladder of docs/modules/oauth.md.
//
// Connection-time OAuth failures do NOT use the ordinary exponential
// backoff: that one retries every few seconds, and every retry either pops
// a browser window at the user or hammers the provider's authorization
// endpoint. An unauthorized server is waiting on a HUMAN, so the ladder is
// scaled to human response times.
var SlowBackoffLadder = []time.Duration{
	5 * time.Minute,
	15 * time.Minute,
	1 * time.Hour,
	4 * time.Hour,
	24 * time.Hour,
}

// SlowBackoff returns the wait before OAuth retry number attempt (0-based),
// saturating at the last rung.
func SlowBackoff(attempt int) time.Duration {
	if attempt < 0 {
		attempt = 0
	}
	if attempt >= len(SlowBackoffLadder) {
		attempt = len(SlowBackoffLadder) - 1
	}
	return SlowBackoffLadder[attempt]
}

// ShouldRefreshOnStatus reports whether an HTTP status observed on a
// downstream call should trigger a passive refresh.
//
// 403 is included alongside 401 deliberately: several providers answer 403
// to an expired token, and docs/modules/oauth.md records the inverse case too —
// servers that accept the connection and tools/list anonymously and only
// 401 on tools/call. Treating 403 as "permission denied, do not refresh"
// leaves those servers permanently broken with a Ready badge.
func ShouldRefreshOnStatus(code int) bool { return code == 401 || code == 403 }

// CoordinatorConfig configures a Coordinator.
type CoordinatorConfig struct {
	// Store is the vault face. Required.
	Store *Store
	// Client performs the refresh request. Required.
	Client *Client
	// LockDir is where <server>.refresh.lock files live. Required for the
	// offline path; the wiring passes the secrets directory so the lock is
	// a sibling of the vault it guards.
	//
	// It is an explicit parameter rather than a platform lookup so this
	// package keeps its narrow dependency budget and so tests need no
	// environment mutation.
	LockDir string
	// Online reports whether the daemon owns refreshes for this process.
	// nil means offline.
	//
	// Failure direction: defaulting to OFFLINE is the safe default. Taking
	// the file lock when it was not needed costs a syscall; skipping it
	// when it was needed costs the user their refresh token.
	Online func() bool
	// Now overrides time.Now (tests).
	Now func() time.Time
}

// refreshOutcome is the singleflight payload.
type refreshOutcome struct {
	State *State
	Token string
}

// Coordinator implements the two-tier refresh serialization of ruling A.2
// #10.
//
//	online  (daemon present) → in-process singleflight only. The daemon is
//	                           the single vault writer, so nothing outside
//	                           this process can race us.
//	offline (CLI direct refresh, or a standalone gateway's 401 passive
//	         refresh) → the <server>.refresh.lock sibling file lock, and
//	                    after acquiring it a RE-READ of the state.
//
// The re-read is the part that actually prevents the double spend: the lock
// only serializes, it does not tell the second acquirer that the work is
// already done. If the re-read shows expires_at has moved past what we
// observed before queuing for the lock, someone else already refreshed;
// this call abandons its own refresh and returns ErrRefreshSuperseded with
// the fresh credentials. Refreshing anyway would burn the brand-new refresh
// token the other writer just stored.
type Coordinator struct {
	cfg CoordinatorConfig
	// group is the in-process singleflight used on the online path. One
	// Coordinator per process is what makes the dedup meaningful: two
	// Coordinators over the same vault would each dedup their own callers
	// and then race each other.
	group Group[refreshOutcome]
}

// NewCoordinator builds a Coordinator.
func NewCoordinator(cfg CoordinatorConfig) *Coordinator { return &Coordinator{cfg: cfg} }

var _ Refresher = (*Coordinator)(nil)

func (c *Coordinator) now() time.Time {
	if c.cfg.Now != nil {
		return c.cfg.Now()
	}
	return time.Now()
}

func (c *Coordinator) online() bool {
	return c.cfg.Online != nil && c.cfg.Online()
}

// Refresh renews serverID's token under the appropriate serialization.
func (c *Coordinator) Refresh(ctx context.Context, serverID string) (*State, string, error) {
	if c.online() {
		out, _, err := c.group.Do(ctx, serverID, func(ctx context.Context) (refreshOutcome, error) {
			st, tok, err := c.refreshNow(ctx, serverID, nil)
			return refreshOutcome{State: st, Token: tok}, err
		})
		return out.State, out.Token, err
	}
	return c.refreshLocked(ctx, serverID)
}

// EnsureFresh returns a usable access token, refreshing first if the token
// is within the refresh grace of expiry. It is the entry point for the
// proactive path; ShouldRefreshOnStatus drives the passive one.
func (c *Coordinator) EnsureFresh(ctx context.Context, serverID string) (*State, string, error) {
	st, tok, err := c.cfg.Store.Load(ctx, serverID)
	switch {
	case err == nil && !st.NeedsRefresh(c.now()):
		return st, tok, nil
	case err != nil && !errors.Is(err, ErrNoToken):
		// ErrNoState and persistence failures are terminal; only a missing
		// access token next to valid state is refreshable.
		return nil, "", err
	}
	return c.Refresh(ctx, serverID)
}

// refreshLocked is the offline path: observe, lock, re-read, decide.
func (c *Coordinator) refreshLocked(ctx context.Context, serverID string) (*State, string, error) {
	// Observe BEFORE queuing for the lock. This value is the whole basis of
	// the supersede check: it is what we knew when we decided a refresh was
	// needed.
	observed := int64(-1)
	if st, err := c.cfg.Store.LoadState(ctx, serverID); err == nil {
		observed = st.ExpiresAt
	}
	lock, err := c.acquireRefreshLock(ctx, serverID)
	if err != nil {
		return nil, "", err
	}
	defer lock.release()
	return c.refreshNow(ctx, serverID, &observed)
}

// refreshNow performs the actual refresh. When observedExpiresAt is
// non-nil, it first re-reads the state and abandons the refresh if
// expires_at advanced.
func (c *Coordinator) refreshNow(ctx context.Context, serverID string, observedExpiresAt *int64) (*State, string, error) {
	st, err := c.cfg.Store.LoadState(ctx, serverID)
	if err != nil {
		return nil, "", err
	}
	if observedExpiresAt != nil && *observedExpiresAt >= 0 && st.ExpiresAt > *observedExpiresAt {
		// Someone refreshed while we waited. Their refresh token is the
		// live one; ours is already invalid on rotating providers.
		tok, tokErr := c.cfg.Store.LoadAccessToken(ctx, serverID)
		if tokErr != nil {
			return st, "", tokErr
		}
		return st, tok, fmt.Errorf("%w (server %q)", ErrRefreshSuperseded, serverID)
	}
	if strings.TrimSpace(st.RefreshToken) == "" {
		e := newFlowError(ErrorTypeRefresh, fmt.Errorf("%w: server %q", ErrNoRefreshToken, serverID))
		e.ServerID = serverID
		e.Suggestion = "this provider issued no refresh token; run `agenthub auth login " + serverID + "`"
		return nil, "", e
	}
	tok, err := c.cfg.Client.Refresh(ctx, RefreshRequest{
		TokenEndpoint: st.TokenEndpoint,
		ClientID:      st.ClientID,
		ClientSecret:  st.ClientSecret,
		RefreshToken:  st.RefreshToken,
		Resource:      st.Resource,
	})
	if err != nil {
		var fe *FlowError
		if errors.As(err, &fe) && fe.ServerID == "" {
			fe.ServerID = serverID
		}
		return nil, "", err
	}
	next := *st
	next.RefreshToken = "" // let SaveFromToken carry forward or rotate
	saved, err := c.cfg.Store.SaveFromToken(ctx, serverID, st, next, tok, c.now())
	if err != nil {
		return nil, "", err
	}
	return saved, tok.AccessToken, nil
}

// --- refresh lock ---------------------------------------------------------

// refreshLock is a held cross-process advisory lock.
type refreshLock struct{ f *os.File }

// RefreshLockPath is the sibling lock file for a server. Exported so the
// daemon can report which path a stuck refresh is waiting on.
func RefreshLockPath(dir, serverID string) string {
	return filepath.Join(dir, sanitizeLockName(serverID)+".refresh.lock")
}

// sanitizeLockName keeps a server ID from escaping LockDir. Server IDs are
// registry-validated, but a lock path built from an identifier is exactly
// the kind of place where "it is validated upstream" stops being true one
// refactor later.
func sanitizeLockName(id string) string {
	var b strings.Builder
	for _, r := range id {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_', r == '.':
			b.WriteRune(r)
		default:
			b.WriteByte('_')
		}
	}
	s := b.String()
	if s == "" || s == "." || s == ".." {
		return "_"
	}
	return s
}

func (c *Coordinator) acquireRefreshLock(ctx context.Context, serverID string) (*refreshLock, error) {
	if strings.TrimSpace(c.cfg.LockDir) == "" {
		return nil, newFlowError(ErrorTypeRefresh,
			fmt.Errorf("oauthflow: offline refresh requires a lock directory"))
	}
	if err := os.MkdirAll(c.cfg.LockDir, 0o700); err != nil {
		return nil, newFlowError(ErrorTypeRefresh, err)
	}
	path := RefreshLockPath(c.cfg.LockDir, serverID)
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, newFlowError(ErrorTypeRefresh, err)
	}
	deadline := time.Now().Add(refreshLockTimeout)
	for {
		err := flockExclusiveNB(f)
		if err == nil {
			return &refreshLock{f: f}, nil
		}
		if !isWouldBlock(err) {
			_ = f.Close()
			return nil, newFlowError(ErrorTypeRefresh,
				fmt.Errorf("oauthflow: lock %s: %w", path, err))
		}
		if ctx.Err() != nil {
			_ = f.Close()
			return nil, newFlowError(ErrorTypeRefresh, ctx.Err())
		}
		if time.Now().After(deadline) {
			_ = f.Close()
			return nil, newFlowError(ErrorTypeRefresh,
				fmt.Errorf("oauthflow: timed out after %s waiting for %s", refreshLockTimeout, path))
		}
		time.Sleep(refreshLockPoll)
	}
}

func (l *refreshLock) release() {
	if l == nil || l.f == nil {
		return
	}
	_ = flockUnlock(l.f)
	_ = l.f.Close()
	l.f = nil
}
