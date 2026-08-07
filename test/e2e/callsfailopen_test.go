package e2e_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestAnUnwritableLedgerDropsTheRecordAndNotTheCall is the regression test for
// a breaking change that went the other way on purpose.
//
// The ledger used to refuse a tools/call it could not record, on the rule that
// an unrecorded call is a governance gap. Three things were wrong with that:
// it protected nothing, since the record was already lost when the write
// failed; it put availability in the wrong place, so a full disk took away
// every tool a client had, from a subsystem that decides no permission; and
// the finish is written AFTER the downstream has run, so replacing that
// response reported a failure that had not happened and invited a client to
// repeat a side effect.
//
// What replaced it is one path with one failure direction: the record is
// dropped, logged at Error, and the call is unaffected. That is not something
// a unit test can show convincingly — "the call still ran" is a claim about a
// real downstream and a real client — and it is the kind of rule that gets
// quietly reintroduced by somebody adding a `return err` to a write path.
//
// The ledger is broken by putting a FILE where its directory has to be. That
// works regardless of who the test runs as, which a permission bit does not:
// CI runners are root often enough that a chmod-based version of this test
// would pass by never breaking anything at all.
func TestAnUnwritableLedgerDropsTheRecordAndNotTheCall(t *testing.T) {
	dataDir := t.TempDir()
	runAgenthub(t, dataDir, "", "server", "add", "alpha", "--cmd", fakemcpBin, "--json")
	enableServer(t, dataDir, "alpha")
	// Evidence on as well as metadata: the tier that needs a key and writes
	// packs is the one with the most write paths to fail on.
	runAgenthub(t, dataDir, "", "calls", "enable", "--json")

	// Nothing has been recorded yet, so whatever `calls enable` created here
	// holds no history to lose.
	callsRoot := filepath.Join(dataDir, "calls")
	if err := os.RemoveAll(callsRoot); err != nil {
		t.Fatalf("clearing %s: %v", callsRoot, err)
	}
	if err := os.WriteFile(callsRoot, []byte("not a directory\n"), 0o600); err != nil {
		t.Fatalf("blocking %s: %v", callsRoot, err)
	}

	c := startGateway(t, dataDir, "failopenclient")
	c.initialize()
	c.waitForTool("alpha__echo", 45*time.Second)

	// The whole point, three times over: a single success could be the one
	// call that happened to be recorded before the writer noticed.
	for i, marker := range []string{"unrecorded-1", "unrecorded-2", "unrecorded-3"} {
		got := c.textContent(c.callTool("alpha__echo", map[string]any{"marker": marker}, 45*time.Second))
		if !strings.Contains(got, marker) {
			t.Fatalf("call %d was refused or altered by a ledger that could not record it: %q", i, got)
		}
	}

	// tools/list too. Every upstream request is recorded on the same path, so
	// a refusal reintroduced there would take the catalog away rather than one
	// call — and would look like a discovery bug.
	if !hasTool(c.listTools(30*time.Second), "alpha__echo") {
		t.Fatal("tools/list stopped answering while the ledger was unwritable")
	}
	c.close()

	// The hole is REPORTED. A history that can have holes is acceptable only
	// because it says where they are; failing open and silently would leave an
	// auditor with a ledger that looks complete.
	//
	// This is also the one place the gateway's own log is the only witness —
	// the ledger cannot record its own failure to record — which is why the
	// assertion is on `agenthub logs` at Error rather than on the ledger.
	//
	// The wording here is the OPEN failure, reported once at gateway start
	// ("ledger unavailable; calls run unrecorded"), not the per-record
	// "ledger record dropped" that a store which opened and then failed a
	// write emits. Once is right for this shape: the condition cannot change
	// while the process runs, and a line per call would bury it.
	errLog := gatewayLogText(t, dataDir, "error")
	if !strings.Contains(errLog, "unrecorded") || !strings.Contains(errLog, "ledger") {
		t.Fatalf("nothing at Error says the calls went unrecorded:\n%s", errLog)
	}

	// And the directory was never quietly replaced to make the writes work:
	// a ledger that repaired its own root would have swallowed the condition
	// this test creates.
	info, err := os.Stat(callsRoot)
	if err != nil || info.IsDir() {
		t.Fatalf("%s is no longer the file this test put there (err %v)", callsRoot, err)
	}
}

// gatewayLogText returns `agenthub logs` at or above level, as raw output.
func gatewayLogText(t *testing.T, dataDir, level string) string {
	t.Helper()
	out, _ := runAgenthub(t, dataDir, "", "logs", "--since", "all", "--limit", "0",
		"--level", level, "--source", "gateway", "--json")
	return out
}
