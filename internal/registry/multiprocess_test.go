package registry

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
)

// Cross-process concurrency acceptance test (M0-6): the test binary re-execs
// itself as helper processes (TestMain helper pattern); several helpers call
// Update concurrently on the same registry directory. Every update inserts a
// distinct server, so every commit is a real change and must bump the
// generation by exactly 1 — the final state proves no update was lost, and
// the per-process generation sequences prove monotonicity under the flock.

const (
	helperModeEnv = "AGENTHUB_REGISTRY_TEST_HELPER"
	helperDirEnv  = "AGENTHUB_REGISTRY_TEST_DIR"
	helperIDEnv   = "AGENTHUB_REGISTRY_TEST_WORKER"
	helperNEnv    = "AGENTHUB_REGISTRY_TEST_ITERS"
)

func TestMain(m *testing.M) {
	if os.Getenv(helperModeEnv) == "1" {
		helperMain()
		return
	}
	os.Exit(m.Run())
}

// helperMain runs inside the re-exec'd test binary: n Updates, each adding a
// unique server, printing the observed generation after each commit.
func helperMain() {
	fail := func(err error) {
		fmt.Fprintln(os.Stderr, "helper:", err)
		os.Exit(1)
	}
	dir := os.Getenv(helperDirEnv)
	id := os.Getenv(helperIDEnv)
	n, err := strconv.Atoi(os.Getenv(helperNEnv))
	if err != nil || dir == "" || id == "" {
		fail(fmt.Errorf("bad helper env (dir=%q id=%q n=%v)", dir, id, err))
	}
	// Generous lock timeout: helpers serialize on the flock and fsync-heavy
	// commits can be slow on CI machines.
	st, err := OpenOptions(dir, Options{LockTimeout: 30 * time.Second})
	if err != nil {
		fail(err)
	}
	for i := 0; i < n; i++ {
		key := fmt.Sprintf("w%s-%d", id, i)
		err := st.Update(context.Background(), func(tx *Tx) error {
			if tx.Servers.V.Servers == nil {
				tx.Servers.V.Servers = map[string]Doc[ServerEntry]{}
			}
			tx.Servers.V.Servers[key] = Doc[ServerEntry]{V: ServerEntry{
				Transport: "stdio", Command: "true", Enabled: true, Source: "helper-" + id,
			}}
			return nil
		})
		if err != nil {
			fail(err)
		}
		fmt.Println(st.Snapshot().Generation)
	}
	os.Exit(0)
}

func TestCrossProcessConcurrentUpdates(t *testing.T) {
	if testing.Short() {
		t.Skip("multi-process test skipped in -short mode")
	}
	dir := t.TempDir()
	const (
		workers = 4
		iters   = 5
		total   = workers * iters
	)

	// Initialize the registry once so helpers race on updates, not on init.
	mustOpen(t, dir)

	type result struct {
		out bytes.Buffer
		err bytes.Buffer
		cmd *exec.Cmd
	}
	procs := make([]*result, workers)
	for w := 0; w < workers; w++ {
		r := &result{}
		// Re-exec this test binary; TestMain diverts to helperMain.
		r.cmd = exec.Command(os.Args[0])
		r.cmd.Env = append(os.Environ(),
			helperModeEnv+"=1",
			helperDirEnv+"="+dir,
			helperIDEnv+"="+strconv.Itoa(w),
			helperNEnv+"="+strconv.Itoa(iters),
		)
		r.cmd.Stdout = &r.out
		r.cmd.Stderr = &r.err
		if err := r.cmd.Start(); err != nil {
			t.Fatalf("start helper %d: %v", w, err)
		}
		procs[w] = r
	}

	seen := make(map[uint64]int, total) // generation -> worker
	for w, r := range procs {
		if err := r.cmd.Wait(); err != nil {
			t.Fatalf("helper %d failed: %v\nstderr: %s", w, err, r.err.String())
		}
		var prev uint64
		lines := strings.Fields(r.out.String())
		if len(lines) != iters {
			t.Fatalf("helper %d printed %d generations, want %d: %q", w, len(lines), iters, r.out.String())
		}
		for _, line := range lines {
			g, err := strconv.ParseUint(line, 10, 64)
			if err != nil {
				t.Fatalf("helper %d printed non-generation %q", w, line)
			}
			if g <= prev {
				t.Errorf("helper %d generations not strictly increasing: %d after %d", w, g, prev)
			}
			prev = g
			if other, dup := seen[g]; dup {
				t.Errorf("generation %d observed by both worker %d and %d (lost update)", g, other, w)
			}
			seen[g] = w
		}
	}

	// Every commit was a real change, so generations must be exactly 1..total.
	for g := uint64(1); g <= total; g++ {
		if _, ok := seen[g]; !ok {
			t.Errorf("generation %d never observed — a bump was skipped or lost", g)
		}
	}

	final := mustOpen(t, dir).Snapshot()
	if final.Generation != total {
		t.Errorf("final generation = %d, want %d", final.Generation, total)
	}
	if n := len(final.Servers.V.Servers); n != total {
		t.Errorf("final server count = %d, want %d (lost update)", n, total)
	}
	for w := 0; w < workers; w++ {
		for i := 0; i < iters; i++ {
			key := fmt.Sprintf("w%d-%d", w, i)
			if _, ok := final.Servers.V.Servers[key]; !ok {
				t.Errorf("server %s missing from final state", key)
			}
		}
	}
}
