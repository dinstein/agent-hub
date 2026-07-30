package e2e_test

import (
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
)

// This file covers the `server` verbs the rest of the suite only ever used
// as scaffolding: ls, inspect, disable and rm were never asserted on, and
// `enable` was never once run the way an operator runs it — every other
// fixture goes through enableServer, which passes --no-probe precisely so
// that setup does not depend on a downstream being reachable yet.
//
// So the probe — connect to the server before putting it into service,
// report what it needs — has no coverage outside internal/cli, where
// "connect" means an in-process fake rather than a spawned child. Both
// cases below run it for real.

// serverRow is the slice of cli.ServerRow these tests read back.
type serverRow struct {
	ID        string   `json:"id"`
	Transport string   `json:"transport"`
	Command   string   `json:"command"`
	Args      []string `json:"args"`
	Enabled   bool     `json:"enabled"`
	Trace     bool     `json:"trace"`
}

// serverToggle is the `server enable` / `server disable` result.
type serverToggle struct {
	ID      string `json:"id"`
	Enabled bool   `json:"enabled"`
	Changed bool   `json:"changed"`
	Probe   *struct {
		Reachable bool   `json:"reachable"`
		NeedsAuth bool   `json:"needsAuth"`
		Tools     int    `json:"tools"`
		Detail    string `json:"detail"`
	} `json:"probe"`
}

// serverInspectView is the slice of cli.ServerInspect these tests read back.
type serverInspectView struct {
	Server    serverRow `json:"server"`
	ToolCount int       `json:"tool_count"`
	CacheOnly bool      `json:"cache_only"`
	TraceLog  string    `json:"trace_log"`
}

// listServers reads `server ls --json`, whose data is a plain array.
func listServers(t *testing.T, dataDir string) []serverRow {
	t.Helper()
	out, _ := runAgenthub(t, dataDir, "", "server", "ls", "--json")
	env := lastEnvelope(t, out)
	if !env.OK {
		t.Fatalf("server ls --json: %s", out)
	}
	var rows []serverRow
	if err := json.Unmarshal(env.Data, &rows); err != nil {
		t.Fatalf("server ls data: %v\n%s", err, env.Data)
	}
	return rows
}

// serverByID returns the one row for id, failing when it is absent.
func serverByID(t *testing.T, dataDir, id string) serverRow {
	t.Helper()
	rows := listServers(t, dataDir)
	for _, r := range rows {
		if r.ID == id {
			return r
		}
	}
	t.Fatalf("server %q is not in `server ls`: %+v", id, rows)
	return serverRow{}
}

// inspectServer reads `server inspect <id> --json`.
func inspectServer(t *testing.T, dataDir, id string) serverInspectView {
	t.Helper()
	out, _ := runAgenthub(t, dataDir, "", "server", "inspect", id, "--json")
	env := lastEnvelope(t, out)
	if !env.OK {
		t.Fatalf("server inspect %s: %s", id, out)
	}
	var view serverInspectView
	if err := json.Unmarshal(env.Data, &view); err != nil {
		t.Fatalf("server inspect data: %v\n%s", err, env.Data)
	}
	return view
}

// toggleServer runs `server enable`/`server disable` and decodes the result.
// enable is deliberately run WITHOUT --no-probe: the probe is what this file
// is here for.
func toggleServer(t *testing.T, dataDir, verb, id string) serverToggle {
	t.Helper()
	out, _ := runAgenthub(t, dataDir, "", "server", verb, id, "--json")
	env := lastEnvelope(t, out)
	if !env.OK {
		t.Fatalf("server %s %s: %s", verb, id, out)
	}
	var tog serverToggle
	if err := json.Unmarshal(env.Data, &tog); err != nil {
		t.Fatalf("server %s data: %v\n%s", verb, err, env.Data)
	}
	return tog
}

// TestServerLifecycleThroughTheRealBinary walks one entry through add -> ls
// -> inspect -> enable -> disable -> rm against the real binary, asserting
// at each step the thing the help page promises:
//
//   - `add` writes the definition and leaves it OFF ("'enable' connects and
//     puts it into service" — an add that switched things on would put a
//     server in front of every client the moment it was recorded);
//   - `inspect` answers before anything has ever connected, and says so
//     rather than reporting an empty tool list as fact;
//   - `enable` really does dial the downstream first;
//   - `disable` takes it away from everybody, and `rm` leaves nothing behind.
func TestServerLifecycleThroughTheRealBinary(t *testing.T) {
	dataDir := t.TempDir()

	runAgenthub(t, dataDir, "", "server", "add", "life", "--cmd", fakemcpBin, "--json")

	added := serverByID(t, dataDir, "life")
	if added.Enabled {
		t.Errorf("`server add` put the entry into service: %+v", added)
	}
	if added.Command != fakemcpBin {
		t.Errorf("command = %q, want %q", added.Command, fakemcpBin)
	}
	if added.Transport != "stdio" {
		t.Errorf("transport = %q, want stdio", added.Transport)
	}

	// Nothing has connected yet, so there is no tool list to show. The
	// distinction that matters is between "no tools" and "not asked yet":
	// inspect reads the cache the gateway fills, and reports cache_only so
	// the zero is not mistaken for an answer from the server.
	before := inspectServer(t, dataDir, "life")
	if before.Server.Enabled {
		t.Errorf("inspect disagrees with ls about enabled: %+v", before.Server)
	}
	if before.ToolCount != 0 {
		t.Errorf("tool_count = %d before anything connected, want 0", before.ToolCount)
	}

	// The probe: a real spawn of the real downstream binary, before the
	// entry goes into service.
	tog := toggleServer(t, dataDir, "enable", "life")
	if !tog.Enabled || !tog.Changed {
		t.Errorf("enable = %+v, want enabled and changed", tog)
	}
	if tog.Probe == nil {
		t.Fatalf("enable ran no probe: %+v", tog)
	}
	if !tog.Probe.Reachable {
		t.Fatalf("probe could not reach the fake downstream: %+v", *tog.Probe)
	}
	// fakemcp.Minimal() serves exactly one tool. A probe that connects but
	// counts nothing would mean the handshake completed and tools/list did
	// not, which is the failure this count is here to separate out.
	if tog.Probe.Tools != 1 {
		t.Errorf("probe tools = %d, want 1", tog.Probe.Tools)
	}

	if row := serverByID(t, dataDir, "life"); !row.Enabled {
		t.Errorf("the entry is not enabled after `server enable`: %+v", row)
	}

	if tog := toggleServer(t, dataDir, "disable", "life"); tog.Enabled {
		t.Errorf("disable = %+v, want enabled=false", tog)
	}
	if row := serverByID(t, dataDir, "life"); row.Enabled {
		t.Errorf("the entry survived `server disable` switched on: %+v", row)
	}

	// rm takes the definition with it: the id must be gone from the listing
	// AND unaddressable, not merely hidden from `ls`.
	runAgenthub(t, dataDir, "", "server", "rm", "life", "--json")
	for _, r := range listServers(t, dataDir) {
		if r.ID == "life" {
			t.Fatalf("`server rm` left the entry in the listing: %+v", r)
		}
	}
	code, out := runAgenthubExit(t, dataDir, "", "server", "inspect", "life", "--json")
	if code == 0 {
		t.Fatalf("`server inspect` still answers for a removed server: %s", out)
	}
}

// TestServerEnableReportsAnUnreachableProbeWithoutRefusing pins the
// deliberate asymmetry documented at internal/cli/serverinspect.go:400 —
// "the probe REPORTS, it does not veto".
//
// Enabling is a statement that the operator wants to use this server, and a
// downstream that happens to be down right now must not be able to reverse
// it: refusing would turn a transient outage into a configuration change,
// and the operator would find the entry switched off later with nothing
// saying who did it.
//
// It is also the only e2e case where the probe's failure path runs at all —
// everywhere else the downstream answers.
func TestServerEnableReportsAnUnreachableProbeWithoutRefusing(t *testing.T) {
	dataDir := t.TempDir()

	// A command that cannot be spawned: the probe fails at exec, before any
	// protocol is spoken.
	missing := filepath.Join(t.TempDir(), "no-such-downstream")
	runAgenthub(t, dataDir, "", "server", "add", "broken", "--cmd", missing, "--json")

	// Exit 0 is half the assertion: runAgenthub fails the test on a non-zero
	// exit, so reaching the next line already proves the enable was not
	// refused.
	tog := toggleServer(t, dataDir, "enable", "broken")
	if tog.Probe == nil {
		t.Fatalf("enable ran no probe: %+v", tog)
	}
	if tog.Probe.Reachable {
		t.Errorf("probe reported a spawnable downstream at %s: %+v", missing, *tog.Probe)
	}
	if tog.Probe.Detail == "" {
		t.Error("an unreachable probe must say what went wrong, or the operator " +
			"is told only that something failed")
	}
	if tog.Probe.NeedsAuth {
		t.Errorf("a failed spawn was classified as a login problem: %+v", *tog.Probe)
	}

	// The point of the case: the entry really is in service.
	if row := serverByID(t, dataDir, "broken"); !row.Enabled {
		t.Errorf("an unreachable probe vetoed the enable: %+v", row)
	}
	if !strings.Contains(inspectServer(t, dataDir, "broken").Server.Command, "no-such-downstream") {
		t.Error("inspect lost the configured command")
	}
}
