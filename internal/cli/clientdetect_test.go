package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestClientDetect runs against an isolated HOME so the result is
// deterministic and no test reads the developer's real client
// configuration.
func TestClientDetect(t *testing.T) {
	setDataDir(t)
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))

	var empty DetectList
	decodeInto(t, mustRun(t, "", "client", "detect", "--json"), &empty)
	if len(empty.Found) != 0 {
		t.Fatalf("found = %+v, want nothing in an empty home", empty.Found)
	}
	if len(empty.Supported) == 0 {
		t.Errorf("detect must always report the directly writable clients")
	}

	// A user-level Claude Desktop configuration should now be detected.
	cfgDir := filepath.Join(home, "Library", "Application Support", "Claude")
	if err := os.MkdirAll(cfgDir, 0o700); err != nil {
		t.Fatal(err)
	}
	linuxDir := filepath.Join(home, ".config", "Claude")
	if err := os.MkdirAll(linuxDir, 0o700); err != nil {
		t.Fatal(err)
	}
	body := []byte(`{"mcpServers":{"linear":{"command":"npx","args":["-y","linear-mcp"]}}}`)
	for _, dir := range []string{cfgDir, linuxDir} {
		if err := os.WriteFile(filepath.Join(dir, "claude_desktop_config.json"), body, 0o600); err != nil {
			t.Fatal(err)
		}
	}

	var found DetectList
	decodeInto(t, mustRun(t, "", "client", "detect", "--json"), &found)
	hit := false
	for _, d := range found.Found {
		if d.Client == "claude-desktop" {
			hit = true
			if d.Size == 0 || d.Modified == "" {
				t.Errorf("detected row lacks stat data: %+v", d)
			}
		}
	}
	if !hit {
		t.Errorf("claude-desktop not detected: %+v", found.Found)
	}
}

// TestClientImport covers the adoption path: entries become registry
// servers tagged source=imported:<client>, and an existing name is a
// reported conflict rather than a silent redefinition.
func TestClientImport(t *testing.T) {
	dir := setDataDir(t)
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))

	cwd := t.TempDir()
	restore, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(cwd); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(restore) })

	// Project-level Claude Code configuration in the working directory.
	body := []byte(`{"mcpServers":{
        "linear":{"command":"npx","args":["-y","linear-mcp"]},
        "github":{"command":"gh-mcp"}
    }}`)
	if err := os.WriteFile(filepath.Join(cwd, ".mcp.json"), body, 0o600); err != nil {
		t.Fatal(err)
	}
	// One of the two names already exists in the registry.
	mustRun(t, "", "server", "add", "github", "--cmd", "already-mine")

	var dry ImportResult
	decodeInto(t, mustRun(t, "", "client", "import", "claude-code", "--dry-run", "--json"), &dry)
	if len(dry.Added) != 1 || dry.Added[0].Name != "linear" {
		t.Fatalf("dry-run added = %+v, want only linear", dry.Added)
	}
	if len(dry.Conflicts) != 1 || !strings.Contains(dry.Conflicts[0], "github") {
		t.Errorf("conflicts = %v, want github reported", dry.Conflicts)
	}
	if !dry.DryRun {
		t.Errorf("dry_run flag not set: %+v", dry)
	}

	// --dry-run wrote nothing.
	raw, err := os.ReadFile(filepath.Join(dir, "registry", "servers.json"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "linear") {
		t.Fatalf("--dry-run wrote to the registry:\n%s", raw)
	}

	var done ImportResult
	decodeInto(t, mustRun(t, "", "client", "import", "claude-code", "--json"), &done)
	if len(done.Added) != 1 || done.Added[0].Source != "imported:claude-code" {
		t.Fatalf("import = %+v, want source=imported:claude-code", done.Added)
	}

	var servers ServerList
	decodeInto(t, mustRun(t, "", "server", "ls", "--json"), &servers)
	byID := map[string]ServerRow{}
	for _, s := range servers {
		byID[s.ID] = s
	}
	if byID["linear"].Source != "imported:claude-code" {
		t.Errorf("linear = %+v", byID["linear"])
	}
	// The pre-existing entry was NOT redefined.
	if byID["github"].Command != "already-mine" {
		t.Errorf("import overwrote a governed server: %+v", byID["github"])
	}
}

func TestClientImportUnknownClient(t *testing.T) {
	setDataDir(t)
	code, _, stderr := runCLI(t, "", "client", "import", "not-a-client")
	if code != ExitNotFound {
		t.Fatalf("exit = %d, want %d", code, ExitNotFound)
	}
	if !strings.Contains(stderr, "known clients") {
		t.Errorf("the error must list the known clients: %q", stderr)
	}
}

// TestClientImportAsTeamIsRecordedNotSilent: an accepted flag that does
// nothing yet must SAY it did nothing, otherwise the operator assumes team
// scoping happened.
func TestClientImportAsTeamIsRecordedNotSilent(t *testing.T) {
	setDataDir(t)
	home := t.TempDir()
	t.Setenv("HOME", home)
	cwd := t.TempDir()
	restore, _ := os.Getwd()
	if err := os.Chdir(cwd); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(restore) })
	if err := os.WriteFile(filepath.Join(cwd, ".mcp.json"),
		[]byte(`{"mcpServers":{"linear":{"command":"npx"}}}`), 0o600); err != nil {
		t.Fatal(err)
	}

	out := mustRun(t, "", "client", "import", "claude-code", "--as-team", "--json")
	var res ImportResult
	decodeInto(t, out, &res)
	if !res.AsTeam {
		t.Errorf("--as-team not recorded: %s", out)
	}
	// The human rendering says so out loud too.
	human := mustRun(t, "", "client", "import", "claude-code", "--as-team", "--dry-run")
	if !strings.Contains(human, "M2") {
		t.Errorf("human output must say team scoping is not implemented yet:\n%s", human)
	}
}

// TestImportedEntriesAreValidJSON guards the registry round trip of an
// import (entries must be plain ServerEntry documents).
func TestImportedEntriesAreValidJSON(t *testing.T) {
	dir := setDataDir(t)
	home := t.TempDir()
	t.Setenv("HOME", home)
	cwd := t.TempDir()
	restore, _ := os.Getwd()
	if err := os.Chdir(cwd); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(restore) })
	if err := os.WriteFile(filepath.Join(cwd, ".mcp.json"),
		[]byte(`{"mcpServers":{"linear":{"command":"npx","args":["-y","linear-mcp"]}}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	mustRun(t, "", "client", "import", "claude-code")

	raw, err := os.ReadFile(filepath.Join(dir, "registry", "servers.json"))
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Servers map[string]map[string]any `json:"servers"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("servers.json is not valid JSON after an import: %v\n%s", err, raw)
	}
	entry, ok := doc.Servers["linear"]
	if !ok {
		t.Fatalf("linear missing:\n%s", raw)
	}
	if entry["command"] != "npx" || entry["source"] != "imported:claude-code" {
		t.Errorf("entry = %v", entry)
	}
}
