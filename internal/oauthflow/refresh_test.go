package oauthflow

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

func seedState(t *testing.T, s *Store, as *fakeAS, serverID string, expiresAt int64) {
	t.Helper()
	st := &State{
		TokenEndpoint: as.srv.URL + "/token",
		ClientID:      "client-abc",
		RefreshToken:  "refresh-0",
		IssuedAt:      1,
		ExpiresAt:     expiresAt,
	}
	if err := s.Save(context.Background(), serverID, st, "at-old"); err != nil {
		t.Fatal(err)
	}
}

func TestRefreshRotatesAndPersists(t *testing.T) {
	as := newFakeAS(t)
	as.rotateRefresh = true
	v := newFakeVault()
	store := NewStore(v)
	seedState(t, store, as, "gh", 100)

	co := NewCoordinator(CoordinatorConfig{
		Store: store, Client: as.client(), LockDir: t.TempDir(),
		Now: func() time.Time { return time.Unix(1000, 0) },
	})
	st, tok, err := co.Refresh(context.Background(), "gh")
	if err != nil {
		t.Fatalf("refresh: %v", err)
	}
	if tok != "access-refreshed-1" {
		t.Fatalf("token = %q", tok)
	}
	if st.RefreshToken != "refresh-1" {
		t.Fatalf("rotated refresh token not stored: %q", st.RefreshToken)
	}
	if st.ExpiresAt != 1000+3600 {
		t.Fatalf("expires_at = %d", st.ExpiresAt)
	}
	// The refresh must obey the same write ordering as a login.
	log := v.writeLog()
	last := log[len(log)-2:]
	if last[0] != "__oauth_state__" || last[1] != "__http_auth__" {
		t.Fatalf("refresh write order = %v", log)
	}
}

func TestRefreshSendsResourceBinding(t *testing.T) {
	as := newFakeAS(t)
	store := NewStore(newFakeVault())
	st := &State{TokenEndpoint: as.srv.URL + "/token", ClientID: "c", RefreshToken: "refresh-0",
		Resource: "https://mcp.example/mcp", IssuedAt: 1, ExpiresAt: 100}
	if err := store.Save(context.Background(), "gh", st, "at"); err != nil {
		t.Fatal(err)
	}
	co := NewCoordinator(CoordinatorConfig{Store: store, Client: as.client(), LockDir: t.TempDir()})
	if _, _, err := co.Refresh(context.Background(), "gh"); err != nil {
		t.Fatal(err)
	}
	as.mu.Lock()
	form := as.lastTokenForm
	as.mu.Unlock()
	if form.Get("resource") != "https://mcp.example/mcp" {
		t.Fatalf("resource = %q; RFC 8707 binding must survive a refresh", form.Get("resource"))
	}
	if form.Get("grant_type") != GrantRefreshToken {
		t.Fatalf("grant_type = %q", form.Get("grant_type"))
	}
}

func TestRefreshWithoutRefreshToken(t *testing.T) {
	as := newFakeAS(t)
	store := NewStore(newFakeVault())
	st := &State{TokenEndpoint: as.srv.URL + "/token", ClientID: "c", IssuedAt: 1, ExpiresAt: 100}
	if err := store.Save(context.Background(), "gh", st, "at"); err != nil {
		t.Fatal(err)
	}
	co := NewCoordinator(CoordinatorConfig{Store: store, Client: as.client(), LockDir: t.TempDir()})
	_, _, err := co.Refresh(context.Background(), "gh")
	if !errors.Is(err, ErrNoRefreshToken) {
		t.Fatalf("err = %v, want ErrNoRefreshToken", err)
	}
	var fe *FlowError
	if !errors.As(err, &fe) || fe.ServerID != "gh" {
		t.Fatalf("server id not stamped: %+v", fe)
	}
}

// TestOfflineRefreshSerializesOnFileLock is ruling A.2 #10's offline half:
// concurrent refreshers take <server>.refresh.lock, and the loser sees the
// advanced expires_at and abandons rather than spending the (already
// rotated, already invalid) refresh token a second time.
func TestOfflineRefreshSerializesOnFileLock(t *testing.T) {
	as := newFakeAS(t)
	as.rotateRefresh = true
	as.refreshDelay = 60 * time.Millisecond
	store := NewStore(newFakeVault())
	seedState(t, store, as, "gh", 100)

	lockDir := t.TempDir()
	// Two independent Coordinators: no shared in-process state, so ONLY
	// the file lock plus the post-lock re-read can serialize them. This is
	// the shape two agenthub processes have.
	newCo := func() *Coordinator {
		return NewCoordinator(CoordinatorConfig{
			Store: store, Client: as.client(), LockDir: lockDir,
			Now: func() time.Time { return time.Unix(1000, 0) },
		})
	}

	const n = 5
	var wg sync.WaitGroup
	errs := make([]error, n)
	toks := make([]string, n)
	start := make(chan struct{})
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			_, tok, err := newCo().Refresh(context.Background(), "gh")
			errs[i], toks[i] = err, tok
		}(i)
	}
	close(start)
	wg.Wait()

	as.mu.Lock()
	refreshes := as.refreshCount
	as.mu.Unlock()
	if refreshes != 1 {
		t.Fatalf("the authorization server saw %d refreshes; a one-time refresh token was spent more than once", refreshes)
	}
	winners, superseded := 0, 0
	for i, err := range errs {
		switch {
		case err == nil:
			winners++
			if toks[i] != "access-refreshed-1" {
				t.Fatalf("winner %d got token %q", i, toks[i])
			}
		case errors.Is(err, ErrRefreshSuperseded):
			superseded++
			// A superseded caller must still be handed the LIVE token, or
			// it would retry with nothing.
			if toks[i] != "access-refreshed-1" {
				t.Fatalf("superseded caller %d got token %q, want the winner's", i, toks[i])
			}
		default:
			t.Fatalf("caller %d: %v", i, err)
		}
	}
	if winners != 1 {
		t.Fatalf("%d winners, want exactly 1", winners)
	}
	if superseded != n-1 {
		t.Fatalf("%d superseded, want %d", superseded, n-1)
	}
	if _, err := os.Stat(RefreshLockPath(lockDir, "gh")); err != nil {
		t.Fatalf("lock file missing: %v", err)
	}
}

// TestOnlineRefreshCollapsesOnSingleflight is the online half: one
// Coordinator, N callers, one network refresh — no file lock needed because
// the daemon is the only vault writer.
func TestOnlineRefreshCollapsesOnSingleflight(t *testing.T) {
	as := newFakeAS(t)
	as.rotateRefresh = true
	as.refreshDelay = 60 * time.Millisecond
	store := NewStore(newFakeVault())
	seedState(t, store, as, "gh", 100)

	lockDir := t.TempDir()
	co := NewCoordinator(CoordinatorConfig{
		Store: store, Client: as.client(), LockDir: lockDir,
		Online: func() bool { return true },
		Now:    func() time.Time { return time.Unix(1000, 0) },
	})

	const n = 8
	var wg sync.WaitGroup
	toks := make([]string, n)
	start := make(chan struct{})
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			_, tok, err := co.Refresh(context.Background(), "gh")
			if err != nil {
				t.Errorf("caller %d: %v", i, err)
				return
			}
			toks[i] = tok
		}(i)
	}
	close(start)
	wg.Wait()

	as.mu.Lock()
	refreshes := as.refreshCount
	as.mu.Unlock()
	if refreshes != 1 {
		t.Fatalf("the authorization server saw %d refreshes, want 1", refreshes)
	}
	for i, tok := range toks {
		if tok != "access-refreshed-1" {
			t.Fatalf("caller %d got %q", i, tok)
		}
	}
	// The online path must not have touched the file lock at all.
	if _, err := os.Stat(RefreshLockPath(lockDir, "gh")); !os.IsNotExist(err) {
		t.Fatalf("the online path created a refresh lock file: %v", err)
	}
}

// TestSingleflightGroup covers the primitive itself, including the
// context-honouring waiter.
func TestSingleflightGroup(t *testing.T) {
	var g Group[int]
	var calls int
	release := make(chan struct{})
	entered := make(chan struct{})

	var wg sync.WaitGroup
	results := make([]int, 4)
	shared := make([]bool, 4)
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			v, sh, err := g.Do(context.Background(), "k", func(context.Context) (int, error) {
				calls++
				close(entered)
				<-release
				return 42, nil
			})
			if err != nil {
				t.Errorf("caller %d: %v", i, err)
			}
			results[i], shared[i] = v, sh
		}(i)
	}
	<-entered
	time.Sleep(20 * time.Millisecond) // let the others queue
	close(release)
	wg.Wait()

	if calls != 1 {
		t.Fatalf("fn ran %d times, want 1", calls)
	}
	for i, v := range results {
		if v != 42 {
			t.Fatalf("caller %d got %d", i, v)
		}
	}
	sharedCount := 0
	for _, s := range shared {
		if s {
			sharedCount++
		}
	}
	if sharedCount != 3 {
		t.Fatalf("%d shared callers, want 3", sharedCount)
	}

	// A finished call must not be joined: the next Do starts fresh work.
	if _, sh, _ := g.Do(context.Background(), "k", func(context.Context) (int, error) { return 7, nil }); sh {
		t.Fatal("a completed call must not be shared with a later caller")
	}
}

func TestSingleflightWaiterHonoursItsOwnContext(t *testing.T) {
	var g Group[int]
	release := make(chan struct{})
	entered := make(chan struct{})
	go func() {
		_, _, _ = g.Do(context.Background(), "k", func(context.Context) (int, error) {
			close(entered)
			<-release
			return 1, nil
		})
	}()
	<-entered
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	_, shared, err := g.Do(ctx, "k", func(context.Context) (int, error) { return 2, nil })
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v, want DeadlineExceeded", err)
	}
	if !shared {
		t.Fatal("the waiter did join an in-flight call")
	}
	close(release)
}

func TestSingleflightKeysAreIndependent(t *testing.T) {
	var g Group[string]
	var wg sync.WaitGroup
	block := make(chan struct{})
	wg.Add(1)
	go func() {
		defer wg.Done()
		_, _, _ = g.Do(context.Background(), "a", func(context.Context) (string, error) {
			<-block
			return "a", nil
		})
	}()
	v, shared, err := g.Do(context.Background(), "b", func(context.Context) (string, error) { return "b", nil })
	if err != nil || v != "b" || shared {
		t.Fatalf("key b blocked on key a: %q %v %v", v, shared, err)
	}
	close(block)
	wg.Wait()
}

func TestEnsureFreshSkipsRefreshWhenTokenIsHealthy(t *testing.T) {
	as := newFakeAS(t)
	store := NewStore(newFakeVault())
	now := time.Unix(1000, 0)
	st := &State{TokenEndpoint: as.srv.URL + "/token", ClientID: "c", RefreshToken: "refresh-0",
		IssuedAt: now.Unix(), ExpiresAt: now.Add(time.Hour).Unix()}
	if err := store.Save(context.Background(), "gh", st, "at-live"); err != nil {
		t.Fatal(err)
	}
	co := NewCoordinator(CoordinatorConfig{
		Store: store, Client: as.client(), LockDir: t.TempDir(),
		Now: func() time.Time { return now },
	})
	_, tok, renewed, err := co.EnsureFresh(context.Background(), "gh")
	if err != nil {
		t.Fatal(err)
	}
	if tok != "at-live" {
		t.Fatalf("token = %q, want the stored one", tok)
	}
	if renewed {
		t.Error("renewed = true for a healthy token: the caller would log a renewal that never happened")
	}
	as.mu.Lock()
	defer as.mu.Unlock()
	if as.refreshCount != 0 {
		t.Fatal("a healthy token must not be refreshed")
	}
}

func TestEnsureFreshRefreshesInsideTheGraceWindow(t *testing.T) {
	as := newFakeAS(t)
	store := NewStore(newFakeVault())
	now := time.Unix(1000, 0)
	// Long-lived token, 30s from expiry: inside the 60s grace.
	st := &State{TokenEndpoint: as.srv.URL + "/token", ClientID: "c", RefreshToken: "refresh-0",
		IssuedAt: now.Add(-time.Hour).Unix(), ExpiresAt: now.Add(30 * time.Second).Unix()}
	if err := store.Save(context.Background(), "gh", st, "at-old"); err != nil {
		t.Fatal(err)
	}
	co := NewCoordinator(CoordinatorConfig{
		Store: store, Client: as.client(), LockDir: t.TempDir(),
		Now: func() time.Time { return now },
	})
	_, tok, renewed, err := co.EnsureFresh(context.Background(), "gh")
	if err != nil {
		t.Fatal(err)
	}
	if tok != "access-refreshed-1" {
		t.Fatalf("token = %q, want a refreshed one", tok)
	}
	if !renewed {
		t.Error("renewed = false although the token was renewed")
	}
}

// TestEnsureFreshRefreshesDCROnlyRecord: a record with state but no access
// token (ErrNoToken) is refreshable, not terminal.
func TestEnsureFreshRefreshesDCROnlyRecord(t *testing.T) {
	as := newFakeAS(t)
	v := newFakeVault()
	store := NewStore(v)
	seedState(t, store, as, "gh", 100)
	// Drop just the access token, leaving the DCR-only shape.
	v.mu.Lock()
	delete(v.data, vaultKey("gh", "_global", "__http_auth__"))
	v.mu.Unlock()

	co := NewCoordinator(CoordinatorConfig{Store: store, Client: as.client(), LockDir: t.TempDir()})
	_, tok, renewed, err := co.EnsureFresh(context.Background(), "gh")
	if err != nil {
		t.Fatalf("ensure fresh: %v", err)
	}
	if tok != "access-refreshed-1" {
		t.Fatalf("token = %q", tok)
	}
	if !renewed {
		t.Error("renewed = false although the DCR-only record was renewed")
	}
}

func TestOfflineRefreshNeedsALockDir(t *testing.T) {
	as := newFakeAS(t)
	store := NewStore(newFakeVault())
	seedState(t, store, as, "gh", 100)
	co := NewCoordinator(CoordinatorConfig{Store: store, Client: as.client()})
	if _, _, err := co.Refresh(context.Background(), "gh"); err == nil {
		t.Fatal("an offline refresh without a lock directory must be refused, not run unserialized")
	}
}

func TestRefreshLockPathIsSanitized(t *testing.T) {
	dir := "/var/lib/agenthub/secrets"
	if got := RefreshLockPath(dir, "github"); got != filepath.Join(dir, "github.refresh.lock") {
		t.Fatalf("path = %q", got)
	}
	// A server ID must never escape the lock directory.
	got := RefreshLockPath(dir, "../../etc/passwd")
	if filepath.Dir(got) != dir {
		t.Fatalf("path escaped the lock dir: %q", got)
	}
	if RefreshLockPath(dir, "..") != filepath.Join(dir, "_.refresh.lock") {
		t.Fatalf("path = %q", RefreshLockPath(dir, ".."))
	}
}

func TestShouldRefreshOnStatus(t *testing.T) {
	// 403 counts: several providers answer it for an expired token, and
	// call-time-only 401/403 servers exist too (docs/status/oauth.md).
	for _, code := range []int{401, 403} {
		if !ShouldRefreshOnStatus(code) {
			t.Fatalf("%d must trigger a passive refresh", code)
		}
	}
	for _, code := range []int{200, 400, 404, 429, 500} {
		if ShouldRefreshOnStatus(code) {
			t.Fatalf("%d must not trigger a refresh", code)
		}
	}
}

// TestSlowBackoffLadder pins the OAuth-specific ladder of docs/status/oauth.md:
// connection-time OAuth failures wait for a HUMAN, so the ordinary
// few-seconds exponential backoff is wrong.
func TestSlowBackoffLadder(t *testing.T) {
	want := []time.Duration{5 * time.Minute, 15 * time.Minute, time.Hour, 4 * time.Hour, 24 * time.Hour}
	for i, w := range want {
		if got := SlowBackoff(i); got != w {
			t.Fatalf("rung %d = %s want %s", i, got, w)
		}
	}
	if got := SlowBackoff(99); got != 24*time.Hour {
		t.Fatalf("saturation = %s", got)
	}
	if got := SlowBackoff(-1); got != 5*time.Minute {
		t.Fatalf("negative attempt = %s", got)
	}
	if RefreshGrace != 60*time.Second || RefreshRetryBackoff != 15*time.Second {
		t.Fatalf("docs/status/oauth.md fixes 60s grace / 15s retry, got %s / %s", RefreshGrace, RefreshRetryBackoff)
	}
}

// TestRefresherInterface keeps the seam honest: the daemon is handed a
// Refresher, not a *Coordinator, so it can later be swapped for an RPC to
// the daemon without touching the gateway.
func TestRefresherInterface(t *testing.T) {
	as := newFakeAS(t)
	store := NewStore(newFakeVault())
	seedState(t, store, as, "gh", 100)
	var r Refresher = NewCoordinator(CoordinatorConfig{
		Store: store, Client: as.client(), LockDir: t.TempDir(),
	})
	if _, tok, err := r.Refresh(context.Background(), "gh"); err != nil || tok == "" {
		t.Fatalf("refresh through the interface: %q %v", tok, err)
	}
}
