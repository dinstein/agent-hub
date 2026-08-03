package api

import (
	"context"
	"net/http"
	"net/url"
	"time"
)

// Caller tiers of an agent token: the operation class the credential may
// reach. Frozen wire values.
const (
	// TierRead may call read-classified tools only.
	TierRead = "read"
	// TierWrite adds write-classified tools.
	TierWrite = "write"
	// TierDestructive adds destructive-classified tools.
	TierDestructive = "destructive"
)

// TokenServerWildcard in a token's server allowlist means "every server":
// the explicit spelling of the nil allowlist.
const TokenServerWildcard = "*"

// Token lifecycle states, as reported by Token.State.
const (
	// TokenStateActive: usable now.
	TokenStateActive = "active"
	// TokenStateRevoked: deliberately withdrawn. Revocation wins over
	// expiry — it is the deliberate act.
	TokenStateRevoked = "revoked"
	// TokenStateExpired: the deadline passed.
	TokenStateExpired = "expired"
	// TokenStateInvalid: the stored tier is one this daemon does not know.
	// It denies rather than defaulting — an unknown tier must never fall
	// back to a permissive one.
	TokenStateInvalid = "invalid"
)

// Token is one stored agent token as listed.
//
// RED LINE: there is no value field and no hash field. The plaintext is
// printed exactly once, by Create, and is never recoverable afterwards — the
// daemon keeps only its HMAC. Prefix (12 characters) identifies a row in a
// list and reveals nothing usable.
type Token struct {
	Name   string `json:"name"`
	Prefix string `json:"prefix"`
	// Tier is one of the Tier* constants.
	Tier string `json:"tier"`
	// Servers is the per-token server allowlist: null = every server,
	// ["*"] = every server explicitly, otherwise exactly those ids. An
	// EMPTY (non-null) list allows NOTHING — the closed direction, and the
	// reason this field has no omitempty.
	Servers []string `json:"servers"`
	// Profile pins the token to one profile permanently, becoming an extra
	// constraint source of the scope intersection. "" = no pin.
	Profile string `json:"profile,omitempty"`
	// State is one of the TokenState* constants.
	State     string     `json:"state"`
	CreatedAt time.Time  `json:"created_at"`
	ExpiresAt *time.Time `json:"expires_at,omitempty"`
	RevokedAt *time.Time `json:"revoked_at,omitempty"`
}

// TokenSpec mints one agent token.
type TokenSpec struct {
	Name string `json:"name"`
	// Tier is one of the Tier* constants. An empty tier selects TierRead,
	// the CLOSED end of the ladder: a token whose grade the caller forgot
	// to state must not come out able to delete things.
	Tier string `json:"tier,omitempty"`
	// Servers is the allowlist with the same three states as Token.Servers:
	// null = every server, [] = nothing, [...] = exactly those. No
	// omitempty, so the empty (closed) list survives the wire — with it,
	// "this token may reach nothing" would arrive as "every server".
	Servers []string `json:"servers"`
	Profile string   `json:"profile,omitempty"`
	// ExpiresInSeconds sets a hard deadline; 0 means "never expires".
	ExpiresInSeconds int64 `json:"expires_in_seconds,omitempty"`
}

// TokenCreated is the answer to Create, and the ONLY place a token value
// ever appears.
//
// A frontend must treat Value as write-only-to-the-user: show it once, offer
// a copy button, say plainly that closing the dialog loses it forever, and
// never persist it — agenthub itself cannot print it again.
type TokenCreated struct {
	Token Token  `json:"token"`
	Value string `json:"value"`
}

// TokenRevoked is the answer to Revoke.
type TokenRevoked struct {
	Name      string    `json:"name"`
	Prefix    string    `json:"prefix"`
	RevokedAt time.Time `json:"revoked_at"`
}

// TokensService manages the agent tokens of the daemon's HTTP data plane.
//
// These calls carry no expectedGeneration: the token store is not the
// registry, so there is no shared document to lose a compare-and-swap
// against.
type TokensService struct{ c *Client }

// List returns every stored token — prefixes and metadata only. Revoked rows
// are KEPT: the name stays reserved and an operator reading a ledger record
// can still resolve the name that produced it.
func (s *TokensService) List(ctx context.Context) ([]Token, error) {
	var out []Token
	if err := s.c.do(ctx, http.MethodGet, "/tokens", nil, nil, &out); err != nil {
		return nil, err
	}
	return out, nil
}

// Create mints a token and returns its plaintext exactly once.
func (s *TokensService) Create(ctx context.Context, spec TokenSpec) (TokenCreated, error) {
	var out TokenCreated
	err := s.c.do(ctx, http.MethodPost, "/tokens", nil, spec, &out)
	return out, err
}

// Revoke withdraws one token. Existing sessions stop at their next request:
// the check is per request, not per connection.
func (s *TokensService) Revoke(ctx context.Context, name string) (TokenRevoked, error) {
	var out TokenRevoked
	err := s.c.do(ctx, http.MethodDelete, "/tokens/"+url.PathEscape(name), nil, nil, &out)
	return out, err
}
