package httpbridge_test

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/dinstein/agent-hub/internal/httpbridge"
	"github.com/dinstein/agent-hub/internal/tier"
)

func newStore(t *testing.T) *httpbridge.Store {
	t.Helper()
	s, err := httpbridge.OpenStore(t.TempDir())
	if err != nil {
		t.Fatalf("OpenStore: %v", err)
	}
	s.SetLockTimeout(2 * time.Second)
	return s
}

func mustCreate(t *testing.T, s *httpbridge.Store, spec httpbridge.CreateSpec) (httpbridge.Token, string) {
	t.Helper()
	tok, value, err := s.Create(context.Background(), spec)
	if err != nil {
		t.Fatalf("Create(%s): %v", spec.Name, err)
	}
	return tok, value
}

// The token's shape is ABI: the prefix dispatches authentication and the
// display prefix is what `token ls` prints.
func TestMintedTokenShape(t *testing.T) {
	t.Parallel()
	s := newStore(t)
	tok, value := mustCreate(t, s, httpbridge.CreateSpec{Name: "ci", Tier: tier.Read})

	if !strings.HasPrefix(value, httpbridge.TokenPrefix) {
		t.Errorf("token %q does not carry the %q prefix", value, httpbridge.TokenPrefix)
	}
	if got, want := len(value), len(httpbridge.TokenPrefix)+64; got != want {
		t.Errorf("token length = %d, want %d (prefix + 64 hex)", got, want)
	}
	hexPart := value[len(httpbridge.TokenPrefix):]
	for _, r := range hexPart {
		if !strings.ContainsRune("0123456789abcdef", r) {
			t.Fatalf("token body %q is not lowercase hex", hexPart)
		}
	}
	if tok.Prefix != value[:httpbridge.DisplayPrefixLen] {
		t.Errorf("display prefix = %q, want %q", tok.Prefix, value[:httpbridge.DisplayPrefixLen])
	}
	// Two mints must not collide.
	_, second := mustCreate(t, s, httpbridge.CreateSpec{Name: "ci2", Tier: tier.Read})
	if second == value {
		t.Fatal("two mints produced the same token")
	}
}

// Only the HMAC is persisted. This is the property the whole storage design
// exists for, so it is asserted against the RAW FILE BYTES rather than
// against the struct.
func TestStoredFileNeverHoldsThePlaintext(t *testing.T) {
	t.Parallel()
	s := newStore(t)
	_, value := mustCreate(t, s, httpbridge.CreateSpec{Name: "agent", Tier: tier.Write})

	raw, err := os.ReadFile(s.Path())
	if err != nil {
		t.Fatal(err)
	}
	body := string(raw)
	if strings.Contains(body, value) {
		t.Fatal("tokens.json contains the token plaintext")
	}
	// The secret half must not be there in any form: the display prefix is
	// fine (it is 12 characters), the 64-hex body is not.
	if strings.Contains(body, value[len(httpbridge.TokenPrefix):]) {
		t.Fatal("tokens.json contains the token body")
	}
	var doc struct {
		Tokens []struct {
			Hash string `json:"hash"`
		} `json:"tokens"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	if len(doc.Tokens) != 1 || len(doc.Tokens[0].Hash) != 64 {
		t.Fatalf("stored hash = %+v, want one 64-hex digest", doc.Tokens)
	}
}

// The HMAC is keyed: the same plaintext under a different key file must not
// verify. That is what makes an exfiltrated tokens.json useless on its own.
func TestHashIsKeyedNotPlainDigest(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	s, err := httpbridge.OpenStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	_, value := mustCreate(t, s, httpbridge.CreateSpec{Name: "a", Tier: tier.Read})
	raw, err := os.ReadFile(filepath.Join(dir, httpbridge.TokensFileName))
	if err != nil {
		t.Fatal(err)
	}

	// Same token list, different key: the lookup must fail.
	other := t.TempDir()
	if err := os.WriteFile(filepath.Join(other, httpbridge.TokensFileName), raw, 0o600); err != nil {
		t.Fatal(err)
	}
	s2, err := httpbridge.OpenStore(other) // mints its own key
	if err != nil {
		t.Fatal(err)
	}
	if _, ok, err := s2.Lookup(value, time.Now()); err != nil || ok {
		t.Fatalf("token verified under a foreign key (ok=%v, err=%v)", ok, err)
	}
	// Sanity: it still verifies under its own key.
	if _, ok, err := s.Lookup(value, time.Now()); err != nil || !ok {
		t.Fatalf("token does not verify under its own key (ok=%v, err=%v)", ok, err)
	}
}

// The key file and the token file are credentials-adjacent: 0600, both.
func TestStorageFilePermissions(t *testing.T) {
	if os.Getuid() == 0 {
		t.Skip("running as root: mode bits are not enforced")
	}
	t.Parallel()
	s := newStore(t)
	mustCreate(t, s, httpbridge.CreateSpec{Name: "a", Tier: tier.Read})
	for _, path := range []string{s.Path(), s.KeyPath()} {
		info, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if perm := info.Mode().Perm(); perm != 0o600 {
			t.Errorf("%s mode = %04o, want 0600", path, perm)
		}
	}
}

// A malformed key file must NOT be regenerated: doing so would silently
// invalidate every issued token while looking like a healthy store.
func TestMalformedKeyFileIsFatal(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, httpbridge.KeyFileName), []byte("not hex\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := httpbridge.OpenStore(dir); err == nil {
		t.Fatal("OpenStore accepted a malformed key file")
	}
}

// Revocation takes effect through the lookup path, which is the only path
// authentication uses.
func TestRevocationDeniesLookup(t *testing.T) {
	t.Parallel()
	s := newStore(t)
	_, value := mustCreate(t, s, httpbridge.CreateSpec{Name: "agent", Tier: tier.Write})

	if _, ok, _ := s.Lookup(value, time.Now()); !ok {
		t.Fatal("fresh token does not authenticate")
	}
	if _, err := s.Revoke(context.Background(), "agent", time.Now()); err != nil {
		t.Fatalf("Revoke: %v", err)
	}
	if _, ok, _ := s.Lookup(value, time.Now()); ok {
		t.Fatal("revoked token still authenticates")
	}
	// The record survives so audit records keep resolving.
	toks, err := s.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(toks) != 1 || !toks[0].Revoked() || toks[0].State(time.Now()) != "revoked" {
		t.Fatalf("revoked token was dropped or mis-stated: %+v", toks)
	}
	// Revoking twice is not silently idempotent: the second call reports it.
	if _, err := s.Revoke(context.Background(), "agent", time.Now()); !errors.Is(err, httpbridge.ErrAlreadyRevoked) {
		t.Errorf("second Revoke err = %v, want ErrAlreadyRevoked", err)
	}
	if _, err := s.Revoke(context.Background(), "nope", time.Now()); !errors.Is(err, httpbridge.ErrTokenNotFound) {
		t.Errorf("Revoke of an unknown name err = %v, want ErrTokenNotFound", err)
	}
}

func TestExpiryDeniesLookup(t *testing.T) {
	t.Parallel()
	s := newStore(t)
	now := time.Now()
	_, value := mustCreate(t, s, httpbridge.CreateSpec{
		Name: "short", Tier: tier.Read, ExpiresAt: now.Add(time.Hour), Now: now,
	})
	if _, ok, _ := s.Lookup(value, now); !ok {
		t.Fatal("unexpired token does not authenticate")
	}
	if _, ok, _ := s.Lookup(value, now.Add(2*time.Hour)); ok {
		t.Fatal("expired token still authenticates")
	}
	toks, _ := s.List()
	if got := toks[0].State(now.Add(2 * time.Hour)); got != "expired" {
		t.Errorf("state = %q, want expired", got)
	}
}

// Uniqueness and the ceiling are transaction properties, not snapshot
// properties: concurrent creators must not both win.
func TestNameUniquenessUnderConcurrency(t *testing.T) {
	t.Parallel()
	s := newStore(t)
	const racers = 8
	var wg sync.WaitGroup
	errs := make([]error, racers)
	for i := 0; i < racers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, _, err := s.Create(context.Background(), httpbridge.CreateSpec{
				Name: "same", Tier: tier.Read,
			})
			errs[i] = err
		}(i)
	}
	wg.Wait()
	winners := 0
	for _, err := range errs {
		switch {
		case err == nil:
			winners++
		case errors.Is(err, httpbridge.ErrTokenExists):
		default:
			t.Fatalf("unexpected error: %v", err)
		}
	}
	if winners != 1 {
		t.Fatalf("%d creators won the same name, want exactly 1", winners)
	}
	toks, _ := s.List()
	if len(toks) != 1 {
		t.Fatalf("store holds %d tokens, want 1", len(toks))
	}
}

// A revoked name stays taken: audit entries must resolve to exactly one
// credential forever.
func TestRevokedNameStaysReserved(t *testing.T) {
	t.Parallel()
	s := newStore(t)
	mustCreate(t, s, httpbridge.CreateSpec{Name: "agent", Tier: tier.Read})
	if _, err := s.Revoke(context.Background(), "agent", time.Now()); err != nil {
		t.Fatal(err)
	}
	_, _, err := s.Create(context.Background(), httpbridge.CreateSpec{Name: "agent", Tier: tier.Read})
	if !errors.Is(err, httpbridge.ErrTokenExists) {
		t.Fatalf("err = %v, want ErrTokenExists", err)
	}
}

func TestTokenLimit(t *testing.T) {
	t.Parallel()
	s := newStore(t)
	for i := 0; i < httpbridge.MaxTokens; i++ {
		if _, _, err := s.Create(context.Background(), httpbridge.CreateSpec{
			Name: "t" + itoa(i), Tier: tier.Read,
		}); err != nil {
			t.Fatalf("create %d: %v", i, err)
		}
	}
	_, _, err := s.Create(context.Background(), httpbridge.CreateSpec{Name: "overflow", Tier: tier.Read})
	if !errors.Is(err, httpbridge.ErrTooManyTokens) {
		t.Fatalf("err = %v, want ErrTooManyTokens", err)
	}
}

func TestNameAndTierValidation(t *testing.T) {
	t.Parallel()
	s := newStore(t)
	bad := []struct {
		name string
		tier tier.Tier
		want error
	}{
		{"", tier.Read, httpbridge.ErrInvalidName},
		{"has space", tier.Read, httpbridge.ErrInvalidName},
		{"slash/name", tier.Read, httpbridge.ErrInvalidName},
		{strings.Repeat("x", 65), tier.Read, httpbridge.ErrInvalidName},
		{"ok", "", httpbridge.ErrInvalidTier},
		{"ok", tier.Tier("root"), httpbridge.ErrInvalidTier},
	}
	for _, tc := range bad {
		_, _, err := s.Create(context.Background(), httpbridge.CreateSpec{Name: tc.name, Tier: tc.tier})
		if !errors.Is(err, tc.want) {
			t.Errorf("Create(%q, %q) err = %v, want %v", tc.name, tc.tier, err, tc.want)
		}
	}
	// Nothing was written.
	if toks, _ := s.List(); len(toks) != 0 {
		t.Fatalf("rejected creates left %d tokens behind", len(toks))
	}
}

// nil vs empty allowlist is the three-state the registry's ToolSelector
// uses, and the empty one must stay CLOSED.
func TestServerAllowlistThreeState(t *testing.T) {
	t.Parallel()
	s := newStore(t)
	all, _ := mustCreate(t, s, httpbridge.CreateSpec{Name: "all", Tier: tier.Read})
	none, _ := mustCreate(t, s, httpbridge.CreateSpec{Name: "none", Tier: tier.Read, Servers: []string{}})
	some, _ := mustCreate(t, s, httpbridge.CreateSpec{Name: "some", Tier: tier.Read, Servers: []string{"git", "fs"}})
	star, _ := mustCreate(t, s, httpbridge.CreateSpec{Name: "star", Tier: tier.Read, Servers: []string{"fs", "*"}})

	if !all.AllowsServer("anything") {
		t.Error("a nil allowlist must allow every server")
	}
	if none.AllowsServer("fs") {
		t.Error("an empty allowlist must allow nothing")
	}
	if !some.AllowsServer("fs") || !some.AllowsServer("git") || some.AllowsServer("web") {
		t.Errorf("narrow allowlist misbehaves: %v", some.Servers)
	}
	if !star.AllowsServer("web") {
		t.Error("the wildcard must allow every server")
	}
	if len(star.Servers) != 1 || star.Servers[0] != httpbridge.ServerWildcard {
		t.Errorf("a wildcard list must collapse to the wildcard, got %v", star.Servers)
	}
	// Deterministic storage order: the list is sorted.
	if some.Servers[0] != "fs" || some.Servers[1] != "git" {
		t.Errorf("allowlist not normalised deterministically: %v", some.Servers)
	}
	// The distinction survives a round trip through the file.
	reopened, err := httpbridge.OpenStore(filepath.Dir(s.Path()))
	if err != nil {
		t.Fatal(err)
	}
	toks, err := reopened.List()
	if err != nil {
		t.Fatal(err)
	}
	byName := map[string]httpbridge.Token{}
	for _, tk := range toks {
		byName[tk.Name] = tk
	}
	if byName["all"].Servers != nil {
		t.Errorf("nil allowlist became %v after a round trip", byName["all"].Servers)
	}
	if byName["none"].Servers == nil || len(byName["none"].Servers) != 0 {
		t.Errorf("empty allowlist became %v after a round trip (fail-open!)", byName["none"].Servers)
	}
}

func TestActiveCountIgnoresRevokedAndExpired(t *testing.T) {
	t.Parallel()
	s := newStore(t)
	now := time.Now()
	mustCreate(t, s, httpbridge.CreateSpec{Name: "live", Tier: tier.Read, Now: now})
	mustCreate(t, s, httpbridge.CreateSpec{Name: "gone", Tier: tier.Read, Now: now})
	mustCreate(t, s, httpbridge.CreateSpec{
		Name: "old", Tier: tier.Read, ExpiresAt: now.Add(time.Minute), Now: now,
	})
	if _, err := s.Revoke(context.Background(), "gone", now); err != nil {
		t.Fatal(err)
	}
	n, err := s.ActiveCount(now.Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("ActiveCount = %d, want 1 (live only)", n)
	}
}

// A corrupt token file must NOT read as "no tokens": bind authorization
// consults exactly that count, so failing open there would unlock the
// endpoint.
func TestMalformedTokenFileIsAnError(t *testing.T) {
	t.Parallel()
	s := newStore(t)
	mustCreate(t, s, httpbridge.CreateSpec{Name: "a", Tier: tier.Read})
	if err := os.WriteFile(s.Path(), []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := s.List(); err == nil {
		t.Fatal("List accepted a malformed token file")
	}
	if _, err := s.ActiveCount(time.Now()); err == nil {
		t.Fatal("ActiveCount accepted a malformed token file")
	}
}

func TestLookupMissesAreUndifferentiated(t *testing.T) {
	t.Parallel()
	s := newStore(t)
	mustCreate(t, s, httpbridge.CreateSpec{Name: "a", Tier: tier.Read})
	for _, candidate := range []string{
		"agt_" + strings.Repeat("0", 64),
		"agt_short",
		"",
		"not-a-token",
	} {
		tok, ok, err := s.Lookup(candidate, time.Now())
		if err != nil || ok || tok.Name != "" {
			t.Errorf("Lookup(%q) = (%+v, %v, %v), want a plain miss", candidate, tok, ok, err)
		}
	}
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	return string(buf[i:])
}
