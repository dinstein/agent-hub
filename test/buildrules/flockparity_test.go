package buildrules

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestEveryFileLockGoesThroughPlatform keeps the next single-writer file from
// silently shipping with a lock on one platform and nothing on another.
//
// Seven packages keep such a file, each with the same two-file shape: flock.go
// (darwin || linux || windows, delegating to internal/platform) and
// flock_stub.go, which fails closed everywhere else. It used to be three
// files — one per platform — and the missing one was the problem this test
// was written for: a new package copies the pair it finds, and the pair that
// was there first was the Unix implementation and the stub. The result
// compiled everywhere, passed every test on darwin and linux, and locked
// nothing on Windows.
//
// One build-tagged file over all three platforms removes that failure mode
// rather than testing for it — but only while the syscalls stay in
// internal/platform. So the check moved with them, and is now two claims:
// every package with a lock reaches it through internal/platform, and nobody
// outside internal/platform calls the syscalls directly.
//
// The second claim is the one that catches drift, which is what actually
// happened while the copies were per-package: internal/secrets retried EINTR
// around its non-blocking flock and internal/oauthflow did not, so the same
// interrupted syscall cost one a retry and told the other its offline refresh
// was broken. Two files claiming to be the same primitive is how that hides;
// CI does not run on Windows and cannot see the other half at all.
func TestEveryFileLockGoesThroughPlatform(t *testing.T) {
	root := repoRoot(t)

	pkgs := packagesWithFile(t, filepath.Join(root, "internal"), "flock.go")
	if len(pkgs) == 0 {
		t.Fatal("found no package with a flock.go; the walk is wrong, not the tree")
	}

	for _, pkg := range pkgs {
		src, err := os.ReadFile(filepath.Join(root, "internal", pkg, "flock.go"))
		if err != nil {
			t.Fatalf("reading internal/%s/flock.go: %v", pkg, err)
		}
		if !strings.Contains(string(src), "internal/platform") {
			t.Errorf("internal/%s/flock.go does not reach internal/platform.\n"+
				"LockFile/TryLockFile/UnlockFile/IsLockBusy live there so that one EINTR policy, one "+
				"lock byte and one busy predicate serve every caller. A private copy drifts, and the "+
				"Windows half of it is never executed by anything.", pkg)
		}
		if !exists(root, filepath.Join("internal", pkg, "flock_stub.go")) {
			t.Errorf("internal/%s has a flock.go but no flock_stub.go.\n"+
				"flock.go is tagged darwin || linux || windows; without the stub the package does not "+
				"build elsewhere, and the tempting repair — widening the tag — would silently give "+
				"that platform no lock at all.", pkg)
		}
	}

	// The syscalls themselves, anywhere but internal/platform. A package that
	// grows its own is exactly the copy this consolidation removed.
	for rel, src := range goSources(t, root) {
		if strings.HasPrefix(rel, "internal/platform/") {
			continue
		}
		for _, call := range []string{"syscall.Flock(", `NewProc("LockFileEx")`} {
			if strings.Contains(src, call) {
				t.Errorf("%s calls the lock syscall (%s) directly.\n"+
					"File locking has exactly one implementation, in internal/platform "+
					"(filelock_unix.go, filelock_windows.go). Call LockFile/TryLockFile/UnlockFile "+
					"and let IsLockBusy classify the error.", rel, call)
			}
		}
	}
}

// packagesWithFile returns the names of dir's immediate subdirectories that
// contain a file called name.
func packagesWithFile(t *testing.T, dir, name string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("reading %s: %v", dir, err)
	}
	var out []string
	for _, e := range entries {
		if !e.IsDir() || strings.HasPrefix(e.Name(), ".") {
			continue
		}
		if _, err := os.Stat(filepath.Join(dir, e.Name(), name)); err == nil {
			out = append(out, e.Name())
		}
	}
	return out
}
