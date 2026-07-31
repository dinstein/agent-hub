package daemon_test

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/dinstein/agent-hub/api"
	"github.com/dinstein/agent-hub/internal/daemon"
)

// TestShutdownLeavesAReplacementsFilesAlone is the regression for the
// finding the 2026-07-31 sweep confirmed.
//
// srv.Shutdown closes the listener BEFORE it drains, and Go's unix listener
// unlinks the socket on close. So for up to ShutdownGrace the run directory
// looks free: `agenthub daemon start` finds no stale socket, binds, and
// writes its own daemon.json. When the first daemon's drain then finished,
// cleanup removed both paths by name — unlinking the replacement's LIVE
// control socket and deleting its readiness file. The replacement kept
// running, unreachable and invisible, and the next start bound a fresh
// socket beside it.
//
// The drain is held open here by a live SSE subscription, which is what
// makes the window deterministic rather than a race the test hopes to win.
func TestShutdownLeavesAReplacementsFilesAlone(t *testing.T) {
	h := startDaemon(t, func(c *daemon.Config) {
		c.ShutdownGrace = 3 * time.Second
	})

	// An open events stream is a connection Shutdown cannot drain by
	// itself, so the grace period is actually spent.
	ctx, cancelSub := context.WithCancel(context.Background())
	defer cancelSub()
	client := api.New(h.socket)
	defer client.Close()
	if _, err := client.Events.Subscribe(ctx, "servers"); err != nil {
		t.Fatalf("Subscribe: %v", err)
	}

	infoPath := filepath.Join(h.runDir, daemon.InfoFileName)
	if _, err := os.Stat(infoPath); err != nil {
		t.Fatalf("precondition: the running daemon has no %s: %v", daemon.InfoFileName, err)
	}

	h.cancel()

	// Stand in for the replacement: a foreign daemon.json and a socket file
	// at the same paths, written while the first daemon drains.
	const replacementPid = 999999
	body, err := json.Marshal(daemon.Info{Endpoint: h.socket, Pid: replacementPid, Version: "replacement"})
	if err != nil {
		t.Fatalf("marshalling the replacement's info: %v", err)
	}
	if err := os.WriteFile(infoPath, body, 0o600); err != nil {
		t.Fatalf("writing the replacement's %s: %v", daemon.InfoFileName, err)
	}
	// Wait for the departing daemon's listener to unlink the socket before
	// planting the replacement's. That is the real sequence — Shutdown
	// closes the listener, and only then drains — and it is the window a
	// replacement actually starts in: it finds no stale socket and binds.
	// Planting earlier raced the unlink, which removed the stand-in and, on
	// Linux, could not even create it (writing over a live unix socket inode
	// is ENXIO).
	freed := false
	for deadline := time.Now().Add(testTimeout); time.Now().Before(deadline); {
		if _, err := os.Stat(h.socket); errors.Is(err, os.ErrNotExist) {
			freed = true
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if !freed {
		t.Fatal("precondition: the departing daemon never unlinked its socket, so the replacement's window never opened")
	}

	// The replacement would bind its own socket here. The assertion below
	// is only that the departing daemon leaves the path alone, so a plain
	// file standing in for a bound socket is enough.
	if err := os.WriteFile(h.socket, []byte("stand-in for a bound socket"), 0o600); err != nil {
		t.Fatalf("creating the replacement's socket file: %v", err)
	}

	// h.stop is idempotent and records its result, so the cleanup hook does
	// not go on to wait for a channel this test already drained.
	if err := h.stop(t); err != nil {
		t.Fatalf("daemon.Run: %v", err)
	}

	// Both paths must have survived, with the replacement's content.
	got, err := daemon.ReadInfo(h.runDir)
	if err != nil {
		t.Fatalf("the replacement's %s was deleted by the departing daemon: %v", daemon.InfoFileName, err)
	}
	if got.Pid != replacementPid {
		t.Errorf("daemon.json pid = %d, want the replacement's %d", got.Pid, replacementPid)
	}
	if _, err := os.Stat(h.socket); err != nil {
		t.Errorf("the replacement's control socket was unlinked by the departing daemon: %v", err)
	}
}
