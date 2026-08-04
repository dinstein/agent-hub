package downstream

import (
	"context"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/dinstein/agent-hub/internal/eventlog"
	"github.com/dinstein/agent-hub/internal/secrets"
)

// authRoundTripper attaches the downstream's bearer credential to every
// request and performs the passive refresh of docs/modules/oauth.md, docs/flows.md §5:
//
//	401 or 403 ─► refresh ONCE ─► replay the request ONCE ─► give up
//
// Why here and not in internal/downstream's call loop: the HTTP status is
// only visible at this layer. The transport facade maps 401/403 to
// ClassFatal with a textual message, and reconstructing the status by
// parsing that message would make an error string load-bearing.
//
// Non-idempotency (frozen rule: tools/call must never be silently executed
// twice) is preserved by two conditions, both required:
//
//   - the status is 401/403, which every MCP server decides BEFORE dispatching
//     the call — an authorization rejection is proof the request had no
//     effect, unlike a 5xx or a broken read;
//   - the body is replayable (GetBody is set, which the transport facade
//     guarantees by handing http.NewRequest a *bytes.Reader). Without it the
//     request is returned as-is rather than reconstructed.
//
// 403 is included with 401 deliberately: several providers answer 403 to an
// expired token (oauthflow.ShouldRefreshOnStatus documents the same
// choice), and treating 403 as "permission denied, never refresh" leaves
// those servers permanently broken.
type authRoundTripper struct {
	base http.RoundTripper
	auth TokenSource

	// endpoint is the configured origin the credential belongs to. attach
	// refuses to set Authorization on a request aimed anywhere else, which
	// is the second of the two independent gates against a downstream
	// redirecting its own credential away (the first is the client's
	// CheckRedirect — see newAuthClient).
	endpoint *url.URL

	// events records a renewal that stopped working. A call-time refresh
	// failure is the one credential fault with no other trace: the server
	// stays connected and answering until the current token expires, so
	// nothing flips state and nothing reconnects — the first symptom is calls
	// failing for a reason the connection itself cannot explain.
	events serverEvents

	// mu guards the cached token. The cache exists because the alternative
	// is a vault read — on macOS, a keychain round trip — on every single
	// HTTP request.
	//
	// epoch is the credential epoch the cached value was read AT, when the
	// TokenSource reports one (credentialEpoch). It is what turns a credential
	// announcement into a cache drop without a request having to be rejected
	// first — see token().
	//
	// The credential DEADLINE is deliberately not cached here: like the
	// epoch, it is re-read from the source on every request. The source is
	// the only thing that knows when its own value stops being good, and a
	// copy taken at load time would keep serving a token past a deadline the
	// source had already moved.
	mu     sync.Mutex
	tok    string
	loaded bool
	epoch  uint64
	// refreshBroken is whether the last renewal FAILED, and it exists only to
	// make the event below a transition. A dead credential fails every
	// request, so emitting per failure would let one broken server fill a
	// shared file — the flip is the fact, which is the same rule the health
	// tracker and the breaker already follow.
	refreshBroken bool
}

// credentialEpoch is the optional face of a TokenSource that can say when its
// stored credential may have changed. A source that does not implement it
// keeps the previous behaviour exactly: the cache is dropped only by a 401.
//
// It is a counter and not a timestamp on purpose — the writer is usually
// another process, and "different from what I last saw" is the only
// comparison this needs to support.
type credentialEpoch interface{ Epoch() uint64 }

// credentialDeadline is the optional face of a TokenSource that knows when
// the credential it last handed out stops being worth sending.
//
// It exists because the other three cache rules are all REACTIVE: a miss, a
// 401, or an announcement by another process. None of them fires for a token
// that simply ages out inside a live connection while no other process is
// running — the standalone gateway's case, since there is no daemon there to
// rotate the vault and announce it. Worse, a server that answers an expired
// token with 200 and an error *result* rather than 401 (they exist; see
// docs/modules/oauth.md) never produces the rejection the second rule waits
// for, so without a deadline such a connection stays broken until the client
// restarts.
//
// A deadline is a wall-clock instant rather than a TTL because the value it
// comes from is one: `expires_at` in the stored OAuth state. It is the
// instant a REFRESH becomes due, not the hard expiry — the point is to drop
// the cache early enough that the re-read has somewhere to go.
//
// The zero instant means "no deadline" and is what a source that cannot know
// reports; the cache then behaves exactly as it did before this face existed.
type credentialDeadline interface{ NotAfter() time.Time }

// EpochFunc reports the current credential epoch of one server.
type EpochFunc func() uint64

// WithEpoch attaches a credential epoch to a TokenSource, so a round tripper
// built over it drops its cached credential the moment the epoch moves
// instead of waiting for the downstream to reject what it is holding.
//
// It is a decorator rather than a constructor parameter because only an
// assembly that HAS an announcement plane to read (the gateway) can supply
// one; the daemon and the CLI build the same sources without it and must not
// be made to pass nil.
func WithEpoch(ts TokenSource, epoch EpochFunc) TokenSource {
	if ts == nil || epoch == nil {
		return ts
	}
	return epochTokenSource{TokenSource: ts, epoch: epoch}
}

type epochTokenSource struct {
	TokenSource
	epoch EpochFunc
}

func (e epochTokenSource) Epoch() uint64 { return e.epoch() }

// NotAfter forwards the wrapped source's deadline, because a decorator that
// embeds the TokenSource *interface* does not carry the wrapped value's other
// methods: the credentialDeadline assertion in the round tripper is made
// against the outermost value, and without this it would silently miss a
// deadline the innermost source does report. A source with no deadline
// reports the zero instant, which is what "no deadline" already means.
func (e epochTokenSource) NotAfter() time.Time { return deadlineOf(e.TokenSource) }

// deadlineOf reports a source's credential deadline, or the zero instant when
// it has none.
func deadlineOf(ts TokenSource) time.Time {
	if d, ok := ts.(credentialDeadline); ok {
		return d.NotAfter()
	}
	return time.Time{}
}

func newAuthRoundTripper(
	base http.RoundTripper, auth TokenSource, endpoint *url.URL, events serverEvents,
) *authRoundTripper {
	return &authRoundTripper{base: base, auth: auth, endpoint: endpoint, events: events}
}

// currentEpoch reports the source's epoch and whether it has one at all.
func (a *authRoundTripper) currentEpoch() (uint64, bool) {
	if es, ok := a.auth.(credentialEpoch); ok {
		return es.Epoch(), true
	}
	return 0, false
}

// RoundTrip implements http.RoundTripper.
//
// It never mutates the incoming request (RoundTripper contract): the
// Authorization header is set on a clone.
func (a *authRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	tok := a.token(req.Context())
	resp, err := a.base.RoundTrip(a.attach(req, tok))
	if err != nil {
		return nil, err
	}
	if !shouldRefreshOnStatus(resp.StatusCode) {
		return resp, nil
	}
	// Everything below is the passive-refresh ladder, and every rung of it
	// ended in a silent `return resp, nil`. An operator saw a 401 and could
	// not tell whether a refresh had even been attempted, whether it worked,
	// or whether the replay came back 401 again — three different problems
	// with three different fixes and one indistinguishable symptom.
	//
	// Debug throughout: a healthy connection never reaches here, and the
	// outcome this produces is already reported at its own level upstream.
	log := a.events.logger()
	replay, ok := replayable(req)
	if !ok {
		// Not a failure of ours: a request whose body cannot be rebuilt must
		// never be guessed at, so the 401 stands. But "agenthub did not even
		// retry" reads as a bug until the reason is written down.
		log.Debug("not replaying an unauthorized request: its body cannot be rebuilt",
			"status", resp.StatusCode, "method", req.Method)
		return resp, nil
	}
	fresh, rerr := a.refresh(req.Context(), tok)
	if rerr != nil || fresh == "" || fresh == tok {
		// Refresh failed, or produced the very credential that was just
		// rejected. Hand the original 401/403 back: it carries the server's
		// WWW-Authenticate hint, which is what the operator needs.
		//
		// One branch in code, three answers to the operator — the renewal
		// broke, there was nothing to renew, or the renewal returned what the
		// server had already refused (a revoked grant, or a vault holding a
		// credential nobody rotated). Named apart so the log does not merge
		// them back into the branch they share.
		reason := "refresh returned the credential that was just rejected"
		switch {
		case rerr != nil:
			reason = "refresh failed"
		case fresh == "":
			reason = "no credential to refresh"
		}
		log.Debug("not replaying an unauthorized request", "status", resp.StatusCode,
			"reason", reason, "error", rerr)
		return resp, nil
	}
	drain(resp)
	replayed, rtErr := a.base.RoundTrip(a.attach(replay, fresh))
	if rtErr != nil {
		log.Debug("the replay after a refresh did not complete", "error", rtErr)
		return nil, rtErr
	}
	// Whether the fresh credential actually helped. A replay that comes back
	// 401 again is the case that sends people to the wrong place: the vault is
	// fine, the refresh worked, and the server still says no — which is a
	// scope or audience problem, not a credential one.
	log.Debug("replayed an unauthorized request with a refreshed credential",
		"was", resp.StatusCode, "now", replayed.StatusCode)
	return replayed, nil
}

// token returns the cached credential, loading it from the vault on first
// use. A load failure yields the empty string: the request then goes out
// unauthenticated and the server's own 401 is the diagnostic — failing the
// request here would hide a working anonymous endpoint behind a vault
// problem.
//
// A miss is NOT cached. The connection outlives the vault read, and a
// server enabled before its credential existed (`server add` → `server
// enable` → `auth login`, the order the CLI recommends) would otherwise
// hold the empty string for the life of the process. The 401 path recovers
// either way, but only after a request has already been rejected; leaving
// the miss uncached means the very next request carries the credential.
// The cache still does its job — sparing a keychain round trip per
// request — for the case it was written for: a credential that is there.
//
// A cached value is also dropped when the credential epoch has moved, for
// the case a 401 cannot cover: a credential ROTATED while the one in hand
// is still accepted. Without it the daemon's proactive refresher, which
// rewrites the vault every 60s, could never deliver its work to a live
// connection — the new token would sit in the vault until the old one was
// finally rejected, which is correct but late, and late is what the
// announcement plane exists to fix.
//
// It is dropped a fourth time when the source reports a credentialDeadline
// that has passed. That is the only rule of the four which needs neither a
// rejection nor another process: it is what lets a standalone gateway renew a
// token that ages out mid-connection, including against a server that answers
// an expired credential with 200 rather than 401.
func (a *authRoundTripper) token(ctx context.Context) string {
	epoch, versioned := a.currentEpoch()
	deadline := deadlineOf(a.auth)

	a.mu.Lock()
	usable := a.loaded &&
		(!versioned || a.epoch == epoch) &&
		(deadline.IsZero() || time.Now().Before(deadline))
	if usable {
		cur := a.tok
		a.mu.Unlock()
		return cur
	}
	a.mu.Unlock()

	// Loaded OUTSIDE the lock. A source that reports a deadline renews inside
	// Token, and that is a network round trip; holding mu across it would
	// queue every concurrent request to this server behind it, which is the
	// same reason refresh() below unlocks before renewing. Two callers racing
	// here cost one extra vault read at worst — the serialization that
	// actually matters, a one-time refresh token spent twice, belongs to the
	// TokenSource and not to this cache.
	tok, ok, err := a.auth.Token(ctx)
	if err != nil || !ok {
		return ""
	}
	// The epoch is re-read AFTER the load, for the same reason refresh() does
	// after renewing: a source that renewed inside Token has already moved
	// it, and caching under the pre-load value would discard the new
	// credential on the very next request.
	epoch, _ = a.currentEpoch()
	a.mu.Lock()
	a.tok = tok
	a.loaded = true
	a.epoch = epoch
	a.mu.Unlock()
	return tok
}

// refresh renews the credential that produced a 401/403.
//
// stale is the token the rejected request carried. If the cache has already
// moved past it, another request's refresh won (or the daemon refreshed
// proactively and this process re-read the vault) and the new value is
// returned WITHOUT touching the authorization server: refreshing again
// would burn a one-time refresh token that has already been rotated.
func (a *authRoundTripper) refresh(ctx context.Context, stale string) (string, error) {
	a.mu.Lock()
	if a.loaded && a.tok != stale {
		cur := a.tok
		a.mu.Unlock()
		return cur, nil
	}
	a.mu.Unlock()

	// Re-read the vault before going to the authorization server. The
	// rejected credential may be nothing but this connection's stale cache:
	// another process — `agenthub auth login`, or the daemon's proactive
	// refresher, neither of which holds a handle on a live round tripper —
	// may already have written the successor. A read is not an authorization
	// flow: it burns no refresh token and never prompts anyone.
	//
	// The tok != stale guard is what keeps this from swallowing the real
	// refresh path: re-reading the very credential that was just rejected
	// proves the vault has nothing newer, so fall through and renew.
	epoch, _ := a.currentEpoch()
	if tok, ok, err := a.auth.Token(ctx); err == nil && ok && tok != stale {
		a.mu.Lock()
		a.tok, a.loaded, a.epoch = tok, true, epoch
		a.mu.Unlock()
		return tok, nil
	}

	tok, err := a.auth.Refresh(ctx)
	if err != nil {
		a.noteRefresh(false, err)
		return "", err
	}
	a.noteRefresh(true, nil)
	// Re-read the epoch AFTER the renewal: the renew path writes the vault,
	// which announces, so the value read before it is already one behind.
	// Caching the new token under the stale epoch would make the very next
	// request throw it away and read the vault again — harmless, but it
	// would defeat the cache on exactly the servers that refresh most.
	epoch, _ = a.currentEpoch()
	a.mu.Lock()
	a.tok = tok
	a.loaded = true
	a.epoch = epoch
	a.mu.Unlock()
	return tok, nil
}

// shouldRefreshOnStatus mirrors oauthflow.ShouldRefreshOnStatus. It is
// duplicated rather than imported because internal/downstream must not
// depend on the OAuth flow (the TokenSource seam exists precisely so it
// does not).
func shouldRefreshOnStatus(code int) bool {
	return code == http.StatusUnauthorized || code == http.StatusForbidden
}

// attach clones req and sets Authorization when tok is non-empty and the
// caller did not already provide the header (an explicit header always
// wins — see Deps.buildHeader).
//
// Failure direction: FAIL-CLOSED on origin. A request aimed anywhere other
// than the configured endpoint's origin goes out WITHOUT the credential
// rather than with it, and a round tripper built with no endpoint at all
// attaches nothing. This is the inner of the two gates that keep a
// redirecting downstream from choosing where its credential is delivered;
// the client's CheckRedirect is the outer one, and per AGENTS.md neither
// is collapsed into the other.
func (a *authRoundTripper) attach(req *http.Request, tok string) *http.Request {
	if tok == "" {
		return req
	}
	if req.Header.Get(headerAuthorization) != "" {
		return req
	}
	if !sameOrigin(a.endpoint, req.URL) {
		return req
	}
	clone := req.Clone(req.Context())
	clone.Header.Set(headerAuthorization, bearerValue(tok))
	return clone
}

// bearerValue formats the Authorization value. A stored token that already
// carries its scheme ("Bearer ey…", "DPoP …") is passed through: the vault
// slot is shared with hand-pasted credentials and double-prefixing would
// produce "Bearer Bearer …".
func bearerValue(tok string) string {
	if strings.ContainsAny(tok, " \t") {
		return tok
	}
	return "Bearer " + tok
}

// replayable returns a fresh copy of req with an unread body, or ok=false
// when the body cannot be rebuilt.
func replayable(req *http.Request) (*http.Request, bool) {
	clone := req.Clone(req.Context())
	if req.Body == nil || req.Body == http.NoBody {
		clone.Body = req.Body
		return clone, true
	}
	if req.GetBody == nil {
		return nil, false
	}
	body, err := req.GetBody()
	if err != nil {
		return nil, false
	}
	clone.Body = body
	return clone, true
}

// drain consumes and closes a response body so its connection returns to
// the idle pool. The bound keeps a hostile server from turning a discarded
// error body into a memory amplifier.
func drain(resp *http.Response) {
	if resp == nil || resp.Body == nil {
		return
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<10))
	_ = resp.Body.Close()
}

// vaultTokenSource is the standard TokenSource: the access token comes from
// the vault entry (serverID, "_global", __http_auth__) — shared by OAuth
// and hand-pasted tokens — and renewal is delegated to the wiring's
// refresher (the daemon's singleflight coordinator online, the sibling file
// lock offline).
type vaultTokenSource struct {
	serverID string
	// scopeName is the vault scope component: "_global" for the base
	// instance, the derive key for a derived one (docs/modules/dataplane.md).
	scopeName string
	resolve   secrets.Resolver
	renew     RefreshFunc
}

// NewVaultTokenSource builds the standard TokenSource for one server under
// the default vault scope. resolve may be nil (then no stored credential is
// ever found) and renew may be nil (then a 401/403 is reported as-is
// instead of triggering a refresh).
func NewVaultTokenSource(serverID string, resolve secrets.Resolver, renew RefreshFunc) TokenSource {
	return NewScopedVaultTokenSource(serverID, secrets.DefaultScope, resolve, renew)
}

// NewScopedVaultTokenSource is NewVaultTokenSource for one derived
// instance: the access token is looked up under (serverID, scopeName) and
// falls back to the (serverID, "_global") entry when that scope stores
// none — same specific-wins rule as ${SECRET_X} resolution, so a derivation
// inherits the shared login until someone gives it its own.
func NewScopedVaultTokenSource(serverID, scopeName string, resolve secrets.Resolver, renew RefreshFunc) TokenSource {
	return &vaultTokenSource{serverID: serverID, scopeName: scopeName, resolve: resolve, renew: renew}
}

// Token implements TokenSource.
func (v *vaultTokenSource) Token(ctx context.Context) (string, bool, error) {
	if v.resolve == nil {
		return "", false, nil
	}
	tok, ok, err := resolveScoped(ctx, v.serverID, v.scopeName, secrets.KeyHTTPAuth, v.resolve)
	if err != nil || !ok || strings.TrimSpace(tok) == "" {
		return "", false, err
	}
	return tok, true, nil
}

// Refresh implements TokenSource.
func (v *vaultTokenSource) Refresh(ctx context.Context) (string, error) {
	if v.renew == nil {
		return "", ErrNoRefresher
	}
	return v.renew(ctx)
}

// noteRefresh records that renewal STOPPED working, once per transition.
//
// Only the failing direction is emitted, because the kind is
// `oauth_refresh_failed` and a record of that name reporting a recovery
// would be a row whose name and content disagree. Recovery is still
// TRACKED — the flag clears — so a credential that breaks, is fixed and
// breaks again reports both failures rather than only the first.
//
// What this deliberately does not do is invent an `oauth_refresh_ok` to
// pair with it, the way health_down/health_up pair. Health is a level that
// is always one thing or the other; a working renewal is the silent normal
// case, and a kind that fires on every successful hourly refresh would be
// the loudest thing in the file while saying nothing happened.
func (a *authRoundTripper) noteRefresh(ok bool, err error) {
	a.mu.Lock()
	was := a.refreshBroken
	a.refreshBroken = !ok
	a.mu.Unlock()
	if ok || was {
		return
	}
	a.events.emit(eventlog.Record{
		Kind: eventlog.KindOAuthRefreshFailed, Detail: err.Error(),
	}, "downstream access token refresh failed", "error", err)
}
