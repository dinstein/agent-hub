//go:build unix

package cli

import (
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// TestLockContentionExit7 holds the registry's sibling .lock flock and
// verifies a write command times out with exit 7 and the E_LOCK_TIMEOUT
// code. flock conflicts across open file descriptions, so holding it on a
// separate fd in this process is equivalent to another process holding it.
func TestLockContentionExit7(t *testing.T) {
	dir := setDataDir(t)
	// Materialize the registry first so only the lock is contended.
	if code, _, _ := runCLI(t, "", "server", "ls"); code != ExitOK {
		t.Fatalf("bootstrap ls failed")
	}

	lockPath := filepath.Join(dir, "registry", ".lock")
	f, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN) }()

	code, out, stderr := runCLIWithTimeout(t, "", 150*time.Millisecond,
		"server", "add", "x", "--cmd", "foo", "--json")
	if code != ExitLocked {
		t.Fatalf("exit = %d, want %d\nstdout: %s\nstderr: %s", code, ExitLocked, out, stderr)
	}
	env := decodeEnvelope(t, out)
	if env.OK || env.Error == nil || env.Error.Code != CodeLockTimeout {
		t.Errorf("envelope = %s, want error code %s", out, CodeLockTimeout)
	}
	if !strings.Contains(env.Error.Message, ".lock") {
		t.Errorf("message should name the lock file: %q", env.Error.Message)
	}
}
