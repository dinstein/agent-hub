package buildrules

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestEveryFileLockHasAWindowsImplementation keeps the next single-writer file
// from silently shipping without a lock on one platform.
//
// Five packages keep one, each with the same three-file shape: flock_unix.go
// (syscall.Flock), flock_windows.go (internal/platform's LockFileEx) and
// flock_stub.go for everything else. The sixth is the problem this test
// exists for. Copying the pair a new package starts from is copying
// flock_unix.go and flock_stub.go — the two that were there first — and the
// result compiles on every platform, passes every test on darwin and linux,
// and locks nothing on Windows.
//
// That omission is invisible from CI, which does not run on Windows and never
// executes the branch: `go build` is happy, the stub satisfies the call sites,
// and the failure needs two Windows processes writing the same file to show
// up at all. So the check is on the FILES, which is the only place the gap is
// visible from here.
func TestEveryFileLockHasAWindowsImplementation(t *testing.T) {
	root := repoRoot(t)
	pkgs := packagesWithFile(t, filepath.Join(root, "internal"), "flock_unix.go")
	if len(pkgs) == 0 {
		t.Fatal("found no package with a flock_unix.go; the walk is wrong, not the tree")
	}

	for _, pkg := range pkgs {
		if !exists(root, filepath.Join("internal", pkg, "flock_windows.go")) {
			t.Errorf("internal/%s locks a file on Unix but has no flock_windows.go.\n"+
				"Delegate to internal/platform (LockFile/TryLockFile/UnlockFile/IsLockBusy) the way "+
				"the other packages do. Without it the Windows build takes whatever flock_stub.go "+
				"does — which for two of these packages used to be 'return nil', i.e. no lock at "+
				"all, indistinguishable from a working one until two processes corrupt the file.", pkg)
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
