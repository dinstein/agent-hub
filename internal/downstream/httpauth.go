package downstream

import (
	"context"
	"io"
	"net/http"
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

	// mu guards the cached token. The cache exists because the alternative
	// is a vault read — on macOS, a keychain round trip — on every single
	// HTTP request.
	mu     sync.Mutex
	tok    string
	loaded bool
}

func newAuthRoundTripper(base http.RoundTripper, auth TokenSource) *authRoundTripper {
	return &authRoundTripper{base: base, auth: auth}
}

// RoundTrip implements http.RoundTripper.
//
// It never mutates the incoming request (RoundTripper contract): the
// Authorization header is set on a clone.
func (a *authRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	tok := a.token(req.Context())
	resp, err := a.base.RoundTrip(attachBearer(req, tok))
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
	return a.base.RoundTrip(attachBearer(replay, fresh))
}

// token returns the cached credential, loading it from the vault on first
// use. A load failure yields the empty string: the request then goes out
// unauthenticated and the server's own 401 is the diagnostic — failing the
// request here would hide a working anonymous endpoint behind a vault
// problem.
func (a *authRoundTripper) token(ctx context.Context) string {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.loaded {
		return a.tok
	}
	a.loaded = true
	tok, ok, err := a.auth.Token(ctx)
	if err != nil || !ok {
		a.tok = ""
		return ""
	}
	a.tok = tok
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

	tok, err := a.auth.Refresh(ctx)
	if err != nil {
		return "", err
	}
	a.mu.Lock()
	a.tok = tok
	a.loaded = true
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

// attachBearer clones req and sets Authorization when tok is non-empty and
// the caller did not already provide the header (an explicit header always
// wins — see Deps.buildHeader).
func attachBearer(req *http.Request, tok string) *http.Request {
	if tok == "" {
		return req
	}
	if req.Header.Get(headerAuthorization) != "" {
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
