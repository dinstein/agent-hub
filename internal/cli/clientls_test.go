package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestClientLsJoinsConnectAndBind: one row per client, answering both
// questions — is agenthub in its configuration file, and which profile
// decides what it sees.
func TestClientLsJoinsConnectAndBind(t *testing.T) {
	setDataDir(t)
	home := t.TempDir()
	t.Setenv("HOME", home)

	// A client that exists but was never connected.
	write(t, filepath.Join(home, ".cursor", "mcp.json"), `{"mcpServers":{"linear":{"command":"npx"}}}`)
	// A client agenthub is wired into.
	mustRun(t, "", "client", "connect", "claude-code")
	mustRun(t, "", "profile", "create", "work")
	mustRun(t, "", "client", "bind", "cursor", "work")

	var list ClientList
	decodeInto(t, mustRun(t, "", "client", "ls", "--json"), &list)
	rows := map[string]ClientRow{}
	for _, c := range list.Clients {
		rows[c.Client] = c
	}

	cc, ok := rows["claude-code"]
	if !ok {
		t.Fatalf("claude-code missing from the overview: %+v", list.Clients)
	}
	if !cc.Connected || cc.State != "connected" {
		t.Errorf("claude-code = %+v, want connected", cc)
	}
	if len(cc.Placements) != 1 || cc.Placements[0] != "user" {
		t.Errorf("claude-code placements = %v, want the user file", cc.Placements)
	}
	if cc.Binding != "followActive" {
		t.Errorf("an unbound client must follow the active profile: %+v", cc)
	}

	cur, ok := rows["cursor"]
	if !ok {
		t.Fatalf("cursor missing from the overview: %+v", list.Clients)
	}
	if cur.Connected || cur.State != "not_connected" {
		t.Errorf("cursor = %+v, want a read file with no gateway entry", cur)
	}
	if !cur.Read {
		t.Errorf("cursor's file was parsed; Read must say so: %+v", cur)
	}
	if cur.Profile != "work" || cur.Dangling {
		t.Errorf("cursor binding = %+v, want profile work", cur)
	}

	// A client with no configuration file here is settled by the stat
	// alone: nothing was opened, and the answer is still "no".
	var all ClientList
	decodeInto(t, mustRun(t, "", "client", "ls", "--all", "--json"), &all)
	found := false
	for _, c := range all.Clients {
		if c.Client != "windsurf" {
			continue
		}
		found = true
		if c.State != "not_connected" || c.Read {
			t.Errorf("windsurf = %+v, want not_connected without opening anything", c)
		}
	}
	if !found {
		t.Errorf("--all must list every supported client: %+v", all.Clients)
	}

	// The human table carries both answers.
	out := mustRun(t, "", "client", "ls")
	if !strings.Contains(out, "CONNECTED") || !strings.Contains(out, "PROFILE") {
		t.Errorf("client ls table = %q", out)
	}
}

// TestClientLsStatOnlyOpensNothing is the invariant --stat-only exists for.
// The file is readable only by its mode bits: any attempt to open it fails
// with a permission error and would surface as "denied", so a run that
// reports plain "?" and no warning is a run that never opened it.
func TestClientLsStatOnlyOpensNothing(t *testing.T) {
	requireUnprivilegedCLI(t)
	setDataDir(t)
	home := t.TempDir()
	t.Setenv("HOME", home)
	path := filepath.Join(home, ".cursor", "mcp.json")
	write(t, path, `{"mcpServers":{}}`)
	if err := os.Chmod(path, 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(path, 0o644) })

	// The default listing opens it, is refused, and says so — never "no".
	var read ClientList
	env := decodeInto(t, mustRun(t, "", "client", "ls", "--json"), &read)
	if len(read.Clients) != 1 || read.Clients[0].State != "denied" {
		t.Fatalf("default listing = %+v, want denied", read.Clients)
	}
	if len(env.Warnings) == 0 || !strings.Contains(strings.Join(env.Warnings, " "), "cursor") {
		t.Errorf("a file that could not be read must warn: %v", env.Warnings)
	}

	var statted ClientList
	quiet := decodeInto(t, mustRun(t, "", "client", "ls", "--stat-only", "--json"), &statted)
	if len(statted.Clients) != 1 {
		t.Fatalf("stat-only listing = %+v, want the client still listed", statted.Clients)
	}
	row := statted.Clients[0]
	if row.State != "unknown" || row.Read || row.Connected {
		t.Errorf("stat-only row = %+v, want an admitted unknown", row)
	}
	if !statted.StatOnly {
		t.Error("the result must record that nothing was opened")
	}
	if len(quiet.Warnings) != 0 {
		t.Errorf("stat-only opened the file after all: %v", quiet.Warnings)
	}
}

// TestClientLsAdmitsFormatsItDoesNotParse: a client whose format agenthub
// does not read is "?" with an explanation, never "no". Reporting that as
// not connected would send the user to run connect against a file connect
// also refuses to write.
func TestClientLsAdmitsFormatsItDoesNotParse(t *testing.T) {
	setDataDir(t)
	home := t.TempDir()
	t.Setenv("HOME", home)
	write(t, filepath.Join(home, ".continue", "config.yaml"), "mcpServers:\n  - name: x\n")

	var list ClientList
	decodeInto(t, mustRun(t, "", "client", "ls", "--json"), &list)
	if len(list.Clients) != 1 {
		t.Fatalf("listing = %+v, want the continue row", list.Clients)
	}
	row := list.Clients[0]
	if row.State != "unknown" || row.Connected || row.Note == "" {
		t.Errorf("continue row = %+v, want an explained unknown", row)
	}
	if out := mustRun(t, "", "client", "ls"); !strings.Contains(out, "NOTE") {
		t.Errorf("the note column must appear when a row has one: %q", out)
	}
}

// TestClientLsReadsCodexTOML: agenthub will not rewrite codex's TOML, but
// it does read it, so the row is a real answer rather than a "?" — and the
// entry is matched by ownership, not by the name it was given.
func TestClientLsReadsCodexTOML(t *testing.T) {
	setDataDir(t)
	home := t.TempDir()
	t.Setenv("HOME", home)
	cfg := filepath.Join(home, ".codex", "config.toml")
	write(t, cfg, "# my notes\n[mcp_servers.linear]\ncommand = \"npx\"\n")

	var before ClientList
	decodeInto(t, mustRun(t, "", "client", "ls", "--json"), &before)
	if len(before.Clients) != 1 || before.Clients[0].State != "not_connected" || !before.Clients[0].Read {
		t.Fatalf("codex row = %+v, want a read file with no gateway entry", before.Clients)
	}

	// Named anything, owned all the same.
	write(t, cfg, "# my notes\n[mcp_servers.linear]\ncommand = \"npx\"\n\n"+
		"[mcp_servers.hub]\ncommand = \"/opt/homebrew/bin/agenthub\"\n"+
		"args = [\"connect\", \"--client\", \"codex\"]\n")
	var after ClientList
	decodeInto(t, mustRun(t, "", "client", "ls", "--json"), &after)
	row := after.Clients[0]
	if row.State != "connected" || !row.Connected {
		t.Fatalf("codex row = %+v, want connected", row)
	}
	if len(row.Placements) != 1 || row.Placements[0] != "user" {
		t.Errorf("placements = %v, want the user file", row.Placements)
	}

	// Connect still refuses — reading is not writing — and the refusal
	// hands over the command that does work.
	code, _, errOut := runCLI(t, "", "client", "connect", "codex")
	if code != ExitGeneral {
		t.Errorf("connect exit = %d, want %d (agenthub must not rewrite TOML)", code, ExitGeneral)
	}
	if !strings.Contains(errOut, "codex mcp add") {
		t.Errorf("the refusal must name the command that works: %q", errOut)
	}
}

// write creates a file and its parents.
func write(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

// requireUnprivilegedCLI skips permission tests for root, which bypasses
// the mode bits they depend on.
func requireUnprivilegedCLI(t *testing.T) {
	t.Helper()
	if os.Geteuid() == 0 {
		t.Skip("running as root: permission bits are not enforced")
	}
}

// TestClientListRendersRowsItWasNotGiven: a row whose fields were never
// filled (an empty binding kind, no placements) still renders. The zero
// VALUE of ClientList takes the early return and never reaches the table,
// so this is the case that actually exercises the row formatting.
func TestClientListRendersRowsItWasNotGiven(t *testing.T) {
	var sb strings.Builder
	if err := (ClientList{Clients: []ClientRow{{Client: "x"}}}).Human(&sb); err != nil {
		t.Fatalf("Human: %v", err)
	}
	out := sb.String()
	if !strings.Contains(out, "x") || !strings.Contains(out, "?") || !strings.Contains(out, "-") {
		t.Errorf("empty row rendered as %q, want an admitted unknown", out)
	}
}

// TestClientInspectExplainsTheRow: inspect opens the files ls only
// summarises — which location holds agenthub's entry, what else is in that
// file, and the entry that points at a binary which is no longer there.
func TestClientInspectExplainsTheRow(t *testing.T) {
	setDataDir(t)
	home := t.TempDir()
	t.Setenv("HOME", home)
	mustRun(t, "", "client", "connect", "cursor", "--bin", filepath.Join(home, "gone", "agenthub"))
	write(t, filepath.Join(home, ".cursor", "extra.json"), `{}`) // not a config location

	var view ClientInspectView
	decodeInto(t, mustRun(t, "", "client", "inspect", "cursor", "--json"), &view)
	if view.State != "connected" || !view.Connected {
		t.Fatalf("inspect = %+v, want connected", view)
	}
	if len(view.Placements) != 1 || view.Placements[0] != "user" {
		t.Errorf("placements = %v, want user", view.Placements)
	}
	var owned *ClientInspectServer
	for _, f := range view.Files {
		for i, s := range f.Servers {
			if s.Owned {
				owned = &f.Servers[i]
			}
		}
	}
	if owned == nil {
		t.Fatalf("no owned entry reported: %+v", view.Files)
	}
	if !owned.Stale {
		t.Errorf("entry = %+v, want it flagged: connect wrote a binary that is not there", owned)
	}

	// Every location is reported, present or not — "we looked here too" is
	// half the answer to "why does it say no".
	if len(view.Files) < 2 {
		t.Errorf("files = %+v, want both of cursor's locations", view.Files)
	}

	// An id nobody supports is a not-found, not a crash or an empty answer.
	if code, _, _ := runCLI(t, "", "client", "inspect", "nope"); code != ExitNotFound {
		t.Errorf("unknown client exit = %d, want %d", code, ExitNotFound)
	}
}
