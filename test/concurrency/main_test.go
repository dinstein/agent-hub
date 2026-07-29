// Package concurrency_test holds the cross-process concurrency acceptance
// tests of canonical.md §6 that span more than one internal package, and
// therefore have no natural home inside one of them.
//
// The multi-writer discipline is a CORRECTNESS
// dependency in the v2 topology (N gateways + the daemon share every state
// file), not an insurance policy — so each mechanism gets a test that runs
// REAL processes, not goroutines. Goroutine-level tests would pass even if
// the flock were removed, because the Go mutexes inside each store would
// still serialize them.
//
// The per-package halves live where their mechanism does:
//
//	internal/registry  — generation monotonicity under concurrent Update
//	internal/integrity — pin writes (CheckServer) under concurrent processes
//	internal/audit     — O_APPEND single-line writes, security dedup window
//
// This package adds the quarantine file lock and the pins/quarantine
// interleaving that crosses both stores.
package concurrency_test

import (
	"fmt"
	"os"
	"testing"
)

// Helper protocol: the test binary re-execs ITSELF with helperModeEnv set to
// the name of the helper to run (the pattern used by internal/registry and
// internal/audit). Every helper writes its observations to stdout, one line
// per observation, and exits non-zero on any error.
const (
	helperModeEnv = "AGENTHUB_CONCURRENCY_HELPER"
	helperDirEnv  = "AGENTHUB_CONCURRENCY_DIR"
	helperIDEnv   = "AGENTHUB_CONCURRENCY_WORKER"
	helperNEnv    = "AGENTHUB_CONCURRENCY_ITERS"
)

func TestMain(m *testing.M) {
	switch mode := os.Getenv(helperModeEnv); mode {
	case "":
		os.Exit(m.Run())
	case "quarantine":
		helperQuarantine()
	case "quarantine-churn":
		helperQuarantineChurn()
	default:
		fmt.Fprintf(os.Stderr, "unknown helper mode %q\n", mode)
		os.Exit(2)
	}
}

func helperFail(err error) {
	fmt.Fprintln(os.Stderr, "helper:", err)
	os.Exit(1)
}
