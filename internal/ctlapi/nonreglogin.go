package ctlapi

import (
	"context"
	"errors"
	"net/http"

	"github.com/dinstein/agent-hub/internal/oauthflow"
	"github.com/dinstein/agent-hub/internal/oauthlogin"
	"github.com/dinstein/agent-hub/internal/registry"
)

// POST /v1/auth/{server}/login, GET /v1/logins/{id}, DELETE /v1/logins/{id}.
//
// This file walks back the scope limit the auth endpoints were written under:
// "an interactive login is not on this API". The reason it was there is real —
// a login is far too long for one request/response — and the reason it did not
// survive is more expensive: with no login here, every graphical frontend's
// answer to a server that needs authorizing was a dialog telling the user to
// go and run a terminal command, inside an application whose whole premise is
// that clients never handle credentials.
//
// What makes it affordable is that NO PROTOCOL CODE LIVES HERE. The flow is
// internal/oauthflow's, the same one `agenthub auth login` drives;
// internal/oauthlogin wraps it in a session so it can be started, polled and
// cancelled across separate requests. This file is routing, registry lookup
// and rendering.
//
// A LOGIN SESSION IS ITS OWN RESOURCE, at /v1/logins/{id} and not under
// /v1/auth/{server}/... That is not tidiness: as a sub-resource it would sit
// at /v1/auth/{server}/{id}, one segment shape away from
// /v1/auth/{server}/refresh, and a server whose id happened to be "logins"
// would decide which route won by the order the cases are written in. A
// separate top-level path has no such tie to break.
//
// NO CREDENTIAL IS RENDERED, same as everywhere else on this plane. The wire
// type carries the user code (the string a human types into the provider's
// site) and never the device code polled with, never an authorization code
// and never a token.

// LoginSessions is the ctlapi face of *oauthlogin.Manager.
type LoginSessions interface {
	Start(req oauthlogin.Request) (oauthlogin.Session, error)
	Get(id string) (oauthlogin.Session, error)
	Cancel(id string) (oauthlogin.Session, error)
}

// AuthLoginWire is one login session as this plane reports it. It mirrors
// api.AuthLogin field for field.
type AuthLoginWire struct {
	ID     string `json:"id"`
	Server string `json:"server"`
	// Phase is pending | complete | failed.
	Phase string `json:"phase"`
	// Mode is loopback | device, empty until the flow has chosen.
	Mode string `json:"mode,omitempty"`
	// AuthorizationURL is the page the CALLER opens. The daemon deliberately
	// does not: it may be headless, may have been started by a service
	// manager with no session to draw into, and may not be where the user is.
	AuthorizationURL        string `json:"authorization_url,omitempty"`
	VerificationURI         string `json:"verification_uri,omitempty"`
	VerificationURIComplete string `json:"verification_uri_complete,omitempty"`
	UserCode                string `json:"user_code,omitempty"`
	// Deadline is when the SESSION gives up (Unix seconds), not when the
	// credential expires.
	Deadline int64 `json:"deadline,omitempty"`
	// Issuer, Scope, TokenExpiresAt and HasRefreshToken are set on success.
	// TokenExpiresAt 0 means the provider advertised no expiry — "never
	// expires", not "expired" (docs/status/oauth.md).
	Issuer          string `json:"issuer,omitempty"`
	Scope           string `json:"scope,omitempty"`
	TokenExpiresAt  int64  `json:"token_expires_at,omitempty"`
	HasRefreshToken bool   `json:"has_refresh_token,omitempty"`
	// Error and Hint are set on failure.
	Error string `json:"error,omitempty"`
	Hint  string `json:"hint,omitempty"`
}

// loginWire renders a session.
//
// A FAILED SESSION IS A 200. The read succeeded; what failed is the thing it
// describes, and the caller polling it needs the reason far more than it needs
// an HTTP status to branch on. Only an id that names no session at all is a
// 404.
func loginWire(s oauthlogin.Session) AuthLoginWire {
	out := AuthLoginWire{
		ID:                      s.ID,
		Server:                  s.Server,
		Phase:                   string(s.Phase),
		Mode:                    s.Mode,
		AuthorizationURL:        s.AuthorizationURL,
		VerificationURI:         s.VerificationURI,
		VerificationURIComplete: s.VerificationURIComplete,
		UserCode:                s.UserCode,
		Issuer:                  s.Issuer,
		Scope:                   s.Scope,
		TokenExpiresAt:          s.ExpiresAt,
		HasRefreshToken:         s.HasRefreshToken,
	}
	if !s.Deadline.IsZero() {
		out.Deadline = s.Deadline.Unix()
	}
	if s.Err != nil {
		out.Error = s.Err.Error()
		out.Hint = loginHint(s.Err)
	}
	return out
}

// loginHint turns a flow failure into the next thing to try.
//
// oauthflow already computes a suggestion per error type and it is the one the
// CLI prints; carrying it through means the two surfaces answer the same
// failure with the same sentence instead of drifting into two vocabularies for
// one problem.
//
// CANCELLATION AND EXPIRY ARE TESTED FIRST, and that order is the whole point
// of this function rather than an accident of it. A cancelled wait surfaces
// wrapped in a FlowError of type authorization — the flow cannot tell "the
// user gave up" from "the consent screen was never completed" — whose stock
// suggestion is "use --manual on a host without a browser". On this path that
// is wrong twice: nobody cancelled because they lacked a browser, and manual
// mode is unreachable here anyway (docs/status/oauth.md — Paste is nil, so
// SelectMode can never choose it). The specific fact outranks the generic one.
func loginHint(err error) string {
	switch {
	case errors.Is(err, context.Canceled):
		return "the sign-in was cancelled; nothing was stored"
	case errors.Is(err, context.DeadlineExceeded):
		return "the sign-in was not finished in time; nothing was stored, so it can simply be started again"
	}
	var fe *oauthflow.FlowError
	if errors.As(err, &fe) && fe.Suggestion != "" {
		return fe.Suggestion
	}
	return ""
}

// handleAuthLoginStart implements POST /v1/auth/{server}/login.
//
// It answers as soon as the session exists, BEFORE there is anything to show:
// choosing between the device and loopback flows needs the authorization
// server's metadata, which needs a network round trip. Holding the response
// until then would put a discovery timeout inside a button press. The caller
// polls.
func (s *Server) handleAuthLoginStart(w http.ResponseWriter, r *http.Request, server string) {
	reqID := requestIDFrom(r.Context())
	logins := s.opts.NonRegistry.Logins
	if logins == nil {
		writeErr(w, http.StatusNotFound, CodeNotFound,
			"this daemon cannot run an interactive login",
			"run 'agenthub auth login "+server+"' instead", reqID)
		return
	}

	doc, ok := s.opts.Registry.Snapshot().Servers.V.Servers[server]
	if !ok {
		// Uniform 404: an unknown server reads exactly like an unknown route.
		writeNotFound(w, r)
		return
	}
	entry := doc.V

	req := oauthlogin.Request{
		ServerID:    server,
		ResourceURL: entry.URL,
		// The loopback carve-out follows the SERVER's own provenance, exactly
		// as `auth login` does: a server declared local may legitimately have
		// an authorization server on this machine too. Every other entry stays
		// SSRF-screened, and there is deliberately no request field that could
		// ask for the exemption — it is a property of the stored entry, not of
		// whoever is calling.
		AllowLoopback: entry.Provenance == registry.ProvenanceLocal,
	}
	if h := entry.OAuth; h != nil {
		req.Issuer = h.Issuer
		req.Scopes = h.Scopes
		req.ResourceMetadataURL = h.ResourceMetadataURL
		req.AuthorizationEndpoint = h.AuthorizationEndpoint
	}
	if req.ResourceURL == "" && req.Issuer == "" {
		writeErr(w, http.StatusBadRequest, CodeBadRequest,
			"server "+server+" has no url to authorize against",
			"give the entry a url, or an oauth issuer hint", reqID)
		return
	}
	// Re-bind the port a previous login registered, if any. Many providers
	// match the redirect_uri byte for byte, so a fresh random port is refused
	// by exactly the providers that were hardest to get working once.
	// A vault that cannot be read is not fatal here: the login simply takes a
	// new port, which is the pre-existing behaviour for a first login.
	if st, err := s.opts.NonRegistry.OAuth.LoadState(r.Context(), server); err == nil && st != nil {
		req.CallbackPort = st.CallbackPort
	}

	sess, err := logins.Start(req)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, CodeInternal,
			"starting a login for "+server+" failed: "+err.Error(), "", reqID)
		return
	}
	writeOK(w, http.StatusAccepted, loginWire(sess))
}

// handleLoginGet implements GET /v1/logins/{id}.
func (s *Server) handleLoginGet(w http.ResponseWriter, r *http.Request, id string) {
	sess, err := s.opts.NonRegistry.Logins.Get(id)
	if err != nil {
		// Includes a session that finished long enough ago to be swept. The
		// uniform 404 is right: there is nothing to report, and saying which
		// of the two it was would tell an unauthenticated prober that an id
		// once existed.
		writeNotFound(w, r)
		return
	}
	writeOK(w, http.StatusOK, loginWire(sess))
}

// handleLoginCancel implements DELETE /v1/logins/{id}.
//
// Cancelling stops the WAIT, not the authorization. A consent the user already
// granted at the provider stays granted, and a login that had already stored a
// credential keeps it — reporting that as abandoned would be the more damaging
// answer of the two.
func (s *Server) handleLoginCancel(w http.ResponseWriter, r *http.Request, id string) {
	sess, err := s.opts.NonRegistry.Logins.Cancel(id)
	if err != nil {
		writeNotFound(w, r)
		return
	}
	writeOK(w, http.StatusOK, loginWire(sess))
}
