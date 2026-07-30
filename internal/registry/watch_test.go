package registry

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"
)

// fastWatch returns options tuned so tests settle quickly; behavior is
// identical to production, only the timers shrink.
func fastWatch(disableFSNotify bool) WatchOptions {
	return WatchOptions{
		Debounce:        20 * time.Millisecond,
		Poll:            50 * time.Millisecond,
		DisableFSNotify: disableFSNotify,
	}
}

func mustWatch(t *testing.T, st *Store, opts WatchOptions) *Watcher {
	t.Helper()
	w, err := st.WatchWith(opts)
	if err != nil {
		t.Fatalf("WatchWith: %v", err)
	}
	t.Cleanup(w.Close)
	return w
}

// waitChange blocks until an event for kind arrives or the deadline passes.
func waitChange(t *testing.T, w *Watcher, kind DocKind, timeout time.Duration) Change {
	t.Helper()
	deadline := time.After(timeout)
	for {
		select {
		case ev, ok := <-w.Events():
			if !ok {
				t.Fatalf("events channel closed while waiting for %s", kind)
			}
			if ev.Kind == kind {
				return ev
			}
		case <-deadline:
			t.Fatalf("no Change{Kind:%s} within %s", kind, timeout)
		}
	}
}

// assertNoChange asserts that no event at all arrives within window.
func assertNoChange(t *testing.T, w *Watcher, window time.Duration) {
	t.Helper()
	select {
	case ev, ok := <-w.Events():
		if ok {
			t.Fatalf("unexpected event %+v", ev)
		}
	case <-time.After(window):
	}
}

func TestWatchExternalWriteEmitsKindedEvent(t *testing.T) {
	dir := t.TempDir()
	watched := mustOpen(t, dir)
	w := mustWatch(t, watched, fastWatch(false))

	// A second Store on the same directory stands in for another process:
	// its self-write registrations live in its own set, so the watched Store
	// must classify the write as external.
	writer := mustOpen(t, dir)
	addServer(t, writer, "ext", "npx")

	// Rev is a hint (a scan may race the meta bump), so bound it instead of
	// pinning it: it must never exceed the writer's committed generation.
	ev := waitChange(t, w, DocServers, 5*time.Second)
	if ev.Rev > 1 {
		t.Errorf("Change.Rev = %d, want <= 1", ev.Rev)
	}

	// A different document must surface with its own Kind.
	err := writer.Update(context.Background(), func(tx *Tx) error {
		tx.Governance.V.Discovery = "lazy"
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	ev = waitChange(t, w, DocGovernance, 5*time.Second)
	if ev.Rev < 1 || ev.Rev > 2 {
		t.Errorf("Change.Rev = %d, want in [1, 2]", ev.Rev)
	}
}

func TestWatchSelfWriteEmitsNoEvent(t *testing.T) {
	dir := t.TempDir()
	st := mustOpen(t, dir)
	w := mustWatch(t, st, fastWatch(false))

	addServer(t, st, "own", "npx") // write through the watched Store itself

	// Long enough for debounce + at least two poll rounds to have scanned.
	assertNoChange(t, w, 300*time.Millisecond)
}

func TestWatchPollFallbackWithoutFSNotify(t *testing.T) {
	dir := t.TempDir()
	watched := mustOpen(t, dir)
	w := mustWatch(t, watched, fastWatch(true))
	if w.usingFSNotify {
		t.Fatal("DisableFSNotify: watcher must run in poll-only mode")
	}

	writer := mustOpen(t, dir)
	addServer(t, writer, "polled", "npx")

	ev := waitChange(t, w, DocServers, 5*time.Second)
	if ev.Rev > 1 {
		t.Errorf("Change.Rev = %d, want <= 1", ev.Rev)
	}
}

// TestWatchLoadFailureDoesNotAdvanceBaseline: a half-written (invalid) file
// must produce no event and must not poison the baseline — when readable
// content appears later, the change is still detected and reported.
func TestWatchLoadFailureDoesNotAdvanceBaseline(t *testing.T) {
	dir := t.TempDir()
	watched := mustOpen(t, dir)
	w := mustWatch(t, watched, fastWatch(false))

	// Simulate an external non-atomic writer caught mid-write.
	if err := os.WriteFile(docPath(dir, DocServers), []byte(`{"servers": {"tr`), 0o600); err != nil {
		t.Fatal(err)
	}
	assertNoChange(t, w, 300*time.Millisecond)

	// The writer finishes; the watcher must now report the change. Open
	// quarantines the torn file (reported as *UnreadableError, tolerated
	// here) and restores a readable default.
	writer, err := Open(dir)
	if err != nil {
		var u *UnreadableError
		if !errors.As(err, &u) {
			t.Fatalf("Open: %v", err)
		}
	}
	addServer(t, writer, "recovered", "npx")
	ev := waitChange(t, w, DocServers, 5*time.Second)
	if ev.Kind != DocServers {
		t.Fatalf("Kind = %s, want servers", ev.Kind)
	}
}

// TestWatchRapidWritesConvergeViaApplier is the end-to-end §5c scenario:
// many rapid external writes, coalesced events, and a consumer that re-reads
// on each event and adopts by the >= criterion. The consumer must converge on
// the final generation — never stuck waiting for a per-write event.
func TestWatchRapidWritesConvergeViaApplier(t *testing.T) {
	dir := t.TempDir()
	watched := mustOpen(t, dir)
	w := mustWatch(t, watched, fastWatch(false))

	writer := mustOpen(t, dir)
	const writes = 8
	for i := 0; i < writes; i++ {
		addServer(t, writer, "srv", "cmd-v"+string(rune('a'+i)))
	}
	finalGen := writer.Snapshot().Generation
	if finalGen != writes {
		t.Fatalf("writer generation = %d, want %d", finalGen, writes)
	}

	var applier Applier
	applier.MarkApplied(watched.Snapshot().Generation)

	deadline := time.After(10 * time.Second)
	for {
		applied, _ := applier.Applied()
		if applied == finalGen {
			break
		}
		select {
		case _, ok := <-w.Events():
			if !ok {
				t.Fatal("events channel closed prematurely")
			}
			// The event is a hint only: re-read, adopt by >= criterion.
			snap, err := watched.Reload(context.Background())
			if err != nil {
				t.Fatalf("Reload: %v", err)
			}
			if _, err := applier.Apply(snap.Generation, func() error { return nil }); err != nil {
				t.Fatalf("Apply: %v", err)
			}
		case <-deadline:
			applied, _ := applier.Applied()
			t.Fatalf("stuck at generation %d, want %d", applied, finalGen)
		}
	}

	if got := watched.Snapshot().Servers.V.Servers["srv"].V.Command; got != "cmd-v"+string(rune('a'+writes-1)) {
		t.Errorf("final command = %q, want last write", got)
	}
}

func TestWatchCloseClosesEvents(t *testing.T) {
	dir := t.TempDir()
	st := mustOpen(t, dir)
	w := mustWatch(t, st, fastWatch(false))
	w.Close()
	w.Close() // idempotent
	if _, ok := <-w.Events(); ok {
		t.Fatal("events channel must be closed after Close")
	}
}
