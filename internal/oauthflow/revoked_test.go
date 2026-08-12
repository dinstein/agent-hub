package oauthflow

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// The vault half of a terminal refusal: what gets written, what refuses to
// write, and — the point of the whole mechanism — what stops being asked.

func tokenHits(as *fakeAS) int {
	n := 0
	for _, p := range as.hitList() {
		if p == "/token" {
			n++
		}
	}
	return n
}

// seedRefusedState stores a record whose refresh token the fake AS will
// reject: it hands out invalid_grant for anything but the one it issued.
func seedRefusedState(t *testing.T, s *Store, as *fakeAS, serverID string) {
	t.Helper()
	st := &State{
		TokenEndpoint: as.srv.URL + "/token",
		ClientID:      "client-abc",
		RefreshToken:  "a-token-this-server-already-rotated-away",
		IssuedAt:      1,
		ExpiresAt:     100,
	}
	if err := s.Save(context.Background(), serverID, st, "at-old"); err != nil {
		t.Fatal(err)
	}
}

func revokedCoordinator(t *testing.T, store *Store, as *fakeAS) *Coordinator {
	t.Helper()
	return NewCoordinator(CoordinatorConfig{
		Store: store, Client: as.client(), LockDir: t.TempDir(),
		Now: func() time.Time { return time.Unix(1000, 0) },
	})
}

// TestARefusedGrantIsAskedExactlyOnce is the behaviour this whole file
// exists for. Before it, every renewer offered the same dead credential on
// its own schedule for as long as the server stayed configured.
func TestARefusedGrantIsAskedExactlyOnce(t *testing.T) {
	as := newFakeAS(t)
	store := NewStore(newFakeVault())
	seedRefusedState(t, store, as, "gh")
	co := revokedCoordinator(t, store, as)

	_, _, err := co.Refresh(context.Background(), "gh")
	if !errors.Is(err, ErrGrantRevoked) || !NeedsLogin(err) {
		t.Fatalf("first refresh err = %v, want a terminal ErrGrantRevoked", err)
	}
	if got := tokenHits(as); got != 1 {
		t.Fatalf("token endpoint hits = %d, want 1", got)
	}

	st, err := store.LoadState(context.Background(), "gh")
	if err != nil {
		t.Fatal(err)
	}
	if !st.GrantRevoked() || st.GrantRevokedAt != 1000 {
		t.Fatalf("the refusal was not recorded: %+v", st)
	}
	if !strings.Contains(st.GrantRevokedReason, "invalid_grant") {
		t.Fatalf("reason = %q, want the authorization server's own code", st.GrantRevokedReason)
	}
	// The access token is deliberately untouched: it may well still be
	// accepted, and taking it out of service early turns "you will have to
	// log in eventually" into "you have to log in now".
	if tok, err := store.LoadAccessToken(context.Background(), "gh"); err != nil || tok != "at-old" {
		t.Fatalf("access token = %q, %v — a revocation must not delete it", tok, err)
	}

	// Every later attempt, from any renewer, is answered from the vault.
	for i := range 3 {
		_, _, err := co.Refresh(context.Background(), "gh")
		if !errors.Is(err, ErrGrantRevoked) {
			t.Fatalf("attempt %d err = %v, want ErrGrantRevoked", i+2, err)
		}
	}
	if got := tokenHits(as); got != 1 {
		t.Fatalf("token endpoint hits = %d after four refreshes, want 1", got)
	}
}

// TestEnsureFreshReportsARevokedGrantWithoutAsking: the proactive path runs
// on a timer, so it is the one that would produce the traffic.
func TestEnsureFreshReportsARevokedGrantWithoutAsking(t *testing.T) {
	as := newFakeAS(t)
	store := NewStore(newFakeVault())
	seedRefusedState(t, store, as, "gh")
	co := revokedCoordinator(t, store, as)

	if _, _, _, err := co.EnsureFresh(context.Background(), "gh"); !NeedsLogin(err) {
		t.Fatalf("err = %v, want the terminal classification", err)
	}
	before := tokenHits(as)
	_, _, renewed, err := co.EnsureFresh(context.Background(), "gh")
	if !errors.Is(err, ErrGrantRevoked) {
		t.Fatalf("err = %v", err)
	}
	if !renewed {
		t.Fatal("EnsureFresh must report that it attempted a renewal, even a refused one")
	}
	if got := tokenHits(as); got != before {
		t.Fatalf("token endpoint hits went %d -> %d; a recorded refusal must reach the network never again", before, got)
	}
}

// TestATransientFailureIsNeverRecordedAsARefusal is the guard on the other
// side: an authorization server having a bad day must not park a server that
// would have recovered on the next retry.
func TestATransientFailureIsNeverRecordedAsARefusal(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, 500, map[string]any{"error": "invalid_grant", "error_description": "not really"})
	}))
	defer srv.Close()

	store := NewStore(newFakeVault())
	st := &State{TokenEndpoint: srv.URL + "/token", ClientID: "c", RefreshToken: "r", ExpiresAt: 100}
	if err := store.Save(context.Background(), "gh", st, "at-old"); err != nil {
		t.Fatal(err)
	}
	co := NewCoordinator(CoordinatorConfig{
		Store: store, Client: NewClient(Config{AllowLoopback: true}), LockDir: t.TempDir(),
		Now: func() time.Time { return time.Unix(1000, 0) },
	})
	if _, _, err := co.Refresh(context.Background(), "gh"); NeedsLogin(err) {
		t.Fatalf("err = %v, want a retryable failure", err)
	}
	stored, err := store.LoadState(context.Background(), "gh")
	if err != nil {
		t.Fatal(err)
	}
	if stored.GrantRevoked() {
		t.Fatal("a 500 must never park the credential")
	}
}

// TestALoginClearsARecordedRefusal: recovery has to be structural. Every
// path that obtains a token ends in SaveFromToken, so none of them has to
// remember to reset the flag.
func TestALoginClearsARecordedRefusal(t *testing.T) {
	as := newFakeAS(t)
	store := NewStore(newFakeVault())
	seedRefusedState(t, store, as, "gh")
	co := revokedCoordinator(t, store, as)
	if _, _, err := co.Refresh(context.Background(), "gh"); !errors.Is(err, ErrGrantRevoked) {
		t.Fatalf("setup: %v", err)
	}

	prev, err := store.LoadState(context.Background(), "gh")
	if err != nil {
		t.Fatal(err)
	}
	next := *prev
	next.RefreshToken = as.refreshToken
	saved, err := store.SaveFromToken(context.Background(), "gh", prev, next,
		&TokenResponse{AccessToken: "at-new", ExpiresIn: 3600}, time.Unix(2000, 0))
	if err != nil {
		t.Fatal(err)
	}
	if saved.GrantRevoked() {
		t.Fatalf("a stored token must clear the refusal: %+v", saved)
	}
	if _, _, err := co.Refresh(context.Background(), "gh"); err != nil {
		t.Fatalf("refresh after a fresh login: %v", err)
	}
}

// TestClearGrantRevokedAsksOnceMore is `auth refresh --force`: the human
// override for a provider that answered invalid_grant and was wrong, or that
// has since been reconfigured.
func TestClearGrantRevokedAsksOnceMore(t *testing.T) {
	as := newFakeAS(t)
	store := NewStore(newFakeVault())
	seedRefusedState(t, store, as, "gh")
	co := revokedCoordinator(t, store, as)
	if _, _, err := co.Refresh(context.Background(), "gh"); !errors.Is(err, ErrGrantRevoked) {
		t.Fatalf("setup: %v", err)
	}

	if err := store.ClearGrantRevoked(context.Background(), "gh"); err != nil {
		t.Fatal(err)
	}
	before := tokenHits(as)
	if _, _, err := co.Refresh(context.Background(), "gh"); !errors.Is(err, ErrGrantRevoked) {
		t.Fatalf("err = %v, want the provider asked again and refusing again", err)
	}
	if got := tokenHits(as); got != before+1 {
		t.Fatalf("token endpoint hits = %d, want exactly one more ask", got)
	}
	// And the second refusal is recorded again, so the override does not
	// leave the server unparked.
	st, err := store.LoadState(context.Background(), "gh")
	if err != nil {
		t.Fatal(err)
	}
	if !st.GrantRevoked() {
		t.Fatal("the second refusal must be recorded like the first")
	}
	if err := store.ClearGrantRevoked(context.Background(), "absent"); !errors.Is(err, ErrNoState) {
		t.Fatalf("clearing a server with no state = %v, want ErrNoState", err)
	}
}

// TestARefusalIsNotRecordedOverNewerCredentials: the mark is a read-modify-
// write against a key `auth login` also writes in whole, so a login that
// lands while the refusal is in flight must win.
func TestARefusalIsNotRecordedOverNewerCredentials(t *testing.T) {
	store := NewStore(newFakeVault())
	ctx := context.Background()
	stale := &State{TokenEndpoint: "https://as.example/token", ClientID: "c",
		RefreshToken: "old", IssuedAt: 1, ExpiresAt: 100}
	if err := store.Save(ctx, "gh", stale, "at-old"); err != nil {
		t.Fatal(err)
	}
	// A login lands: new refresh token, new expiry.
	fresh := *stale
	fresh.RefreshToken, fresh.ExpiresAt = "new", 5000
	if err := store.Save(ctx, "gh", &fresh, "at-new"); err != nil {
		t.Fatal(err)
	}

	marked, err := store.MarkGrantRevoked(ctx, "gh", stale, "invalid_grant", time.Unix(1000, 0))
	if err != nil {
		t.Fatal(err)
	}
	if marked {
		t.Fatal("a refusal of the OLD grant must not be recorded against the new one")
	}
	st, err := store.LoadState(ctx, "gh")
	if err != nil {
		t.Fatal(err)
	}
	if st.GrantRevoked() || st.RefreshToken != "new" {
		t.Fatalf("the fresh login was damaged: %+v", st)
	}

	// The same refusal against the record it applies to IS recorded, and
	// recording it twice is a no-op rather than a second write.
	if marked, err := store.MarkGrantRevoked(ctx, "gh", &fresh, "invalid_grant", time.Unix(1000, 0)); err != nil || !marked {
		t.Fatalf("marked = %t, err = %v", marked, err)
	}
	if marked, err := store.MarkGrantRevoked(ctx, "gh", &fresh, "invalid_grant", time.Unix(2000, 0)); err != nil || marked {
		t.Fatalf("a second mark = %t, err = %v, want no-op", marked, err)
	}
	st, err = store.LoadState(ctx, "gh")
	if err != nil {
		t.Fatal(err)
	}
	if st.GrantRevokedAt != 1000 {
		t.Fatalf("the first refusal's timestamp must stand: %d", st.GrantRevokedAt)
	}
	if _, err := store.MarkGrantRevoked(ctx, "gh", nil, "x", time.Unix(1, 0)); err == nil {
		t.Fatal("marking without the record it applies to must be refused")
	}
}
