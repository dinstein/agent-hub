package daemon_test

import (
	"os"
	"os/exec"
	"runtime"
	"testing"
	"time"

	"github.com/dinstein/agent-hub/internal/daemon"
)

// The owner watch is the mechanism that makes "the hub dies with the
// application" true even when the application never gets to say so. Each test
// here kills the owner in a way that sends the daemon nothing at all, and
// requires Run to return on its own.

// awaitSelfStop waits for a daemon expected to end its own run.
//
// It latches the outcome through the same sync.Once the cleanup hook uses.
// Without that, the deferred stop would wait out the full testTimeout on a
// channel this function has already drained, and report the daemon as hung
// after it had shut down exactly as the test required.
func awaitSelfStop(t *testing.T, h *daemonHandle, what string) {
	t.Helper()
	select {
	case err := <-h.done:
		h.stopOnce.Do(func() { h.stopErr = err })
		h.cancel()
		if err != nil {
			t.Fatalf("daemon returned %v after %s, want a clean stop", err, what)
		}
	case <-time.After(testTimeout):
		t.Fatalf("daemon kept running after %s", what)
	}
}

func TestOwnerLifelineCloseStopsTheDaemon(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("os/exec cannot pass a lifeline descriptor on windows; see docs/windows.md")
	}
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}
	defer func() { _ = w.Close() }()

	h := startDaemon(t, func(c *daemon.Config) {
		// No pid: this pins the lifeline ALONE, so a passing test cannot be
		// the poll having noticed something.
		c.Owner = daemon.Owner{Lifeline: r}
	})

	// Closing the write end is what the kernel does for a dead owner.
	if err := w.Close(); err != nil {
		t.Fatalf("closing the lifeline write end: %v", err)
	}

	awaitSelfStop(t, h, "its owner's lifeline closed")
}

func TestOwnerProcessDeathStopsTheDaemon(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the windows process probe has never run on a real machine; see docs/windows.md")
	}
	owner := exec.Command("sh", "-c", "sleep 30")
	if err := owner.Start(); err != nil {
		t.Fatalf("starting a stand-in owner: %v", err)
	}
	pid := owner.Process.Pid

	h := startDaemon(t, func(c *daemon.Config) {
		// No lifeline: this pins the POLL alone, which is all Windows has.
		c.Owner = daemon.Owner{PID: pid}
		c.OwnerPollInterval = 20 * time.Millisecond
	})

	if err := owner.Process.Kill(); err != nil {
		t.Fatalf("killing the stand-in owner: %v", err)
	}
	// Reap it. A zombie still answers the existence probe, so without this
	// the test would be waiting for something that has not become true.
	_ = owner.Wait()

	awaitSelfStop(t, h, "the owning process died")
}

func TestHeadlessDaemonHasNoOwnerToLose(t *testing.T) {
	h := startDaemon(t, func(c *daemon.Config) {
		c.OwnerPollInterval = 20 * time.Millisecond
	})
	// Long enough for several poll ticks. A headless daemon watches nothing,
	// so the only way this fails is a watch that armed itself anyway — which
	// would shut down every daemon an operator starts.
	select {
	case err := <-h.done:
		t.Fatalf("headless daemon stopped on its own: %v", err)
	case <-time.After(200 * time.Millisecond):
	}

	info, err := daemon.ReadInfo(h.runDir)
	if err != nil {
		t.Fatalf("reading daemon.json: %v", err)
	}
	if info.Owner != 0 {
		t.Fatalf("daemon.json owner = %d for a headless daemon, want 0", info.Owner)
	}
}

func TestDaemonInfoRecordsItsOwner(t *testing.T) {
	h := startDaemon(t, func(c *daemon.Config) {
		// Our own pid: a live owner, so the daemon stays up for the read.
		c.Owner = daemon.Owner{PID: os.Getpid()}
		c.OwnerPollInterval = 20 * time.Millisecond
	})
	info, err := daemon.ReadInfo(h.runDir)
	if err != nil {
		t.Fatalf("reading daemon.json: %v", err)
	}
	if info.Owner != os.Getpid() {
		t.Fatalf("daemon.json owner = %d, want %d", info.Owner, os.Getpid())
	}
}

func TestLifelineFromFDRefusesAStandardStream(t *testing.T) {
	// Adopting fd 2 would tie the hub's life to its own stderr.
	for _, fd := range []int{0, 1, 2} {
		if _, err := daemon.LifelineFromFD(fd); err == nil {
			t.Errorf("LifelineFromFD(%d) succeeded; a standard stream is not a lifeline", fd)
		}
	}
}
