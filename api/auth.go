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
	// at all, which is "never expires" — NOT "expired" (docs/modules/oauth.md).
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

// AuthService reports and maintains stored OAuth credentials.
//
// SCOPE LIMIT (docs/modules/controlplane.md): only the non-interactive
// operations live here. An interactive login is NOT on this API — the
// loopback callback needs a local browser and a random port, which is a
// second, easily-broken code path with little to show for it. The device and
// manual flows belong to the CLI; status/refresh/logout are what a frontend
// needs to keep a credential healthy.
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
