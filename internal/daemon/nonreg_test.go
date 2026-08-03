package daemon

import (
	"log/slog"
	"testing"

	"github.com/dinstein/agent-hub/internal/calllog"
)

// The control plane and the CLI must read the SAME ledger. They did not: the
// rename moved the constant and left `filepath.Join(dataDir, "audit")` behind
// here, so the CLI read <data>/calls while the daemon read a directory the
// CLI's own migration had just renamed away — the GUI's Calls page went empty
// against a ledger full of records.
func TestNonRegistryDepsUseTheLedgersOwnRoot(t *testing.T) {
	dataDir := t.TempDir()
	deps, _ := nonRegistryDeps(Config{}, dataDir, nil, slog.New(slog.DiscardHandler), nil, nil)

	want := calllog.DirFor(dataDir)
	if deps.CallsRoot != want {
		t.Fatalf("CallsRoot = %q, want the ledger's own root %q", deps.CallsRoot, want)
	}
}
