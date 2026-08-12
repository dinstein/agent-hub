package gateway

import (
	"context"
	"io"
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
	// No epoch: these tests are about the schedule a source decides on its
	// own. The one that is about the announcement plane installs its own.
	p := newProactiveSource(
		downstream.NewScopedVaultTokenSource("srv", secrets.DefaultScope, vault.resolve, nil),
		coord, "srv", secrets.DefaultScope, nil, slog.New(h))
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

// seedRevoked stores a state the provider has already refused. The token
// endpoint is a literal loopback address the SSRF screen refuses, so if the
// recorded refusal were ever ignored the renewal would fail loudly rather
// than quietly reaching a real server.
func seedRevoked(t *testing.T, vault *memVault, now time.Time) {
	t.Helper()
	st := &oauthflow.State{
		TokenEndpoint: "http://127.0.0.1:1/token", ClientID: "c", RefreshToken: "r",
		IssuedAt: now.Add(-time.Hour).Unix(), ExpiresAt: now.Add(10 * time.Second).Unix(),
		GrantRevokedAt: now.Add(-time.Minute).Unix(), GrantRevokedReason: "invalid_grant: consent withdrawn",
	}
	if err := oauthflow.NewStore(vault).Save(context.Background(), "srv", st, "at-stale"); err != nil {
		t.Fatal(err)
	}
}

// TestProactiveSourceParksARevokedGrant: the credential is still handed over
// — it may well still be accepted — but the renewal is held at the slowest
// rung and says the one thing an operator can act on.
func TestProactiveSourceParksARevokedGrant(t *testing.T) {
	t.Parallel()
	now := time.Unix(1_000_000, 0)
	p, vault, h := newFreshFixture(t, now)
	seedRevoked(t, vault, now)

	tok, ok, err := p.Token(context.Background())
	if err != nil || !ok || tok != "at-stale" {
		t.Fatalf("Token() = %q, %v, %v; a refused grant must not take the access token away", tok, ok, err)
	}
	if _, found := h.find("token cannot be refreshed without a new login"); !found {
		t.Fatalf("the refusal was not reported; got %q", h.messages())
	}
	want := now.Add(oauthflow.RetryBackoff(oauthflow.FastRetries + 5))
	if got := p.NotAfter(); !got.Equal(want) {
		t.Errorf("NotAfter() = %v, want the slowest rung at %v", got, want)
	}
}

// TestProactiveSourceForgetsAScheduleTakenAboutOldCredentials: the rung above
// is a day long, and `auth login` is what an operator does about it. Without
// the epoch the fix would sit unused until the rung expired — on a credential
// that no longer exists.
func TestProactiveSourceForgetsAScheduleTakenAboutOldCredentials(t *testing.T) {
	t.Parallel()
	now := time.Unix(1_000_000, 0)
	p, vault, _ := newFreshFixture(t, now)
	var epoch atomic.Uint64
	p.epoch = epoch.Load
	seedRevoked(t, vault, now)

	if _, _, err := p.Token(context.Background()); err != nil {
		t.Fatal(err)
	}
	if p.NotAfter().IsZero() {
		t.Fatal("setup: the refusal must have earned a hold")
	}
	if p.due(now) {
		t.Fatal("inside the hold and at the same epoch, nothing is due")
	}

	// A login lands: new credentials, and the announcement plane says so.
	st := &oauthflow.State{
		TokenEndpoint: "https://as.example/token", ClientID: "c", RefreshToken: "r2",
		IssuedAt: now.Unix(), ExpiresAt: now.Add(time.Hour).Unix(),
	}
	if err := oauthflow.NewStore(vault).Save(context.Background(), "srv", st, "at-new"); err != nil {
		t.Fatal(err)
	}
	epoch.Add(1)

	if !p.due(now) {
		t.Fatal("a credential change must retire a schedule decided about the credential it replaced")
	}
	if _, _, err := p.Token(context.Background()); err != nil {
		t.Fatal(err)
	}
	want, _ := st.RefreshAt()
	if got := p.NotAfter(); !got.Equal(want) {
		t.Errorf("NotAfter() = %v, want the new credential's own schedule %v", got, want)
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

// TestUninjectedAssemblyCarriesBothCredentialFaces asks the same question one
// level up, of the ASSEMBLY rather than of vaultAuth: does a gateway built the
// way production builds one actually end up with the wiring above attached?
//
// The test above can pass while this one fails, and that is the whole reason
// this one exists. vaultAuth is reached only from the branch newGateway takes
// when Config.Secrets is nil, and everything that branch produces travels
// together — the deadline face, the epoch face, and the credEpochs counter set
// that startCredWatch refuses to run without. A caller that hands in a
// credential of its own skips the branch and gets none of the three, while the
// bearer is still attached and the vault is still read, so nothing observable
// changes until a token needs replacing inside a live connection.
//
// The daemon's HTTP data plane was such a caller. Its gateways held a bare
// vault read that no announcement and no deadline could invalidate, leaving
// them recoverable only by a downstream rejection — which a server answering
// an expired token with 200 and an error result never issues. The daemon half
// of that rule is pinned by TestDataPlaneLeavesCredentialsToTheGateway; this
// is the half that says what the nil is FOR.
func TestUninjectedAssemblyCarriesBothCredentialFaces(t *testing.T) {
	t.Parallel()
	// Secrets and Auth deliberately unset: this is the production shape, for
	// the stdio gateway and the data plane's in-process ones alike.
	g, err := newGateway(Config{
		ClientID: "uninjected",
		In:       strings.NewReader(""),
		Out:      io.Discard,
		Resolver: testResolver(t.TempDir()),
		Log:      slog.New(slog.DiscardHandler),
	})
	if err != nil {
		t.Fatalf("newGateway: %v", err)
	}
	defer g.shutdown()

	if g.cfg.Auth == nil {
		t.Fatal("no bearer factory: every OAuth downstream would be dialed bare and answer 401")
	}
	ts := g.cfg.Auth("srv", secrets.DefaultScope)
	if _, ok := ts.(interface{ NotAfter() time.Time }); !ok {
		t.Error("the assembled TokenSource carries no refresh deadline: a credential that ages out " +
			"inside a live connection is never renewed, and a downstream answering an expired " +
			"token with 200 rather than 401 is never recovered from at all")
	}
	if _, ok := ts.(interface{ Epoch() uint64 }); !ok {
		t.Error("the assembled TokenSource carries no credential epoch: a login or refresh by any " +
			"other process cannot drop the cached bearer, so the daemon's proactive refresher " +
			"cannot reach a connection that is already up")
	}

	// The epoch face is only half of that second rule — something has to move
	// the counter. startCredWatch is the subscriber, and its first line is a
	// nil check on exactly the field an injected credential leaves unset, so
	// asserting the counter set alone would not prove the announcement plane
	// is connected. Called directly rather than through run(): shutdown closes
	// the watcher either way, and the write to g.credWatcher is unlocked, so
	// letting the run loop race this assertion would be a data race rather
	// than a test.
	if g.credEpochs == nil {
		t.Fatal("no credential epoch counters: startCredWatch returns before it subscribes")
	}
	g.startCredWatch()
	if g.credWatcher == nil {
		t.Error("no credential watcher: nothing bumps the epoch, so the face above never fires")
	}
}
