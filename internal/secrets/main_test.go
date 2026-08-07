package secrets

import (
	"context"
	"fmt"
	"os"
	"strconv"
	"testing"
)

// The multi-process acceptance test re-execs this test binary as helper
// processes (the same pattern as internal/ratelimit and internal/registry):
// TestMain diverts to the helper before m.Run when the mode env is set.
//
// It has to be real processes. The in-process tests hold the lock and watch a
// write refuse to proceed, which proves the write takes the lock — but the
// property ruling A.3 #1 asks for is that N processes sharing one vault
// directory do not lose each other's entries, and only N processes can show
// that.

const (
	helperModeEnv   = "AGENTHUB_SECRETS_TEST_HELPER"
	helperDirEnv    = "AGENTHUB_SECRETS_TEST_DIR"
	helperNEnv      = "AGENTHUB_SECRETS_TEST_N"
	helperPrefixEnv = "AGENTHUB_SECRETS_TEST_PREFIX"
)

func TestMain(m *testing.M) {
	switch os.Getenv(helperModeEnv) {
	case "":
		os.Exit(m.Run())
	case "set":
		helperSet()
	default:
		fmt.Fprintln(os.Stderr, "unknown helper mode")
		os.Exit(2)
	}
}

func helperFail(err error) {
	fmt.Fprintln(os.Stderr, "helper:", err)
	os.Exit(1)
}

// helperSet writes N distinct entries into the SHARED vault directory from
// its own process. Every helper writes under its own key prefix, so nothing
// here is a legitimate overwrite: an entry that is missing at the end was
// destroyed by another process's read-modify-write cycle.
//
// It runs the dev fallback (AGENTHUB_DEV_SECRETS=1) rather than a fake
// keyring, because that path exercises the most: the shared secrets.enc map
// AND the read-then-create of secrets.enc.key that every helper reaches at
// the same moment on the first write.
func helperSet() {
	dir := os.Getenv(helperDirEnv)
	n, err := strconv.Atoi(os.Getenv(helperNEnv))
	prefix := os.Getenv(helperPrefixEnv)
	if err != nil || dir == "" || prefix == "" {
		helperFail(fmt.Errorf("bad helper env (dir=%q prefix=%q n=%v)", dir, prefix, err))
	}
	c := NewChain(ChainConfig{
		Dir:       dir,
		LookupEnv: envOf(map[string]string{EnvDevSecrets: "1"}),
		Keyring:   newFakeBackend(nil),
	})
	ctx := context.Background()
	for i := range n {
		ref := Ref{ServerID: "srv", Key: fmt.Sprintf("%s-%d", prefix, i)}
		if err := c.Set(ctx, ref, prefix); err != nil {
			helperFail(fmt.Errorf("set %s: %w", ref.Key, err))
		}
	}
	os.Exit(0)
}
