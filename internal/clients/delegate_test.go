package clients_test

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dinstein/agent-hub/internal/clients"
)

// fakeCodex puts a stand-in `codex` on PATH that edits the file it is told
// about, and returns the table that is allowed to run it.
//
// No test may invoke the REAL codex: delegation runs another application's
// CLI, and that application edits the developer's own configuration. The
// default table refuses to delegate for exactly that reason; this helper is
// the only way back in.
func fakeCodex(t *testing.T, e env, script string) *clients.Table {
	t.Helper()
	dir := t.TempDir()
	bin := filepath.Join(dir, "codex")
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("FAKE_CODEX_CONFIG", filepath.Join(e.home, ".codex", "config.toml"))
	return clients.New(clients.Options{GOOS: "darwin", Home: e.home, BackupDir: e.backups})
}

// workingCodex edits the TOML the way codex does: append a table for add,
// drop it for remove.
const workingCodex = `#!/bin/sh
set -e
cfg="$FAKE_CODEX_CONFIG"
case "$1 $2" in
"mcp add")
  name="$3"; shift 4
  cmd="$1"; shift
  args=""
  for a in "$@"; do args="$args, \"$a\""; done
  args="${args#, }"
  printf '\n[mcp_servers.%s]\ncommand = "%s"\nargs = [%s]\n' "$name" "$cmd" "$args" >> "$cfg"
  ;;
"mcp remove")
  name="$3"
  awk -v n="[mcp_servers.$name]" '/^\[/{skip=($0==n)} skip!=1{print}' "$cfg" > "$cfg.tmp"
  mv "$cfg.tmp" "$cfg"
  ;;
esac
`

func TestDelegateConnectRunsTheClientsOwnCLI(t *testing.T) {
	e := newEnv(t, "darwin")
	cfg := filepath.Join(e.home, ".codex", "config.toml")
	write(t, cfg, "# keep me\n[mcp_servers.linear]\ncommand = \"npx\"\n")
	tbl := fakeCodex(t, e, workingCodex)
	f, _ := tbl.Lookup("codex")

	res, err := f.Connect(cfg, entry("codex"))
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	if !res.Changed {
		t.Errorf("result = %+v, want a change", res)
	}
	// The undo does not get weaker just because agenthub was not the one
	// holding the pen.
	if res.Backup == "" {
		t.Errorf("no backup taken before handing the file to another program: %+v", res)
	}
	body, err := os.ReadFile(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(body), "# keep me") {
		t.Errorf("delegation lost the rest of the file:\n%s", body)
	}
	insp, _ := tbl.Inspect("codex", e.project)
	if state, _ := insp.ConnectState(); state != clients.ConnectedYes {
		t.Errorf("after connect the client reads as %q", state)
	}

	// Idempotent: the state already holds, so nothing runs and nothing
	// changes.
	again, err := f.Connect(cfg, entry("codex"))
	if err != nil || again.Changed {
		t.Errorf("second connect = (%+v, %v), want an unchanged result", again, err)
	}
}

func TestDelegateDisconnectRunsTheClientsOwnCLI(t *testing.T) {
	e := newEnv(t, "darwin")
	cfg := filepath.Join(e.home, ".codex", "config.toml")
	write(t, cfg, "# keep me\n[mcp_servers.hub]\ncommand = \"/opt/agenthub/bin/agenthub\"\n"+
		"args = [\"connect\", \"--client\", \"codex\"]\n")
	tbl := fakeCodex(t, e, workingCodex)
	f, _ := tbl.Lookup("codex")

	res, err := f.Disconnect(cfg)
	if err != nil {
		t.Fatalf("disconnect: %v", err)
	}
	// Removed by ownership, under the name it actually had.
	if len(res.Removed) != 1 || res.Removed[0] != "hub" {
		t.Errorf("removed = %v, want [hub]", res.Removed)
	}
	if res.Backup == "" {
		t.Errorf("no backup: %+v", res)
	}
	body, _ := os.ReadFile(cfg)
	if strings.Contains(string(body), "mcp_servers.hub") {
		t.Errorf("the entry is still there:\n%s", body)
	}
	if !strings.Contains(string(body), "# keep me") {
		t.Errorf("delegation lost the rest of the file:\n%s", body)
	}

	// Nothing left of ours: not-connected, not another delegation.
	var nc *clients.NotConnectedError
	if _, err := f.Disconnect(cfg); !errors.As(err, &nc) {
		t.Errorf("second disconnect = %v, want *NotConnectedError", err)
	}
}

// TestDelegateVerifiesRatherThanTrusts: an exit status is the delegate's
// opinion about its own success. A CLI that exits 0 and writes nothing must
// be a failure, or agenthub reports a connection that does not exist.
func TestDelegateVerifiesRatherThanTrusts(t *testing.T) {
	e := newEnv(t, "darwin")
	cfg := filepath.Join(e.home, ".codex", "config.toml")
	write(t, cfg, "[mcp_servers.linear]\ncommand = \"npx\"\n")
	tbl := fakeCodex(t, e, "#!/bin/sh\nexit 0\n")
	f, _ := tbl.Lookup("codex")

	var de *clients.DelegateError
	if _, err := f.Connect(cfg, entry("codex")); !errors.As(err, &de) {
		t.Fatalf("connect = %v, want *DelegateError", err)
	}
	if len(de.Command) == 0 || !strings.Contains(strings.Join(de.Command, " "), "mcp add") {
		t.Errorf("the error must name what was run: %+v", de.Command)
	}
	// And it still says how to do it by hand.
	if !strings.Contains(de.Snippet, "codex mcp add") {
		t.Errorf("snippet = %q, want the manual fallback", de.Snippet)
	}
}

// TestDelegateReportsTheCLIsOwnFailure: a delegate that fails is reported
// with its output, because that output is the only explanation there is.
func TestDelegateReportsTheCLIsOwnFailure(t *testing.T) {
	e := newEnv(t, "darwin")
	cfg := filepath.Join(e.home, ".codex", "config.toml")
	write(t, cfg, "[mcp_servers.linear]\ncommand = \"npx\"\n")
	tbl := fakeCodex(t, e, "#!/bin/sh\necho 'codex: unknown flag' >&2\nexit 2\n")
	f, _ := tbl.Lookup("codex")

	var de *clients.DelegateError
	if _, err := f.Connect(cfg, entry("codex")); !errors.As(err, &de) {
		t.Fatalf("connect = %v, want *DelegateError", err)
	}
	if !strings.Contains(de.Output, "unknown flag") {
		t.Errorf("output = %q, want the delegate's own message", de.Output)
	}
	if de.Err == nil {
		t.Errorf("a failed delegate must carry its exit error: %+v", de)
	}
	// The file is untouched, so nothing needs undoing.
	body, _ := os.ReadFile(cfg)
	if strings.Contains(string(body), "agenthub") {
		t.Errorf("a failed delegation wrote something:\n%s", body)
	}
}

// TestDelegateRefusesWhenTheCLIIsAbsent: no delegate on PATH is the manual
// path, which is what agenthub did before it could delegate — never a
// silent no-op reported as success.
func TestDelegateRefusesWhenTheCLIIsAbsent(t *testing.T) {
	e := newEnv(t, "darwin")
	cfg := filepath.Join(e.home, ".codex", "config.toml")
	write(t, cfg, "[mcp_servers.linear]\ncommand = \"npx\"\n")
	t.Setenv("PATH", t.TempDir()) // nothing on it
	tbl := clients.New(clients.Options{GOOS: "darwin", Home: e.home, BackupDir: e.backups})
	f, _ := tbl.Lookup("codex")

	var unsupported *clients.UnsupportedError
	if _, err := f.Connect(cfg, entry("codex")); !errors.As(err, &unsupported) {
		t.Fatalf("connect without the CLI = %v, want *UnsupportedError", err)
	}
	if !strings.Contains(unsupported.Snippet, "codex mcp add") {
		t.Errorf("snippet = %q, want instructions", unsupported.Snippet)
	}
}

// TestNoDelegateOptionIsObeyed: a caller that said "do not run other
// programs" gets the manual path even with the CLI right there.
func TestNoDelegateOptionIsObeyed(t *testing.T) {
	e := newEnv(t, "darwin")
	cfg := filepath.Join(e.home, ".codex", "config.toml")
	write(t, cfg, "[mcp_servers.linear]\ncommand = \"npx\"\n")
	fakeCodex(t, e, workingCodex) // on PATH, and still not run
	tbl := clients.New(clients.Options{
		GOOS: "darwin", Home: e.home, BackupDir: e.backups, NoDelegate: true,
	})
	f, _ := tbl.Lookup("codex")

	var unsupported *clients.UnsupportedError
	if _, err := f.Connect(cfg, entry("codex")); !errors.As(err, &unsupported) {
		t.Fatalf("connect = %v, want *UnsupportedError", err)
	}
	body, _ := os.ReadFile(cfg)
	if strings.Contains(string(body), "agenthub") {
		t.Errorf("NoDelegate ran the CLI anyway:\n%s", body)
	}
}

// TestNoClientCLIEnvForbidsDelegation: the environment can only ever forbid
// execution. It must not be able to turn it back on for a caller that
// disabled it.
func TestNoClientCLIEnvForbidsDelegation(t *testing.T) {
	e := newEnv(t, "darwin")
	cfg := filepath.Join(e.home, ".codex", "config.toml")
	write(t, cfg, "[mcp_servers.linear]\ncommand = \"npx\"\n")
	tbl := fakeCodex(t, e, workingCodex)
	t.Setenv("AGENTHUB_NO_CLIENT_CLI", "1")
	f, _ := tbl.Lookup("codex")

	var unsupported *clients.UnsupportedError
	if _, err := f.Connect(cfg, entry("codex")); !errors.As(err, &unsupported) {
		t.Fatalf("connect = %v, want *UnsupportedError", err)
	}
}
