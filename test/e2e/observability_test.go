package e2e_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// `events` and `logs` are the other two observability streams, and neither had
// any end-to-end coverage. Both make one claim a single-process test cannot:
// the files are written by whichever processes happened to be running and read
// later by a process that was not there.
//
// For `events` that shows up as "works offline" — a stdio gateway writes the
// file with no daemon anywhere — and as the class split, which is the closest
// thing the hub has to "show me what went wrong". For `logs` it is the merge:
// the daemon never dials a downstream, so every connection failure is observed
// and written by a gateway, into a file that until recently nothing could open.

// eventRow is the slice of cli.EventRow these tests read back.
type eventRow struct {
	Scope  string `json:"scope"`
	Kind   string `json:"kind"`
	Class  string `json:"class"`
	Server string `json:"server"`
	Client string `json:"client"`
	Detail string `json:"detail"`
}

// readEvents runs `events` with the given selectors and decodes the rows.
func readEvents(t *testing.T, dataDir string, args ...string) []eventRow {
	t.Helper()
	full := append([]string{"events", "--since", "all", "--limit", "0", "--json"}, args...)
	out, _ := runAgenthub(t, dataDir, "", full...)
	env := lastEnvelope(t, out)
	if !env.OK {
		t.Fatalf("events %v: %s", args, out)
	}
	var list struct {
		Events []eventRow `json:"events"`
		Files  []string   `json:"files"`
	}
	if err := json.Unmarshal(env.Data, &list); err != nil {
		t.Fatalf("events data: %v\n%s", err, env.Data)
	}
	return list.Events
}

func hasEvent(rows []eventRow, kind, server string) bool {
	for _, r := range rows {
		if r.Kind == kind && (server == "" || r.Server == server) {
			return true
		}
	}
	return false
}

// TestEventsAreWrittenOfflineAndSplitByClass drives the event log the way its
// help page describes it: a gateway writes it with no daemon running, and the
// class selector separates the hub working from the hub reacting.
//
// The fixture holds one server that connects and one that cannot, because the
// class split is only observable when both classes are present. A test with
// only the good server could not tell a working filter from one that returns
// everything.
func TestEventsAreWrittenOfflineAndSplitByClass(t *testing.T) {
	dataDir := t.TempDir()

	runAgenthub(t, dataDir, "", "server", "add", "alpha", "--cmd", fakemcpBin, "--json")
	enableServer(t, dataDir, "alpha")
	// A command that does not exist. The spawn fails, which is a real
	// connect_failed rather than a simulated one.
	runAgenthub(t, dataDir, "", "server", "add", "broken",
		"--cmd", filepath.Join(t.TempDir(), "no-such-downstream"), "--json")
	runAgenthub(t, dataDir, "", "server", "enable", "broken", "--no-probe")

	c := startGateway(t, dataDir, "eventclient")
	c.initialize()
	c.waitForTool("alpha__echo", 45*time.Second)
	c.callTool("alpha__echo", map[string]any{"marker": "events"}, 45*time.Second)
	c.close()

	// No daemon was ever started. `events` reading anything at all here is
	// the offline claim, and it is the ordinary case: a stdio client that
	// never opened the application is how most sessions run.
	if _, err := os.Stat(filepath.Join(dataDir, "run", "daemon.json")); err == nil {
		t.Fatal("a daemon was running; this test cannot make the offline claim")
	}

	// The failure has to arrive before it can be filtered. The gateway dials
	// its downstreams in the background, so the broken one lands some time
	// after the good one has already answered.
	var all []eventRow
	deadline := time.Now().Add(30 * time.Second)
	for {
		all = readEvents(t, dataDir)
		if hasEvent(all, "connect_failed", "broken") {
			break
		}
		if !time.Now().Before(deadline) {
			t.Fatalf("no connect_failed for the broken server within 30s: %+v", all)
		}
		time.Sleep(300 * time.Millisecond)
	}
	if !hasEvent(all, "connected", "alpha") {
		t.Fatalf("the working server left no `connected` event: %+v", all)
	}

	// The two classes, each of which must exclude the other's marker kind.
	// Asserting only that a filter returns SOMETHING is the mistake here: it
	// passes against a selector that was never applied.
	disruptions := readEvents(t, dataDir, "--class", "disruption")
	if !hasEvent(disruptions, "connect_failed", "broken") {
		t.Fatalf("a failed connection is not a disruption: %+v", disruptions)
	}
	if hasEvent(disruptions, "connected", "alpha") {
		t.Fatalf("a successful connection was classed as a disruption: %+v", disruptions)
	}
	routine := readEvents(t, dataDir, "--class", "routine")
	if !hasEvent(routine, "connected", "alpha") {
		t.Fatalf("a successful connection is not routine: %+v", routine)
	}
	if hasEvent(routine, "connect_failed", "") {
		t.Fatalf("a failure was classed as routine: %+v", routine)
	}

	// And the other two selectors an operator reaches for first.
	perServer := readEvents(t, dataDir, "--server", "broken")
	if len(perServer) == 0 {
		t.Fatal("--server broken selected nothing")
	}
	for _, r := range perServer {
		if r.Server != "broken" {
			t.Fatalf("--server broken returned a row about %q: %+v", r.Server, r)
		}
	}
	gatewayScope := readEvents(t, dataDir, "--scope", "gateway")
	if len(gatewayScope) == 0 {
		t.Fatal("--scope gateway selected nothing; the gateway records its own lifecycle")
	}
	for _, r := range gatewayScope {
		if r.Scope != "gateway" {
			t.Fatalf("--scope gateway returned a %q row: %+v", r.Scope, r)
		}
	}
}

// logOrigins returns the origin column of every line `logs` printed, in order.
// The human renderer is the instrument here rather than --json, which emits
// each record's ORIGINAL line verbatim: the origin is what the merge adds, and
// the raw line by definition does not carry it.
func logOrigins(t *testing.T, dataDir string, args ...string) []string {
	t.Helper()
	full := append([]string{"logs", "--since", "all", "--limit", "0"}, args...)
	out, _ := runAgenthub(t, dataDir, "", full...)
	var origins []string
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		// time level origin message…
		if fields := strings.Fields(line); len(fields) >= 3 {
			origins = append(origins, fields[2])
		}
	}
	return origins
}

func containsString(all []string, want string) bool {
	for _, s := range all {
		if s == want {
			return true
		}
	}
	return false
}

// TestLogsMergeTheDaemonAndTheGateways is the merge `logs` exists for.
//
// The daemon never dials a downstream, so every connection failure, circuit
// transition and respawn is observed by a gateway and written to
// gateway-<client>.log. Reading either file alone is what hid that half in the
// first place, and a merge is only checkable when two processes have really
// written two files — which makes this a case only this suite can hold.
func TestLogsMergeTheDaemonAndTheGateways(t *testing.T) {
	dataDir, socket, env := sandbox(t)

	runAgenthubEnv(t, env, "", "server", "add", "alpha", "--cmd", fakemcpBin, "--json")
	runAgenthubEnv(t, env, "", "server", "enable", "alpha", "--no-probe")
	runAgenthubEnv(t, env, "", "config", "set", "discovery", "full")
	runAgenthubEnv(t, env, "", "daemon", "start", "--headless")
	h := &daemonEnv{dataDir: dataDir, socket: socket, env: env}
	t.Cleanup(func() { h.killDaemon(t) })

	// Two gateways, so that --client is narrowing to one file rather than
	// selecting the only one there is.
	for _, id := range []string{"loga", "logb"} {
		c := startGatewayEnv(t, env, id)
		c.initialize()
		c.waitForTool("alpha__echo", 45*time.Second)
		c.callTool("alpha__echo", map[string]any{"marker": "logs-" + id}, 45*time.Second)
		c.close()
	}

	merged := logOrigins(t, dataDir)
	if !containsString(merged, "daemon") || !containsString(merged, "gateway") {
		t.Fatalf("the merged stream is missing one of its two halves: %v", merged)
	}

	// Each half alone. A --source that returned everything would satisfy the
	// merge assertion above on its own.
	if got := logOrigins(t, dataDir, "--source", "daemon"); containsString(got, "gateway") || !containsString(got, "daemon") {
		t.Fatalf("--source daemon returned %v", got)
	}
	if got := logOrigins(t, dataDir, "--source", "gateway"); containsString(got, "daemon") || !containsString(got, "gateway") {
		t.Fatalf("--source gateway returned %v", got)
	}

	// --client narrows to one gateway's file. The assertion is on the JSON,
	// where the client field of the record itself is visible: the origin
	// column would say "gateway" for either of them.
	out, _ := runAgenthub(t, dataDir, "", "logs", "--since", "all", "--limit", "0",
		"--client", "loga", "--json")
	if strings.TrimSpace(out) == "" {
		t.Fatal("--client loga returned nothing")
	}
	if strings.Contains(out, `"client":"logb"`) {
		t.Fatalf("--client loga returned records from logb:\n%s", out)
	}
	if !strings.Contains(out, `"client":"loga"`) {
		t.Fatalf("--client loga returned records that name no client:\n%s", out)
	}
}
