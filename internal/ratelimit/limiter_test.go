package ratelimit

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeClock is a manually advanced clock: window rolling must be proven
// deterministically, never by sleeping.
type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func newClock() *fakeClock {
	return &fakeClock{t: time.Date(2026, 7, 27, 12, 0, 0, 0, time.UTC)}
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *fakeClock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

type testLimiter struct {
	*Limiter
	clock *fakeClock
	dir   string

	mu     sync.Mutex
	logs   *bytes.Buffer
	events []Event
}

func (tl *testLimiter) record(ev Event) {
	tl.mu.Lock()
	defer tl.mu.Unlock()
	tl.events = append(tl.events, ev)
}

func (tl *testLimiter) recorded() []Event {
	tl.mu.Lock()
	defer tl.mu.Unlock()
	return append([]Event(nil), tl.events...)
}

func (tl *testLimiter) logText() string {
	tl.mu.Lock()
	defer tl.mu.Unlock()
	return tl.logs.String()
}

// lockedWriter serializes log writes with the event recorder so a
// concurrent test can read both without racing.
type lockedWriter struct{ tl *testLimiter }

func (w *lockedWriter) Write(p []byte) (int, error) {
	w.tl.mu.Lock()
	defer w.tl.mu.Unlock()
	return w.tl.logs.Write(p)
}

func newTestLimiter(t *testing.T, cfg Config) *testLimiter {
	t.Helper()
	dir := t.TempDir()
	clock := newClock()
	logs := &bytes.Buffer{}
	tl := &testLimiter{clock: clock, dir: dir, logs: logs}
	lim, err := New(Options{
		Config:   cfg,
		StateDir: dir,
		Logger:   slog.New(slog.NewTextHandler(&lockedWriter{tl: tl}, nil)),
		Now:      clock.Now,
		OnEvent:  tl.record,
	})
	if err != nil {
		t.Fatal(err)
	}
	tl.Limiter = lim
	return tl
}

// mustAllow asserts that the next n calls are admitted.
func mustAllow(t *testing.T, tl *testLimiter, key Key, n int, msg string) {
	t.Helper()
	for i := 0; i < n; i++ {
		if dec := tl.Allow(key); !dec.Allowed {
			t.Fatalf("%s (call %d: %s)", msg, i, dec)
		}
	}
}

func TestBurstThenDeny(t *testing.T) {
	tl := newTestLimiter(t, Config{Rules: []Rule{{Server: "gh", Limit: 3, Window: Duration(time.Minute)}}})
	key := Key{Client: "claude-code", Server: "gh", Tool: "search"}

	for i := 0; i < 3; i++ {
		if dec := tl.Allow(key); !dec.Allowed {
			t.Fatalf("call %d denied: %s", i, dec)
		}
	}
	dec := tl.Allow(key)
	if dec.Allowed {
		t.Fatal("4th call within the window must be denied")
	}
	if dec.Rule != "*/gh/*" {
		t.Fatalf("Rule = %q", dec.Rule)
	}
	// A zero retry hint invites the immediate retry that caused the denial.
	if dec.RetryAfter <= 0 {
		t.Fatalf("RetryAfter = %v, must be > 0", dec.RetryAfter)
	}
	if want := 20 * time.Second; dec.RetryAfter != want {
		t.Fatalf("RetryAfter = %v, want %v (one token = window/limit)", dec.RetryAfter, want)
	}
	if evs := tl.recorded(); len(evs) != 1 || evs[0].Allowed {
		t.Fatalf("expected exactly one denial event, got %+v", evs)
	}
}

// The bucket refills continuously; a fixed window would let 2*limit through
// across a boundary.
func TestWindowRolls(t *testing.T) {
	tl := newTestLimiter(t, Config{Rules: []Rule{{Limit: 2, Window: Duration(10 * time.Second)}}})
	key := Key{Client: "c", Server: "s", Tool: "t"}

	mustAllow(t, tl, key, 2, "burst of 2 must be allowed")
	if tl.Allow(key).Allowed {
		t.Fatal("3rd call must be denied")
	}
	// Half a token later: still denied.
	tl.clock.advance(2 * time.Second)
	if tl.Allow(key).Allowed {
		t.Fatal("2s (0.4 token) later the call must still be denied")
	}
	// One whole token later: allowed again, and only one.
	tl.clock.advance(3 * time.Second)
	if !tl.Allow(key).Allowed {
		t.Fatal("5s (1 token) later the call must be allowed")
	}
	if tl.Allow(key).Allowed {
		t.Fatal("the refilled token was spent; the next call must be denied")
	}
	// A full window of idleness refills to capacity, never beyond.
	tl.clock.advance(time.Hour)
	mustAllow(t, tl, key, 2, "after a full window the bucket must be back at capacity")
	if tl.Allow(key).Allowed {
		t.Fatal("capacity must cap at limit, not accumulate over idle time")
	}
}

// All-or-nothing: a rule that has tokens must not be charged for a call
// another rule rejects.
func TestAllOrNothingAcrossRules(t *testing.T) {
	tl := newTestLimiter(t, Config{Rules: []Rule{
		// One shared budget for everything (ScopePerRule), plus a
		// per-key cap on the expensive tool.
		{Limit: 10, Window: Duration(time.Minute), Scope: ScopePerRule},
		{Tool: "expensive", Limit: 1, Window: Duration(time.Minute)},
	}})
	expensive := Key{Client: "c", Server: "s", Tool: "expensive"}
	cheap := Key{Client: "c", Server: "s", Tool: "cheap"}

	if !tl.Allow(expensive).Allowed {
		t.Fatal("first expensive call must pass")
	}
	for range 3 {
		if dec := tl.Allow(expensive); dec.Allowed {
			t.Fatal("further expensive calls must be denied by the narrow rule")
		}
	}
	// The broad rule was charged exactly once (by the first expensive call),
	// so 9 cheap calls remain.
	for i := 0; i < 9; i++ {
		if dec := tl.Allow(cheap); !dec.Allowed {
			t.Fatalf("cheap call %d denied (%s): rejected calls must not spend the broad rule's tokens", i, dec)
		}
	}
	if tl.Allow(cheap).Allowed {
		t.Fatal("the broad rule must be exhausted after 10 admitted calls")
	}
}

func TestNoRulesMeansNoEnforcement(t *testing.T) {
	tl := newTestLimiter(t, Config{})
	if tl.Enabled() {
		t.Fatal("an empty config must report disabled")
	}
	for range 100 {
		if !tl.Allow(Key{Server: "s", Tool: "t"}).Allowed {
			t.Fatal("no rules means no quota")
		}
	}
	if _, err := os.Stat(filepath.Join(tl.dir, StateFileName)); !os.IsNotExist(err) {
		t.Fatal("a limiter with no rules must not touch the counter file")
	}
}

// Corrupt counter file: FAIL OPEN, loudly (package doc). The bad file is
// quarantined and counting restarts.
func TestCorruptStateFailsOpenLoudly(t *testing.T) {
	tl := newTestLimiter(t, Config{Rules: []Rule{{Limit: 1, Window: Duration(time.Minute)}}})
	key := Key{Client: "c", Server: "s", Tool: "t"}
	if !tl.Allow(key).Allowed {
		t.Fatal("first call must pass")
	}
	if tl.Allow(key).Allowed {
		t.Fatal("second call must be denied before corruption")
	}
	if err := os.WriteFile(filepath.Join(tl.dir, StateFileName), []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	dec := tl.Allow(key)
	if !dec.Allowed {
		t.Fatal("a corrupt counter file must not block calls (rate limiting is not a security boundary)")
	}
	if !dec.Degraded {
		t.Fatal("the fail-open admission must be reported as degraded")
	}
	if !strings.Contains(tl.logText(), "counter file corrupt") {
		t.Fatalf("the fail-open path must warn loudly; log was:\n%s", tl.logText())
	}
	if evs := tl.recorded(); len(evs) == 0 || !evs[len(evs)-1].Degraded || !evs[len(evs)-1].Allowed {
		t.Fatalf("degraded admission must be reported to audit, got %+v", tl.recorded())
	}
	// The bad bytes are kept for forensics and the file is usable again.
	matches, _ := filepath.Glob(filepath.Join(tl.dir, StateFileName+quarantineSuffix+"*"))
	if len(matches) != 1 {
		t.Fatalf("corrupt file must be quarantined exactly once, found %v", matches)
	}
	if tl.Allow(key).Allowed {
		t.Fatal("counting must resume after the restart (the fresh bucket spent its token above)")
	}
}

// An unknown schema version is treated exactly like corruption: never
// half-interpreted.
func TestUnknownVersionIsCorruption(t *testing.T) {
	tl := newTestLimiter(t, Config{Rules: []Rule{{Limit: 1, Window: Duration(time.Minute)}}})
	if err := os.WriteFile(filepath.Join(tl.dir, StateFileName),
		[]byte(`{"version":99,"buckets":{}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	dec := tl.Allow(Key{Server: "s", Tool: "t"})
	if !dec.Allowed || !dec.Degraded {
		t.Fatalf("unknown version must fail open and report degraded, got %s", dec)
	}
}

// The counter file bytes are contract: several processes read and merge
// them, so the encoding must be exact integers, not floats.
func TestStateFileGolden(t *testing.T) {
	tl := newTestLimiter(t, Config{Rules: []Rule{
		{Limit: 4, Window: Duration(time.Minute)},
		{Server: "gh", Tool: "create_issue", Limit: 2, Window: Duration(time.Hour)},
	}})
	key := Key{Client: "claude-code", Server: "gh", Tool: "create_issue"}
	if !tl.Allow(key).Allowed {
		t.Fatal("first call must pass")
	}
	tl.clock.advance(3 * time.Second)
	if !tl.Allow(key).Allowed {
		t.Fatal("second call must pass")
	}
	raw, err := os.ReadFile(filepath.Join(tl.dir, StateFileName))
	if err != nil {
		t.Fatal(err)
	}
	var pretty bytes.Buffer
	if err := json.Indent(&pretty, raw, "", "  "); err != nil {
		t.Fatal(err)
	}
	pretty.WriteByte('\n')
	assertGolden(t, "state.json", pretty.Bytes())
}

func assertGolden(t *testing.T, name string, got []byte) {
	t.Helper()
	path := filepath.Join("testdata", name)
	if os.Getenv("UPDATE_GOLDEN") == "1" {
		if err := os.MkdirAll("testdata", 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, got, 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read golden %s: %v (regenerate with UPDATE_GOLDEN=1)", path, err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("%s drifted — the counter encoding is shared across processes\n--- got ---\n%s\n--- want ---\n%s",
			path, got, want)
	}
}

// Buckets nobody has touched for idleTTL are dropped: they have refilled to
// capacity anyway, so dropping them is equivalent to keeping them.
func TestIdleBucketsArePruned(t *testing.T) {
	tl := newTestLimiter(t, Config{Rules: []Rule{{Limit: 2, Window: Duration(time.Minute)}}})
	tl.Allow(Key{Server: "old", Tool: "t"})
	tl.clock.advance(2 * idleTTL)
	tl.Allow(Key{Server: "new", Tool: "t"})

	raw, err := os.ReadFile(filepath.Join(tl.dir, StateFileName))
	if err != nil {
		t.Fatal(err)
	}
	var st state
	if err := json.Unmarshal(raw, &st); err != nil {
		t.Fatal(err)
	}
	if len(st.Buckets) != 1 {
		t.Fatalf("expected only the fresh bucket, got %v", st.Buckets)
	}
	for k := range st.Buckets {
		if !strings.Contains(k, "new") {
			t.Fatalf("wrong bucket survived pruning: %q", k)
		}
	}
}

// A clock that steps backwards (NTP, or a peer process with a skewed clock)
// must never mint tokens.
func TestClockGoingBackwardsDoesNotMintTokens(t *testing.T) {
	tl := newTestLimiter(t, Config{Rules: []Rule{{Limit: 1, Window: Duration(time.Minute)}}})
	key := Key{Server: "s", Tool: "t"}
	if !tl.Allow(key).Allowed {
		t.Fatal("first call must pass")
	}
	tl.clock.advance(-time.Hour)
	if tl.Allow(key).Allowed {
		t.Fatal("a backwards clock step must not refill the bucket")
	}
}

func TestConcurrentAllowInOneProcess(t *testing.T) {
	tl := newTestLimiter(t, Config{Rules: []Rule{{Limit: 20, Window: Duration(time.Hour)}}})
	key := Key{Client: "c", Server: "s", Tool: "t"}

	var mu sync.Mutex
	allowed := 0
	var wg sync.WaitGroup
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 10; j++ {
				if tl.Allow(key).Allowed {
					mu.Lock()
					allowed++
					mu.Unlock()
				}
			}
		}()
	}
	wg.Wait()
	if allowed != 20 {
		t.Fatalf("allowed %d calls, want exactly 20 (the limit)", allowed)
	}
}

// TestUnusableCounterFileReportsAtError pins the level, not just the text.
//
// The level is the difference between an operator alerting on a failed
// protective capability and one who never hears that quotas stopped applying.
// eventlog's Level names this exact condition as what Error is reserved for,
// and its sibling example — "ledger unavailable; calls run unrecorded" — is
// Error in internal/gateway. Nothing else ties the three together, and the
// site sat at Warn for want of a check.
//
// Recovered corruption is deliberately NOT this case: counters restart, which
// is degraded rather than absent, and TestCorruptStateFailsOpenLoudly
// covers it. Asserted here too, so a future tightening cannot quietly promote
// both and lose the distinction.
func TestUnusableCounterFileReportsAtError(t *testing.T) {
	tl := newTestLimiter(t, Config{Rules: []Rule{{Limit: 1, Window: Duration(time.Minute)}}})

	// The store must become unwritable AFTER a successful build: New refuses
	// to build at all on an unusable counter file while rules are configured,
	// so this branch is only reachable by breaking one that worked. A
	// read-only directory is how — the lock file and the atomic replacement
	// both need to create a file in it.
	if err := os.Chmod(tl.dir, 0o500); err != nil {
		t.Fatalf("staging an unwritable state directory: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(tl.dir, 0o700) })
	// Fail hard rather than skip: running as root (or on a filesystem that
	// ignores the mode) would make every assertion below vacuous, and a test
	// that quietly proves nothing is worse than one that is absent.
	if f, err := os.Create(filepath.Join(tl.dir, "precondition")); err == nil {
		_ = f.Close()
		t.Fatal("the state directory is still writable after chmod 0500, so this test would " +
			"assert nothing; it cannot run as root or on a mode-ignoring filesystem")
	}

	dec := tl.Allow(Key{Server: "s", Tool: "t"})
	if !dec.Allowed || !dec.Degraded {
		t.Fatalf("an unusable counter file must fail open and report degraded, got %s", dec)
	}

	logs := tl.logText()
	if !strings.Contains(logs, "counter file unusable") {
		t.Fatalf("the uncounted pass was not reported at all:\n%s", logs)
	}
	if !strings.Contains(logs, "level=ERROR") {
		t.Errorf("an unusable counter file reported below ERROR — an operator alerting on "+
			"Error never learns the quota stopped applying:\n%s", logs)
	}
}
