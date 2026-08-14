package ctlapi

import (
	"context"
	"errors"
	"net/http"
	"slices"
	"time"

	"github.com/dinstein/agent-hub/api"
	"github.com/dinstein/agent-hub/internal/oauthflow"
)

// GET /v1/auth, POST /v1/auth/{server}/refresh, DELETE /v1/auth/{server}.
//
// Login itself is NOT here. It is interactive by nature (a device code to
// read and poll, or an authorization URL to open and a callback to paste)
// and docs/subsystems/docs/subsystems/controlplane.md scopes the GUI to the two non-interactive modes, which need a
// streaming exchange rather than one request/response. Status, refresh and
// logout are the parts that are pure state transitions, and they are what
// this file serves.
//
// NO CREDENTIAL IS RENDERED. AuthStatusWire reports issuer, scope, expiry
// and WHETHER a refresh token exists; the access token is loaded only to
// tell "registered but unusable" apart from "authorized", and is discarded.
// There is no --show equivalent to add later, because there is no field.

// AuthStatusWire is one server's authorization state.
type AuthStatusWire struct {
	Server string `json:"server"`
	// State is an api.AuthState* value: authorized | expiring | expired |
	// revoked | none | error.
	State  string `json:"state"`
	Issuer string `json:"issuer,omitempty"`
	Scope  string `json:"scope,omitempty"`
	// ExpiresAt is Unix seconds; 0 means the provider advertised no expiry
	// (docs/status/oauth.md: that is "never expires", NOT "expired").
	ExpiresAt int64 `json:"expires_at"`
	ExpiresIn int64 `json:"expires_in"`
	// HasRefreshToken decides whether an expiry is recoverable without a
	// human. It is "stored AND usable": a grant the provider has refused
	// answers false however many bytes are still in the vault, because the
	// unattended repair is what the flag is read for and there is none. The
	// token itself is never included.
	HasRefreshToken bool   `json:"has_refresh_token"`
	ClientRegistrar string `json:"client_registrar,omitempty"`
	Detail          string `json:"detail,omitempty"`
}

// AuthRefreshWire is the answer to a forced refresh.
type AuthRefreshWire struct {
	Server    string `json:"server"`
	ExpiresAt int64  `json:"expires_at"`
	ExpiresIn int64  `json:"expires_in"`
	// Superseded reports that another writer refreshed first and this call
	// adopted its result. It is a SUCCESS with a different provenance, not a
	// failure: refreshing anyway would burn the refresh token the other
	// writer just stored.
	Superseded bool `json:"superseded"`
}

// AuthLogoutWire is the answer to a logout.
type AuthLogoutWire struct {
	Server string `json:"server"`
}

// handleAuthStatus implements GET /v1/auth (optional ?server=).
//
// Without a filter, servers that never had credentials are omitted: on a
// registry with thirty servers, twenty-eight "none" rows are noise, not
// information. Asking for one server by name always returns its row, even
// when that row is "none" — an explicit question deserves an explicit answer.
func (s *Server) handleAuthStatus(w http.ResponseWriter, r *http.Request) {
	one := r.URL.Query().Get("server")
	var ids []string
	if one != "" {
		ids = []string{one}
	} else {
		for id := range s.opts.Registry.Snapshot().Servers.V.Servers {
			ids = append(ids, id)
		}
		slices.Sort(ids)
	}

	now := time.Now()
	out := make([]AuthStatusWire, 0, len(ids))
	for _, id := range ids {
		row := s.authStatusOf(r.Context(), id, now)
		if one == "" && row.State == api.AuthStateNone {
			continue
		}
		out = append(out, row)
	}
	writeOK(w, http.StatusOK, out)
}

// authStatusOf reports one server's stored state. Every failure is folded
// into the row rather than aborting the listing: this is a diagnostic, and
// one corrupt entry must not hide the other twenty-nine.
func (s *Server) authStatusOf(ctx context.Context, id string, now time.Time) AuthStatusWire {
	row := AuthStatusWire{Server: id, State: api.AuthStateNone}
	st, err := s.opts.NonRegistry.OAuth.LoadState(ctx, id)
	if err != nil {
		if !errors.Is(err, oauthflow.ErrNoState) {
			row.State = api.AuthStateError
			row.Detail = err.Error()
		}
		return row
	}
	row.Issuer = st.Issuer
	row.Scope = st.Scope
	row.ExpiresAt = st.ExpiresAt
	row.ExpiresIn = secondsUntil(st.ExpiresAt, now)
	row.ClientRegistrar = st.RegistrarKind

	// The lifecycle comes from OAuthLifecycleOf, the one copy `auth status` and
	// `server ls` also read. This file used to decide it inline, and by the
	// time a refused grant became a state worth reporting it was the copy
	// nobody remembered: no revoked arm, and HasRefreshToken spelled
	// `RefreshToken != ""`. So this endpoint answered `authorized` with
	// `has_refresh_token: true` about a credential `agenthub auth status`
	// called revoked, and a frontend reading api.AuthStatus — which documents
	// the state and promises the flag is false beside it — was told to offer
	// the one repair that cannot work.
	//
	// What is local to here is that the access token's existence is learned by
	// READING it. This endpoint is a diagnostic and the daemon holds the vault
	// open already, so the cost `server ls` refuses to pay is the right one.
	_, terr := s.opts.NonRegistry.OAuth.LoadAccessToken(ctx, id)
	lc := OAuthLifecycleOf(st, terr == nil, now)
	row.State, row.Detail, row.HasRefreshToken = lc.State, lc.Detail, lc.HasRefreshToken
	return row
}

// handleAuthRefresh implements POST /v1/auth/{server}/refresh.
func (s *Server) handleAuthRefresh(w http.ResponseWriter, r *http.Request, server string) {
	reqID := requestIDFrom(r.Context())
	ref := s.refresher()
	if ref == nil {
		writeErr(w, http.StatusNotFound, CodeNotFound,
			"this daemon has no refresh coordinator", "status and logout still work", reqID)
		return
	}
	st, _, err := ref.Refresh(r.Context(), server)
	superseded := errors.Is(err, oauthflow.ErrRefreshSuperseded)
	ok := err == nil || superseded
	switch {
	case ok:
	case errors.Is(err, oauthflow.ErrNoState):
		// Nothing stored for this server: the uniform 404. Refreshing what
		// was never authorized is not a server error.
		writeNotFound(w, r)
		return
	default:
		// The authorization server (or the network) refused. Not a 500 in
		// spirit, but the control plane's code table has no upstream-refusal
		// code and inventing a wire value here would fork it from api's
		// mirror; the hint carries the actionable part.
		writeErr(w, http.StatusInternalServerError, CodeInternal,
			"refreshing "+server+" failed: "+err.Error(),
			"run an OAuth login for this server; the stored refresh token may be spent or revoked", reqID)
		return
	}
	out := AuthRefreshWire{Server: server, Superseded: superseded}
	if st != nil {
		out.ExpiresAt = st.ExpiresAt
		out.ExpiresIn = secondsUntil(st.ExpiresAt, time.Now())
	}
	writeOK(w, http.StatusOK, out)
}

// handleAuthLogout implements DELETE /v1/auth/{server}: drop the locally
// stored credentials.
//
// Idempotent by design — logging out of a server with nothing stored
// succeeds. The alternative (404) would make cleanup flows branch on state
// they cannot observe without a second round trip.
func (s *Server) handleAuthLogout(w http.ResponseWriter, r *http.Request, server string) {
	reqID := requestIDFrom(r.Context())
	err := s.opts.NonRegistry.OAuth.Clear(r.Context(), server)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, CodeInternal,
			"removing the stored credentials of "+server+" failed: "+err.Error(), "", reqID)
		return
	}
	writeOK(w, http.StatusOK, AuthLogoutWire{Server: server})
}

// secondsUntil returns the seconds left until a Unix expiry, clamped at 0.
// An expiry of 0 means "never expires" and yields 0 too; the caller tells
// the two apart by ExpiresAt.
func secondsUntil(expiresAt int64, now time.Time) int64 {
	if expiresAt == 0 {
		return 0
	}
	if d := expiresAt - now.Unix(); d > 0 {
		return d
	}
	return 0
}
