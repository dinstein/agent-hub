package ctlapi

import (
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/dinstein/agent-hub/internal/httpbridge"
	"github.com/dinstein/agent-hub/internal/tier"
)

// GET|POST /v1/tokens, DELETE /v1/tokens/{name} — agent tokens for the
// daemon's HTTP data plane (docs/modules/controlplane.md).
//
// THE INVARIANT: the plaintext appears exactly once, in the response that
// minted it. Nothing can reproduce it afterwards — the store keeps only an
// HMAC — so TokenWire has no field for it and the listing renders the
// display prefix. This is the one deliberate exception to "never return a
// credential": the value has to leave the process once or it could not be
// given to an agent at all.
//
// A revoke KEEPS the record (the name stays reserved and the row stays
// visible) so a log line naming a token keeps resolving to exactly one
// credential. That is why the endpoint is DELETE but the row does not
// disappear.

// TokenWire is one stored agent token. No value field, and no HMAC either.
type TokenWire struct {
	Name string `json:"name"`
	// Prefix is the first characters of the plaintext, for display only.
	Prefix string `json:"prefix"`
	Tier   string `json:"tier"`
	// Servers is the allowlist: null = every server, [] = nothing.
	Servers []string `json:"servers"`
	Profile string   `json:"profile,omitempty"`
	// State is active | revoked | expired.
	State     string     `json:"state"`
	CreatedAt time.Time  `json:"created_at"`
	ExpiresAt *time.Time `json:"expires_at,omitempty"`
	RevokedAt *time.Time `json:"revoked_at,omitempty"`
}

// TokenCreateRequest is the body of POST /v1/tokens.
type TokenCreateRequest struct {
	Name string `json:"name"`
	// Tier is read | write | destructive (default read — the closed end of
	// the ladder; a token whose tier the caller forgot to state must not
	// come out able to delete things).
	Tier string `json:"tier,omitempty"`
	// Servers restricts the token: omitted/null = every server, [] = none.
	//
	// omitzero, never omitempty: this type is EXPORTED and marshalled by
	// callers, and omitempty dropped an explicit [] off the wire entirely —
	// so a request for a token scoped to no servers arrived as one scoped to
	// every server. Fail-open, on a credential.
	Servers []string `json:"servers,omitzero"`
	Profile string   `json:"profile,omitempty"`
	// ExpiresInSeconds sets a hard deadline (0 = never expires).
	ExpiresInSeconds int64 `json:"expires_in_seconds,omitempty"`
}

// TokenCreatedWire is the answer to POST /v1/tokens. Value is populated
// exactly once, here, and never again.
type TokenCreatedWire struct {
	Token TokenWire `json:"token"`
	Value string    `json:"value"`
}

// TokenRevokedWire is the answer to DELETE /v1/tokens/{name}.
type TokenRevokedWire struct {
	Name      string    `json:"name"`
	Prefix    string    `json:"prefix"`
	RevokedAt time.Time `json:"revoked_at"`
}

// handleTokensList implements GET /v1/tokens: prefixes and metadata only.
func (s *Server) handleTokensList(w http.ResponseWriter, r *http.Request) {
	reqID := requestIDFrom(r.Context())
	toks, err := s.opts.NonRegistry.Tokens.List()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, CodeInternal,
			"listing agent tokens failed: "+err.Error(), "", reqID)
		return
	}
	now := time.Now()
	out := make([]TokenWire, 0, len(toks))
	for _, t := range toks {
		out = append(out, tokenWire(t, now))
	}
	writeOK(w, http.StatusOK, out)
}

// handleTokenCreate implements POST /v1/tokens.
func (s *Server) handleTokenCreate(w http.ResponseWriter, r *http.Request) {
	reqID := requestIDFrom(r.Context())
	var req TokenCreateRequest
	if err := decodeBody(r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, CodeBadRequest,
			"decoding token request: "+err.Error(), "", reqID)
		return
	}
	if strings.TrimSpace(req.Name) == "" {
		writeErr(w, http.StatusBadRequest, CodeBadRequest,
			"name is required", "a token's name is its primary key", reqID)
		return
	}
	want := tier.Tier(req.Tier)
	if want == "" {
		want = tier.Read
	}
	spec := httpbridge.CreateSpec{
		Name:    req.Name,
		Tier:    want,
		Servers: req.Servers,
		Profile: req.Profile,
	}
	if req.ExpiresInSeconds > 0 {
		spec.ExpiresAt = time.Now().Add(time.Duration(req.ExpiresInSeconds) * time.Second)
	}

	tok, value, err := s.opts.NonRegistry.Tokens.Create(r.Context(), spec)
	if err != nil {
		s.writeTokenError(w, r, err)
		return
	}
	writeOK(w, http.StatusCreated, TokenCreatedWire{
		Token: tokenWire(tok, time.Now()),
		Value: value,
	})
}

// handleTokenRevoke implements DELETE /v1/tokens/{name}.
func (s *Server) handleTokenRevoke(w http.ResponseWriter, r *http.Request, name string) {
	tok, err := s.opts.NonRegistry.Tokens.Revoke(r.Context(), name, time.Now())
	if err != nil {
		s.writeTokenError(w, r, err)
		return
	}
	writeOK(w, http.StatusOK, TokenRevokedWire{
		Name: tok.Name, Prefix: tok.Prefix, RevokedAt: tok.RevokedAt,
	})
}

// writeTokenError maps a token-store failure onto the wire. An unknown name
// is the uniform 404; everything the operator can resolve by choosing
// different inputs is 400 or 409, never 500.
func (s *Server) writeTokenError(w http.ResponseWriter, r *http.Request, err error) {
	reqID := requestIDFrom(r.Context())
	switch {
	case errors.Is(err, httpbridge.ErrTokenNotFound):
		writeNotFound(w, r)
	case errors.Is(err, httpbridge.ErrAlreadyRevoked):
		// Not a 404: the row exists and is visible in the listing. Telling
		// the caller "no such token" would send them looking for a typo.
		writeErr(w, http.StatusConflict, CodeConflict, err.Error(), "", reqID)
	case errors.Is(err, httpbridge.ErrTokenExists):
		writeErr(w, http.StatusConflict, CodeConflict, err.Error(),
			"revoked tokens keep their name; pick another one", reqID)
	case errors.Is(err, httpbridge.ErrTooManyTokens):
		writeErr(w, http.StatusConflict, CodeConflict, err.Error(),
			"revoke tokens you no longer use", reqID)
	case errors.Is(err, httpbridge.ErrInvalidName), errors.Is(err, httpbridge.ErrInvalidTier):
		writeErr(w, http.StatusBadRequest, CodeBadRequest, err.Error(), "", reqID)
	default:
		writeErr(w, http.StatusInternalServerError, CodeInternal, err.Error(), "", reqID)
	}
}

// tokenWire projects a stored token for output. The HMAC never crosses this
// boundary — there is no field for it.
func tokenWire(t httpbridge.Token, now time.Time) TokenWire {
	out := TokenWire{
		Name:      t.Name,
		Prefix:    t.Prefix,
		Tier:      string(t.Tier),
		Servers:   t.Servers,
		Profile:   t.Profile,
		State:     t.State(now),
		CreatedAt: t.CreatedAt,
	}
	if !t.ExpiresAt.IsZero() {
		e := t.ExpiresAt
		out.ExpiresAt = &e
	}
	if !t.RevokedAt.IsZero() {
		rv := t.RevokedAt
		out.RevokedAt = &rv
	}
	return out
}
