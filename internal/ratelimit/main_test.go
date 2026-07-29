package ratelimit

import (
	"fmt"
	"os"
	"strconv"
	"testing"
	"time"
)

// The multi-process acceptance test re-execs this test binary as helper
// processes (same pattern as internal/audit and internal/registry): TestMain
// diverts to the helper before m.Run when the mode env is set.

const (
	helperModeEnv   = "AGENTHUB_RATELIMIT_TEST_HELPER"
	helperDirEnv    = "AGENTHUB_RATELIMIT_TEST_DIR"
	helperNEnv      = "AGENTHUB_RATELIMIT_TEST_N"
	helperLimitEnv  = "AGENTHUB_RATELIMIT_TEST_LIMIT"
	helperWindowEnv = "AGENTHUB_RATELIMIT_TEST_WINDOW"
)

func TestMain(m *testing.M) {
	switch os.Getenv(helperModeEnv) {
	case "":
		os.Exit(m.Run())
	case "allow":
		helperAllow()
	default:
		fmt.Fprintln(os.Stderr, "unknown helper mode")
		os.Exit(2)
	}
}

func helperFail(err error) {
	fmt.Fprintln(os.Stderr, "helper:", err)
	os.Exit(1)
}

// helperAllow hammers the SHARED counter file from its own process and
// prints how many calls it was granted. The parent sums the grants of every
// helper: the sum must equal the limit exactly, which is only true if each
// process re-read and merged the file under the lock instead of writing back
// its own stale copy.
func helperAllow() {
	dir := os.Getenv(helperDirEnv)
	n, err := strconv.Atoi(os.Getenv(helperNEnv))
	if err != nil || dir == "" {
		helperFail(fmt.Errorf("bad helper env (dir=%q n=%v)", dir, err))
	}
	limit, err := strconv.Atoi(os.Getenv(helperLimitEnv))
	if err != nil {
		helperFail(err)
	}
	window, err := time.ParseDuration(os.Getenv(helperWindowEnv))
	if err != nil {
		helperFail(err)
	}
	lim, err := New(Options{
		Config:   Config{Rules: []Rule{{Limit: limit, Window: Duration(window)}}},
		StateDir: dir,
	})
	if err != nil {
		helperFail(err)
	}
	key := Key{Client: "c", Server: "s", Tool: "t"}
	granted := 0
	for range n {
		dec := lim.Allow(key)
		if dec.Degraded {
			helperFail(fmt.Errorf("degraded decision in helper: %s", dec))
		}
		if dec.Allowed {
			granted++
		}
	}
	fmt.Println(granted)
	os.Exit(0)
}
