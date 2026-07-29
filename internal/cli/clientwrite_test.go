package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/dinstein/agent-hub/internal/clients"
)

// readPlan decodes a --json ConnectPlan envelope.
func readPlan(t *testing.T, out string) ConnectPlan {
	t.Helper()
	env := decodeEnvelope(t, out)
	if !env.OK {
		t.Fatalf("envelope not ok: %s", out)
	}
	var plan ConnectPlan
	if err := json.Unmarshal(env.Data, &plan); err != nil {
		t.Fatalf("data: %v", err)
	}
	return plan
}

// TestClientConnectWritesAndDisconnects drives the full CLI round trip
// against a --path temp file: connect writes, re-connect is a no-op,
// disconnect removes only our entry, a second disconnect is exit 3.
func TestClientConnectWritesAndDisconnects(t *testing.T) {
	setDataDir(t)
	path := filepath.Join(t.TempDir(), ".mcp.json")

	code, out, stderr := runCLI(t, "", "client", "connect", "claude-code",
		"--path", path, "--bin", "/opt/agenthub", "--json")
	if code != ExitOK {
		t.Fatalf("connect exit = %d, stderr: %s", code, stderr)
	}
	plan := readPlan(t, out)
	if plan.DryRun || !plan.Changed || plan.Path != path || plan.Backup != "" {
		t.Errorf("plan = %+v", plan)
	}
	if plan.Entry.Command != "/opt/agenthub" {
		t.Errorf("entry command = %q, want --bin override", plan.Entry.Command)
	}

	// The file on disk matches the emitted plan (write goes through the
	// same ConnectSnippet seam as the preview).
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	var cfg struct {
		Servers map[string]GatewayEntry `json:"mcpServers"`
	}
	if err := json.Unmarshal(data, &cfg); err != nil {
		t.Fatalf("written file: %v\n%s", err, data)
	}
	got, ok := cfg.Servers["agenthub"]
	if !ok || got.Command != "/opt/agenthub" {
		t.Fatalf("agenthub entry = %+v (present=%t)", got, ok)
	}
	wantArgs := []string{"connect", "--client", "claude-code"}
	if len(got.Args) != len(wantArgs) {
		t.Fatalf("args = %v, want %v", got.Args, wantArgs)
	}
	for i := range wantArgs {
		if got.Args[i] != wantArgs[i] {
			t.Errorf("args = %v, want %v", got.Args, wantArgs)
		}
	}

	// Idempotent re-connect: changed=false, still no backup.
	code, out, stderr = runCLI(t, "", "client", "connect", "claude-code",
		"--path", path, "--bin", "/opt/agenthub", "--json")
	if code != ExitOK {
		t.Fatalf("re-connect exit = %d, stderr: %s", code, stderr)
	}
	if plan := readPlan(t, out); plan.Changed || plan.Backup != "" {
		t.Errorf("re-connect plan = %+v, want no-op", plan)
	}

	// Disconnect removes the entry.
	code, out, stderr = runCLI(t, "", "client", "disconnect", "claude-code", "--path", path, "--json")
	if code != ExitOK {
		t.Fatalf("disconnect exit = %d, stderr: %s", code, stderr)
	}
	env := decodeEnvelope(t, out)
	var res DisconnectResult
	if err := json.Unmarshal(env.Data, &res); err != nil {
		t.Fatalf("disconnect data: %v", err)
	}
	if len(res.Removed) != 1 || res.Removed[0] != "agenthub" {
		t.Errorf("removed = %v", res.Removed)
	}

	// Second disconnect: nothing owned left -> exit 3, stable code.
	code, out, _ = runCLI(t, "", "client", "disconnect", "claude-code", "--path", path, "--json")
	if code != ExitNotFound {
		t.Fatalf("second disconnect exit = %d, want %d", code, ExitNotFound)
	}
	if env := decodeEnvelope(t, out); env.OK || env.Error == nil || env.Error.Code != CodeClientNotConnected {
		t.Errorf("envelope = %s", out)
	}
}

// TestClientConnectDefaultPlacementIsUser drives the whole placement
// contract through the CLI: a bare connect lands in $HOME and leaves the
// working tree untouched, --placement project opts back in, and a bare
// disconnect still finds a project-level entry written before the default
// moved.
func TestClientConnectDefaultPlacementIsUser(t *testing.T) {
	setDataDir(t)
	home := t.TempDir()
	t.Setenv("HOME", home)
	project := t.TempDir()
	t.Chdir(project)

	userPath := filepath.Join(home, ".claude.json")
	projectPath := filepath.Join(project, ".mcp.json")

	code, out, stderr := runCLI(t, "", "client", "connect", "claude-code", "--bin", "/opt/agenthub", "--json")
	if code != ExitOK {
		t.Fatalf("connect exit = %d, stderr: %s", code, stderr)
	}
	if plan := readPlan(t, out); plan.Path != userPath {
		t.Errorf("wrote to %q, want the user-level file %q", plan.Path, userPath)
	}
	if _, err := os.Stat(projectPath); !os.IsNotExist(err) {
		t.Errorf("a bare connect touched the working tree: stat = %v", err)
	}

	// --placement project is how the old default is asked for.
	code, out, stderr = runCLI(t, "", "client", "connect", "claude-code",
		"--placement", "project", "--bin", "/opt/agenthub", "--json")
	if code != ExitOK {
		t.Fatalf("connect --placement project exit = %d, stderr: %s", code, stderr)
	}
	if plan := readPlan(t, out); plan.Path != projectPath {
		t.Errorf("wrote to %q, want %q", plan.Path, projectPath)
	}

	// Bare disconnect takes the default target first...
	code, _, stderr = runCLI(t, "", "client", "disconnect", "claude-code", "--json")
	if code != ExitOK {
		t.Fatalf("disconnect exit = %d, stderr: %s", code, stderr)
	}
	if _, err := os.Stat(userPath); err != nil {
		t.Fatalf("stat user file: %v", err)
	}
	// ...and then falls back to the project entry a pre-move connect left
	// behind, instead of reporting "not connected" at a file that has it.
	code, out, stderr = runCLI(t, "", "client", "disconnect", "claude-code", "--json")
	if code != ExitOK {
		t.Fatalf("fallback disconnect exit = %d, stderr: %s", code, stderr)
	}
	env := decodeEnvelope(t, out)
	var res DisconnectResult
	if err := json.Unmarshal(env.Data, &res); err != nil {
		t.Fatalf("disconnect data: %v", err)
	}
	if res.Path != projectPath {
		t.Errorf("fallback removed from %q, want %q", res.Path, projectPath)
	}
}

// TestClientConnectPlacementRefusals: a named placement is honoured exactly
// or refused. Nothing here may end up writing to the other file.
func TestClientConnectPlacementRefusals(t *testing.T) {
	setDataDir(t)
	t.Setenv("HOME", t.TempDir())
	t.Chdir(t.TempDir())

	for _, tc := range []struct {
		name     string
		args     []string
		wantExit int
		wantCode string
	}{
		{
			name:     "unknown placement",
			args:     []string{"client", "connect", "claude-code", "--placement", "global"},
			wantExit: ExitUsage, wantCode: CodeUsage,
		},
		{
			// Two different targets: guessing which one the user meant is
			// how an entry ends up in a file nobody named.
			name:     "path and placement together",
			args:     []string{"client", "connect", "claude-code", "--path", "/tmp/x.json", "--placement", "user"},
			wantExit: ExitUsage, wantCode: CodeUsage,
		},
		{
			// claude-desktop is user-only; there is no project file to fall
			// back from, and the user file is NOT an acceptable substitute.
			name:     "placement the client does not have",
			args:     []string{"client", "connect", "claude-desktop", "--placement", "project"},
			wantExit: ExitNotFound, wantCode: CodeClientUnsupported,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			code, out, _ := runCLI(t, "", append(tc.args, "--json")...)
			if code != tc.wantExit {
				t.Fatalf("exit = %d, want %d (%s)", code, tc.wantExit, out)
			}
			if env := decodeEnvelope(t, out); env.OK || env.Error == nil || env.Error.Code != tc.wantCode {
				t.Errorf("envelope = %s", out)
			}
		})
	}
}

// TestClientConnectRefusesBadJSON: an unparseable existing .mcp.json aborts
// with E_INVALID_JSON and the file survives byte-identically.
func TestClientConnectRefusesBadJSON(t *testing.T) {
	dataDir := setDataDir(t)
	path := filepath.Join(t.TempDir(), ".mcp.json")
	const bad = `{"mcpServers": {`
	if err := os.WriteFile(path, []byte(bad), 0o644); err != nil {
		t.Fatal(err)
	}
	code, out, _ := runCLI(t, "", "client", "connect", "claude-code", "--path", path, "--json")
	if code != ExitGeneral {
		t.Fatalf("exit = %d, want %d", code, ExitGeneral)
	}
	if env := decodeEnvelope(t, out); env.OK || env.Error == nil || env.Error.Code != CodeInvalidJSON {
		t.Errorf("envelope = %s", out)
	}
	got, err := os.ReadFile(path)
	if err != nil || string(got) != bad {
		t.Errorf("file modified despite refusal: %q (err=%v)", got, err)
	}
	if n := len(clientBackups(t, dataDir, "claude-code")); n != 0 {
		t.Errorf("backup must not appear on refusal (found %d)", n)
	}
}

// TestClientConnectUnsupportedClient: an unregistered client ID is exit 3
// unless --dry-run (which previews the snippet for any ID).
func TestClientConnectUnsupportedClient(t *testing.T) {
	setDataDir(t)
	path := filepath.Join(t.TempDir(), ".mcp.json")
	code, out, _ := runCLI(t, "", "client", "connect", "no-such-client", "--path", path, "--json")
	if code != ExitNotFound {
		t.Fatalf("exit = %d, want %d", code, ExitNotFound)
	}
	if env := decodeEnvelope(t, out); env.OK || env.Error == nil || env.Error.Code != CodeClientUnsupported {
		t.Errorf("envelope = %s", out)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Errorf("no file may be written for an unsupported client")
	}

	// --dry-run still previews any client ID.
	code, out, _ = runCLI(t, "", "client", "connect", "cursor", "--dry-run", "--json")
	if code != ExitOK {
		t.Fatalf("dry-run exit = %d", code)
	}
	if plan := readPlan(t, out); !plan.DryRun || plan.Client != "cursor" {
		t.Errorf("plan = %+v", plan)
	}
}

// TestClientConnectPreservesForeignEntries: CLI-level check that a foreign
// server entry and unknown top-level fields survive the merge.
func TestClientConnectPreservesForeignEntries(t *testing.T) {
	dataDir := setDataDir(t)
	path := filepath.Join(t.TempDir(), ".mcp.json")
	orig := `{"mcpServers":{"other":{"command":"npx","args":["-y","x"],"custom":true}},"top":{"keep":1}}`
	if err := os.WriteFile(path, []byte(orig), 0o644); err != nil {
		t.Fatal(err)
	}
	code, _, stderr := runCLI(t, "", "client", "connect", "claude-code", "--path", path)
	if code != ExitOK {
		t.Fatalf("exit = %d, stderr: %s", code, stderr)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var top map[string]json.RawMessage
	if err := json.Unmarshal(data, &top); err != nil {
		t.Fatalf("merged file: %v", err)
	}
	if string(top["top"]) == "" {
		t.Errorf("unknown top-level field dropped:\n%s", data)
	}
	var servers map[string]json.RawMessage
	if err := json.Unmarshal(top["mcpServers"], &servers); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"other", "agenthub"} {
		if _, ok := servers[want]; !ok {
			t.Errorf("entry %q missing after merge:\n%s", want, data)
		}
	}
	// The pre-merge bytes land in the CENTRAL backup directory, not in a
	// sidecar next to the (committed) project file.
	backups := clientBackups(t, dataDir, "claude-code")
	if len(backups) != 1 {
		t.Fatalf("central backups = %v, want exactly one", backups)
	}
	backup, err := os.ReadFile(backups[0])
	if err != nil || string(backup) != orig {
		t.Errorf("backup = %q (err=%v)", backup, err)
	}
}

// clientBackups lists the central pre-write backups recorded for clientID.
func clientBackups(t *testing.T, dataDir, clientID string) []string {
	t.Helper()
	matches, err := filepath.Glob(filepath.Join(clients.BackupDir(dataDir), clientID+"-*.json"))
	if err != nil {
		t.Fatalf("glob backups: %v", err)
	}
	return matches
}
