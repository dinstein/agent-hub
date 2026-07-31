package e2e_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestProfileSwitchHotReload drives a live stdio gateway and switches the
// active profile underneath it, asserting the exposed tool surface follows
// without a restart. This is the end-to-end proof of the registry watch ->
// snapshot swap -> scope invalidation -> notifications/tools/list_changed
// chain (internal/gateway/hotreload.go, internal/gateway/scope.go).
func TestProfileSwitchHotReload(t *testing.T) {
	dataDir := t.TempDir()

	// Two downstream servers, one tool each, so a profile switch moves the
	// visible surface in both directions.
	runAgenthub(t, dataDir, "", "server", "add", "alpha", "--cmd", fakemcpBin, "--json")
	enableServer(t, dataDir, "alpha")
	runAgenthub(t, dataDir, "", "server", "add", "beta", "--cmd", fakemcpBin, "--json")
	enableServer(t, dataDir, "beta")

	runAgenthub(t, dataDir, "", "profile", "create", "onlyalpha", "--servers", "alpha")
	runAgenthub(t, dataDir, "", "profile", "create", "onlybeta", "--servers", "beta")
	runAgenthub(t, dataDir, "", "profile", "use", "onlyalpha")

	c := startGateway(t, dataDir, "hotreload")
	c.initialize()

	// Baseline: the active profile admits alpha only.
	c.waitForTool("alpha__echo", 30*time.Second)
	if names := c.listTools(30 * time.Second); hasTool(names, "beta__echo") {
		c.fatalf("beta__echo visible under profile onlyalpha: %v", names)
	}
	t.Logf("baseline tools under onlyalpha: %v", c.listTools(30*time.Second))

	notesBefore := len(c.noteSnapshot())

	// Flip the active profile while the gateway keeps running.
	start := time.Now()
	runAgenthub(t, dataDir, "", "profile", "use", "onlybeta")

	c.waitForTool("beta__echo", 30*time.Second)
	elapsed := time.Since(start)
	if names := c.listTools(30 * time.Second); hasTool(names, "alpha__echo") {
		c.fatalf("alpha__echo still visible after switching to onlybeta: %v", names)
	}
	t.Logf("switched to onlybeta in %s; tools now: %v", elapsed, c.listTools(30*time.Second))

	// The new surface must actually be callable, not just listed.
	text := c.textContent(c.callTool("beta__echo", map[string]any{"marker": "post-switch"}, 30*time.Second))
	if !strings.Contains(text, "post-switch") {
		c.fatalf("beta__echo did not echo the marker: %q", text)
	}

	// And the gateway must have pushed list_changed, not merely answered a poll.
	notes := c.noteSnapshot()
	if !hasTool(notes[notesBefore:], "notifications/tools/list_changed") {
		c.fatalf("no tools/list_changed pushed after profile switch; notifications seen: %v", notes)
	}
	t.Logf("notifications observed after switch: %v", notes[notesBefore:])

	// Switch back, to prove it is not a one-shot.
	start = time.Now()
	runAgenthub(t, dataDir, "", "profile", "use", "onlyalpha")
	c.waitForTool("alpha__echo", 30*time.Second)
	t.Logf("switched back to onlyalpha in %s", time.Since(start))

	c.close()
}

// TestClientBindHotReload covers the per-client binding path (clients.json)
// rather than the global activeProfile marker.
//
// Rebinding a LIVE client is the reason the binding lives in agenthub's own
// registry instead of in the client's MCP config file: a file the client owns
// could only take effect on its next start. This test is what keeps that
// property honest.
func TestClientBindHotReload(t *testing.T) {
	dataDir := t.TempDir()

	runAgenthub(t, dataDir, "", "server", "add", "alpha", "--cmd", fakemcpBin, "--json")
	enableServer(t, dataDir, "alpha")
	runAgenthub(t, dataDir, "", "server", "add", "beta", "--cmd", fakemcpBin, "--json")
	enableServer(t, dataDir, "beta")
	runAgenthub(t, dataDir, "", "profile", "create", "onlyalpha", "--servers", "alpha")
	runAgenthub(t, dataDir, "", "profile", "create", "onlybeta", "--servers", "beta")
	runAgenthub(t, dataDir, "", "client", "bind", "boundclient", "onlyalpha")

	c := startGateway(t, dataDir, "boundclient")
	c.initialize()
	c.waitForTool("alpha__echo", 30*time.Second)

	start := time.Now()
	runAgenthub(t, dataDir, "", "client", "bind", "boundclient", "onlybeta")
	c.waitForTool("beta__echo", 30*time.Second)
	t.Logf("clients.json rebinding took effect in %s", time.Since(start))

	if names := c.listTools(30 * time.Second); hasTool(names, "alpha__echo") {
		c.fatalf("alpha__echo still visible after rebinding: %v", names)
	}
	c.close()
}

// TestProfileToolSelectorHotReload edits profiles.json narrowing (not the
// server set) to confirm tool-level changes propagate too.
func TestProfileToolSelectorHotReload(t *testing.T) {
	dataDir := t.TempDir()

	script := filepath.Join(t.TempDir(), "two.json")
	writeScript(t, script, "read_thing", "write_thing")
	runAgenthub(t, dataDir, "", "server", "add", "alpha", "--cmd", fakemcpBin, "--args", script, "--json")
	enableServer(t, dataDir, "alpha")

	runAgenthub(t, dataDir, "", "profile", "create", "narrow", "--servers", "alpha")
	runAgenthub(t, dataDir, "", "profile", "use", "narrow")

	c := startGateway(t, dataDir, "narrowclient")
	c.initialize()
	c.waitForTool("alpha__read_thing", 30*time.Second)
	c.waitForTool("alpha__write_thing", 30*time.Second)

	start := time.Now()
	runAgenthub(t, dataDir, "", "profile", "tool", "allow", "narrow", "alpha", "--only", "read_thing")

	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		names := c.listTools(30 * time.Second)
		if !hasTool(names, "alpha__write_thing") && hasTool(names, "alpha__read_thing") {
			t.Logf("tool selector narrowed in %s; tools now: %v", time.Since(start), names)
			c.close()
			return
		}
		time.Sleep(300 * time.Millisecond)
	}
	c.fatalf("alpha__write_thing never disappeared; last: %v", c.listTools(30*time.Second))
}

func writeScript(t *testing.T, path string, toolNames ...string) {
	t.Helper()
	type toolDef struct {
		Name        string          `json:"name"`
		Description string          `json:"description"`
		InputSchema json.RawMessage `json:"inputSchema"`
	}
	type tool struct {
		Def toolDef `json:"def"`
	}
	tools := make([]tool, 0, len(toolNames))
	for _, n := range toolNames {
		tools = append(tools, tool{Def: toolDef{
			Name:        n,
			Description: "echoes its arguments back as text",
			InputSchema: json.RawMessage(`{"type":"object"}`),
		}})
	}
	data, err := json.Marshal(map[string]any{"tools": tools})
	if err != nil {
		t.Fatalf("marshal fakemcp script: %v", err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatalf("write fakemcp script: %v", err)
	}
}
