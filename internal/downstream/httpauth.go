package downstream

import (
	"context"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"

	"github.com/dinstein/agent-hub/internal/secrets"
)

// authRoundTripper attaches the downstream's bearer credential to every
// request and performs the passive refresh of docs/modules/oauth.md, docs/flows.md §6:
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

	// mu guards the cached token. The cache exists because the alternative
	// is a vault read — on macOS, a keychain round trip — on every single
	// HTTP request.
	//
	// epoch is the credential epoch the cached value was read AT, when the
	// TokenSource reports one (credentialEpoch). It is what turns a credential
	// announcement into a cache drop without a request having to be rejected
	// first — see token().
	mu     sync.Mutex
	tok    string
	loaded bool
	epoch  uint64
}

// credentialEpoch is the optional face of a TokenSource that can say when its
// stored credential may have changed. A source that does not implement it
// keeps the previous behaviour exactly: the cache is dropped only by a 401.
//
// It is a counter and not a timestamp on purpose — the writer is usually
// another process, and "different from what I last saw" is the only
// comparison this needs to support.
type credentialEpoch interface{ Epoch() uint64 }

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

func newAuthRoundTripper(base http.RoundTripper, auth TokenSource, endpoint *url.URL) *authRoundTripper {
	return &authRoundTripper{base: base, auth: auth, endpoint: endpoint}
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
	replay, ok := replayable(req)
	if !ok {
		return resp, nil // cannot rebuild the body: never guess, never replay
	}
	fresh, rerr := a.refresh(req.Context(), tok)
	if rerr != nil || fresh == "" || fresh == tok {
		// Refresh failed, or produced the very credential that was just
		// rejected. Hand the original 401/403 back: it carries the server's
		// WWW-Authenticate hint, which is what the operator needs.
		return resp, nil
	}
	drain(resp)
	return a.base.RoundTrip(a.attach(replay, fresh))
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
func (a *authRoundTripper) token(ctx context.Context) string {
	epoch, versioned := a.currentEpoch()

	a.mu.Lock()
	defer a.mu.Unlock()
	if a.loaded && (!versioned || a.epoch == epoch) {
		return a.tok
	}
	tok, ok, err := a.auth.Token(ctx)
	if err != nil || !ok {
		return ""
	}
	a.tok = tok
	a.loaded = true
	a.epoch = epoch
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
		return "", err
	}
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
