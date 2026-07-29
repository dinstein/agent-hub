package httpbridge

import (
	"crypto/subtle"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/dinstein/agent-hub/internal/tier"
)

// CallerKind distinguishes the credential that authenticated a request.
type CallerKind string

const (
	// CallerAdmin is the operator's own token (AGENTHUB_HTTP_TOKEN
	// semantics): full tier, every server, no profile pin.
	CallerAdmin CallerKind = "admin"
	// CallerAgent is an agent token from the Store.
	CallerAgent CallerKind = "agent"
	// CallerLoopback is the --insecure-loopback escape hatch: no credential
	// was presented and none is configured. It is recorded as its own kind
	// so audit can tell "the operator disabled authentication" from "the
	// operator's token was used".
	CallerLoopback CallerKind = "loopback"
)

// Caller is the authenticated identity behind one HTTP request. It is the
// bridge between this package and the governance chain: Tier feeds
// pipeline.CallRequest.CallerTier, Servers narrows visibility, and Profile
// is the sixth constraint source of the scope intersection (docs/architecture.md §9).
type Caller struct {
	Kind CallerKind
	// Token is the agent token's NAME (never its value); empty for admin
	// and loopback callers.
	Token string
	Tier  tier.Tier
	// Servers is the allowlist; nil = every server.
	Servers []string
	// Profile is the pinned profile, "" = no pin.
	Profile string
}

// AllowsServer reports whether this caller may reach serverID.
func (c *Caller) AllowsServer(serverID string) bool {
	if c == nil {
		return false // no identity, no access (fail-closed)
	}
	if c.Servers == nil {
		return true
	}
	for _, s := range c.Servers {
		if s == ServerWildcard || s == serverID {
			return true
		}
	}
	return false
}

// Identity is the owner fingerprint of a caller: the value an established
// session is pinned to, and the key the HTTP data plane uses to deduplicate
// per-credential gateways. Both consumers compare the WHOLE fingerprint, not
// just the token name (docs/canonical.md §2 "HTTP session owner is validated as a whole") — a
// token whose tier or allowlist was narrowed since must not keep riding the
// old session or gateway at its old authority.
func (c *Caller) Identity() string {
	if c == nil {
		return ""
	}
	return string(c.Kind) + "\x00" + c.Token + "\x00" + string(c.Tier) +
		"\x00" + strings.Join(c.Servers, ",") + "\x00" + c.Profile
}

// ErrUnauthorized is the single authentication failure. It is deliberately
// undifferentiated: unknown token, revoked token, expired token and missing
// token are one answer, so the endpoint cannot be used to probe which
// credentials exist.
var ErrUnauthorized = errors.New("httpbridge: unauthorized")

// Authenticator resolves a request to a Caller.
type Authenticator struct {
	// AdminToken is the operator's bearer (AGENTHUB_HTTP_TOKEN). Empty
	// means no admin token is configured.
	AdminToken string
	// Tokens is the agent-token store. nil means agent tokens are not
	// available in this assembly.
	Tokens *Store
	// InsecureLoopback allows unauthenticated requests. It exists only for
	// the documented escape hatch and must never be set by default.
	InsecureLoopback bool
	// Now overrides the clock (tests).
	Now func() time.Time
}

func (a *Authenticator) now() time.Time {
	if a.Now != nil {
		return a.Now()
	}
	return time.Now()
}

// Authenticate resolves the request's bearer token.
//
// Dispatch is by PREFIX, and it is exclusive in both directions: a bearer
// starting with "agt_" is only ever looked up in the store, anything else is
// only ever compared against the admin token. Without that exclusivity a
// caller could probe the store by presenting admin-shaped candidates, and an
// admin token that happened to start with the agent prefix would be
// unusable.
//
// Failure direction: everything that is not a positive match returns
// ErrUnauthorized. The one exception is InsecureLoopback, which only applies
// when NO credential was presented — an invalid token stays invalid even
// with the escape hatch on, because a caller that tried to authenticate has
// told us which identity it claims.
func (a *Authenticator) Authenticate(r *http.Request) (*Caller, error) {
	bearer := bearerOf(r)
	if bearer == "" {
		if a.InsecureLoopback {
			return &Caller{Kind: CallerLoopback, Tier: tier.Destructive}, nil
		}
		return nil, ErrUnauthorized
	}
	if looksLikeAgentToken(bearer) {
		if a.Tokens == nil {
			return nil, ErrUnauthorized
		}
		tok, ok, err := a.Tokens.Lookup(bearer, a.now())
		if err != nil {
			// A store we cannot read is not a store that grants access.
			return nil, ErrUnauthorized
		}
		if !ok {
			return nil, ErrUnauthorized
		}
		return &Caller{
			Kind:    CallerAgent,
			Token:   tok.Name,
			Tier:    tok.Tier,
			Servers: tok.Servers,
			Profile: tok.Profile,
		}, nil
	}
	if a.AdminToken == "" {
		return nil, ErrUnauthorized
	}
	if subtle.ConstantTimeCompare([]byte(bearer), []byte(a.AdminToken)) != 1 {
		return nil, ErrUnauthorized
	}
	return &Caller{Kind: CallerAdmin, Tier: tier.Destructive}, nil
}

// bearerOf extracts the bearer credential from the Authorization header.
// The scheme match is case-insensitive per RFC 7235; the value is not
// trimmed beyond surrounding spaces, because a token is exactly its bytes.
func bearerOf(r *http.Request) string {
	if r == nil {
		return ""
	}
	h := r.Header.Get("Authorization")
	if h == "" {
		return ""
	}
	const scheme = "bearer "
	if len(h) < len(scheme) || !strings.EqualFold(h[:len(scheme)], scheme) {
		return ""
	}
	return strings.TrimSpace(h[len(scheme):])
}
