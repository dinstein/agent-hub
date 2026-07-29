package concurrency_test

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/dinstein/agent-hub/internal/integrity"
)

// helperLockTimeout is generous on purpose: helpers serialize on the flock
// and fsync-heavy commits are slow on CI machines.
const helperLockTimeout = 30 * time.Second

// helperQuarantine adds n worker-unique entries to the shared quarantine
// store, printing the entry count observed after each Add. If the file lock
// were missing, concurrent read-modify-write cycles would drop entries and
// the parent's final count would be short.
func helperQuarantine() {
	dir := os.Getenv(helperDirEnv)
	id := os.Getenv(helperIDEnv)
	n, err := strconv.Atoi(os.Getenv(helperNEnv))
	if err != nil || dir == "" || id == "" {
		helperFail(fmt.Errorf("bad helper env (dir=%q id=%q n=%v)", dir, id, err))
	}
	store, err := integrity.OpenQuarantineStore(dir, integrity.Options{LockTimeout: helperLockTimeout})
	if err != nil {
		helperFail(err)
	}
	ctx := context.Background()
	for i := 0; i < n; i++ {
		exposed := fmt.Sprintf("srv__w%s-%d", id, i)
		if err := store.Add(ctx, exposed, integrity.QuarantineEntry{
			Server: "srv", Tool: fmt.Sprintf("w%s-%d", id, i), Reason: "concurrency probe",
		}); err != nil {
			helperFail(err)
		}
		snap, err := store.Snapshot(ctx)
		if err != nil {
			helperFail(err)
		}
		fmt.Println(len(snap))
	}
	os.Exit(0)
}

// helperQuarantineChurn adds and releases the SAME key n times. Release must
// report found exactly when this process's own Add is still there; the point
// is that no cycle ever observes a half-written file.
func helperQuarantineChurn() {
	dir := os.Getenv(helperDirEnv)
	id := os.Getenv(helperIDEnv)
	n, err := strconv.Atoi(os.Getenv(helperNEnv))
	if err != nil || dir == "" || id == "" {
		helperFail(fmt.Errorf("bad helper env (dir=%q id=%q n=%v)", dir, id, err))
	}
	store, err := integrity.OpenQuarantineStore(dir, integrity.Options{LockTimeout: helperLockTimeout})
	if err != nil {
		helperFail(err)
	}
	ctx := context.Background()
	exposed := "srv__churn-" + id
	for range n {
		if err := store.Add(ctx, exposed, integrity.QuarantineEntry{
			Server: "srv", Tool: "churn-" + id, Reason: "churn",
		}); err != nil {
			helperFail(err)
		}
		_, found, err := store.Release(ctx, exposed)
		if err != nil {
			helperFail(err)
		}
		fmt.Println(found)
	}
	os.Exit(0)
}

// spawnHelpers re-execs this test binary `workers` times in the given helper
// mode and returns each worker's stdout lines.
func spawnHelpers(t *testing.T, mode, dir string, workers, iters int) [][]string {
	t.Helper()
	type proc struct {
		cmd *exec.Cmd
		out bytes.Buffer
		err bytes.Buffer
	}
	procs := make([]*proc, workers)
	for w := 0; w < workers; w++ {
		p := &proc{cmd: exec.Command(os.Args[0])}
		p.cmd.Env = append(os.Environ(),
			helperModeEnv+"="+mode,
			helperDirEnv+"="+dir,
			helperIDEnv+"="+strconv.Itoa(w),
			helperNEnv+"="+strconv.Itoa(iters),
		)
		p.cmd.Stdout = &p.out
		p.cmd.Stderr = &p.err
		if err := p.cmd.Start(); err != nil {
			t.Fatalf("start helper %d: %v", w, err)
		}
		procs[w] = p
	}
	lines := make([][]string, workers)
	for w, p := range procs {
		if err := p.cmd.Wait(); err != nil {
			t.Fatalf("helper %d failed: %v\nstderr: %s", w, err, p.err.String())
		}
		lines[w] = strings.Fields(p.out.String())
	}
	return lines
}

// A.3 #1: the quarantine file lock. N processes each add M distinct entries;
// every single one must survive. A lost entry means a tool the operator
// quarantined is silently callable again — the failure this lock exists to
// prevent.
func TestCrossProcessQuarantineWritesAreNeverLost(t *testing.T) {
	if testing.Short() {
		t.Skip("multi-process test skipped in -short mode")
	}
	dir := t.TempDir()
	const (
		workers = 4
		iters   = 5
		total   = workers * iters
	)

	lines := spawnHelpers(t, "quarantine", dir, workers, iters)
	for w, out := range lines {
		if len(out) != iters {
			t.Fatalf("helper %d printed %d counts, want %d: %q", w, len(out), iters, out)
		}
		var prev int
		for _, s := range out {
			n, err := strconv.Atoi(s)
			if err != nil {
				t.Fatalf("helper %d printed non-count %q", w, s)
			}
			// Entries are only ever added here, so a shrinking count means a
			// concurrent writer clobbered a committed state.
			if n < prev {
				t.Errorf("helper %d observed the entry count shrink %d -> %d (lost write)", w, prev, n)
			}
			prev = n
		}
	}

	store, err := integrity.OpenQuarantineStore(dir, integrity.Options{})
	if err != nil {
		t.Fatal(err)
	}
	snap, err := store.Snapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(snap) != total {
		t.Fatalf("final entry count = %d, want %d (a quarantine write was lost)", len(snap), total)
	}
	for w := 0; w < workers; w++ {
		for i := 0; i < iters; i++ {
			exposed := fmt.Sprintf("srv__w%d-%d", w, i)
			e, ok := snap[exposed]
			if !ok {
				t.Errorf("entry %s missing from the final state", exposed)
				continue
			}
			if e.Server != "srv" || e.At.IsZero() {
				t.Errorf("entry %s malformed: %+v", exposed, e)
			}
		}
	}
}

// Add/Release churn from several processes must never observe a torn file:
// every Release of a key this process just added has to find it, and the
// store must end up empty.
func TestCrossProcessQuarantineChurnStaysConsistent(t *testing.T) {
	if testing.Short() {
		t.Skip("multi-process test skipped in -short mode")
	}
	dir := t.TempDir()
	const (
		workers = 4
		iters   = 8
	)

	lines := spawnHelpers(t, "quarantine-churn", dir, workers, iters)
	for w, out := range lines {
		if len(out) != iters {
			t.Fatalf("helper %d printed %d results, want %d: %q", w, len(out), iters, out)
		}
		for i, s := range out {
			if s != "true" {
				t.Errorf("helper %d iteration %d released a key it had just added and got %q "+
					"— another process clobbered the entry", w, i, s)
			}
		}
	}

	store, err := integrity.OpenQuarantineStore(dir, integrity.Options{})
	if err != nil {
		t.Fatal(err)
	}
	snap, err := store.Snapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(snap) != 0 {
		t.Fatalf("final entry count = %d, want 0 (every add was released): %+v", len(snap), snap)
	}
}
