package e2e_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"
)

// `client detect`, `client inspect` and `client unbind` are the three client
// verbs with no e2e coverage, and they are the ones that touch somebody
// else's files: detect and inspect look at whatever an AI client already has
// on this machine, and connect/disconnect rewrite it.
//
// That is testable end to end because client discovery goes through HOME
// (internal/cli/clientls_test.go does the same in-process). The cases below
// plant a home directory, so nothing here can see — or write — the real one.

// detectedRow is the slice of cli.DetectedRow these tests read back.
type detectedRow struct {
	Client    string `json:"client"`
	Placement string `json:"placement"`
	Path      string `json:"path"`
	Writable  bool   `json:"writable"`
	Size      int64  `json:"size"`
}

// inspectServerRow is one MCP server as `client inspect` reports it.
type inspectServerRow struct {
	Name    string `json:"name"`
	Command string `json:"command"`
	Owned   bool   `json:"owned"`
}

// inspectFileRow is one configuration file as `client inspect` reports it.
type inspectFileRow struct {
	Path      string             `json:"path"`
	Placement string             `json:"placement"`
	Exists    bool               `json:"exists"`
	Parsed    bool               `json:"parsed"`
	Connected bool               `json:"connected"`
	Servers   []inspectServerRow `json:"servers"`
	Error     string             `json:"error"`
}

// clientInspectResult is the slice of cli.ClientInspectView these tests read.
type clientInspectResult struct {
	Client     string           `json:"client"`
	State      string           `json:"state"`
	Connected  bool             `json:"connected"`
	Placements []string         `json:"placements"`
	Files      []inspectFileRow `json:"files"`
}

// userFile returns the user-placement file of an inspect result. Every client
// has more than one candidate path (a project file beside the working
// directory, a user file under HOME), and only the user one is planted here.
func (v clientInspectResult) userFile(t *testing.T) inspectFileRow {
	t.Helper()
	for _, f := range v.Files {
		if f.Placement == "user" {
			return f
		}
	}
	t.Fatalf("no user-placement file in the inspect result: %+v", v.Files)
	return inspectFileRow{}
}

// names lists the MCP servers of one file, in the order reported.
func (f inspectFileRow) names() []string {
	out := make([]string, 0, len(f.Servers))
	for _, s := range f.Servers {
		out = append(out, s.Name)
	}
	return out
}

// plantedHome is a temp HOME holding one Cursor configuration with a foreign
// MCP server already in it, plus the child environment that points at it.
// The foreign entry is the point: every write agenthub makes to a client
// file has to leave it alone.
func plantedHome(t *testing.T, dataDir string) (home string, env []string) {
	t.Helper()
	home = t.TempDir()
	cfg := filepath.Join(home, ".cursor", "mcp.json")
	if err := os.MkdirAll(filepath.Dir(cfg), 0o700); err != nil {
		t.Fatal(err)
	}
	const doc = `{"mcpServers":{"linear":{"command":"npx","args":["-y","linear-mcp"]}}}`
	if err := os.WriteFile(cfg, []byte(doc), 0o600); err != nil {
		t.Fatal(err)
	}
	return home, envWith(t, testEnv(dataDir), "HOME="+home)
}

// inspectClient reads `client inspect <id> --json`.
func inspectClient(t *testing.T, env []string, id string) clientInspectResult {
	t.Helper()
	out, _ := runAgenthubEnv(t, env, "", "client", "inspect", id, "--json")
	e := lastEnvelope(t, out)
	if !e.OK {
		t.Fatalf("client inspect %s: %s", id, out)
	}
	var view clientInspectResult
	if err := json.Unmarshal(e.Data, &view); err != nil {
		t.Fatalf("client inspect data: %v\n%s", err, e.Data)
	}
	return view
}

// detectClients reads `client detect --json`.
func detectClients(t *testing.T, env []string) []detectedRow {
	t.Helper()
	out, _ := runAgenthubEnv(t, env, "", "client", "detect", "--json")
	e := lastEnvelope(t, out)
	if !e.OK {
		t.Fatalf("client detect: %s", out)
	}
	var list struct {
		Found []detectedRow `json:"found"`
	}
	if err := json.Unmarshal(e.Data, &list); err != nil {
		t.Fatalf("client detect data: %v\n%s", err, e.Data)
	}
	return list.Found
}

// findDetected returns the detected row for id.
func findDetected(t *testing.T, rows []detectedRow, id string) detectedRow {
	t.Helper()
	for _, r := range rows {
		if r.Client == id {
			return r
		}
	}
	t.Fatalf("client %q was not detected: %+v", id, rows)
	return detectedRow{}
}

// TestClientDetectStatsWhileInspectReads pins the division of labour the two
// commands' help pages describe, and the reason for it: detect "checks only
// that the config files exist, never opening them", because reading every
// client's data on sight sets off one macOS privacy prompt per client.
//
// A regression here is invisible in a unit test and loud on a user's machine
// — detect is the FIRST command a new user runs, and a detect that parses
// would greet them with a row of permission dialogs.
//
// The corrupted file is how the difference is made observable without
// inspecting syscalls: unparseable content changes what a reader can say and
// nothing about what a stat can say. It also pins the second half — inspect
// classifies that file as `unreadable`, not `not_connected`. Those two
// states exist precisely so a consumer cannot treat "we looked and agenthub
// is not there" and "we could not look" alike (internal/cli/clientbind.go:84).
func TestClientDetectStatsWhileInspectReads(t *testing.T) {
	dataDir := t.TempDir()
	home, env := plantedHome(t, dataDir)
	cfg := filepath.Join(home, ".cursor", "mcp.json")

	row := findDetected(t, detectClients(t, env), "cursor")
	if row.Placement != "user" || row.Path != cfg {
		t.Errorf("detected %+v, want the planted user file %s", row, cfg)
	}
	if !row.Writable || row.Size == 0 {
		t.Errorf("detected row says nothing useful about the file: %+v", row)
	}

	// inspect DOES read it, and reports the neighbour it found.
	view := inspectClient(t, env, "cursor")
	if view.State != "not_connected" || view.Connected {
		t.Errorf("state = %q/%v, want not_connected", view.State, view.Connected)
	}
	file := view.userFile(t)
	if !file.Exists || !file.Parsed {
		t.Fatalf("inspect did not read the planted file: %+v", file)
	}
	if got := file.names(); !slices.Equal(got, []string{"linear"}) {
		t.Errorf("servers = %v, want [linear]", got)
	}

	// Break the file. A stat still answers; a parse cannot.
	if err := os.WriteFile(cfg, []byte("this is not json {{{"), 0o600); err != nil {
		t.Fatal(err)
	}

	// runAgenthubEnv fails the test on a non-zero exit, so detect surviving
	// at all is half the assertion.
	broken := findDetected(t, detectClients(t, env), "cursor")
	if broken.Path != cfg {
		t.Errorf("detect stopped naming the file once it stopped parsing: %+v", broken)
	}

	view = inspectClient(t, env, "cursor")
	if view.State != "unreadable" {
		t.Errorf("state = %q for an unparseable config, want unreadable — "+
			"reporting not_connected would claim agenthub is absent from a file nobody could read",
			view.State)
	}
	file = view.userFile(t)
	if !file.Exists || file.Parsed {
		t.Errorf("file = %+v, want exists and not parsed", file)
	}
	if file.Error == "" {
		t.Error("an unreadable file must carry the reason it could not be read")
	}
}

// TestClientConnectLeavesForeignServersAlone drives connect and disconnect
// against a client file that already has somebody else's MCP server in it.
//
// This is the highest-consequence write agenthub makes: the file belongs to
// another application, the user did not ask for anything else in it to
// change, and an over-broad rewrite is discovered only when a colleague's
// tooling stops working. TestClientConnectWritesConfig already pins what the
// agenthub entry looks like — through --path, on a file agenthub created —
// so what is left untested is the neighbours, and the default placement
// resolution that finds the file under HOME with no --path at all.
func TestClientConnectLeavesForeignServersAlone(t *testing.T) {
	dataDir := t.TempDir()
	home, env := plantedHome(t, dataDir)
	cfg := filepath.Join(home, ".cursor", "mcp.json")

	out, _ := runAgenthubEnv(t, env, "", "client", "connect", "cursor", "--json")
	e := lastEnvelope(t, out)
	if !e.OK {
		t.Fatalf("client connect cursor: %s", out)
	}
	var connected struct {
		Path    string `json:"path"`
		Backup  string `json:"backup"`
		Changed bool   `json:"changed"`
	}
	if err := json.Unmarshal(e.Data, &connected); err != nil {
		t.Fatalf("client connect data: %v\n%s", err, e.Data)
	}
	if connected.Path != cfg || !connected.Changed {
		t.Errorf("connect wrote %+v, want a change to %s", connected, cfg)
	}
	// The backup is what makes the rewrite recoverable, so its absence is a
	// failure even when the rewrite itself is correct.
	if connected.Backup == "" {
		t.Error("connect reported no backup of the file it rewrote")
	} else if _, err := os.Stat(connected.Backup); err != nil {
		t.Errorf("the reported backup is not on disk: %v", err)
	}

	view := inspectClient(t, env, "cursor")
	if view.State != "connected" || !view.Connected {
		t.Errorf("state = %q/%v after connect, want connected", view.State, view.Connected)
	}
	if !slices.Contains(view.Placements, "user") {
		t.Errorf("placements = %v, want the user file named", view.Placements)
	}
	file := view.userFile(t)
	if !slices.Contains(file.names(), "linear") {
		t.Fatalf("connect dropped the foreign server: %v", file.names())
	}
	var ours inspectServerRow
	for _, s := range file.Servers {
		if s.Owned {
			ours = s
		}
	}
	if ours.Name == "" {
		t.Fatalf("no agenthub-owned entry after connect: %+v", file.Servers)
	}
	if ours.Command != agenthubBin {
		// Resolve symlinks before complaining: t.TempDir on macOS hands back
		// a /var path that is really /private/var.
		wantBin, err1 := filepath.EvalSymlinks(agenthubBin)
		gotBin, err2 := filepath.EvalSymlinks(ours.Command)
		if err1 != nil || err2 != nil || gotBin != wantBin {
			t.Errorf("entry command = %q, want %q", ours.Command, agenthubBin)
		}
	}

	runAgenthubEnv(t, env, "", "client", "disconnect", "cursor", "--json")

	after := inspectClient(t, env, "cursor").userFile(t)
	if got := after.names(); !slices.Equal(got, []string{"linear"}) {
		t.Errorf("servers after disconnect = %v, want [linear] — disconnect must take "+
			"exactly what connect added and nothing else", got)
	}
}

// TestClientUnbindWidensTheSurface is the live half of `client unbind`.
//
// Its help page says so outright: unbinding "can WIDEN what it sees if it was
// on a narrow profile", because the client falls back to the active profile
// or, with none set, to every enabled server. That makes it the one client
// verb that grants access, and a claim of that shape should not rest on
// reading clients.json — the gateway is what decides what the client
// actually gets.
func TestClientUnbindWidensTheSurface(t *testing.T) {
	dataDir := t.TempDir()
	twoServerProfile(t, dataDir, "kept", "unbindclient")

	c := startGateway(t, dataDir, "unbindclient")
	c.initialize()
	c.waitForTool("alpha__echo", 30*time.Second)
	if names := c.listTools(30 * time.Second); hasTool(names, "beta__echo") {
		c.fatalf("the bound client already saw beta: %v", names)
	}

	// No profile is active, so the fallback is every enabled server.
	runAgenthub(t, dataDir, "", "client", "unbind", "unbindclient")
	c.waitForTool("beta__echo", 30*time.Second)
	if names := c.listTools(30 * time.Second); !hasTool(names, "alpha__echo") {
		c.fatalf("unbinding lost alpha while gaining beta: %v", names)
	}
	c.close()
}
