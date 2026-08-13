package oauthflow

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/dinstein/agent-hub/internal/secrets"
)

// State is the __oauth_state__ vault entry: everything needed to refresh a
// token and to reproduce an authorization request, minus the access token
// itself (which lives in __http_auth__).
//
// JSON member names are storage format. Adding members is fine; renaming or
// repurposing one orphans every stored credential, so don't.
type State struct {
	// TokenEndpoint is where refreshes go. Persisted rather than
	// rediscovered so a refresh needs zero network round trips beyond the
	// refresh itself, and so a provider that breaks its discovery document
	// cannot lock out an already-authorized server.
	TokenEndpoint string `json:"token_endpoint"`
	// Issuer is the AS identity, for diagnostics and re-discovery.
	Issuer string `json:"issuer,omitempty"`
	// ClientID and ClientSecret are the client credentials. Persisted with
	// the token because dynamic registrations are per-installation and
	// re-registering on every start both spams the provider and, on
	// providers that rate-limit DCR, eventually fails.
	//
	// Sharing one vault entry with the refresh token is what makes a
	// registration update and a token update the same read-modify-write:
	// Save marshals this whole struct and writes it as one value, so the
	// two cannot be updated independently. Splitting registration into a
	// third vault key would close that window and is additive — it does not
	// touch the state-before-token ordering invariant below. Recorded under
	// "Credential lifecycle" in docs/modules/oauth.md, which describes the
	// two-entry model as it stands, not the split.
	ClientID     string `json:"client_id"`
	ClientSecret string `json:"client_secret,omitempty"`
	// RegistrarKind records where the client credentials came from
	// ("dcr", "preconfigured", ...) so a failure can say whether
	// re-registration is even possible.
	RegistrarKind string `json:"registrar_kind,omitempty"`
	// RefreshToken is the rotated-on-use credential. This field is the
	// reason Save writes state first.
	RefreshToken string `json:"refresh_token,omitempty"`
	// Resource is the RFC 8707 indicator this token is bound to. Refreshes
	// must send the same value or the AS may mint a token for a different
	// audience.
	Resource string `json:"resource,omitempty"`
	// Scope is the granted scope, kept for reporting and for carry-forward
	// when a refresh response omits it (RFC 6749 §5.1: omitted means
	// unchanged).
	//
	// It is deliberately NOT sent back on a refresh — refreshNow leaves
	// RefreshRequest.Scope empty, which is what makes the AS reuse the
	// original grant. This said "echoed back on refresh when the provider
	// requires it", which no code does and which reads as an invitation to
	// add it; docs/modules/oauth.md records omitting it as the ruling.
	Scope string `json:"scope,omitempty"`
	// RedirectURI and CallbackPort are persisted because many providers
	// require an EXACT redirect_uri match; a dynamically chosen loopback
	// port must therefore survive a restart (docs/modules/oauth.md).
	RedirectURI  string `json:"redirect_uri,omitempty"`
	CallbackPort int    `json:"callback_port,omitempty"`
	// GrantRevokedAt is when the authorization server REFUSED this record's
	// refresh grant, in Unix seconds; 0 means it has not. It is the one fact
	// about a credential that cannot be re-derived from the credential: a
	// dead refresh token and a live one are the same bytes, and the only way
	// to tell them apart is to have been told, once, by the provider. Losing
	// it means asking again, which is what this field exists to stop.
	//
	// It is never set from a timeout, a 5xx or a network error (terminalGrant),
	// and it is cleared by any successful save — so a re-login recovers
	// structurally rather than by anyone remembering to reset it.
	GrantRevokedAt int64 `json:"grant_revoked_at,omitempty"`
	// GrantRevokedReason is the authorization server's own error code and
	// description for that refusal, usually the only provider-specific
	// diagnosis anyone gets ("refresh token expired" vs "consent withdrawn"
	// vs "client deleted"). It carries no credential: these are the same
	// class of string FlowError.Error already interpolates.
	GrantRevokedReason string `json:"grant_revoked_reason,omitempty"`
	// IssuedAt / ExpiresAt are Unix seconds. ExpiresAt == 0 means NEVER
	// EXPIRES (docs/modules/oauth.md) — several providers issue no expires_in and
	// treating that as "expired" produces a permanent refresh storm.
	IssuedAt  int64 `json:"issued_at"`
	ExpiresAt int64 `json:"expires_at"`
	// TokenType is normally "Bearer".
	TokenType string `json:"token_type,omitempty"`
}

// minLifetimeForGrace is the threshold below which the refresh grace is NOT
// subtracted (docs/modules/oauth.md). Subtracting 60s from a 60s token makes every
// token expired at birth and the gateway never stops refreshing.
const minLifetimeForGrace = 5 * time.Minute

// RefreshGrace is how long before expiry a proactive refresh fires.
const RefreshGrace = 60 * time.Second

// RefreshRetryBackoff is the wait after a failed proactive refresh
// (docs/modules/oauth.md).
const RefreshRetryBackoff = 15 * time.Second

// NeverExpires reports the "no expires_in" case.
func (s *State) NeverExpires() bool { return s == nil || s.ExpiresAt == 0 }

// GrantRevoked reports a record whose refresh grant the authorization server
// has refused. Such a record is NOT worthless: the access token beside it may
// still be accepted for the rest of its life, and taking it out of service
// early would turn "you will have to log in eventually" into "you have to log
// in now". What it stops is asking the provider again.
func (s *State) GrantRevoked() bool { return s != nil && s.GrantRevokedAt != 0 }

// Lifetime is the advertised token lifetime, or 0 when unknown.
func (s *State) Lifetime() time.Duration {
	if s == nil || s.ExpiresAt == 0 || s.IssuedAt == 0 || s.ExpiresAt <= s.IssuedAt {
		return 0
	}
	return time.Duration(s.ExpiresAt-s.IssuedAt) * time.Second
}

// RefreshAt returns the instant at which a proactive refresh should fire,
// and whether one is scheduled at all.
//
// The grace is applied only to tokens whose lifetime exceeds
// minLifetimeForGrace; short-lived tokens refresh exactly at expiry.
func (s *State) RefreshAt() (time.Time, bool) {
	if s.NeverExpires() {
		return time.Time{}, false
	}
	exp := time.Unix(s.ExpiresAt, 0)
	if s.Lifetime() > minLifetimeForGrace {
		return exp.Add(-RefreshGrace), true
	}
	return exp, true
}

// NeedsRefresh reports whether the token should be refreshed at now.
func (s *State) NeedsRefresh(now time.Time) bool {
	at, ok := s.RefreshAt()
	if !ok {
		return false
	}
	return !now.Before(at)
}

// Expired reports whether the token is past its hard expiry (no grace).
func (s *State) Expired(now time.Time) bool {
	if s.NeverExpires() {
		return false
	}
	return now.After(time.Unix(s.ExpiresAt, 0))
}

// Store persists OAuth credentials into the secrets vault under the
// composite key (serverID, secrets.DefaultScope).
//
// Two entries per remote server (docs/modules/oauth.md):
//
//	__oauth_state__  State as JSON — includes the refresh token
//	__http_auth__    the access token, verbatim
//
// The access token shares __http_auth__ with hand-pasted tokens on purpose:
// the downstream connector reads one key regardless of how the credential
// was obtained.
type Store struct {
	sec secrets.Store
}

// NewStore wraps a secrets.Store.
func NewStore(sec secrets.Store) *Store { return &Store{sec: sec} }

// stateRef and tokenRef delegate to internal/secrets rather than spelling the
// composite key again. That package's wiring.go exists to hold the
// (ServerID, Scope, Key) shape in exactly one place, for the reason its header
// gives: a caller that builds the Ref literal is one refactor away from
// forgetting the scope component and silently reading a different entry.
//
// These two WERE that caller. The helpers had no production callers at all —
// only tests — so the vault key was computed by one path in the tests and
// another in production. A change to the helpers would have been followed by
// the tests and not by the code, and the suite would have stayed green while
// the two drifted.
func stateRef(serverID string) secrets.Ref { return secrets.OAuthStateRef(serverID) }

func tokenRef(serverID string) secrets.Ref { return secrets.HTTPAuthRef(serverID) }

// LoadState reads the OAuth state. Returns ErrNoState when absent.
func (s *Store) LoadState(ctx context.Context, serverID string) (*State, error) {
	raw, ok, err := s.sec.Get(ctx, stateRef(serverID))
	if err != nil {
		return nil, newFlowError(ErrorTypePersistence, err)
	}
	if !ok || strings.TrimSpace(raw) == "" {
		return nil, newFlowError(ErrorTypePersistence, fmt.Errorf("%w: server %q", ErrNoState, serverID))
	}
	var st State
	if err := json.Unmarshal([]byte(raw), &st); err != nil {
		// Fail-closed and loud: a corrupt state entry must not be silently
		// treated as "no state", which would trigger a fresh registration
		// and orphan a working refresh token.
		return nil, newFlowError(ErrorTypePersistence,
			fmt.Errorf("oauthflow: stored oauth state for %q is corrupt: %w", serverID, err))
	}
	return &st, nil
}

// LoadAccessToken reads the access token. Returns ErrNoToken when absent.
func (s *Store) LoadAccessToken(ctx context.Context, serverID string) (string, error) {
	tok, ok, err := s.sec.Get(ctx, tokenRef(serverID))
	if err != nil {
		return "", newFlowError(ErrorTypePersistence, err)
	}
	if !ok || strings.TrimSpace(tok) == "" {
		return "", newFlowError(ErrorTypePersistence, fmt.Errorf("%w: server %q", ErrNoToken, serverID))
	}
	return tok, nil
}

// Load reads both entries.
//
// Failure direction (docs/modules/oauth.md): when the state exists but the access
// token does not — the shape a DCR-credentials-only record has — the error
// is ErrNoToken, never (state, "", nil). A caller handed an empty token
// would attach an empty Authorization header, get a 401, "refresh", and
// loop forever.
func (s *Store) Load(ctx context.Context, serverID string) (*State, string, error) {
	st, err := s.LoadState(ctx, serverID)
	if err != nil {
		return nil, "", err
	}
	tok, err := s.LoadAccessToken(ctx, serverID)
	if err != nil {
		return st, "", err
	}
	return st, tok, nil
}

// Save persists a completed authorization or refresh.
//
// ORDERING INVARIANT — state first, access token second.
//
// The refresh token rotates: an AS that issues a new one invalidates the
// old one the moment it answers. So the two possible crash windows are not
// symmetric.
//
//	state then token (this order):
//	    crash after write 1 → vault holds the NEW refresh token and the OLD
//	    access token. The old access token is merely stale; the next refresh
//	    uses the new refresh token and succeeds. Self-healing.
//
//	token then state (the reverse):
//	    crash after write 1 → vault holds the NEW access token and the OLD,
//	    already-invalidated refresh token. Nothing can refresh it. When the
//	    access token expires the server is dead until a human re-runs
//	    `auth login`. Unrecoverable.
//
// Save therefore never parallelizes the two writes and never continues past
// a failed state write.
func (s *Store) Save(ctx context.Context, serverID string, st *State, accessToken string) error {
	if st == nil {
		return newFlowError(ErrorTypePersistence, fmt.Errorf("oauthflow: refusing to save nil oauth state"))
	}
	if strings.TrimSpace(accessToken) == "" {
		return newFlowError(ErrorTypePersistence,
			fmt.Errorf("oauthflow: refusing to save an empty access token for %q", serverID))
	}
	raw, err := json.Marshal(st)
	if err != nil {
		return newFlowError(ErrorTypePersistence, err)
	}
	// Write 1: state (carries the rotated refresh token).
	if err := s.sec.Set(ctx, stateRef(serverID), string(raw)); err != nil {
		e := newFlowError(ErrorTypePersistence,
			fmt.Errorf("oauthflow: write oauth state for %q: %w", serverID, err))
		e.Suggestion = "nothing was changed; the previous credentials are intact"
		return e
	}
	// Write 2: access token. A failure here leaves the self-healing
	// combination described above.
	if err := s.sec.Set(ctx, tokenRef(serverID), accessToken); err != nil {
		e := newFlowError(ErrorTypePersistence,
			fmt.Errorf("oauthflow: write access token for %q: %w", serverID, err))
		e.Suggestion = "the refresh token was stored; retry — the next refresh will restore the access token"
		return e
	}
	return nil
}

// SaveFromToken builds the State for a token response and saves it,
// preserving fields the response does not carry.
//
// prev may be nil (first login). Two carry-forward rules matter:
//
//   - RefreshToken: a response without one does NOT clear the stored one.
//     Non-rotating providers omit refresh_token on every refresh; clearing
//     it would turn the second refresh into a re-login.
//   - Scope: an omitted scope means "unchanged" per RFC 6749 §5.1.
func (s *Store) SaveFromToken(ctx context.Context, serverID string, prev *State, next State, tok *TokenResponse, now time.Time) (*State, error) {
	if tok == nil {
		return nil, newFlowError(ErrorTypePersistence, fmt.Errorf("oauthflow: no token response to save"))
	}
	st := next
	if prev != nil {
		if st.RefreshToken == "" {
			st.RefreshToken = prev.RefreshToken
		}
		if st.Scope == "" {
			st.Scope = prev.Scope
		}
		if st.RedirectURI == "" {
			st.RedirectURI = prev.RedirectURI
			st.CallbackPort = prev.CallbackPort
		}
		if st.ClientID == "" {
			st.ClientID = prev.ClientID
			st.ClientSecret = prev.ClientSecret
			st.RegistrarKind = prev.RegistrarKind
		}
		if st.Issuer == "" {
			st.Issuer = prev.Issuer
		}
		if st.Resource == "" {
			st.Resource = prev.Resource
		}
	}
	if tok.RefreshToken != "" {
		st.RefreshToken = tok.RefreshToken
	}
	if tok.Scope != "" {
		st.Scope = tok.Scope
	}
	if tok.TokenType != "" {
		st.TokenType = tok.TokenType
	}
	// A token in hand is proof the grant behind it works, so this is the ONE
	// place a revocation is cleared. Every recovery path — `auth login`, a
	// forced refresh, a refresh that succeeds after the provider changed its
	// mind — ends here, so none of them has to remember to reset the flag.
	// It is cleared unconditionally rather than carried forward like the
	// fields above: `next` is normally a copy of the previous state, which
	// would carry the mark straight back in.
	st.GrantRevokedAt, st.GrantRevokedReason = 0, ""
	st.IssuedAt = now.Unix()
	if exp := tok.ExpiresAt(now); !exp.IsZero() {
		st.ExpiresAt = exp.Unix()
	} else {
		st.ExpiresAt = 0 // never expires
	}
	if err := s.Save(ctx, serverID, &st, tok.AccessToken); err != nil {
		return nil, err
	}
	return &st, nil
}

// Clear removes both entries.
//
// Order is the mirror of Save: the access token goes first, so a crash
// mid-clear leaves a state with no token, which Load reports as ErrNoToken
// and which the login path handles. The reverse would leave a live access
// token with no way to refresh or revoke it.
func (s *Store) Clear(ctx context.Context, serverID string) error {
	if err := s.sec.Delete(ctx, tokenRef(serverID)); err != nil {
		return newFlowError(ErrorTypePersistence, err)
	}
	if err := s.sec.Delete(ctx, stateRef(serverID)); err != nil {
		return newFlowError(ErrorTypePersistence, err)
	}
	return nil
}

// ClearClientRegistration drops only the client credentials, keeping the
// tokens, so the next login re-registers with the callback port it actually
// gets instead of re-binding a stored one that is occupied.
//
// NOTHING CALLS IT. That is the whole of the recovery half of the fixed-port
// rule, and it has no caller in this repository — see the gap recorded under
// "Credential lifecycle" in docs/modules/oauth.md for what happens instead.
// This comment used to cite that file for a rule stated in the present tense,
// and the file has not carried the rule for some time; it is kept as the seam
// the fix needs rather than deleted as dead surface.
func (s *Store) ClearClientRegistration(ctx context.Context, serverID string) error {
	st, err := s.LoadState(ctx, serverID)
	if err != nil {
		return err
	}
	st.ClientID = ""
	st.ClientSecret = ""
	st.RegistrarKind = ""
	st.RedirectURI = ""
	st.CallbackPort = 0
	return s.writeState(ctx, serverID, st)
}

// MarkGrantRevoked records that the authorization server refused `refused`'s
// refresh grant. It reports whether the mark was written.
//
// It writes only the two revocation fields, and only onto the record it was
// told about: the stored state is re-read and compared with `refused` first,
// and a mismatch abandons the mark. The comparison is the point, not
// pedantry — this is a read-modify-write against a key `auth login` also
// writes in whole (the shared-entry gap in docs/modules/oauth.md), so a login
// completing between the read and the write would otherwise be overwritten by
// a stale record carrying "revoked", and the user would watch a login they
// just performed report itself dead.
//
// Recording it twice is a no-op rather than an error: every renewer that
// meets the refusal calls this, and a keychain write per rejected attempt is
// exactly the traffic the revocation exists to stop.
//
// Failure direction: FAIL-OPEN. A mark that cannot be persisted leaves the
// caller with the retry ladder it had before this existed, which is noisy but
// correct; refusing to report the refusal because it could not be filed would
// be neither.
func (s *Store) MarkGrantRevoked(ctx context.Context, serverID string, refused *State, reason string, now time.Time) (bool, error) {
	if refused == nil {
		return false, newFlowError(ErrorTypePersistence,
			fmt.Errorf("oauthflow: refusing to mark a revocation without the record it applies to"))
	}
	st, err := s.LoadState(ctx, serverID)
	if err != nil {
		return false, err
	}
	if st.GrantRevoked() {
		return false, nil
	}
	if st.RefreshToken != refused.RefreshToken || st.ExpiresAt != refused.ExpiresAt {
		// Someone stored new credentials while the refusal was in flight.
		// Theirs are the live ones and nothing is known against them.
		return false, nil
	}
	st.GrantRevokedAt = now.Unix()
	st.GrantRevokedReason = reason
	if err := s.writeState(ctx, serverID, st); err != nil {
		return false, err
	}
	return true, nil
}

// ClearGrantRevoked drops the revocation mark, which is what `auth refresh
// --force` does before trying again. It exists as a separate operation
// because the ordinary clearing path (SaveFromToken) requires a token
// response, and the whole point of the override is to get one.
//
// It is a no-op on a record that carries no mark, so a caller need not ask
// first.
func (s *Store) ClearGrantRevoked(ctx context.Context, serverID string) error {
	st, err := s.LoadState(ctx, serverID)
	if err != nil {
		return err
	}
	if !st.GrantRevoked() {
		return nil
	}
	st.GrantRevokedAt, st.GrantRevokedReason = 0, ""
	return s.writeState(ctx, serverID, st)
}

// writeState persists a whole state record. Every mutator that leaves the
// access token alone shares it — the two revocation ones and
// ClearClientRegistration; Save does not, because it owns the ordering
// invariant with that token.
//
// ClearClientRegistration wrote its own copy of these six lines until the
// error messages had drifted: the copy reported a failed keychain write with
// no mention of what was being written or for which server, which is the half
// of the message a reader needs.
func (s *Store) writeState(ctx context.Context, serverID string, st *State) error {
	raw, err := json.Marshal(st)
	if err != nil {
		return newFlowError(ErrorTypePersistence, err)
	}
	if err := s.sec.Set(ctx, stateRef(serverID), string(raw)); err != nil {
		return newFlowError(ErrorTypePersistence,
			fmt.Errorf("oauthflow: write oauth state for %q: %w", serverID, err))
	}
	return nil
}
