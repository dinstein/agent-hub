package daemon

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/dinstein/agent-hub/internal/ctlapi"
	"github.com/dinstein/agent-hub/internal/oauthflow"
	"github.com/dinstein/agent-hub/internal/registry"
	"github.com/dinstein/agent-hub/internal/secrets"
)

// memStore is an in-memory secrets.Store: the daemon's refresh coordination
// is tested without ever touching the OS keyring.
type memStore struct {
	mu sync.Mutex
	m  map[string]string
}

func newMemStore() *memStore { return &memStore{m: map[string]string{}} }

func (s *memStore) Get(_ context.Context, ref secrets.Ref) (string, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	v, ok := s.m[ref.StorageKey()]
	return v, ok, nil
}

func (s *memStore) Set(_ context.Context, ref secrets.Ref, val string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.m[ref.StorageKey()] = val
	return nil
}

func (s *memStore) Delete(_ context.Context, ref secrets.Ref) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.m, ref.StorageKey())
	return nil
}

func (s *memStore) get(t *testing.T, ref secrets.Ref) string {
	t.Helper()
	v, _, _ := s.Get(context.Background(), ref)
	return v
}

// fakeAS is a token endpoint that rotates the refresh token on every use —
// the shape that makes double-spending fatal.
type fakeAS struct {
	srv    *httptest.Server
	calls  atomic.Int64
	fail   atomic.Bool
	refuse atomic.Bool
	issued atomic.Int64

	// presented records every refresh_token this endpoint was asked to
	// exchange, in order. A real rotating AS invalidates the old token on
	// each exchange, so a repeat here is the double-spend that locks the
	// user out — the thing the singleflight exists to prevent.
	presentedMu sync.Mutex
	presented   []string
}

// presentedTokens returns a copy of the refresh tokens presented so far.
func (as *fakeAS) presentedTokens() []string {
	as.presentedMu.Lock()
	defer as.presentedMu.Unlock()
	return append([]string(nil), as.presented...)
}

func newFakeAS(t *testing.T) *fakeAS {
	t.Helper()
	as := &fakeAS{}
	as.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		as.calls.Add(1)
		if err := r.ParseForm(); err == nil {
			if rt := r.PostForm.Get("refresh_token"); rt != "" {
				as.presentedMu.Lock()
				as.presented = append(as.presented, rt)
				as.presentedMu.Unlock()
			}
		}
		if as.refuse.Load() {
			// The terminal shape: 400 + invalid_grant is the answer a
			// revoked, spent or rotated-away refresh token gets, and no
			// retry survives it.
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"error":"invalid_grant","error_description":"consent withdrawn"}`))
			return
		}
		if as.fail.Load() {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"error":"temporarily_unavailable"}`))
			return
		}
		n := as.issued.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprintf(w, `{"access_token":"access-%d","refresh_token":"refresh-%d","token_type":"Bearer","expires_in":3600}`, n, n)
	}))
	t.Cleanup(as.srv.Close)
	return as
}

// seedState writes an OAuth state that is already due for a proactive
// refresh (expired 10s ago).
func seedState(t *testing.T, store *memStore, id, tokenEndpoint string, expiresAt int64) {
	t.Helper()
	st := oauthflow.State{
		TokenEndpoint: tokenEndpoint,
		Issuer:        "https://as.example",
		ClientID:      "client-1",
		RefreshToken:  "refresh-0",
		IssuedAt:      time.Now().Add(-time.Hour).Unix(),
		ExpiresAt:     expiresAt,
		TokenType:     "Bearer",
	}
	raw, err := json.Marshal(st)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Set(context.Background(), secrets.OAuthStateRef(id), string(raw)); err != nil {
		t.Fatal(err)
	}
	if err := store.Set(context.Background(), secrets.HTTPAuthRef(id), "access-0"); err != nil {
		t.Fatal(err)
	}
}

func testRegistry(t *testing.T, entries map[string]registry.ServerEntry) *registry.Store {
	t.Helper()
	store, err := registry.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Update(context.Background(), func(tx *registry.Tx) error {
		for id, e := range entries {
			tx.Servers.V.Servers[id] = registry.Doc[registry.ServerEntry]{V: e}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	return store
}

func testRefresher(t *testing.T, cfg refresherConfig) *refresher {
	t.Helper()
	if cfg.Log == nil {
		cfg.Log = slog.New(slog.DiscardHandler)
	}
	if cfg.LockDir == "" {
		cfg.LockDir = t.TempDir()
	}
	cfg.AllowLoopback = true
	return newRefresher(cfg)
}

// TestRefresherRenewsExpiringToken is the core proactive path: a token
// inside the 60s grace is renewed, and the vault ends up with BOTH the new
// access token and the rotated refresh token.
func TestRefresherRenewsExpiringToken(t *testing.T) {
	as := newFakeAS(t)
	vault := newMemStore()
	seedState(t, vault, "remote", as.srv.URL+"/token", time.Now().Add(-10*time.Second).Unix())

	r := testRefresher(t, refresherConfig{
		Store:   testRegistry(t, map[string]registry.ServerEntry{"remote": {Transport: "http", URL: "https://x/mcp", Enabled: true}}),
		Secrets: vault,
	})
	r.cycle(context.Background())

	if got := as.calls.Load(); got != 1 {
		t.Fatalf("token endpoint calls = %d, want 1", got)
	}
	if got := vault.get(t, secrets.HTTPAuthRef("remote")); got != "access-1" {
		t.Fatalf("access token = %q, want the refreshed one", got)
	}
	var st oauthflow.State
	if err := json.Unmarshal([]byte(vault.get(t, secrets.OAuthStateRef("remote"))), &st); err != nil {
		t.Fatal(err)
	}
	if st.RefreshToken != "refresh-1" {
		t.Fatalf("refresh token = %q, want the rotated one", st.RefreshToken)
	}
	if st.ExpiresAt <= time.Now().Unix() {
		t.Fatalf("expires_at = %d, want a future expiry", st.ExpiresAt)
	}
}

// TestRefresherLeavesHealthyAndNeverExpiringTokensAlone pins the two
// non-refresh cases: a token that is not near expiry, and a token with no
// expiry at all (docs/modules/oauth.md: "no expires_in" means never expires, and
// refreshing it on a timer would be a permanent refresh storm).
func TestRefresherLeavesHealthyAndNeverExpiringTokensAlone(t *testing.T) {
	as := newFakeAS(t)
	vault := newMemStore()
	seedState(t, vault, "healthy", as.srv.URL+"/token", time.Now().Add(time.Hour).Unix())
	seedState(t, vault, "eternal", as.srv.URL+"/token", 0)

	r := testRefresher(t, refresherConfig{
		Store: testRegistry(t, map[string]registry.ServerEntry{
			"healthy": {Transport: "http", URL: "https://x/mcp", Enabled: true},
			"eternal": {Transport: "http", URL: "https://y/mcp", Enabled: true},
		}),
		Secrets: vault,
	})
	r.cycle(context.Background())

	if got := as.calls.Load(); got != 0 {
		t.Fatalf("token endpoint calls = %d, want 0", got)
	}
}

// TestRefresherIgnoresStdioAndDisabledServers: only enabled HTTP servers
// have credentials worth renewing.
func TestRefresherIgnoresStdioAndDisabledServers(t *testing.T) {
	as := newFakeAS(t)
	vault := newMemStore()
	seedState(t, vault, "stdio", as.srv.URL+"/token", time.Now().Add(-time.Minute).Unix())
	seedState(t, vault, "off", as.srv.URL+"/token", time.Now().Add(-time.Minute).Unix())

	r := testRefresher(t, refresherConfig{
		Store: testRegistry(t, map[string]registry.ServerEntry{
			"stdio": {Transport: "stdio", Command: "x", Enabled: true},
			"off":   {Transport: "http", URL: "https://x/mcp", Enabled: false},
		}),
		Secrets: vault,
	})
	r.cycle(context.Background())

	if got := as.calls.Load(); got != 0 {
		t.Fatalf("token endpoint calls = %d, want 0", got)
	}
}

// TestRefresherBacksOffOnFailure pins the failure direction: the old token
// is kept, the server is put on the 15s retry, and the next cycle does NOT
// hammer the authorization server.
func TestRefresherBacksOffOnFailure(t *testing.T) {
	as := newFakeAS(t)
	as.fail.Store(true)
	vault := newMemStore()
	seedState(t, vault, "remote", as.srv.URL+"/token", time.Now().Add(-10*time.Second).Unix())

	r := testRefresher(t, refresherConfig{
		Store:   testRegistry(t, map[string]registry.ServerEntry{"remote": {Transport: "http", URL: "https://x/mcp", Enabled: true}}),
		Secrets: vault,
	})
	wait := r.cycle(context.Background())

	if as.calls.Load() != 1 {
		t.Fatalf("calls = %d, want 1", as.calls.Load())
	}
	if got := vault.get(t, secrets.HTTPAuthRef("remote")); got != "access-0" {
		t.Fatalf("access token = %q, want the old one kept", got)
	}
	if wait <= 0 || wait > oauthflow.RefreshRetryBackoff {
		t.Fatalf("next scan in %s, want at most the %s retry backoff", wait, oauthflow.RefreshRetryBackoff)
	}

	// Second cycle inside the backoff window must not retry.
	r.cycle(context.Background())
	if as.calls.Load() != 1 {
		t.Fatalf("calls = %d, want the backoff to suppress the retry", as.calls.Load())
	}
	if r.failCount("remote") != 1 {
		t.Fatalf("failure count = %d, want 1", r.failCount("remote"))
	}
}

// TestRefresherParksServersNeedingALogin: a state with no refresh token
// cannot be renewed by any amount of retrying, so it goes straight to the
// slowest rung instead of burning cycles.
func TestRefresherParksServersNeedingALogin(t *testing.T) {
	as := newFakeAS(t)
	vault := newMemStore()
	st := oauthflow.State{
		TokenEndpoint: as.srv.URL + "/token",
		ClientID:      "c",
		IssuedAt:      time.Now().Add(-time.Hour).Unix(),
		ExpiresAt:     time.Now().Add(-time.Minute).Unix(),
	}
	raw, _ := json.Marshal(st)
	_ = vault.Set(context.Background(), secrets.OAuthStateRef("remote"), string(raw))

	r := testRefresher(t, refresherConfig{
		Store:   testRegistry(t, map[string]registry.ServerEntry{"remote": {Transport: "http", URL: "https://x/mcp", Enabled: true}}),
		Secrets: vault,
	})
	r.cycle(context.Background())

	if as.calls.Load() != 0 {
		t.Fatalf("calls = %d, want 0 (nothing to send)", as.calls.Load())
	}
	hold, ok := r.hold("remote", 0)
	if !ok || time.Until(hold) < time.Hour {
		t.Fatalf("retryAt = %v, want the slow ladder", hold)
	}
}

// TestRefresherStopsAskingAfterARefusal: the ladder's slowest rung is still
// a request every 24 hours, and the answer to every one of them is on file
// from the first. What the counter proves here is the difference between
// "backs off" and "stops" — the two are indistinguishable in a log, and only
// one of them is what a revoked credential deserves.
func TestRefresherStopsAskingAfterARefusal(t *testing.T) {
	as := newFakeAS(t)
	as.refuse.Store(true)
	vault := newMemStore()
	seedState(t, vault, "remote", as.srv.URL+"/token", time.Now().Add(-10*time.Second).Unix())

	r := testRefresher(t, refresherConfig{
		Store:   testRegistry(t, map[string]registry.ServerEntry{"remote": {Transport: "http", URL: "https://x/mcp", Enabled: true}}),
		Secrets: vault,
	})
	r.cycle(context.Background())
	if as.calls.Load() != 1 {
		t.Fatalf("calls = %d, want the one ask that earned the refusal", as.calls.Load())
	}

	var st oauthflow.State
	if err := json.Unmarshal([]byte(vault.get(t, secrets.OAuthStateRef("remote"))), &st); err != nil {
		t.Fatal(err)
	}
	if !st.GrantRevoked() {
		t.Fatalf("the refusal was not recorded: %+v", st)
	}

	for range 5 {
		r.cycle(context.Background())
	}
	if as.calls.Load() != 1 {
		t.Fatalf("calls = %d after six cycles, want the scan to skip a revoked grant entirely", as.calls.Load())
	}

	// A fresh login rewrites the state without the mark. The scan reads the
	// vault every cycle, so recovery needs no signal from anywhere. The
	// credential seeded here is deliberately due already — a login normally
	// yields an hour of validity and nothing would happen for an hour, which
	// would test the clock rather than the recovery.
	as.refuse.Store(false)
	seedState(t, vault, "remote", as.srv.URL+"/token", time.Now().Add(-5*time.Second).Unix())
	r.cycle(context.Background())
	if as.calls.Load() != 2 {
		t.Fatalf("calls = %d, want the fresh credential to be renewed", as.calls.Load())
	}
}

// TestRefresherPublishesEveryCredentialsState: the control plane's token rung
// had no producer at all, so an expired or refused credential reached the GUI
// only as a failed connection, and only if somebody was connected. This loop
// is the producer because it already reads exactly that state — a second
// reader would mean a second vault access, and on macOS that means a keychain
// dialog.
func TestRefresherPublishesEveryCredentialsState(t *testing.T) {
	as := newFakeAS(t)
	vault := newMemStore()
	tokens := ctlapi.NewTokenStates()

	// healthy: hours left. refused: the provider says no. Both are enabled
	// HTTP servers, so both are scanned.
	seedState(t, vault, "healthy", as.srv.URL+"/token", time.Now().Add(time.Hour).Unix())
	seedState(t, vault, "refused", as.srv.URL+"/token", time.Now().Add(-10*time.Second).Unix())
	as.refuse.Store(true)

	entry := registry.ServerEntry{Transport: "http", URL: "https://x/mcp", Enabled: true}
	r := testRefresher(t, refresherConfig{
		Store: testRegistry(t, map[string]registry.ServerEntry{
			"healthy": entry, "refused": entry, "unauthorized": entry,
		}),
		Secrets:     vault,
		TokenStates: tokens,
	})
	r.cycle(context.Background())

	if f, ok := tokens.TokenState("healthy"); !ok || f.State != ctlapi.TokenOK || !f.HasRefreshToken {
		t.Errorf("healthy = %+v, %t", f, ok)
	}
	f, ok := tokens.TokenState("refused")
	if !ok || f.State != ctlapi.TokenRevoked {
		t.Errorf("refused = %+v, %t, want revoked", f, ok)
	}
	if f.HasRefreshToken {
		t.Error("a refused grant must not advertise an unattended repair: " +
			"`auth refresh` for it can only fail")
	}
	// A server with no credential at all is not a credential in an unknown
	// state: it must be absent, so the health contract keeps its own answer.
	if _, ok := tokens.TokenState("unauthorized"); ok {
		t.Error("a server with no OAuth state must not appear in the snapshot")
	}

	// A renewal that just succeeded is reported as healthy immediately.
	// Publishing the state read BEFORE the refresh would show "expiring" for
	// up to a scan interval after every successful renewal — a degraded badge
	// for the one outcome that is entirely fine.
	as.refuse.Store(false)
	seedState(t, vault, "healthy", as.srv.URL+"/token", time.Now().Add(-10*time.Second).Unix())
	r.cycle(context.Background())
	if f, ok := tokens.TokenState("healthy"); !ok || f.State != ctlapi.TokenOK {
		t.Errorf("after a successful renewal = %+v, %t, want ok", f, ok)
	}
}

// TestRefresherConcurrentRefreshNeverDoubleSpends proves what concurrent
// cycles must never do: present the same refresh token twice. This AS rotates
// on every exchange, so a repeat is a token a real server would already have
// invalidated, and the user is locked out until they log in again.
//
// The test was called TestRefresherSingleflight and asserted "1 token endpoint
// call, 2 tolerated for a late arrival". That bound measured goroutine
// scheduling rather than correctness: under CPU contention the eight callers
// form three waves instead of two and it failed with 3, having found nothing
// wrong — three exchanges of three successive tokens is exactly as safe as one.
//
// It also did not test what its name said. Removing r.group entirely leaves
// the count at 1, because this configuration takes the OFFLINE path
// (refresherConfig sets no Online hook), where the sibling refresh lock
// serializes callers and each one re-reads the state before exchanging. Either
// layer alone is sufficient: bypass the refresher's singleflight and the lock
// still holds; drop the lock and the supersede re-read and the singleflight
// still holds. Remove BOTH and the endpoint sees refresh-0 eight times — which
// is how the assertion below was confirmed to fire rather than to be decorative.
//
// So the guarantee is defence in depth, and the duplicate check is what covers
// it: it passes for either layer and fails only when the last one is gone.
func TestRefresherConcurrentRefreshNeverDoubleSpends(t *testing.T) {
	as := newFakeAS(t)
	vault := newMemStore()
	seedState(t, vault, "remote", as.srv.URL+"/token", time.Now().Add(-10*time.Second).Unix())

	r := testRefresher(t, refresherConfig{
		Store:   testRegistry(t, map[string]registry.ServerEntry{"remote": {Transport: "http", URL: "https://x/mcp", Enabled: true}}),
		Secrets: vault,
	})

	var wg sync.WaitGroup
	start := make(chan struct{})
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			r.refreshOne(context.Background(), "remote", time.Now(), 0)
		}()
	}
	close(start)
	wg.Wait()

	// Every exchange must have presented a DIFFERENT refresh token. A caller
	// that arrives late re-reads the state, sees expires_at advanced and
	// abandons (ErrRefreshSuperseded); one that proceeds does so with the
	// current token. A repeat means two callers exchanged the same token, and
	// against a rotating AS the second one is rejected and the login is dead.
	presented := as.presentedTokens()
	seen := make(map[string]bool, len(presented))
	for _, tok := range presented {
		if seen[tok] {
			t.Fatalf("refresh token %q presented more than once (double-spend): %v", tok, presented)
		}
		seen[tok] = true
	}

	// The call COUNT is deliberately only bounded below. It used to be pinned
	// to "1, 2 tolerated for a late arrival", which made the test a function of
	// goroutine scheduling: under CPU contention the eight callers form three
	// waves instead of two and it failed with 3 — having found nothing wrong,
	// because three exchanges of three successive tokens is exactly as safe as
	// one. What is unsafe is a repeat, and that is asserted above.
	if got := as.calls.Load(); got < 1 {
		t.Fatalf("token endpoint calls = %d, want the refresh to have happened at all", got)
	}
	if r.failCount("remote") != 0 {
		t.Fatalf("failures recorded: %d", r.failCount("remote"))
	}
}

// TestRefresherRunStopsWithContext: the loop is owned by the daemon's
// background context and must not outlive it.
func TestRefresherRunStopsWithContext(t *testing.T) {
	vault := newMemStore()
	cycles := make(chan int, 8)
	r := testRefresher(t, refresherConfig{
		Store:   testRegistry(t, nil),
		Secrets: vault,
		MinScan: time.Millisecond,
		MaxScan: 5 * time.Millisecond,
		OnCycle: func(n int) {
			select {
			case cycles <- n:
			default:
			}
		},
	})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { r.run(ctx); close(done) }()

	select {
	case <-cycles:
	case <-time.After(2 * time.Second):
		t.Fatal("no scan cycle ran")
	}
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("refresher outlived its context")
	}
}

// TestRefresherSurvivesUnreadableState: one corrupt entry must not stop the
// others from being refreshed.
func TestRefresherSurvivesUnreadableState(t *testing.T) {
	as := newFakeAS(t)
	vault := newMemStore()
	_ = vault.Set(context.Background(), secrets.OAuthStateRef("broken"), "{not json")
	seedState(t, vault, "good", as.srv.URL+"/token", time.Now().Add(-10*time.Second).Unix())

	r := testRefresher(t, refresherConfig{
		Store: testRegistry(t, map[string]registry.ServerEntry{
			"broken": {Transport: "http", URL: "https://x/mcp", Enabled: true},
			"good":   {Transport: "http", URL: "https://y/mcp", Enabled: true},
		}),
		Secrets: vault,
	})
	r.cycle(context.Background())

	if got := vault.get(t, secrets.HTTPAuthRef("good")); got != "access-1" {
		t.Fatalf("healthy server was not refreshed: %q", got)
	}
}

// TestRefresherBackoffClearedByFreshLogin: a long suppression earned by
// dead credentials must not outlive them. `agenthub auth login` writes a
// state with a later expires_at, and the very next cycle must treat the
// server as healthy again.
func TestRefresherBackoffClearedByFreshLogin(t *testing.T) {
	as := newFakeAS(t)
	vault := newMemStore()
	// No refresh token: the server is parked on the slowest rung.
	st := oauthflow.State{
		TokenEndpoint: as.srv.URL + "/token",
		ClientID:      "c",
		IssuedAt:      time.Now().Add(-time.Hour).Unix(),
		ExpiresAt:     time.Now().Add(-time.Minute).Unix(),
	}
	raw, _ := json.Marshal(st)
	_ = vault.Set(context.Background(), secrets.OAuthStateRef("remote"), string(raw))

	r := testRefresher(t, refresherConfig{
		Store:   testRegistry(t, map[string]registry.ServerEntry{"remote": {Transport: "http", URL: "https://x/mcp", Enabled: true}}),
		Secrets: vault,
	})
	r.cycle(context.Background())
	if _, parked := r.hold("remote", st.ExpiresAt); !parked {
		t.Fatal("server was not parked after an unrecoverable failure")
	}

	// A human runs `auth login`: new credentials, later expiry — but again
	// already inside the refresh grace, so the next cycle must act.
	seedState(t, vault, "remote", as.srv.URL+"/token", time.Now().Add(30*time.Second).Unix())
	r.cycle(context.Background())

	if got := vault.get(t, secrets.HTTPAuthRef("remote")); got != "access-1" {
		t.Fatalf("access token = %q, want the refreshed one (the stale backoff blocked it)", got)
	}
	if n := r.failCount("remote"); n != 0 {
		t.Fatalf("failure count = %d, want it cleared by the fresh login", n)
	}
}
