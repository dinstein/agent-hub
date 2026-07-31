package gateway

import (
	"context"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/dinstein/agent-hub/internal/downstream"
	"github.com/dinstein/agent-hub/internal/oauthflow"
	"github.com/dinstein/agent-hub/internal/secrets"
)

// memVault is an in-memory secrets.Store, counting reads so a test can assert
// that a decision was LATCHED rather than re-taken on every request.
type memVault struct {
	mu    sync.Mutex
	data  map[string]string
	reads atomic.Int64
}

func newMemVault() *memVault { return &memVault{data: map[string]string{}} }

func (m *memVault) Get(_ context.Context, ref secrets.Ref) (string, bool, error) {
	m.reads.Add(1)
	m.mu.Lock()
	defer m.mu.Unlock()
	v, ok := m.data[ref.StorageKey()]
	return v, ok, nil
}

func (m *memVault) Set(_ context.Context, ref secrets.Ref, val string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.data[ref.StorageKey()] = val
	return nil
}

func (m *memVault) Delete(_ context.Context, ref secrets.Ref) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.data, ref.StorageKey())
	return nil
}

func (m *memVault) resolve(ctx context.Context, ref secrets.Ref) (string, bool, error) {
	return m.Get(ctx, ref)
}

// newFreshFixture builds a proactiveSource over an in-memory vault with a
// coordinator whose token endpoint goes nowhere — every test here is about
// the SCHEDULE, and a test that needed a live authorization server to assert
// "nothing was refreshed" would be asserting the wrong thing.
func newFreshFixture(t *testing.T, now time.Time) (*proactiveSource, *memVault, *recordHandler) {
	t.Helper()
	vault := newMemVault()
	store := oauthflow.NewStore(vault)
	coord := oauthflow.NewCoordinator(oauthflow.CoordinatorConfig{
		Store:   store,
		Client:  oauthflow.NewClient(oauthflow.Config{}),
		LockDir: t.TempDir(),
		Now:     func() time.Time { return now },
	})
	h := &recordHandler{}
	p := newProactiveSource(
		downstream.NewScopedVaultTokenSource("srv", secrets.DefaultScope, vault.resolve, nil),
		coord, "srv", secrets.DefaultScope, slog.New(h))
	p.now = func() time.Time { return now }
	return p, vault, h
}

// TestProactiveSourceSchedulesFromTheStoredExpiry: a healthy token is handed
// over untouched, and the deadline the round tripper will read is the instant
// a refresh becomes due — not the hard expiry, so the re-read has somewhere
// to go.
func TestProactiveSourceSchedulesFromTheStoredExpiry(t *testing.T) {
	t.Parallel()
	now := time.Unix(1_000_000, 0)
	p, vault, h := newFreshFixture(t, now)

	st := &oauthflow.State{
		TokenEndpoint: "https://as.example/token", ClientID: "c", RefreshToken: "r",
		IssuedAt: now.Add(-time.Hour).Unix(), ExpiresAt: now.Add(time.Hour).Unix(),
	}
	if err := oauthflow.NewStore(vault).Save(context.Background(), "srv", st, "at-live"); err != nil {
		t.Fatal(err)
	}

	tok, ok, err := p.Token(context.Background())
	if err != nil || !ok {
		t.Fatalf("Token() = %q, %v, %v", tok, ok, err)
	}
	if tok != "at-live" {
		t.Fatalf("token = %q, want the stored one — a healthy token must not be renewed", tok)
	}
	want, scheduled := st.RefreshAt()
	if !scheduled {
		t.Fatal("the fixture's state must schedule a refresh")
	}
	if got := p.NotAfter(); !got.Equal(want) {
		t.Errorf("NotAfter() = %v, want the refresh instant %v", got, want)
	}
	if msg := h.messages(); msg != "" {
		t.Errorf("a no-op renewal logged %q; only a real renewal may", msg)
	}
}

// TestProactiveSourceLatchesAServerWithNoOAuthState: a hand-pasted token (or
// a server never authorized) has nothing to renew. It must report NO deadline
// — which returns the round tripper's cache to exactly its old behaviour —
// and must not consult the vault again on every request.
func TestProactiveSourceLatchesAServerWithNoOAuthState(t *testing.T) {
	t.Parallel()
	now := time.Unix(1_000_000, 0)
	p, vault, h := newFreshFixture(t, now)
	if err := vault.Set(context.Background(),
		secrets.Ref{ServerID: "srv", Scope: secrets.DefaultScope, Key: secrets.KeyHTTPAuth}, "pasted"); err != nil {
		t.Fatal(err)
	}

	tok, ok, err := p.Token(context.Background())
	if err != nil || !ok || tok != "pasted" {
		t.Fatalf("Token() = %q, %v, %v; want the pasted credential", tok, ok, err)
	}
	if got := p.NotAfter(); !got.IsZero() {
		t.Errorf("NotAfter() = %v, want the zero instant: there is no schedule without OAuth state", got)
	}
	if msg := h.messages(); msg != "" {
		t.Errorf("a server with no OAuth state logged %q; that is the normal case, not an event", msg)
	}

	// Latched: the second call reads the vault for the credential itself, and
	// must NOT go looking for OAuth state again.
	before := vault.reads.Load()
	if _, _, err := p.Token(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := vault.reads.Load() - before; got != 1 {
		t.Errorf("the second Token() made %d vault reads, want 1 — the decision is not latched", got)
	}
}

// TestProactiveSourceReportsNoDeadlineWithoutAnExpiry: "no expires_in" means
// "never expires", not "expired" (docs/modules/oauth.md). Such a server stays
// on the passive 401/403 path, and a deadline would drag it off it.
func TestProactiveSourceReportsNoDeadlineWithoutAnExpiry(t *testing.T) {
	t.Parallel()
	now := time.Unix(1_000_000, 0)
	p, vault, _ := newFreshFixture(t, now)

	st := &oauthflow.State{TokenEndpoint: "https://as.example/token", ClientID: "c", RefreshToken: "r"}
	if err := oauthflow.NewStore(vault).Save(context.Background(), "srv", st, "at-eternal"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := p.Token(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := p.NotAfter(); !got.IsZero() {
		t.Errorf("NotAfter() = %v, want zero for a token that advertises no expiry", got)
	}
}

// TestProactiveSourceHoldsOffAfterAFailedRenewal is the property that keeps a
// failing provider from being hit once per request: the retry schedule IS the
// deadline, so the round tripper keeps the credential it has until the rung
// elapses and the next request is the retry.
//
// It also pins the fail-soft direction — the stored token is still handed
// over — and the log line, which is the only trace a standalone gateway's
// failed renewal leaves anywhere.
func TestProactiveSourceHoldsOffAfterAFailedRenewal(t *testing.T) {
	t.Parallel()
	now := time.Unix(1_000_000, 0)
	p, vault, h := newFreshFixture(t, now)

	// Due for a refresh, and the token endpoint is a literal loopback address
	// the OAuth client's SSRF screen refuses: a renewal that always fails,
	// with no authorization server involved.
	st := &oauthflow.State{
		TokenEndpoint: "http://127.0.0.1:1/token", ClientID: "c", RefreshToken: "r",
		IssuedAt: now.Add(-time.Hour).Unix(), ExpiresAt: now.Add(10 * time.Second).Unix(),
	}
	if err := oauthflow.NewStore(vault).Save(context.Background(), "srv", st, "at-stale"); err != nil {
		t.Fatal(err)
	}

	tok, ok, err := p.Token(context.Background())
	if err != nil || !ok {
		t.Fatalf("Token() = %q, %v, %v; a failed renewal must not fail the request", tok, ok, err)
	}
	if tok != "at-stale" {
		t.Fatalf("token = %q, want the stored one: fail-soft hands over what there is", tok)
	}

	want := now.Add(oauthflow.RetryBackoff(1))
	if got := p.NotAfter(); !got.Equal(want) {
		t.Errorf("NotAfter() = %v, want one backoff rung out at %v", got, want)
	}

	r, found := h.find("access token refresh failed")
	if !found {
		t.Fatalf("no failure was logged; got %q", h.messages())
	}
	if v, ok := attr(r, "trigger"); !ok || v.String() != oauthflow.TriggerExpiry {
		t.Errorf("trigger = %v, want %q — this is the proactive path, not a rejection", v, oauthflow.TriggerExpiry)
	}
	if v, ok := attr(r, "retry_in"); !ok || v.Duration() != oauthflow.RetryBackoff(1) {
		t.Errorf("retry_in = %v, want %v", v, oauthflow.RetryBackoff(1))
	}

	// Inside the rung: no second attempt, and nothing new logged.
	before := strings.Count(h.messages(), ";")
	if _, _, err := p.Token(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := strings.Count(h.messages(), ";"); got != before {
		t.Error("a second renewal ran inside the backoff rung: the provider is being hit once per request")
	}

	// Past the rung: the next request retries, and the rung widens.
	p.now = func() time.Time { return want.Add(time.Second) }
	if _, _, err := p.Token(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got, want2 := p.NotAfter(), want.Add(time.Second).Add(oauthflow.RetryBackoff(2)); !got.Equal(want2) {
		t.Errorf("after the second failure NotAfter() = %v, want %v", got, want2)
	}
}

// TestProactiveSourceIsWiredIntoTheGatewaysAuth is the assembly check: it is
// possible to build every part of this and leave the round tripper looking at
// a source that does not carry the deadline face, in which case the whole
// file silently reverts to passive-only. WithEpoch is what would swallow it.
func TestProactiveSourceIsWiredIntoTheGatewaysAuth(t *testing.T) {
	t.Parallel()
	// Nothing here reads the vault — the assertion is about the SHAPE the
	// wiring hands the round tripper — so an empty chain over a temp dir is
	// all this needs.
	chain := secrets.NewChain(secrets.ChainConfig{
		Dir:       t.TempDir(),
		LookupEnv: func(string) (string, bool) { return "", false },
	})
	build := vaultAuth(chain, t.TempDir(), &credEpochs{}, slog.New(&recordHandler{}))

	ts := build("srv", secrets.DefaultScope)
	if _, ok := ts.(interface{ NotAfter() time.Time }); !ok {
		t.Fatal("the gateway's TokenSource does not carry a credential deadline: proactive refresh is off")
	}
}
