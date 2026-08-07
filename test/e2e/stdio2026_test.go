package e2e_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Every downstream this suite has ever spawned spoke the pre-2026 protocol,
// because that was the only thing fakemcp could speak: it answered
// initialize and had no server/discover, so transport.Handshake fell back
// every time. The gateway's 2026 client path — negotiate through discover,
// then carry the per-request _meta on everything after it — was exercised
// only in-process and only over streamable HTTP.
//
// This case is that path with nothing stubbed: the real agenthub binary, a
// real child process, and a downstream that REFUSES a request whose _meta is
// missing or names a version it does not speak.
//
// The strictness is what carries the test. There is no assertion here that
// could pass while the gateway spoke the wrong protocol — a downstream that
// refuses bare requests answers no tools/list, so its catalog never arrives
// and the tool never appears. Getting a result back at all is the proof.

// write2026Script writes a fakemcp script for a server that advertises the
// stateless protocol and serves one echo tool per name.
//
// It is a separate helper from writeScript rather than a flag on it: that
// one is used by a dozen fixtures whose whole point is that they run the
// legacy path, and a shared writer with a version parameter is how they
// would quietly stop.
func write2026Script(t *testing.T, path string, toolNames ...string) {
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
	// The version is written as a literal, not imported. This suite drives
	// the product from the outside, and "2026-07-28" is what a real
	// downstream puts on the wire — a constant would rename itself along
	// with the code and stop being an independent statement of the ABI.
	data, err := json.Marshal(map[string]any{
		"supportedVersions": []string{"2026-07-28"},
		"tools":             tools,
	})
	if err != nil {
		t.Fatalf("marshal 2026 fakemcp script: %v", err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatalf("write 2026 fakemcp script: %v", err)
	}
}

// TestA2026StdioDownstreamServesThroughTheGateway runs the full path over a
// downstream that grades the gateway's conformance as it goes.
func TestA2026StdioDownstreamServesThroughTheGateway(t *testing.T) {
	dataDir := t.TempDir()

	script := filepath.Join(t.TempDir(), "stateless.json")
	write2026Script(t, script, "echo")
	runAgenthub(t, dataDir, "", "server", "add", "modern", "--cmd", fakemcpBin, "--args", script, "--json")
	enableServer(t, dataDir, "modern")

	c := startGateway(t, dataDir, "e2e-2026")
	c.initialize()
	// The catalog arriving is already the assertion: the downstream refuses
	// a tools/list without a conformant _meta, so a gateway that fell back
	// to the legacy handshake — or negotiated 2026 and then forgot to inject
	// — would never get one, and this would time out rather than fail on a
	// value.
	c.waitForTool("modern__echo", 30*time.Second)

	res := c.callTool("modern__echo", map[string]any{"marker": "stateless-e2e"}, 30*time.Second)
	if text := c.textContent(res); !strings.Contains(text, "stateless-e2e") {
		c.fatalf("modern__echo did not echo through a 2026 downstream: %q", text)
	}

	// And the version is stated rather than inferred. A failure that says
	// "the tool never appeared" cannot distinguish a protocol regression
	// from a downstream that would not start; this line names which.
	c.close()
	waitForLog(t, dataDir, 30*time.Second, `"protocol":"2026-07-28"`)
}

// TestALegacyStdioDownstreamStillNegotiatesLegacy is the control, and it
// guards the whole suite rather than this file.
//
// Every other fixture here writes a script with no supportedVersions and
// expects the legacy handshake. If that default ever inverted, they would
// all silently change which protocol they exercise while staying green — so
// the default is asserted once, explicitly, next to the case that departs
// from it.
func TestALegacyStdioDownstreamStillNegotiatesLegacy(t *testing.T) {
	dataDir := t.TempDir()
	runAgenthub(t, dataDir, "", "server", "add", "classic", "--cmd", fakemcpBin, "--json")
	enableServer(t, dataDir, "classic")

	c := startGateway(t, dataDir, "e2e-legacy")
	c.initialize()
	c.waitForTool("classic__echo", 30*time.Second)
	c.close()

	waitForLog(t, dataDir, 30*time.Second, `"protocol":"2025-11-25"`)
}

// waitForLog polls the merged process log until a record containing want
// appears. Polling rather than reading once: the gateway writes its log
// through internal/jsonl and the child has only just exited, so "the line is
// not there yet" and "the line will never be there" are the same read.
func waitForLog(t *testing.T, dataDir string, budget time.Duration, want string) {
	t.Helper()
	deadline := time.Now().Add(budget)
	var last string
	for time.Now().Before(deadline) {
		last, _ = runAgenthub(t, dataDir, "", "logs", "--since", "all", "--limit", "0",
			"--source", "gateway", "--json")
		if strings.Contains(last, want) {
			return
		}
		time.Sleep(300 * time.Millisecond)
	}
	t.Fatalf("no gateway log record contains %s within %s; last read:\n%s", want, budget, last)
}
