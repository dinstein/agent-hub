//go:build !windows

package platform_test

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/dinstein/agent-hub/internal/platform"
)

// TestFileLockUnsupportedOffWindows pins the failure DIRECTION of the
// stand-ins, which is the only half of this API that can be tested on the
// machines that run the tests.
//
// It matters because the alternative shape — returning nil, "there is no lock
// to take here" — is what two of the seven callers' stubs used to do, and a
// lock call that succeeds without locking is indistinguishable from a working
// one until two processes corrupt a file. Anything that reaches these
// functions has picked the wrong implementation for its platform and must be
// told so, not humoured.
func TestFileLockUnsupportedOffWindows(t *testing.T) {
	f, err := os.Create(filepath.Join(t.TempDir(), "lockme"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()

	for name, call := range map[string]func(*os.File) error{
		"LockFile":    platform.LockFile,
		"TryLockFile": platform.TryLockFile,
		"UnlockFile":  platform.UnlockFile,
	} {
		if err := call(f); !errors.Is(err, platform.ErrUnsupportedPlatform) {
			t.Errorf("%s err = %v, want ErrUnsupportedPlatform", name, err)
		}
	}

	// Those errors are permanent, not contention: a caller that read them as
	// "busy" would retry forever instead of refusing to write unlocked.
	if platform.IsLockBusy(platform.TryLockFile(f)) {
		t.Error("IsLockBusy reported an unsupported-platform error as contention")
	}
	if platform.IsLockBusy(nil) {
		t.Error("IsLockBusy(nil) = true")
	}
}
