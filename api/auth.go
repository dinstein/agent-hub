package api

import (
	"context"
	"net/http"
	"net/url"
)

// OAuth authorization states of AuthStatus.State.
const (
	// AuthStateAuthorized: a usable credential is stored.
	AuthStateAuthorized = "authorized"
	// AuthStateExpiring: still valid, but close enough to expiry that a
	// frontend should offer a refresh.
	AuthStateExpiring = "expiring"
	// AuthStateExpired: the credential is past its deadline. Recoverable
	// without a human only when HasRefreshToken is set.
	AuthStateExpired = "expired"
	// AuthStateRevoked: the authorization server refused the stored refresh
	// grant — spent, rotated away, consent withdrawn, or the client
	// registration deleted. Distinct from expired because the repairs
	// differ: an expired credential with a refresh token behind it renews
	// unattended, and this one needs a browser and a human whatever is
	// stored. HasRefreshToken is reported false alongside it for the same
	// reason — the bytes may still be there, but no unattended repair is.
	AuthStateRevoked = "revoked"
	// AuthStateNone: no credential is stored for this server.
	AuthStateNone = "none"
	// AuthStateError: the stored state could not be read. It is a row of
	// its own rather than a dropped one — a listing that hides what it
	// could not read is a listing that lies.
	AuthStateError = "error"
)

// AuthStatus is one server's authorization state.
//
// RED LINE: no token, no client secret, no refresh token — HasRefreshToken
// is a boolean, not the token. There is no reveal escape hatch on this API
// (docs/modules/controlplane.md rule 5).
//
// There is deliberately no persisted "needs auth" flag anywhere: whether a
// server currently requires authorization is runtime state reported through
// the Health contract. A stale "needsAuth: false" is exactly the failure
// that shows a Ready badge on a server whose every call 401s.
type AuthStatus struct {
	Server string `json:"server"`
	// State is one of the AuthState* constants.
	State  string `json:"state"`
	Issuer string `json:"issuer,omitempty"`
	Scope  string `json:"scope,omitempty"`
	// ExpiresAt is Unix seconds; 0 means the provider advertised no expiry
	// at all, which is "never expires" — NOT "expired" (docs/status/oauth.md).
	ExpiresAt int64 `json:"expires_at"`
	// ExpiresIn is seconds from now, negative once expired.
	ExpiresIn int64 `json:"expires_in"`
	// HasRefreshToken decides whether an expiry is recoverable without a
	// human; the token itself is never rendered.
	HasRefreshToken bool   `json:"has_refresh_token"`
	ClientRegistrar string `json:"client_registrar,omitempty"`
	Detail          string `json:"detail,omitempty"`
}

// AuthRefreshed is the answer to Refresh.
type AuthRefreshed struct {
	Server    string `json:"server"`
	ExpiresAt int64  `json:"expires_at"`
	ExpiresIn int64  `json:"expires_in"`
	// Superseded reports that another writer refreshed first and this call
	// adopted its result. It is a SUCCESS with a different provenance, not a
	// race lost: refreshing anyway would burn the refresh token the other
	// writer just stored.
	Superseded bool `json:"superseded"`
}

// AuthLoggedOut is the answer to Logout.
type AuthLoggedOut struct {
	Server string `json:"server"`
}

// Login phases of AuthLogin.Phase.
const (
	// AuthLoginPending: the flow is running. Whether there is anything for
	// the user to DO yet is AuthorizationURL and UserCode, not this.
	AuthLoginPending = "pending"
	// AuthLoginComplete: a credential was obtained and stored.
	AuthLoginComplete = "complete"
	// AuthLoginFailed: the flow errored, timed out or was cancelled.
	AuthLoginFailed = "failed"
)

// Interactive modes of AuthLogin.Mode.
const (
	// AuthLoginModeLoopback: the caller opens AuthorizationURL and the
	// daemon catches the redirect on a loopback port.
	AuthLoginModeLoopback = "loopback"
	// AuthLoginModeDevice: the caller shows UserCode and VerificationURI
	// and the daemon polls.
	AuthLoginModeDevice = "device"
)

// AuthLogin is one interactive login session.
//
// RED LINE, as everywhere else on this API: no access token, no refresh
// token, no authorization code and no device code. UserCode is the short
// string the human types into the provider's own site and is meant to be
// shown; the device code polled with has no field here at all.
//
// THE CALLER OPENS THE BROWSER. AuthorizationURL is returned rather than
// visited, because the daemon may be headless, may have been started by a
// service manager with no session to draw into, and may not be where the user
// is sitting. A frontend that ignores it and waits will wait forever.
type AuthLogin struct {
	ID     string `json:"id"`
	Server string `json:"server"`
	// Phase is one of the AuthLogin* phase constants.
	Phase string `json:"phase"`
	// Mode is empty until the flow has chosen one: that needs the
	// authorization server's metadata, so the first poll commonly has none.
	Mode string `json:"mode,omitempty"`
	// AuthorizationURL is the page to open (loopback mode).
	AuthorizationURL string `json:"authorization_url,omitempty"`
	// VerificationURI, VerificationURIComplete and UserCode are the device
	// flow's half. VerificationURIComplete embeds the code and is what a QR
	// code should encode.
	VerificationURI         string `json:"verification_uri,omitempty"`
	VerificationURIComplete string `json:"verification_uri_complete,omitempty"`
	UserCode                string `json:"user_code,omitempty"`
	// Deadline is when the SESSION gives up, in Unix seconds. It is not the
	// credential's expiry — see TokenExpiresAt.
	Deadline int64 `json:"deadline,omitempty"`
	// Issuer, Scope, TokenExpiresAt and HasRefreshToken describe what was
	// stored and are set only once Phase is AuthLoginComplete.
	//
	// TokenExpiresAt is Unix seconds, and 0 means the provider advertised no
	// expiry at all — "never expires", NOT "expired" (docs/status/oauth.md).
	Issuer          string `json:"issuer,omitempty"`
	Scope           string `json:"scope,omitempty"`
	TokenExpiresAt  int64  `json:"token_expires_at,omitempty"`
	HasRefreshToken bool   `json:"has_refresh_token,omitempty"`
	// Error and Hint are set only once Phase is AuthLoginFailed. A failed
	// login is a 200 carrying this shape, not an HTTP error: the session
	// exists and was read successfully, and what failed is the thing it
	// describes.
	Error string `json:"error,omitempty"`
	Hint  string `json:"hint,omitempty"`
}

// Pending reports that the session is still running.
func (l AuthLogin) Pending() bool { return l.Phase == AuthLoginPending }

// Actionable reports that the session is waiting on the human AND has said
// how to reach them. A pending session that is not actionable is still
// discovering, and there is nothing to show but a spinner.
func (l AuthLogin) Actionable() bool {
	return l.Pending() && (l.AuthorizationURL != "" || l.UserCode != "")
}

// AuthService reports and maintains stored OAuth credentials, and runs the
// interactive login that creates one.
//
// WHY THE INTERACTIVE LOGIN IS HERE. It was previously excluded on the
// grounds that a loopback callback needs a local browser and a random port,
// and that this would be "a second, easily-broken code path". Half of that
// held up and half did not, and the half that did not is the expensive one:
// with no login on this API, every graphical frontend's answer to a server
// that needs authorizing was a modal telling the user to go and run a
// terminal command — in an application whose entire purpose is that clients
// do not handle credentials.
//
// What makes it affordable is that it is NOT a second code path. The daemon
// drives the same oauthflow.Flow the CLI drives; what is new is session
// bookkeeping — start, poll, cancel — around a flow that is too long to fit
// in one request. The protocol keeps exactly one implementation.
//
// The genuinely new obligation is that the CALLER opens the browser. See
// AuthLogin.
type AuthService struct{ c *Client }

// Status returns the authorization state of the named server, or of every
// server that has stored credentials when server is "".
//
// The asymmetry is deliberate: in an unfiltered listing, servers that never
// had credentials are omitted (on thirty servers, twenty-eight "none" rows
// are noise), while asking for one server by name always returns its row,
// even when that row is AuthStateNone — an explicit question deserves an
// explicit answer.
func (s *AuthService) Status(ctx context.Context, server string) ([]AuthStatus, error) {
	var q url.Values
	if server != "" {
		q = url.Values{"server": {server}}
	}
	var out []AuthStatus
	if err := s.c.do(ctx, http.MethodGet, "/auth", q, nil, &out); err != nil {
		return nil, err
	}
	return out, nil
}

// Refresh renews one server's access token using its stored refresh token.
// It carries no precondition: credentials are not registry state and a
// refresh has no competing intent to lose.
func (s *AuthService) Refresh(ctx context.Context, server string) (AuthRefreshed, error) {
	var out AuthRefreshed
	err := s.c.do(ctx, http.MethodPost, "/auth/"+url.PathEscape(server)+"/refresh", nil, nil, &out)
	return out, err
}

// Logout removes the locally stored credentials for one server. It does not
// revoke them at the provider — agenthub cannot promise that — so a frontend
// must say "removed from this machine", not "revoked".
func (s *AuthService) Logout(ctx context.Context, server string) (AuthLoggedOut, error) {
	var out AuthLoggedOut
	err := s.c.do(ctx, http.MethodDelete, "/auth/"+url.PathEscape(server), nil, nil, &out)
	return out, err
}

// StartLogin begins an interactive login and returns immediately, before
// there is anything to show: choosing a mode needs the authorization server's
// metadata. Poll with Login until Actionable, act on what it carries, and
// keep polling until Phase leaves AuthLoginPending.
//
// Starting a login for a server that already has one running returns THAT
// session rather than a second one. Two concurrent flows would each bind
// their own loopback port and race to write the same vault entry, and the
// loser's consent screen would call back into nothing — so a double-clicked
// button must not be able to arrange it.
func (s *AuthService) StartLogin(ctx context.Context, server string) (AuthLogin, error) {
	var out AuthLogin
	err := s.c.do(ctx, http.MethodPost, "/auth/"+url.PathEscape(server)+"/login", nil, nil, &out)
	return out, err
}

// Login reads one login session.
//
// A session that FAILED is a successful read of a failed thing: it answers
// 200 with Phase = AuthLoginFailed and the reason in Error. Only an id that
// names no session at all is a 404 — and a finished session stays readable
// for a retention window afterwards, so a poller that asks one moment late
// gets the outcome instead of a not-found it would have to guess about.
func (s *AuthService) Login(ctx context.Context, id string) (AuthLogin, error) {
	var out AuthLogin
	err := s.c.do(ctx, http.MethodGet, "/logins/"+url.PathEscape(id), nil, nil, &out)
	return out, err
}

// CancelLogin abandons a running login and returns its final state.
//
// Cancelling one that has already completed is not an error and does not undo
// it: the credential is stored, and reporting it as abandoned would be the
// more damaging lie. What is cancelled is the WAIT, not the authorization —
// a consent the user already granted at the provider stays granted.
func (s *AuthService) CancelLogin(ctx context.Context, id string) (AuthLogin, error) {
	var out AuthLogin
	err := s.c.do(ctx, http.MethodDelete, "/logins/"+url.PathEscape(id), nil, nil, &out)
	return out, err
}
