package ctlapi

import (
	"context"
	"errors"
	"net/http"
	"sort"
	"time"

	"github.com/dinstein/agent-hub/internal/oauthflow"
)

// GET /v1/auth, POST /v1/auth/{server}/refresh, DELETE /v1/auth/{server}.
//
// Login itself is NOT here. It is interactive by nature (a device code to
// read and poll, or an authorization URL to open and a callback to paste)
// and docs/modules/controlplane.md scopes the GUI to the two non-interactive modes, which need a
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
	// State is authorized | expiring | expired | none | error.
	State  string `json:"state"`
	Issuer string `json:"issuer,omitempty"`
	Scope  string `json:"scope,omitempty"`
	// ExpiresAt is Unix seconds; 0 means the provider advertised no expiry
	// (docs/modules/oauth.md: that is "never expires", NOT "expired").
	ExpiresAt int64 `json:"expires_at"`
	ExpiresIn int64 `json:"expires_in"`
	// HasRefreshToken decides whether an expiry is recoverable without a
	// human. The token itself is never included.
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
		sort.Strings(ids)
	}

	now := time.Now()
	out := make([]AuthStatusWire, 0, len(ids))
	for _, id := range ids {
		row := s.authStatusOf(r.Context(), id, now)
		if one == "" && row.State == "none" {
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
	row := AuthStatusWire{Server: id, State: "none"}
	st, err := s.opts.NonRegistry.OAuth.LoadState(ctx, id)
	if err != nil {
		if !errors.Is(err, oauthflow.ErrNoState) {
			row.State = "error"
			row.Detail = err.Error()
		}
		return row
	}
	row.Issuer = st.Issuer
	row.Scope = st.Scope
	row.ExpiresAt = st.ExpiresAt
	row.ExpiresIn = secondsUntil(st.ExpiresAt, now)
	row.HasRefreshToken = st.RefreshToken != ""
	row.ClientRegistrar = st.RegistrarKind

	if _, terr := s.opts.NonRegistry.OAuth.LoadAccessToken(ctx, id); terr != nil {
		// State without a token is the registration-only shape: a login was
		// started (or the token write failed) and nothing is usable yet.
		row.State = "none"
		row.Detail = "client registration stored, no access token"
		return row
	}
	switch {
	case st.Expired(now):
		row.State = "expired"
	case st.NeedsRefresh(now):
		row.State = "expiring"
	default:
		row.State = "authorized"
	}
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
	start := time.Now()
	st, _, err := ref.Refresh(r.Context(), server)
	superseded := errors.Is(err, oauthflow.ErrRefreshSuperseded)
	ok := err == nil || superseded
	s.auditNonReg(r, server, "auth/refresh", "", ok, time.Since(start))
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
	start := time.Now()
	err := s.opts.NonRegistry.OAuth.Clear(r.Context(), server)
	s.auditNonReg(r, server, "auth/logout", "", err == nil, time.Since(start))
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
