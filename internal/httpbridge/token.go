package httpbridge

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"
	"time"

	"github.com/dinstein/agent-hub/internal/tier"
)

// Token shape (docs/architecture.md §9). Frozen: the prefix is what dispatches an
// incoming bearer between the admin token and the agent-token store, and the
// display prefix is what `agenthub token ls` prints.
const (
	// TokenPrefix marks an agent token. A bearer that starts with it is
	// looked up in the store and NEVER compared against the admin token.
	TokenPrefix = "agt_"
	// tokenBytes is the entropy behind one token: 32 bytes = 64 hex chars.
	tokenBytes = 32
	// DisplayPrefixLen is how many leading characters of the plaintext are
	// kept for display. Long enough to identify a token in a list, far too
	// short to guess the remaining 52 hex characters.
	DisplayPrefixLen = 12
)

// ServerWildcard in a token's server allowlist means "every server", the
// explicit spelling of the nil allowlist.
const ServerWildcard = "*"

// Token is one stored agent token. The plaintext is NOT here and never was:
// only its HMAC is persisted, so a stolen tokens.json cannot be replayed and
// cannot be brute-forced offline without also stealing the key file.
type Token struct {
	// Name is the human handle and the primary key (unique across the
	// store, including revoked entries).
	Name string `json:"name"`
	// Hash is hex(HMAC-SHA256(key, plaintext)).
	Hash string `json:"hash"`
	// Prefix is the first DisplayPrefixLen characters of the plaintext,
	// for display only.
	Prefix string `json:"prefix"`
	// Tier is the operation tier this credential may reach
	// (tier.Read | TierWrite | TierDestructive).
	Tier tier.Tier `json:"tier"`
	// Servers is the per-token server allowlist: nil = every server,
	// ["*"] = every server explicitly, otherwise exactly those server IDs.
	// An EMPTY (non-nil) list allows nothing — the closed direction, and the
	// reason this field is not `omitempty`.
	Servers []string `json:"servers"`
	// Profile pins this token to one profile permanently. It becomes the
	// SIXTH constraint source of the scope intersection (docs/architecture.md §9;
	// chapter 4 owns the other five) — the assembling process feeds it into
	// the resolver. "" = no pin.
	Profile   string    `json:"profile,omitempty"`
	CreatedAt time.Time `json:"createdAt"`
	// ExpiresAt is the hard deadline; the zero time means "never expires".
	ExpiresAt time.Time `json:"expiresAt,omitzero"`
	// RevokedAt is set by Revoke. A revoked token is KEPT (the name stays
	// taken and the row stays visible in `token ls`) so that an operator
	// reading an audit record can still resolve the name that produced it.
	RevokedAt time.Time `json:"revokedAt,omitzero"`
}

// Revoked reports whether the token has been revoked.
func (t Token) Revoked() bool { return !t.RevokedAt.IsZero() }

// Expired reports whether the token's deadline has passed at now.
func (t Token) Expired(now time.Time) bool {
	return !t.ExpiresAt.IsZero() && !now.Before(t.ExpiresAt)
}

// Active reports whether the token may authenticate at now: not revoked,
// not expired, and carrying a tier the ladder recognises. The tier check is
// part of "active" on purpose — a stored tier this binary does not know
// (hand-edited file, downgrade after a future tier is added) must deny, not
// default.
func (t Token) Active(now time.Time) bool {
	return !t.Revoked() && !t.Expired(now) && tier.Valid(t.Tier)
}

// State renders the token's lifecycle state for display: active, revoked or
// expired. Revocation wins over expiry — it is the deliberate act.
func (t Token) State(now time.Time) string {
	switch {
	case t.Revoked():
		return "revoked"
	case t.Expired(now):
		return "expired"
	case !tier.Valid(t.Tier):
		return "invalid"
	default:
		return "active"
	}
}

// AllowsServer reports whether this token may reach serverID.
//
// Failure direction: a nil allowlist means "no restriction was configured"
// and allows everything; an empty non-nil allowlist allows nothing. That
// nil-vs-empty distinction is the same three-state the registry's
// ToolSelector uses, and it is why Servers is serialized without omitempty.
func (t Token) AllowsServer(serverID string) bool {
	if t.Servers == nil {
		return true
	}
	for _, s := range t.Servers {
		if s == ServerWildcard || s == serverID {
			return true
		}
	}
	return false
}

// mint generates one token plaintext: the frozen prefix plus 64 lowercase
// hex characters from crypto/rand. A failure of the system CSPRNG is fatal
// to the operation — there is no weaker fallback.
func mint() (string, error) {
	buf := make([]byte, tokenBytes)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("httpbridge: generating token: %w", err)
	}
	return TokenPrefix + hex.EncodeToString(buf), nil
}

// looksLikeAgentToken reports whether a bearer value is shaped like an agent
// token. It decides DISPATCH only (store lookup vs admin comparison), never
// validity: a well-shaped token that is not in the store still fails.
func looksLikeAgentToken(bearer string) bool {
	return strings.HasPrefix(bearer, TokenPrefix)
}

// hashToken computes hex(HMAC-SHA256(key, plaintext)).
//
// HMAC rather than a bare SHA-256 is the point (docs/architecture.md §9 "defend against
// offline credential stuffing"): the digest is unforgeable without the key file, so an attacker who
// exfiltrates tokens.json alone cannot test candidate tokens offline. The
// key lives in <data>/.token_key at 0600, next to but separate from the
// token list.
func hashToken(key []byte, plaintext string) string {
	mac := hmac.New(sha256.New, key)
	_, _ = mac.Write([]byte(plaintext))
	return hex.EncodeToString(mac.Sum(nil))
}

// displayPrefix returns the leading DisplayPrefixLen characters of a
// plaintext token (the whole string when it is shorter).
func displayPrefix(plaintext string) string {
	if len(plaintext) <= DisplayPrefixLen {
		return plaintext
	}
	return plaintext[:DisplayPrefixLen]
}
