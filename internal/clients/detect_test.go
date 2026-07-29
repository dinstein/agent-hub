package clients_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dinstein/agent-hub/internal/clients"
)

// TestDetectQuietWhenNothingInstalled: a machine with no AI clients yields
// an empty slice, not a pile of not-found noise. Detect is a scan, and a
// scan that reports absences is unusable.
func TestDetectQuietWhenNothingInstalled(t *testing.T) {
	e := newEnv(t, "darwin")
	got := e.tbl.Detect(context.Background(), e.project)
	if len(got) != 0 {
		t.Fatalf("Detect on a clean machine = %+v, want empty", got)
	}

	// A directory sitting where a config file would be is not a config.
	if err := os.MkdirAll(filepath.Join(e.home, ".cursor", "mcp.json"), 0o755); err != nil {
		t.Fatal(err)
	}
	if got := e.tbl.Detect(context.Background(), e.project); len(got) != 0 {
		t.Fatalf("Detect reported a directory as a config: %+v", got)
	}
}

// TestDetectFindsFilesAcrossPlacements: project and user files of several
// clients are found, ordered by client ID, and carry the shape metadata a
// caller needs to explain them.
func TestDetectFindsFilesAcrossPlacements(t *testing.T) {
	e := newEnv(t, "darwin")
	write(t, filepath.Join(e.project, ".mcp.json"), `{"mcpServers":{}}`)
	write(t, filepath.Join(e.home, ".cursor", "mcp.json"), `{"mcpServers":{}}`)
	write(t, filepath.Join(e.home, ".codex", "config.toml"), "[mcp_servers]\n")

	got := e.tbl.Detect(context.Background(), e.project)
	if len(got) != 3 {
		t.Fatalf("Detect = %+v, want 3 findings", got)
	}
	byClient := map[string]clients.Detected{}
	prev := ""
	for _, d := range got {
		if d.Client < prev {
			t.Errorf("results are not ordered by client id: %v after %v", d.Client, prev)
		}
		prev = d.Client
		byClient[d.Client] = d
	}

	cc := byClient["claude-code"]
	if cc.Placement != clients.Project || cc.Shape != clients.ShapeServerMap || !cc.Writable {
		t.Errorf("claude-code finding = %+v", cc)
	}
	if cc.Size == 0 || cc.Modified.IsZero() {
		t.Errorf("claude-code finding lacks stat metadata: %+v", cc)
	}
	cu := byClient["cursor"]
	if cu.Placement != clients.User || cu.Path != filepath.Join(e.home, ".cursor", "mcp.json") {
		t.Errorf("cursor finding = %+v", cu)
	}
	cx := byClient["codex"]
	if cx.Shape != clients.ShapeTOML || cx.Writable || cx.Note == "" {
		t.Errorf("codex finding = %+v (must be probe-only and explained)", cx)
	}
	for _, d := range got {
		if d.Denied || d.Err != nil {
			t.Errorf("%s reported denied on a readable file: %+v", d.Client, d)
		}
	}
}

// TestDetectStatsButNeverReads is the macOS TCC invariant expressed as a
// behaviour: a file whose CONTENT is unreadable but
// whose metadata is visible must still be detected. If Detect ever opened
// files, this would fail — and on a real Mac it would raise a privacy
// prompt per client instead.
func TestDetectStatsButNeverReads(t *testing.T) {
	requireUnprivileged(t)
	e := newEnv(t, "darwin")
	path := filepath.Join(e.home, ".cursor", "mcp.json")
	write(t, path, `{"mcpServers":{"x":{"command":"npx"}}}`)
	if err := os.Chmod(path, 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(path, 0o644) })

	got := e.tbl.Detect(context.Background(), e.project)
	if len(got) != 1 || got[0].Client != "cursor" || got[0].Denied {
		t.Fatalf("Detect = %+v, want one undenied cursor finding (stat only)", got)
	}

	// Reading the same file IS refused, and with the typed error.
	insp, err := e.tbl.Inspect("cursor", e.project)
	var pe *clients.PermissionError
	if !errors.As(err, &pe) {
		t.Fatalf("Inspect err = %v, want *PermissionError", err)
	}
	if pe.Op != "read" || pe.Remediation == "" || pe.HTTPStatus() != 403 {
		t.Errorf("PermissionError = %+v (op/remediation/status)", pe)
	}
	if !clients.IsPermission(err) {
		t.Error("IsPermission did not recognise the error")
	}
	denied := false
	for _, f := range insp.Files {
		if f.Err != nil && f.Exists {
			denied = true
		}
	}
	if !denied {
		t.Errorf("Inspect must still report the file it could not read: %+v", insp.Files)
	}
	if insp.Err() == nil {
		t.Error("Inspection.Err() must expose the first per-file failure")
	}
}

// TestDetectClassifiesDeniedStat: when even the stat is refused (an
// unsearchable parent directory, the shape macOS TCC actually takes), the
// location is reported as denied with remediation — never dropped as
// "not found", because the two call for opposite user actions.
func TestDetectClassifiesDeniedStat(t *testing.T) {
	requireUnprivileged(t)
	e := newEnv(t, "darwin")
	dir := filepath.Join(e.home, ".cursor")
	write(t, filepath.Join(dir, "mcp.json"), `{"mcpServers":{}}`)
	if err := os.Chmod(dir, 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o755) })

	got := e.tbl.Detect(context.Background(), e.project)
	if len(got) != 1 {
		t.Fatalf("Detect = %+v, want the denied location reported", got)
	}
	d := got[0]
	if d.Client != "cursor" || !d.Denied || d.Err == nil {
		t.Fatalf("finding = %+v, want denied", d)
	}
	if d.Err.Op != "stat" || d.Remediation == "" || d.Remediation != d.Err.Remediation {
		t.Errorf("denial detail = %+v", d.Err)
	}
	if d.Size != 0 {
		t.Errorf("denied finding must not claim stat metadata: %+v", d)
	}

	// A denial is not a not-found: Connect surfaces the same typed error.
	var pe *clients.PermissionError
	_, err := e.format(t, "cursor").Connect(filepath.Join(dir, "mcp.json"), entry("cursor"))
	if !errors.As(err, &pe) {
		t.Fatalf("Connect err = %v, want *PermissionError", err)
	}
	var nc *clients.NotConnectedError
	if errors.As(err, &nc) {
		t.Error("a denial must never be reported as not-connected")
	}
}

// TestDetectRemediationIsPlatformSpecific: the macOS text names the TCC
// remedy (Full Disk Access); other platforms must not.
func TestDetectRemediationIsPlatformSpecific(t *testing.T) {
	requireUnprivileged(t)
	for _, tc := range []struct{ goos, want string }{
		{"darwin", "Full Disk Access"},
		{"linux", "ownership"},
	} {
		e := newEnv(t, tc.goos)
		dir := filepath.Join(e.home, ".cursor")
		write(t, filepath.Join(dir, "mcp.json"), `{}`)
		if err := os.Chmod(dir, 0o000); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = os.Chmod(dir, 0o755) })

		got := e.tbl.Detect(context.Background(), e.project)
		if len(got) != 1 || !got[0].Denied {
			t.Fatalf("%s: Detect = %+v", tc.goos, got)
		}
		if !contains(got[0].Remediation, tc.want) {
			t.Errorf("%s remediation = %q, want it to mention %q", tc.goos, got[0].Remediation, tc.want)
		}
	}
}

// TestDetectHonoursContextCancellation: a cancelled scan stops early and
// returns what it had, rather than statting the whole table.
func TestDetectHonoursContextCancellation(t *testing.T) {
	e := newEnv(t, "darwin")
	write(t, filepath.Join(e.project, ".mcp.json"), `{}`)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if got := e.tbl.Detect(ctx, e.project); len(got) != 0 {
		t.Errorf("cancelled Detect = %+v, want empty", got)
	}
}

// TestInspectReportsServers: the detail view parses one client's files and
// marks the agenthub entry as owned.
func TestInspectReportsServers(t *testing.T) {
	e := newEnv(t, "darwin")
	write(t, filepath.Join(e.project, ".mcp.json"), `{"mcpServers":{
      "fs": {"command": "npx", "args": ["-y", "server-filesystem"]},
      "remote": {"type": "sse", "url": "https://example.com/sse"},
      "agenthub": {"command": "/usr/local/bin/agenthub", "args": ["connect", "--client", "claude-code"]},
      "off": {"command": "x", "disabled": true}
  }}`)
	insp, err := e.tbl.Inspect("claude-code", e.project)
	if err != nil {
		t.Fatalf("inspect: %v", err)
	}
	if len(insp.Files) != 2 {
		t.Fatalf("files = %+v, want project + user candidates", insp.Files)
	}
	f := insp.Files[0]
	if !f.Exists || !f.Parsed || !f.Connected {
		t.Fatalf("project file = %+v", f)
	}
	if insp.Files[1].Exists {
		t.Errorf("user file must be reported absent: %+v", insp.Files[1])
	}
	want := map[string]struct {
		transport string
		owned     bool
		disabled  bool
	}{
		"agenthub": {"stdio", true, false},
		"fs":       {"stdio", false, false},
		"off":      {"stdio", false, true},
		"remote":   {"sse", false, false},
	}
	if len(f.Servers) != len(want) {
		t.Fatalf("servers = %+v", f.Servers)
	}
	for _, s := range f.Servers {
		w, ok := want[s.Name]
		if !ok {
			t.Errorf("unexpected server %q", s.Name)
			continue
		}
		if s.Transport != w.transport || s.Owned != w.owned || s.Disabled != w.disabled {
			t.Errorf("%s = %+v, want %+v", s.Name, s, w)
		}
	}

	// A client agenthub will not rewrite is still READ when its shape has a
	// reader: refusing to re-encode TOML never required refusing to look.
	write(t, filepath.Join(e.home, ".codex", "config.toml"), "[mcp_servers.x]\ncommand='y'\n")
	ci, err := e.tbl.Inspect("codex", e.project)
	if err != nil {
		t.Fatalf("inspect codex: %v", err)
	}
	if len(ci.Files) != 1 || !ci.Files[0].Exists || !ci.Files[0].Parsed {
		t.Fatalf("codex inspection = %+v, want the file read", ci.Files)
	}
	if len(ci.Files[0].Servers) != 1 || ci.Files[0].Servers[0].Name != "x" ||
		ci.Files[0].Servers[0].Command != "y" || ci.Files[0].Servers[0].Owned {
		t.Errorf("codex servers = %+v, want one foreign entry", ci.Files[0].Servers)
	}
	if ci.Manual == "" || ci.Note == "" {
		t.Errorf("probe-only inspection must explain itself: %+v", ci)
	}

	// A YAML client has no reader, and says so by staying unparsed rather
	// than by reporting an empty server list.
	write(t, filepath.Join(e.home, ".continue", "config.yaml"), "mcpServers:\n  - name: x\n")
	yi, err := e.tbl.Inspect("continue", e.project)
	if err != nil {
		t.Fatalf("inspect continue: %v", err)
	}
	user := yi.Files[len(yi.Files)-1]
	if !user.Exists || user.Parsed || len(user.Servers) != 0 {
		t.Errorf("continue inspection = %+v, want existence only", user)
	}

	var uce *clients.UnknownClientError
	if _, err := e.tbl.Inspect("nope", e.project); !errors.As(err, &uce) {
		t.Fatalf("unknown client: err = %v, want *UnknownClientError", err)
	}
}

// requireUnprivileged skips permission tests for root, which bypasses the
// mode bits they depend on.
func requireUnprivileged(t *testing.T) {
	t.Helper()
	if os.Geteuid() == 0 {
		t.Skip("running as root: permission bits are not enforced")
	}
}

// TestConnectStateReportsWhereAndFailsLoud pins the reduction Inspection ->
// (state, placements): where the gateway entry is, and — the whole point of
// the type — that a location agenthub could not read never comes out as
// "not connected".
func TestConnectStateReportsWhereAndFailsLoud(t *testing.T) {
	e := newEnv(t, "darwin")

	// Nothing on disk: knowably not connected.
	insp, err := e.tbl.Inspect("cursor", e.project)
	if err != nil {
		t.Fatalf("inspect: %v", err)
	}
	if state, where := insp.ConnectState(); state != clients.ConnectedNo || len(where) != 0 {
		t.Errorf("empty machine = (%q, %v), want not_connected", state, where)
	}

	// Connected in both of a client's placements: both are named, because
	// "connected in one file while another still holds an entry" is the
	// state a disconnect has to know about.
	f := e.format(t, "cursor")
	for _, p := range []clients.Placement{clients.Project, clients.User} {
		if _, err := f.Connect(f.PathFor(e.project, p), entry("cursor")); err != nil {
			t.Fatalf("connect %s: %v", p, err)
		}
	}
	insp, err = e.tbl.Inspect("cursor", e.project)
	if err != nil {
		t.Fatalf("inspect: %v", err)
	}
	state, where := insp.ConnectState()
	if state != clients.ConnectedYes || len(where) != 2 {
		t.Fatalf("both placements = (%q, %v), want connected in two", state, where)
	}
	if where[0] != clients.Project || where[1] != clients.User {
		t.Errorf("placements = %v, want the table's own order (project first)", where)
	}

	// A file we may not read is NOT evidence of absence.
	requireUnprivileged(t)
	e2 := newEnv(t, "darwin")
	blocked := filepath.Join(e2.home, ".cursor", "mcp.json")
	write(t, blocked, `{"mcpServers":{}}`)
	if err := os.Chmod(blocked, 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(blocked, 0o644) })
	insp, _ = e2.tbl.Inspect("cursor", e2.project)
	if state, _ := insp.ConnectState(); state != clients.ConnectedDenied {
		t.Errorf("unreadable file = %q, want denied (never not_connected)", state)
	}

	// Neither is a file agenthub refuses to interpret.
	e3 := newEnv(t, "darwin")
	write(t, filepath.Join(e3.project, ".cursor", "mcp.json"), "{not json")
	insp, _ = e3.tbl.Inspect("cursor", e3.project)
	if state, _ := insp.ConnectState(); state != clients.ConnectedUnreadable {
		t.Errorf("unparseable file = %q, want unreadable (never not_connected)", state)
	}

	// Nor a shape whose syntax is not ours to read at all.
	e4 := newEnv(t, "darwin")
	write(t, filepath.Join(e4.home, ".continue", "config.yaml"), "mcpServers:\n  - name: x\n")
	insp, _ = e4.tbl.Inspect("continue", e4.project)
	if state, _ := insp.ConnectState(); state != clients.ConnectedUnknown {
		t.Errorf("unreadable shape = %q, want unknown (never not_connected)", state)
	}

	// A TOML client IS read, so it gets a real answer both ways.
	e5 := newEnv(t, "darwin")
	write(t, filepath.Join(e5.home, ".codex", "config.toml"), "[mcp_servers.x]\ncommand = 'y'\n")
	insp, _ = e5.tbl.Inspect("codex", e5.project)
	if state, _ := insp.ConnectState(); state != clients.ConnectedNo {
		t.Errorf("codex without our entry = %q, want not_connected", state)
	}
	write(t, filepath.Join(e5.home, ".codex", "config.toml"),
		"[mcp_servers.agenthub]\ncommand = '/opt/agenthub/bin/agenthub'\n"+
			"args = [\"connect\", \"--client\", \"codex\"]\n")
	insp, _ = e5.tbl.Inspect("codex", e5.project)
	state, where = insp.ConnectState()
	if state != clients.ConnectedYes || len(where) != 1 || where[0] != clients.User {
		t.Errorf("codex with our entry = (%q, %v), want connected in the user file", state, where)
	}

	// ...and a document the scanner does not fully understand goes back to
	// unknown rather than claiming the entry is absent.
	e6 := newEnv(t, "darwin")
	write(t, filepath.Join(e6.home, ".codex", "config.toml"), "mcp_servers = { agenthub = { command = 'x' } }\n")
	insp, _ = e6.tbl.Inspect("codex", e6.project)
	if state, _ := insp.ConnectState(); state != clients.ConnectedUnknown {
		t.Errorf("unmodelled TOML = %q, want unknown (never not_connected)", state)
	}
}

// TestProbeOpsAnswerFromTheFileTheyWillNotWrite: a client agenthub reads
// but does not rewrite still gets real answers out of connect and
// disconnect — including the one that was plainly wrong, disconnect
// replying with the instructions for ADDING an entry.
func TestProbeOpsAnswerFromTheFileTheyWillNotWrite(t *testing.T) {
	e := newEnv(t, "darwin")
	cfg := filepath.Join(e.home, ".codex", "config.toml")
	f := e.format(t, "codex")
	ours := entry("codex")

	// Nothing of ours in the file: there is nothing to remove, which is a
	// different answer from "I am not allowed to remove it".
	write(t, cfg, "[mcp_servers.linear]\ncommand = \"npx\"\n")
	var nc *clients.NotConnectedError
	if _, err := f.Disconnect(cfg); !errors.As(err, &nc) {
		t.Fatalf("disconnect with nothing of ours = %v, want *NotConnectedError", err)
	}

	// Our entry, under a name the user chose. Disconnect must refuse to
	// write, and hand over the removal — naming the entry that is actually
	// there, not the name agenthub would have used.
	write(t, cfg, "[mcp_servers.hub]\ncommand = \""+ours.Command+"\"\n"+
		"args = [\"connect\", \"--client\", \"codex\"]\n")
	var unsupported *clients.UnsupportedError
	_, err := f.Disconnect(cfg)
	if !errors.As(err, &unsupported) {
		t.Fatalf("disconnect = %v, want *UnsupportedError", err)
	}
	if !strings.Contains(unsupported.Snippet, "codex mcp remove hub") {
		t.Errorf("removal snippet = %q, want it to name the entry that is there", unsupported.Snippet)
	}
	if strings.Contains(unsupported.Snippet, "mcp add") {
		t.Errorf("a disconnect answered with the ADD instructions: %q", unsupported.Snippet)
	}

	// The requested state already holds: report it, do not send the user to
	// hand-edit a file that needs no editing.
	write(t, cfg, "[mcp_servers.agenthub]\ncommand = \""+ours.Command+"\"\n"+
		"args = [\"connect\", \"--client\", \"codex\"]\n")
	res, err := f.Connect(cfg, ours)
	if err != nil {
		t.Fatalf("connect on an already-correct file = %v, want the up-to-date result", err)
	}
	if res.Changed {
		t.Errorf("connect reported a change to a file it cannot write: %+v", res)
	}

	// The entry is there but points elsewhere: refuse, and say which one.
	write(t, cfg, "[mcp_servers.hub]\ncommand = \"/old/agenthub\"\n"+
		"args = [\"connect\", \"--client\", \"codex\"]\n")
	if _, err := f.Connect(cfg, ours); !errors.As(err, &unsupported) {
		t.Fatalf("connect over a stale entry = %v, want *UnsupportedError", err)
	} else if !strings.Contains(unsupported.Snippet, "\"hub\"") {
		t.Errorf("snippet = %q, want the existing entry named", unsupported.Snippet)
	}

	// A shape with no reader keeps the old behaviour: refuse both ways
	// without pretending to know what is in the file.
	yf := e.format(t, "continue")
	ypath := filepath.Join(e.home, ".continue", "config.yaml")
	write(t, ypath, "mcpServers:\n  - name: agenthub\n")
	if _, err := yf.Disconnect(ypath); !errors.As(err, &unsupported) {
		t.Fatalf("continue disconnect = %v, want *UnsupportedError", err)
	}
	if strings.Contains(unsupported.Snippet, "mcpServers:\n  - name") {
		t.Errorf("continue disconnect answered with the ADD snippet: %q", unsupported.Snippet)
	}
}
