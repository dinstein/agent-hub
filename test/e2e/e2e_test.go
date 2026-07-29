package e2e_test

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestFakeDownstreamRoundTrip is the always-on e2e case: a scripted fake
// downstream (the standalone fakemcp binary with its default echo tool) is
// registered offline, then a real spawned gateway serves it end to end:
// initialize -> tools/list (polled until the live catalog is up) ->
// tools/call fake__echo -> clean EOF exit.
func TestFakeDownstreamRoundTrip(t *testing.T) {
	dataDir := t.TempDir()
	out, _ := runAgenthub(t, dataDir, "", "server", "add", "fake", "--cmd", fakemcpBin, "--json")
	if !strings.Contains(out, `"ok":true`) {
		t.Fatalf("server add envelope: %s", out)
	}

	c := startGateway(t, dataDir, "e2e")
	c.initialize()
	c.waitForTool("fake__echo", 30*time.Second)

	res := c.callTool("fake__echo", map[string]any{"marker": "e2e-fake-roundtrip"}, 30*time.Second)
	text := c.textContent(res)
	if !strings.Contains(text, "e2e-fake-roundtrip") {
		c.fatalf("echo result does not contain the marker: %q", text)
	}
	t.Logf("fake__echo answered: %s", text)
	c.close()
}

// TestRealNpxFilesystemServer is the acceptance-standard case
// (docs/canonical.md, "current state"): a
// real @modelcontextprotocol/server-filesystem downstream spawned via npx,
// its list_directory tool called through the gateway, the result naming a
// known file. Skipped only when npx is unavailable or explicitly disabled.
func TestRealNpxFilesystemServer(t *testing.T) {
	if os.Getenv("AGENTHUB_E2E_SKIP_NPX") == "1" {
		t.Skip("AGENTHUB_E2E_SKIP_NPX=1")
	}
	if _, err := exec.LookPath("npx"); err != nil {
		t.Skip("npx not found in PATH")
	}

	dataDir := t.TempDir()
	workDir, err := filepath.EvalSymlinks(t.TempDir()) // macOS: /var -> /private/var
	if err != nil {
		t.Fatal(err)
	}
	const marker = "agenthub-e2e-marker.txt"
	if err := os.WriteFile(filepath.Join(workDir, marker), []byte("hello from agenthub e2e\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	spec, err := json.Marshal(map[string]any{
		"mcpServers": map[string]any{
			"fs": map[string]any{
				"command": "npx",
				"args":    []string{"-y", "@modelcontextprotocol/server-filesystem", workDir},
			},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	runAgenthub(t, dataDir, string(spec), "server", "add", "--stdin")

	c := startGateway(t, dataDir, "e2e-npx")
	c.initialize()
	// First npx run may download the package: give the downstream connect
	// the full 120s budget.
	c.waitForTool("fs__list_directory", 120*time.Second)
	tools := c.listTools(30 * time.Second)
	t.Logf("filesystem tools exposed: %v", tools)

	res := c.callTool("fs__list_directory", map[string]any{"path": workDir}, 60*time.Second)
	text := c.textContent(res)
	if !strings.Contains(text, marker) {
		c.fatalf("list_directory of %s does not name %q: %q", workDir, marker, text)
	}
	t.Logf("fs__list_directory answered: %s", text)
	c.close()
}

// TestClientConnectWritesConfig covers the CLI leg end to end with the real
// binary: `client connect --path` writes a .mcp.json whose entry spawns
// this exact binary, and `client disconnect` removes it again.
func TestClientConnectWritesConfig(t *testing.T) {
	dataDir := t.TempDir()
	path := filepath.Join(t.TempDir(), ".mcp.json")

	runAgenthub(t, dataDir, "", "client", "connect", "claude-code", "--path", path)

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	var cfg struct {
		Servers map[string]struct {
			Command string   `json:"command"`
			Args    []string `json:"args"`
		} `json:"mcpServers"`
	}
	if err := json.Unmarshal(data, &cfg); err != nil {
		t.Fatalf("written config: %v\n%s", err, data)
	}
	entry, ok := cfg.Servers["agenthub"]
	if !ok {
		t.Fatalf("no agenthub entry in %s:\n%s", path, data)
	}
	// The command must point back at the very binary that ran the CLI
	// (os.Executable), modulo symlink resolution of the temp dir.
	wantBin, err := filepath.EvalSymlinks(agenthubBin)
	if err != nil {
		t.Fatal(err)
	}
	gotBin, err := filepath.EvalSymlinks(entry.Command)
	if err != nil {
		t.Fatalf("entry command %q: %v", entry.Command, err)
	}
	if gotBin != wantBin {
		t.Errorf("entry command = %q (resolved %q), want %q", entry.Command, gotBin, wantBin)
	}
	wantArgs := []string{"connect", "--client", "claude-code"}
	if len(entry.Args) != len(wantArgs) {
		t.Fatalf("entry args = %v, want %v", entry.Args, wantArgs)
	}
	for i := range wantArgs {
		if entry.Args[i] != wantArgs[i] {
			t.Errorf("entry args = %v, want %v", entry.Args, wantArgs)
		}
	}

	// Disconnect removes the entry from the same file.
	runAgenthub(t, dataDir, "", "client", "disconnect", "claude-code", "--path", path)
	data, err = os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var after struct {
		Servers map[string]json.RawMessage `json:"mcpServers"`
	}
	if err := json.Unmarshal(data, &after); err != nil {
		t.Fatalf("config after disconnect: %v\n%s", err, data)
	}
	if _, ok := after.Servers["agenthub"]; ok {
		t.Errorf("agenthub entry survived disconnect:\n%s", data)
	}
}
