package daemon

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// TestOwnsRunFiles pins the predicate that decides whether shutdown may
// delete the run directory's shared paths, and its failure direction.
//
// Every doubt must answer false. Deleting a live daemon's control socket is
// unrecoverable for that daemon; leaving a stale file behind costs the next
// start one cleanup pass, which removeStaleSocket already performs.
func TestOwnsRunFiles(t *testing.T) {
	write := func(t *testing.T, body []byte) string {
		t.Helper()
		dir := t.TempDir()
		if err := os.WriteFile(filepath.Join(dir, InfoFileName), body, 0o600); err != nil {
			t.Fatalf("writing %s: %v", InfoFileName, err)
		}
		return dir
	}
	marshal := func(t *testing.T, info Info) []byte {
		t.Helper()
		b, err := json.Marshal(info)
		if err != nil {
			t.Fatalf("marshalling Info: %v", err)
		}
		return b
	}

	t.Run("our own pid owns them", func(t *testing.T) {
		dir := write(t, marshal(t, Info{Pid: os.Getpid(), Endpoint: "s", Version: "v"}))
		if !ownsRunFiles(dir) {
			t.Error("a daemon.json naming this process was not recognised as ours")
		}
	})

	// The case the sweep found: a replacement daemon bound the socket and
	// wrote its own daemon.json while this one was still draining.
	t.Run("another pid does not", func(t *testing.T) {
		dir := write(t, marshal(t, Info{Pid: os.Getpid() + 1, Endpoint: "s", Version: "v"}))
		if ownsRunFiles(dir) {
			t.Error("a daemon.json naming another process was treated as ours to delete")
		}
	})

	t.Run("a missing file does not", func(t *testing.T) {
		if ownsRunFiles(t.TempDir()) {
			t.Error("a missing daemon.json was treated as ownership")
		}
	})

	t.Run("an unparsable file does not", func(t *testing.T) {
		if ownsRunFiles(write(t, []byte("{not json"))) {
			t.Error("an unparsable daemon.json was treated as ownership")
		}
	})

	t.Run("a pidless file does not", func(t *testing.T) {
		if ownsRunFiles(write(t, []byte(`{"endpoint":"s","version":"v"}`))) {
			t.Error("a daemon.json with no pid was treated as ownership")
		}
	})
}
