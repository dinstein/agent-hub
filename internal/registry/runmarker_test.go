package registry

import (
	"os"
	"path/filepath"
	"testing"
)

// A directory nobody has ever run in reports "unknown", not "clean": there
// is no evidence of a clean shutdown, and the diagnostic must not invent
// one.
func TestRunMarkerFirstRunIsUnknown(t *testing.T) {
	dir := t.TempDir()
	if got := PreviousShutdown(dir); got != ShutdownUnknown {
		t.Fatalf("PreviousShutdown on a fresh dir = %q, want %q", got, ShutdownUnknown)
	}
	m, prev, err := ArmRunMarker(dir)
	if err != nil {
		t.Fatalf("ArmRunMarker: %v", err)
	}
	if prev != ShutdownUnknown {
		t.Fatalf("first arm reported previous = %q, want %q", prev, ShutdownUnknown)
	}
	if m == nil {
		t.Fatal("ArmRunMarker returned a nil marker without an error")
	}
}

// Arm → Resolve → Arm reports clean. Arm → (no resolve) → Arm reports crash.
func TestRunMarkerCleanAndCrashCycles(t *testing.T) {
	dir := t.TempDir()

	m, _, err := ArmRunMarker(dir)
	if err != nil {
		t.Fatalf("arm 1: %v", err)
	}
	// While armed, an observer already sees "crash" — that is the point: the
	// verdict does not depend on anyone noticing the death.
	if got := PreviousShutdown(dir); got != ShutdownCrash {
		t.Fatalf("armed marker reads as %q, want %q", got, ShutdownCrash)
	}
	if err := m.Resolve(); err != nil {
		t.Fatalf("resolve 1: %v", err)
	}

	m2, prev, err := ArmRunMarker(dir)
	if err != nil {
		t.Fatalf("arm 2: %v", err)
	}
	if prev != ShutdownClean {
		t.Fatalf("after a resolved run, previous = %q, want %q", prev, ShutdownClean)
	}

	// Simulate kill -9: m2 is never resolved.
	_ = m2
	_, prev, err = ArmRunMarker(dir)
	if err != nil {
		t.Fatalf("arm 3: %v", err)
	}
	if prev != ShutdownCrash {
		t.Fatalf("after an unresolved run, previous = %q, want %q", prev, ShutdownCrash)
	}
}

// Resolve is idempotent and nil-safe: a process that failed to arm must
// still be able to shut down without a special case.
func TestRunMarkerResolveIsIdempotentAndNilSafe(t *testing.T) {
	dir := t.TempDir()
	m, _, err := ArmRunMarker(dir)
	if err != nil {
		t.Fatalf("ArmRunMarker: %v", err)
	}
	for i := 0; i < 3; i++ {
		if err := m.Resolve(); err != nil {
			t.Fatalf("resolve %d: %v", i, err)
		}
	}
	if got := PreviousShutdown(dir); got != ShutdownClean {
		t.Fatalf("after repeated resolves = %q, want %q", got, ShutdownClean)
	}
	var nilMarker *RunMarker
	if err := nilMarker.Resolve(); err != nil {
		t.Fatalf("nil marker Resolve: %v", err)
	}
}

// Every unreadable shape resolves to "unknown" — never to "clean". A
// corrupt marker must not be able to hand out a clean bill of health.
func TestRunMarkerCorruptReadsUnknown(t *testing.T) {
	for name, content := range map[string]string{
		"garbage":       "not json at all",
		"empty":         "",
		"wrong version": `{"version":999,"armed":false}`,
		"truncated":     `{"version":1,"arm`,
	} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, RunMarkerName)
			if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
				t.Fatal(err)
			}
			if got := PreviousShutdown(dir); got != ShutdownUnknown {
				t.Fatalf("corrupt marker (%s) read as %q, want %q", name, got, ShutdownUnknown)
			}
		})
	}
}

// The marker must not disturb the document namespace: no Doc kind may pick
// it up, and Open must keep working with one present.
func TestRunMarkerDoesNotDisturbTheStore(t *testing.T) {
	dir := t.TempDir()
	if _, _, err := ArmRunMarker(dir); err != nil {
		t.Fatalf("ArmRunMarker: %v", err)
	}
	s, err := Open(dir)
	if err != nil {
		t.Fatalf("Open with a marker present: %v", err)
	}
	if s.Snapshot().Generation != 0 {
		t.Fatalf("generation = %d, want 0 (the marker must not count as a write)",
			s.Snapshot().Generation)
	}
	for _, kind := range []DocKind{DocMeta, DocServers, DocProfiles, DocClients, DocGovernance} {
		if RunMarkerName == string(kind)+".json" {
			t.Fatalf("marker name collides with document %q", kind)
		}
	}
}
