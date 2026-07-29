package oauthflow

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/dinstein/agent-hub/internal/secrets"
)

func TestStoreRoundTrip(t *testing.T) {
	v := newFakeVault()
	s := NewStore(v)
	ctx := context.Background()
	st := &State{
		TokenEndpoint: "https://as/token",
		ClientID:      "client-abc",
		RefreshToken:  "r-1",
		Resource:      "https://mcp/mcp",
		IssuedAt:      1000,
		ExpiresAt:     4600,
		CallbackPort:  5731,
		RedirectURI:   "http://127.0.0.1:5731/callback",
	}
	if err := s.Save(ctx, "gh", st, "at-1"); err != nil {
		t.Fatal(err)
	}
	got, tok, err := s.Load(ctx, "gh")
	if err != nil {
		t.Fatal(err)
	}
	if tok != "at-1" {
		t.Fatalf("token = %q", tok)
	}
	if !reflect.DeepEqual(got, st) {
		t.Fatalf("state round trip:\n got %+v\nwant %+v", got, st)
	}
}

// TestVaultKeysAreTheTwoDesignatedEntries pins the storage layout of
// docs/modules/oauth.md: exactly two entries per server, under the composite key
// (serverID, "_global").
func TestVaultKeysAreTheTwoDesignatedEntries(t *testing.T) {
	v := newFakeVault()
	s := NewStore(v)
	if err := s.Save(context.Background(), "gh", &State{TokenEndpoint: "https://as/token"}, "at-1"); err != nil {
		t.Fatal(err)
	}
	if got := v.get("gh", secrets.KeyHTTPAuth); got != "at-1" {
		t.Fatalf("__http_auth__ = %q", got)
	}
	if got := v.get("gh", secrets.KeyOAuthState); got == "" {
		t.Fatal("__oauth_state__ is empty")
	}
	// The scope component must be the default composite key.
	if v.data[vaultKey("gh", secrets.DefaultScope, secrets.KeyHTTPAuth)] == "" {
		t.Fatal("entry is not stored under the (serverID, _global) composite key")
	}
}

// TestSaveWritesStateBeforeAccessToken is THE ordering invariant of
// docs/modules/oauth.md.
func TestSaveWritesStateBeforeAccessToken(t *testing.T) {
	v := newFakeVault()
	s := NewStore(v)
	if err := s.Save(context.Background(), "gh", &State{TokenEndpoint: "https://as/token"}, "at-1"); err != nil {
		t.Fatal(err)
	}
	want := []string{secrets.KeyOAuthState, secrets.KeyHTTPAuth}
	if got := v.writeLog(); !reflect.DeepEqual(got, want) {
		t.Fatalf("write order = %v, want %v (state must precede the access token)", got, want)
	}
}

// TestTokenWriteFailureLeavesRecoverableState injects a failure at the
// SECOND write and asserts the surviving combination is the recoverable
// one: NEW refresh token + OLD access token.
//
// The forbidden combination — new access token next to an already-rotated
// (invalidated) refresh token — is unrecoverable without a human re-login,
// which is exactly why the writes are ordered the way they are.
func TestTokenWriteFailureLeavesRecoverableState(t *testing.T) {
	v := newFakeVault()
	s := NewStore(v)
	ctx := context.Background()

	// Pre-existing credentials from an earlier successful login.
	old := &State{TokenEndpoint: "https://as/token", RefreshToken: "r-old", ExpiresAt: 1000}
	if err := s.Save(ctx, "gh", old, "at-old"); err != nil {
		t.Fatal(err)
	}

	// The provider rotated the refresh token; now the access-token write
	// fails (keychain locked, disk full, process killed).
	v.setFailKey(secrets.KeyHTTPAuth)
	fresh := &State{TokenEndpoint: "https://as/token", RefreshToken: "r-new", ExpiresAt: 5000}
	err := s.Save(ctx, "gh", fresh, "at-new")
	if err == nil {
		t.Fatal("expected the injected write failure to surface")
	}

	// The NEW refresh token survived...
	stored, loadErr := s.LoadState(ctx, "gh")
	if loadErr != nil {
		t.Fatal(loadErr)
	}
	if stored.RefreshToken != "r-new" {
		t.Fatalf("refresh token = %q, want the rotated one", stored.RefreshToken)
	}
	// ...next to the OLD access token. Stale, but self-healing.
	if got := v.get("gh", secrets.KeyHTTPAuth); got != "at-old" {
		t.Fatalf("access token = %q, want the old one to be untouched", got)
	}
	// The forbidden pairing must not exist.
	if v.get("gh", secrets.KeyHTTPAuth) == "at-new" && stored.RefreshToken == "r-old" {
		t.Fatal("new access token next to an invalidated refresh token: unrecoverable state")
	}
	var fe *FlowError
	if !errors.As(err, &fe) || fe.Type != ErrorTypePersistence || fe.Suggestion == "" {
		t.Fatalf("persistence failure not classified: %v", err)
	}
}

// TestStateWriteFailureChangesNothing: a failure at the FIRST write must
// leave both entries exactly as they were.
func TestStateWriteFailureChangesNothing(t *testing.T) {
	v := newFakeVault()
	s := NewStore(v)
	ctx := context.Background()
	if err := s.Save(ctx, "gh", &State{TokenEndpoint: "https://as/token", RefreshToken: "r-old"}, "at-old"); err != nil {
		t.Fatal(err)
	}
	v.setFailKey(secrets.KeyOAuthState)
	if err := s.Save(ctx, "gh", &State{RefreshToken: "r-new"}, "at-new"); err == nil {
		t.Fatal("expected failure")
	}
	if got := v.get("gh", secrets.KeyHTTPAuth); got != "at-old" {
		t.Fatalf("access token = %q; a failed state write must not touch it", got)
	}
	st, err := s.LoadState(ctx, "gh")
	if err != nil {
		t.Fatal(err)
	}
	if st.RefreshToken != "r-old" {
		t.Fatalf("refresh token = %q", st.RefreshToken)
	}
}

// TestLoadReportsNoTokenForDCROnlyRecord is docs/modules/oauth.md's third expiry
// rule: a record with client credentials but no access token must report
// ErrNoToken, not an empty token, or the reconnect path loops forever.
func TestLoadReportsNoTokenForDCROnlyRecord(t *testing.T) {
	v := newFakeVault()
	raw, _ := json.Marshal(&State{TokenEndpoint: "https://as/token", ClientID: "client-abc"})
	v.put("gh", secrets.KeyOAuthState, string(raw))

	s := NewStore(v)
	st, tok, err := s.Load(context.Background(), "gh")
	if !errors.Is(err, ErrNoToken) {
		t.Fatalf("err = %v, want ErrNoToken", err)
	}
	if tok != "" {
		t.Fatalf("token = %q", tok)
	}
	if st == nil || st.ClientID != "client-abc" {
		t.Fatal("the DCR credentials must still be returned so the caller can reuse them")
	}
}

func TestLoadReportsNoStateWhenAbsent(t *testing.T) {
	s := NewStore(newFakeVault())
	if _, err := s.LoadState(context.Background(), "nope"); !errors.Is(err, ErrNoState) {
		t.Fatalf("err = %v, want ErrNoState", err)
	}
}

// TestCorruptStateIsNotTreatedAsAbsent: a corrupt entry must be loud.
// Silently treating it as "no state" would trigger a fresh registration and
// orphan a working refresh token.
func TestCorruptStateIsNotTreatedAsAbsent(t *testing.T) {
	v := newFakeVault()
	v.put("gh", secrets.KeyOAuthState, "{not json")
	s := NewStore(v)
	_, err := s.LoadState(context.Background(), "gh")
	if err == nil {
		t.Fatal("corrupt state must be an error")
	}
	if errors.Is(err, ErrNoState) {
		t.Fatal("corrupt state must NOT be reported as absent")
	}
}

func TestSaveRefusesEmptyInputs(t *testing.T) {
	s := NewStore(newFakeVault())
	ctx := context.Background()
	if err := s.Save(ctx, "gh", nil, "at"); err == nil {
		t.Fatal("nil state must be refused")
	}
	if err := s.Save(ctx, "gh", &State{}, "   "); err == nil {
		t.Fatal("an empty access token must be refused")
	}
}

// TestExpirySemantics covers the three rules of docs/modules/oauth.md.
func TestExpirySemantics(t *testing.T) {
	now := time.Unix(10_000, 0)

	// Rule 1: no expires_at means NEVER EXPIRES, not "already expired".
	forever := &State{IssuedAt: 1}
	if !forever.NeverExpires() {
		t.Fatal("expires_at 0 must mean never expires")
	}
	if forever.NeedsRefresh(now) {
		t.Fatal("a never-expiring token must not schedule a refresh")
	}
	if forever.Expired(now) {
		t.Fatal("a never-expiring token is not expired")
	}
	if _, ok := forever.RefreshAt(); ok {
		t.Fatal("no refresh should be scheduled")
	}

	// Rule 2a: the grace applies only above a 5 minute lifetime.
	longLived := &State{IssuedAt: now.Unix(), ExpiresAt: now.Add(time.Hour).Unix()}
	at, ok := longLived.RefreshAt()
	if !ok {
		t.Fatal("a long-lived token must schedule a refresh")
	}
	if want := time.Unix(longLived.ExpiresAt, 0).Add(-RefreshGrace); !at.Equal(want) {
		t.Fatalf("refresh at %s want %s", at, want)
	}
	if longLived.NeedsRefresh(now) {
		t.Fatal("an hour-old token must not refresh immediately")
	}
	if !longLived.NeedsRefresh(time.Unix(longLived.ExpiresAt, 0).Add(-30 * time.Second)) {
		t.Fatal("inside the grace window a refresh is due")
	}

	// Rule 2b: a short-lived token refreshes AT expiry, never before —
	// subtracting the grace would make it born already expired.
	shortLived := &State{IssuedAt: now.Unix(), ExpiresAt: now.Add(60 * time.Second).Unix()}
	at, ok = shortLived.RefreshAt()
	if !ok {
		t.Fatal("short-lived token must schedule a refresh")
	}
	if want := time.Unix(shortLived.ExpiresAt, 0); !at.Equal(want) {
		t.Fatalf("short-lived refresh at %s want %s (no grace)", at, want)
	}
	if shortLived.NeedsRefresh(now) {
		t.Fatal("a freshly issued 60s token must not be born expired")
	}

	if got := (&State{IssuedAt: 100, ExpiresAt: 4_000}).Lifetime(); got != 3900*time.Second {
		t.Fatalf("lifetime = %s", got)
	}
	if got := (&State{ExpiresAt: 100, IssuedAt: 200}).Lifetime(); got != 0 {
		t.Fatalf("inverted timestamps must yield 0 lifetime, got %s", got)
	}
}

func TestSaveFromTokenCarriesForwardOmittedFields(t *testing.T) {
	v := newFakeVault()
	s := NewStore(v)
	ctx := context.Background()
	now := time.Unix(1_000_000, 0)

	prev := &State{
		TokenEndpoint: "https://as/token",
		ClientID:      "client-abc",
		ClientSecret:  "sec",
		RegistrarKind: "dcr",
		RefreshToken:  "r-1",
		Scope:         "read write",
		RedirectURI:   "http://127.0.0.1:5731/callback",
		CallbackPort:  5731,
		Issuer:        "https://as",
		Resource:      "https://mcp/mcp",
	}
	if err := s.Save(ctx, "gh", prev, "at-0"); err != nil {
		t.Fatal(err)
	}

	// A non-rotating provider omits refresh_token and scope on refresh.
	got, err := s.SaveFromToken(ctx, "gh", prev, State{}, &TokenResponse{
		AccessToken: "at-1", TokenType: "Bearer", ExpiresIn: 3600,
	}, now)
	if err != nil {
		t.Fatal(err)
	}
	if got.RefreshToken != "r-1" {
		t.Fatalf("refresh token = %q; omitting it must mean UNCHANGED, not cleared", got.RefreshToken)
	}
	if got.Scope != "read write" {
		t.Fatalf("scope = %q; omitting it must mean unchanged (RFC 6749 §5.1)", got.Scope)
	}
	if got.ClientID != "client-abc" || got.RedirectURI != prev.RedirectURI || got.CallbackPort != 5731 {
		t.Fatalf("client/callback fields lost: %+v", got)
	}
	if got.ExpiresAt != now.Add(time.Hour).Unix() || got.IssuedAt != now.Unix() {
		t.Fatalf("timestamps = %d/%d", got.IssuedAt, got.ExpiresAt)
	}

	// A rotating provider replaces it.
	got, err = s.SaveFromToken(ctx, "gh", got, State{}, &TokenResponse{
		AccessToken: "at-2", RefreshToken: "r-2",
	}, now)
	if err != nil {
		t.Fatal(err)
	}
	if got.RefreshToken != "r-2" {
		t.Fatalf("refresh token = %q", got.RefreshToken)
	}
	// No expires_in this time: never expires, not "expired".
	if got.ExpiresAt != 0 {
		t.Fatalf("expires_at = %d, want 0 (never expires)", got.ExpiresAt)
	}
}

func TestClearRemovesBothEntries(t *testing.T) {
	v := newFakeVault()
	s := NewStore(v)
	ctx := context.Background()
	if err := s.Save(ctx, "gh", &State{TokenEndpoint: "https://as/token"}, "at"); err != nil {
		t.Fatal(err)
	}
	if err := s.Clear(ctx, "gh"); err != nil {
		t.Fatal(err)
	}
	if v.get("gh", secrets.KeyHTTPAuth) != "" || v.get("gh", secrets.KeyOAuthState) != "" {
		t.Fatal("clear left an entry behind")
	}
}

// TestClearClientRegistration is the 7.7 rule for an occupied preferred
// callback port: drop the DCR credentials so the next login re-registers,
// but keep the tokens.
func TestClearClientRegistration(t *testing.T) {
	v := newFakeVault()
	s := NewStore(v)
	ctx := context.Background()
	st := &State{TokenEndpoint: "https://as/token", ClientID: "c", ClientSecret: "s",
		RegistrarKind: "dcr", RedirectURI: "http://127.0.0.1:5731/callback", CallbackPort: 5731,
		RefreshToken: "r-1"}
	if err := s.Save(ctx, "gh", st, "at"); err != nil {
		t.Fatal(err)
	}
	if err := s.ClearClientRegistration(ctx, "gh"); err != nil {
		t.Fatal(err)
	}
	got, err := s.LoadState(ctx, "gh")
	if err != nil {
		t.Fatal(err)
	}
	if got.ClientID != "" || got.CallbackPort != 0 || got.RedirectURI != "" {
		t.Fatalf("registration not cleared: %+v", got)
	}
	if got.RefreshToken != "r-1" {
		t.Fatal("the refresh token must survive a registration reset")
	}
	if v.get("gh", secrets.KeyHTTPAuth) != "at" {
		t.Fatal("the access token must survive a registration reset")
	}
}

// TestStoreOverRealSecretsChain runs the same round trip against the actual
// four-level vault (encrypted-file level, no keyring), proving the composite
// key and entry names line up with internal/secrets for real.
func TestStoreOverRealSecretsChain(t *testing.T) {
	dir := t.TempDir()
	chain := secrets.NewChain(secrets.ChainConfig{
		Dir: dir,
		LookupEnv: func(k string) (string, bool) {
			if k == "AGENTHUB_SECRET_KEY" {
				return "test-master-key", true
			}
			return "", false
		},
	})
	s := NewStore(chain)
	ctx := context.Background()
	st := &State{TokenEndpoint: "https://as/token", ClientID: "c", RefreshToken: "r-1", ExpiresAt: 42}
	if err := s.Save(ctx, "gh", st, "at-1"); err != nil {
		t.Fatal(err)
	}
	got, tok, err := s.Load(ctx, "gh")
	if err != nil {
		t.Fatal(err)
	}
	if tok != "at-1" || got.RefreshToken != "r-1" {
		t.Fatalf("round trip: %+v / %q", got, tok)
	}
	refs, err := chain.List(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(refs) != 2 {
		t.Fatalf("expected exactly two vault entries, got %d: %+v", len(refs), refs)
	}
	for _, r := range refs {
		if r.ServerID != "gh" || r.Scope != secrets.DefaultScope {
			t.Fatalf("unexpected ref %+v", r)
		}
		if r.Key != secrets.KeyHTTPAuth && r.Key != secrets.KeyOAuthState {
			t.Fatalf("unexpected entry key %q", r.Key)
		}
	}
	if err := s.Clear(ctx, "gh"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.LoadState(ctx, "gh"); !errors.Is(err, ErrNoState) {
		t.Fatalf("after clear: %v", err)
	}
}
