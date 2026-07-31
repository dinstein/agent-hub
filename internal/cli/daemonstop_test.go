package cli

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/dinstein/agent-hub/internal/daemon"
	"github.com/dinstein/agent-hub/internal/platform"
)

// stopApp builds an App pointed at a fresh run directory, and returns that
// directory. Preconditions fail hard: a silent skip here would disguise a
// missing run dir as the daemon behaving correctly.
func stopApp(t *testing.T) (*App, string) {
	t.Helper()
	setDaemonEnv(t)
	app := &App{version: "test", resolver: platform.Default()}
	_, runDir, _, err := app.daemonPaths()
	if err != nil {
		t.Fatalf("daemonPaths: %v", err)
	}
	if err := os.MkdirAll(runDir, 0o700); err != nil {
		t.Fatalf("creating run dir: %v", err)
	}
	return app, runDir
}

// writeInfo plants a run/daemon.json naming pid.
func writeInfo(t *testing.T, runDir string, pid int) {
	t.Helper()
	b, err := json.Marshal(daemon.Info{Endpoint: "unix://not-listening", Pid: pid, Version: "test"})
	if err != nil {
		t.Fatalf("marshalling daemon.json: %v", err)
	}
	if err := os.WriteFile(filepath.Join(runDir, daemon.InfoFileName), b, 0o600); err != nil {
		t.Fatalf("writing daemon.json: %v", err)
	}
}

// liveStranger starts a process that is emphatically not a daemon and
// returns its pid. It outlives the test unless something signals it, which
// is exactly what is being asserted.
func liveStranger(t *testing.T) int {
	t.Helper()
	cmd := exec.Command("sleep", "60")
	if err := cmd.Start(); err != nil {
		t.Fatalf("starting the stand-in process: %v", err)
	}
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_, _ = cmd.Process.Wait()
	})
	return cmd.Process.Pid
}

// TestStopRefusesAPidItCannotVerify is the regression for the finding the
// 2026-07-31 sweep confirmed: stop read a pid out of a possibly stale
// run/daemon.json and signalled it without ever establishing that it was
// the daemon.
//
// An abrupt daemon death leaves that file behind — the package designs for
// it — and the OS then reuses the number. daemonproc_unix.go says stop
// "never signal[s] a pid they cannot verify"; it did.
func TestStopRefusesAPidItCannotVerify(t *testing.T) {
	app, runDir := stopApp(t)
	pid := liveStranger(t)
	writeInfo(t, runDir, pid)

	res, err := app.stopDaemon(context.Background(), false)
	if err == nil {
		t.Fatalf("stop reported %+v instead of refusing an unverifiable pid", res)
	}
	if !strings.Contains(err.Error(), "cannot be confirmed") {
		t.Errorf("the error does not explain why it refused: %v", err)
	}

	// The decisive assertion: the stranger is untouched.
	time.Sleep(200 * time.Millisecond)
	if err := syscall.Kill(pid, 0); err != nil {
		t.Fatalf("the unrelated process was signalled and is gone: %v", err)
	}
}

// TestStopForceRefusesAPidItCannotVerify is the same case with --force,
// which is the more damaging one: it signals the whole process group.
func TestStopForceRefusesAPidItCannotVerify(t *testing.T) {
	app, runDir := stopApp(t)
	pid := liveStranger(t)
	writeInfo(t, runDir, pid)

	if res, err := app.stopDaemon(context.Background(), true); err == nil {
		t.Fatalf("--force reported %+v instead of refusing an unverifiable pid", res)
	}
	time.Sleep(200 * time.Millisecond)
	if err := syscall.Kill(pid, 0); err != nil {
		t.Fatalf("--force killed the unrelated process's group: %v", err)
	}
}

// TestStopOfAStoppedDaemonStaysIdempotent keeps the fix from turning the
// ordinary case into an error: a run file naming a pid that is simply gone
// is still "daemon is not running", not a failure.
func TestStopOfAStoppedDaemonStaysIdempotent(t *testing.T) {
	app, runDir := stopApp(t)

	// A pid that has certainly exited: start one and reap it.
	cmd := exec.Command("true")
	if err := cmd.Run(); err != nil {
		t.Fatalf("running the throwaway process: %v", err)
	}
	writeInfo(t, runDir, cmd.Process.Pid)

	res, err := app.stopDaemon(context.Background(), false)
	if err != nil {
		t.Fatalf("stopping an already-stopped daemon must succeed: %v", err)
	}
	if res.Stopped {
		t.Errorf("reported a stop that did not happen: %+v", res)
	}
	if !strings.Contains(res.Message, "not running") {
		t.Errorf("message = %q, want it to say the daemon is not running", res.Message)
	}
}

// TestStopWithNoRunFileIsNotAnError covers the first-run case: no socket,
// no daemon.json, nothing to verify and nothing to report.
func TestStopWithNoRunFileIsNotAnError(t *testing.T) {
	app, _ := stopApp(t)
	res, err := app.stopDaemon(context.Background(), false)
	if err != nil {
		var e *Error
		if errors.As(err, &e) {
			t.Fatalf("stop with no daemon at all returned %v", e)
		}
		t.Fatalf("stop with no daemon at all returned %v", err)
	}
	if res.Stopped {
		t.Errorf("reported a stop with no daemon present: %+v", res)
	}
}
