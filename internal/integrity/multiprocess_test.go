package integrity

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

// Cross-process concurrency acceptance test: the test binary re-execs itself
// as helper processes (TestMain helper pattern); several helpers run
// CheckServer concurrently against the same pin store. The multi-writer
// discipline (docs/flows.md: N gateways + daemon share the state files with
// no single-writer assumption) requires that no pin write is ever lost and
// that first sight of each tool is classified New in exactly one process.

const (
	helperModeEnv = "AGENTHUB_INTEGRITY_TEST_HELPER"
	helperDirEnv  = "AGENTHUB_INTEGRITY_TEST_DIR"
	helperIDEnv   = "AGENTHUB_INTEGRITY_TEST_WORKER"
	helperNEnv    = "AGENTHUB_INTEGRITY_TEST_ITERS"
)

func TestMain(m *testing.M) {
	if os.Getenv(helperModeEnv) == "1" {
		helperMain()
		return
	}
	os.Exit(m.Run())
}

// helperMain runs inside the re-exec'd test binary: n CheckServer calls,
// each observing one worker-unique tool, printing "<tool> <kind>" per drift
// concerning that tool.
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
	// writes can be slow on CI machines.
	store, err := OpenPinStore(dir, Options{LockTimeout: 30 * time.Second})
	if err != nil {
		fail(err)
	}
	ctx := context.Background()
	for i := 0; i < n; i++ {
		tool := fmt.Sprintf("w%s-%d", id, i)
		drifts, err := store.CheckServer(ctx, "shared", []ToolSnapshot{
			{Name: tool, Description: "worker " + id},
		})
		if err != nil {
			fail(err)
		}
		for _, d := range drifts {
			if d.Tool == tool {
				fmt.Printf("%s %s\n", d.Tool, d.Kind)
			}
		}
	}
	os.Exit(0)
}

func TestCrossProcessConcurrentPinWrites(t *testing.T) {
	if testing.Short() {
		t.Skip("multi-process test skipped in -short mode")
	}
	dir := t.TempDir()
	const (
		workers = 4
		iters   = 5
		total   = workers * iters
	)

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

	// Every worker must see its own tool as New exactly once (first
	// CheckServer that pins it) — a duplicate New for the same tool would
	// mean a lost pin write that a later call re-created.
	newSeen := map[string]int{}
	for w, r := range procs {
		if err := r.cmd.Wait(); err != nil {
			t.Fatalf("helper %d failed: %v\nstderr: %s", w, err, r.err.String())
		}
		for _, line := range strings.Split(strings.TrimSpace(r.out.String()), "\n") {
			fields := strings.Fields(line)
			if len(fields) != 2 {
				t.Fatalf("helper %d printed %q", w, line)
			}
			if fields[1] == string(DriftNew) {
				newSeen[fields[0]]++
			}
		}
	}
	for w := 0; w < workers; w++ {
		for i := 0; i < iters; i++ {
			tool := fmt.Sprintf("w%d-%d", w, i)
			if n := newSeen[tool]; n != 1 {
				t.Errorf("tool %s classified New %d times, want exactly 1 (lost pin write)", tool, n)
			}
		}
	}

	// Final store contains every pin (merge never deletes, no lost updates).
	store, err := OpenPinStore(dir, Options{})
	if err != nil {
		t.Fatal(err)
	}
	pins, err := store.Pins(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if n := len(pins["shared"]); n != total {
		t.Errorf("final pin count = %d, want %d", n, total)
	}
	for w := 0; w < workers; w++ {
		for i := 0; i < iters; i++ {
			tool := fmt.Sprintf("w%d-%d", w, i)
			pin, ok := pins["shared"][tool]
			if !ok {
				t.Errorf("pin for %s missing from final state", tool)
				continue
			}
			if pin.HashSchemaVersion != HashSchemaVersion || !strings.HasPrefix(pin.Hash, HashSchemaVersion+":") {
				t.Errorf("pin for %s malformed: %+v", tool, pin)
			}
		}
	}
}
