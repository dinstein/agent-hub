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

// Refresh triggers, reported as the `trigger` field of every refresh log
// record. Two processes renew tokens — the daemon proactively before expiry,
// a stdio gateway only once a downstream has rejected one — and they log the
// same messages for the same outcomes, so this field is what tells them apart.
//
// They live here rather than in either caller because the whole point is that
// both sides spell them identically, and two independent string literals is
// precisely how a log field stops being greppable.
const (
	// TriggerExpiry is the daemon's proactive scan (internal/daemon/oauth.go).
	TriggerExpiry = "expiry"
	// TriggerRejection is a downstream 401/403
	// (internal/downstream/httpauth.go, wired up in internal/gateway/auth.go).
	TriggerRejection = "rejection"
)

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

// FastRetries is how many consecutive proactive-refresh failures use the flat
// RefreshRetryBackoff before the slow ladder takes over. A blip deserves a
// prompt retry; a persistently failing refresh is waiting on a human.
const FastRetries = 3

// RetryBackoff is the wait after n consecutive proactive-refresh failures
// (n counted from 1): the flat retry of docs/modules/oauth.md for the first
// FastRetries, then the OAuth slow ladder.
//
// It lives here, next to the ladder, because BOTH proactive refreshers use
// it — the daemon's scan loop and the standalone gateway's token source —
// and a schedule reimplemented on each side is a schedule that drifts. The
// same argument as TriggerExpiry / TriggerRejection above.
func RetryBackoff(n int) time.Duration {
	if n <= FastRetries {
		return RefreshRetryBackoff
	}
	return SlowBackoff(n - FastRetries - 1)
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
//
// renewed reports whether a refresh actually ran. The caller needs it to log
// honestly: the overwhelmingly common outcome here is "the stored token was
// already fine", and a renewal record emitted for that would make the
// `access token refreshed` line — which an operator greps to see what the
// authorization server was asked for — meaningless.
//
// On ErrRefreshSuperseded the returned state and token ARE usable (another
// writer's); renewed is true, because a renewal did happen, just not this
// call's.
func (c *Coordinator) EnsureFresh(ctx context.Context, serverID string) (st *State, tok string, renewed bool, err error) {
	st, tok, err = c.cfg.Store.Load(ctx, serverID)
	switch {
	case err == nil && !st.NeedsRefresh(c.now()):
		return st, tok, false, nil
	case err != nil && !errors.Is(err, ErrNoToken):
		// ErrNoState and persistence failures are terminal; only a missing
		// access token next to valid state is refreshable.
		return nil, "", false, err
	}
	st, tok, err = c.Refresh(ctx, serverID)
	return st, tok, true, err
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
	if st.GrantRevoked() {
		// The provider has already refused this exact grant, and said so in
		// the vault. Asking again is a request whose answer is on file. The
		// one honest reason to send it anyway — "in case the provider changed
		// its mind" — is `auth refresh --force`, and nothing does it on a
		// timer.
		return nil, "", revokedError(serverID, st.GrantRevokedReason)
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
		return nil, "", c.recordRefusal(ctx, serverID, st, err)
	}
	next := *st
	next.RefreshToken = "" // let SaveFromToken carry forward or rotate
	saved, err := c.cfg.Store.SaveFromToken(ctx, serverID, st, next, tok, c.now())
	if err != nil {
		return nil, "", err
	}
	return saved, tok.AccessToken, nil
}

// recordRefusal files a terminal refusal in the vault and returns the error
// the caller should propagate. Anything else passes straight through.
//
// It runs inside the same serialization as the refresh that earned it — the
// file lock offline, the singleflight online — which is what lets the mark be
// a read-modify-write at all.
//
// A mark that cannot be written is APPENDED to the error rather than replacing
// it: the caller must still see NeedsLogin (the grant really is dead), and an
// operator whose vault is read-only must still be told that the reason the
// same warning returns every hour is the vault, not the provider.
func (c *Coordinator) recordRefusal(ctx context.Context, serverID string, refused *State, err error) error {
	if !errors.Is(err, ErrGrantRevoked) && !errors.Is(err, ErrClientRejected) {
		return err
	}
	_, markErr := c.cfg.Store.MarkGrantRevoked(ctx, serverID, refused, refusalReason(err), c.now())
	if markErr != nil {
		return fmt.Errorf("%w (the refusal could not be recorded: %v)", err, markErr)
	}
	return err
}

// refusalReason renders what the authorization server said, for the vault and
// for every surface that reads it back. It prefers the AS's own code and
// description over this package's wording: "invalid_grant: consent withdrawn"
// tells an operator which of a dozen provider behaviours they met, and
// "refresh grant rejected" tells them only that we are here.
func refusalReason(err error) string {
	var te *TokenError
	if !errors.As(err, &te) {
		return err.Error()
	}
	if te.Description == "" {
		return te.Code
	}
	return te.Code + ": " + te.Description
}

// revokedError rebuilds the terminal error from what the vault remembers, so
// a refusal recorded an hour ago is reported in the same shape as the one that
// just came back from the provider — same sentinel, same suggestion, same
// server id. A caller that classified the live refusal correctly cannot then
// mis-handle the remembered one.
//
// At rest the two live sentinels collapse into ErrGrantRevoked: what was
// recorded is that the grant this record holds is refused, and a rejected
// client and a rejected grant call for the same login. WHICH answer the
// provider gave survives in the reason, in the provider's own words, which is
// the part an operator can act on.
func revokedError(serverID, reason string) error {
	cause := error(ErrGrantRevoked)
	if reason != "" {
		cause = fmt.Errorf("%w: %s", ErrGrantRevoked, reason)
	}
	e := newFlowError(ErrorTypeRefresh, cause)
	e.ServerID = serverID
	e.Suggestion = "the authorization server refused this grant; run `agenthub auth login " + serverID +
		"` (or `agenthub auth refresh --force " + serverID + "` to ask it once more)"
	return e
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
