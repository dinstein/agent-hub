package audit

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"testing"
	"time"
)

// fakeClock is a manually advanced clock shared by stream and test.
type fakeClock struct{ now time.Time }

func (c *fakeClock) Now() time.Time { return c.now }

func newSecurityForTest(t *testing.T, dir string, window time.Duration, clk *fakeClock) *SecurityStream {
	t.Helper()
	opts := SecurityOptions{
		Window:   window,
		DedupDir: filepath.Join(dir, DedupDirName),
	}
	if clk != nil {
		opts.Writer.Clock = clk.Now
	}
	s, err := NewSecurityStream(filepath.Join(dir, SecurityFileName), opts)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

// An event whose timestamp predates the marker must still be suppressed.
//
// This is the ordinary cross-process case, not an exotic one: every emitter
// stamps its event before it can take the dedup lock, so whichever process
// wins the lock is not necessarily the one holding the earliest timestamp.
// The window used to be checked with `age >= 0`, which let every
// out-of-order arrival through — four processes emitting one identical event
// produced a burst of lines whose timestamps ran backwards.
func TestSecurityDedupToleratesOutOfOrderTimestamps(t *testing.T) {
	dir := t.TempDir()
	base := time.Date(2026, 7, 26, 12, 0, 0, 0, time.UTC)
	clk := &fakeClock{now: base}
	s := newSecurityForTest(t, dir, 10*time.Minute, clk)

	ev := SecurityEvent{Event: "injection.blocked", Severity: SeverityCritical, Server: "srv"}
	if !s.Emit(ev) {
		t.Fatal("first emission must pass")
	}
	// A straggler stamped microseconds BEFORE the one that won the lock.
	clk.now = base.Add(-5 * time.Microsecond)
	if s.Emit(ev) {
		t.Error("event stamped just before the marker must be suppressed")
	}
	// Skew beyond one window is the case the original guard was written for
	// (a restored backup, a stepped clock): it must still emit rather than
	// suppress indefinitely.
	clk.now = base.Add(-11 * time.Minute)
	if !s.Emit(ev) {
		t.Error("a marker more than one window in the future must not suppress")
	}
}

func TestSecurityDedupWindow(t *testing.T) {
	dir := t.TempDir()
	clk := &fakeClock{now: time.Date(2026, 7, 26, 12, 0, 0, 0, time.UTC)}
	s := newSecurityForTest(t, dir, 10*time.Minute, clk)

	ev := SecurityEvent{Event: "ssrf.denied", Severity: SeverityWarning, Server: "srv"}
	if !s.Emit(ev) {
		t.Fatal("first emission must pass")
	}
	// Same key inside the window: suppressed.
	clk.now = clk.now.Add(5 * time.Minute)
	if s.Emit(ev) {
		t.Error("second emission inside window must be suppressed")
	}
	if s.Suppressed() != 1 {
		t.Errorf("suppressed = %d, want 1", s.Suppressed())
	}
	// Severity is part of the dedup key: escalation is a new signal.
	esc := ev
	esc.Severity = SeverityCritical
	if !s.Emit(esc) {
		t.Error("severity escalation must not be deduplicated")
	}
	// Past the window (measured from the first emission): emits again.
	clk.now = clk.now.Add(6 * time.Minute)
	if !s.Emit(ev) {
		t.Error("emission after window expiry must pass")
	}
	s.w.Sync()
	if got := len(readLines(t, filepath.Join(dir, SecurityFileName))); got != 3 {
		t.Errorf("security.jsonl has %d lines, want 3", got)
	}
}

func TestSecurityDedupWindowRefreshes(t *testing.T) {
	// The marker refreshes on emission: an event at t0 and a suppressed
	// try at t0+9m do NOT extend the window — expiry is measured from the
	// last *emission*, not the last attempt.
	dir := t.TempDir()
	clk := &fakeClock{now: time.Date(2026, 7, 26, 12, 0, 0, 0, time.UTC)}
	s := newSecurityForTest(t, dir, 10*time.Minute, clk)

	ev := SecurityEvent{Event: "integrity.drift", Severity: SeverityWarning}
	if !s.Emit(ev) {
		t.Fatal("first emission must pass")
	}
	clk.now = clk.now.Add(9 * time.Minute)
	if s.Emit(ev) {
		t.Fatal("attempt inside window must be suppressed")
	}
	clk.now = clk.now.Add(2 * time.Minute) // 11m after emission
	if !s.Emit(ev) {
		t.Error("window is measured from last emission, must emit at +11m")
	}
}

func TestSecurityDedupFutureMarkerFailsOpen(t *testing.T) {
	// A marker with a future mtime (clock skew) must not suppress
	// indefinitely — fail-open: emit and refresh.
	dir := t.TempDir()
	clk := &fakeClock{now: time.Date(2026, 7, 26, 12, 0, 0, 0, time.UTC)}
	s := newSecurityForTest(t, dir, 10*time.Minute, clk)

	ev := SecurityEvent{Event: "spawn.blocked", Severity: SeverityCritical}
	if !s.Emit(ev) {
		t.Fatal("first emission must pass")
	}
	marker := filepath.Join(dir, DedupDirName, dedupKey(ev))
	future := clk.now.Add(1 * time.Hour)
	if err := os.Chtimes(marker, future, future); err != nil {
		t.Fatal(err)
	}
	if !s.Emit(ev) {
		t.Error("future-dated marker must fail open (emit)")
	}
}

func TestSecurityDedupPrune(t *testing.T) {
	dir := t.TempDir()
	clk := &fakeClock{now: time.Date(2026, 7, 26, 12, 0, 0, 0, time.UTC)}
	s := newSecurityForTest(t, dir, 10*time.Minute, clk)

	old := SecurityEvent{Event: "a.old", Severity: SeverityInfo}
	s.Emit(old)
	oldMarker := filepath.Join(dir, DedupDirName, dedupKey(old))
	if _, err := os.Stat(oldMarker); err != nil {
		t.Fatalf("marker missing after emit: %v", err)
	}
	// A new emission more than 2x window later prunes the stale marker.
	clk.now = clk.now.Add(21 * time.Minute)
	s.Emit(SecurityEvent{Event: "b.new", Severity: SeverityInfo})
	if _, err := os.Stat(oldMarker); !os.IsNotExist(err) {
		t.Errorf("stale marker not pruned: err=%v", err)
	}
}

func TestCrossProcessSecurityDedup(t *testing.T) {
	if testing.Short() {
		t.Skip("multi-process test skipped in -short mode")
	}
	dir := t.TempDir()
	const procs, emits = 4, 10
	type proc struct {
		cmd  *exec.Cmd
		errB *bytes.Buffer
	}
	ps := make([]proc, procs)
	for i := range ps {
		errB := &bytes.Buffer{}
		cmd := exec.Command(os.Args[0])
		cmd.Env = append(os.Environ(),
			helperModeEnv+"=security",
			helperDirEnv+"="+dir,
			helperNEnv+"="+strconv.Itoa(emits),
			helperWindowEnv+"=30s",
		)
		cmd.Stderr = errB
		if err := cmd.Start(); err != nil {
			t.Fatalf("start helper %d: %v", i, err)
		}
		ps[i] = proc{cmd: cmd, errB: errB}
	}
	for i, p := range ps {
		if err := p.cmd.Wait(); err != nil {
			t.Fatalf("helper %d failed: %v\nstderr: %s", i, err, p.errB.String())
		}
	}
	lines := readLines(t, filepath.Join(dir, SecurityFileName))
	if len(lines) != 1 {
		t.Fatalf("cross-process dedup: %d lines emitted, want exactly 1:\n%s",
			len(lines), bytes.Join(lines, []byte("\n")))
	}
}
