package ratelimit

import (
	"bytes"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// TestMultiProcessCountersMerge is the reason this package exists.
//
// N gateway processes share one counter file. The reference implementation
// read the file, decided in memory, and wrote its own copy back — so two
// processes racing each erased the other's increment and the quota silently
// multiplied by N. Here every process re-reads the state INSIDE the
// exclusive lock, so the grants across all processes must sum to exactly the
// limit — no more (mutual overwrite) and no fewer (lost updates).
func TestMultiProcessCountersMerge(t *testing.T) {
	if testing.Short() {
		t.Skip("re-execs the test binary")
	}
	const (
		procs    = 4
		attempts = 25
		limit    = 20
		// A long window means no meaningful refill happens during the test,
		// so the expected total is exact rather than timing-dependent.
		window = time.Hour
	)
	dir := t.TempDir()

	var (
		wg      sync.WaitGroup
		mu      sync.Mutex
		total   int
		outputs []string
	)
	start := make(chan struct{})
	for range procs {
		wg.Add(1)
		go func() {
			defer wg.Done()
			cmd := exec.Command(os.Args[0])
			cmd.Env = append(os.Environ(),
				helperModeEnv+"=allow",
				helperDirEnv+"="+dir,
				helperNEnv+"="+strconv.Itoa(attempts),
				helperLimitEnv+"="+strconv.Itoa(limit),
				helperWindowEnv+"="+window.String(),
			)
			var out, errb bytes.Buffer
			cmd.Stdout = &out
			cmd.Stderr = &errb
			<-start
			if err := cmd.Run(); err != nil {
				mu.Lock()
				outputs = append(outputs, "helper failed: "+err.Error()+": "+errb.String())
				mu.Unlock()
				return
			}
			granted, err := strconv.Atoi(strings.TrimSpace(out.String()))
			if err != nil {
				mu.Lock()
				outputs = append(outputs, "unparsable helper output "+out.String())
				mu.Unlock()
				return
			}
			mu.Lock()
			total += granted
			outputs = append(outputs, strconv.Itoa(granted))
			mu.Unlock()
		}()
	}
	close(start)
	wg.Wait()

	for _, o := range outputs {
		if strings.HasPrefix(o, "helper") || strings.HasPrefix(o, "unparsable") {
			t.Fatal(o)
		}
	}
	if total != limit {
		t.Fatalf("processes were granted %d calls in total (per-process: %v), want exactly %d\n"+
			"more than the limit = counters overwrote each other; fewer = lost admissions",
			total, outputs, limit)
	}
}
