package secrets

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestAnnounceCountsPerServer: counters are per server and monotonic, and a
// reader only ever compares them.
func TestAnnounceCountsPerServer(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()

	if got := Revisions(dir); len(got) != 0 {
		t.Fatalf("a vault with no announcements reported %v, want empty", got)
	}
	for range 3 {
		if err := Announce(dir, "notion"); err != nil {
			t.Fatalf("Announce: %v", err)
		}
	}
	if err := Announce(dir, "linear"); err != nil {
		t.Fatalf("Announce: %v", err)
	}
	got := Revisions(dir)
	if got["notion"] != 3 || got["linear"] != 1 {
		t.Errorf("revisions = %v, want notion:3 linear:1", got)
	}
}

// TestAnnounceHoldsNoSecret is the invariant that lets this file be a plain
// readable sibling of the vault: it records ids and counters, never a value.
func TestAnnounceHoldsNoSecret(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	chain := NewChain(ChainConfig{Dir: dir, Keyring: newFakeBackend(nil), LookupEnv: func(string) (string, bool) { return "", false }})
	const secret = "super-secret-token-value"
	if err := chain.Set(context.Background(), HTTPAuthRef("notion"), secret); err != nil {
		t.Fatalf("Set: %v", err)
	}
	raw, err := os.ReadFile(filepath.Join(dir, credentialsRevFile))
	if err != nil {
		t.Fatalf("the announcement file was not written: %v", err)
	}
	if string(raw) == "" {
		t.Fatal("announcement file is empty")
	}
	if strings.Contains(string(raw), secret) {
		t.Fatalf("the announcement file contains the credential: %s", raw)
	}
	if !strings.Contains(string(raw), "notion") {
		t.Fatalf("the announcement file does not name the server: %s", raw)
	}
}

// TestAnnounceSurvivesACorruptFile: the counters are hints, so a hand-edited
// or torn file must not wedge every future announcement.
func TestAnnounceSurvivesACorruptFile(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, credentialsRevFile)
	if err := os.WriteFile(path, []byte("{not json"), 0o600); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if got := Revisions(dir); len(got) != 0 {
		t.Errorf("a corrupt file reported %v, want empty and no error", got)
	}
	if err := Announce(dir, "notion"); err != nil {
		t.Fatalf("Announce over a corrupt file: %v", err)
	}
	if got := Revisions(dir)["notion"]; got != 1 {
		t.Errorf("notion = %d after recovery, want 1", got)
	}
}

// TestCredWatcherReportsAnotherProcessesWrite is the cross-process case the
// whole plane exists for: `agenthub auth login` runs in its OWN process, so
// the gateway can only learn about it through the filesystem.
func TestCredWatcherReportsAnotherProcessesWrite(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()

	// Something was already stored before the watcher existed: that must NOT
	// be reported, or every gateway start would invalidate every credential.
	if err := Announce(dir, "linear"); err != nil {
		t.Fatalf("seed: %v", err)
	}

	w := NewCredWatcher(dir)
	defer w.Close()

	// A SECOND chain on the same directory is the other process.
	other := NewChain(ChainConfig{Dir: dir, Keyring: newFakeBackend(nil), LookupEnv: func(string) (string, bool) { return "", false }})
	if err := other.Set(context.Background(), HTTPAuthRef("notion"), "tok"); err != nil {
		t.Fatalf("Set: %v", err)
	}

	select {
	case id := <-w.Events():
		if id != "notion" {
			t.Errorf("announced %q, want notion", id)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("no announcement reached the watcher")
	}
}

// TestCredWatcherIgnoresThePreexistingBaseline pins the half above: what was
// already on disk when the watcher started is the baseline, not an event.
func TestCredWatcherIgnoresThePreexistingBaseline(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	if err := Announce(dir, "notion"); err != nil {
		t.Fatalf("seed: %v", err)
	}
	w := NewCredWatcher(dir)
	defer w.Close()

	select {
	case id := <-w.Events():
		t.Fatalf("reported %q for a credential stored before the watcher existed", id)
	case <-time.After(3 * credWatchPoll):
	}
}
