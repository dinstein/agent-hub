package e2e_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

// the halves the approval e2e does not cover:
//
//	A.3 #2 "gateway degradation / re-registration path" — after a daemon kill -9 AND a restart, the
//	       gateway re-registers and receives a BRAND NEW session identity.
//	A.3 #4 "overlay vanishes on both sides after a daemon restart" — the session overlay is
//	       authority the daemon holds; when the daemon dies, the overlay must
//	       vanish on BOTH sides. The gateway falls back to its static scope
//	       (visible immediately, without waiting for the daemon), and the
//	       restarted daemon must not resurrect it.
//
// The gateway re-registers on a 30s ladder (docs/architecture.md §2), so this test is
// slow by construction and skips itself in -short mode.

const (
	// relinkBudget is how long the gateway may take to notice the restarted
	// daemon: two re-register intervals plus slack.
	relinkBudget = 75 * time.Second
	// overlayBudget is how long a scope narrowing may take to reach the
	// gateway (a push + ack round trip on a live link).
	overlayBudget = 20 * time.Second
)

// sessionRow is the slice of api.SessionInfo this test reads.
type sessionRow struct {
	ID             string `json:"id"`
	ClientID       string `json:"client_id"`
	OverlaySummary string `json:"overlay_summary"`
}

// listSessions reads `session ls --json` for one client.
func listSessions(t *testing.T, env []string, clientID string) []sessionRow {
	t.Helper()
	out, _ := runAgenthubEnv(t, env, "", "session", "ls", "--json")
	var envl struct {
		OK   bool `json:"ok"`
		Data struct {
			Sessions []sessionRow `json:"sessions"`
		} `json:"data"`
	}
	if err := json.Unmarshal([]byte(out), &envl); err != nil || !envl.OK {
		t.Fatalf("session ls --json: %v\n%s", err, out)
	}
	var rows []sessionRow
	for _, r := range envl.Data.Sessions {
		if clientID == "" || r.ClientID == clientID {
			rows = append(rows, r)
		}
	}
	return rows
}

// waitSession polls until exactly one session of clientID exists and accept
// returns true for it, then returns it.
func waitSession(t *testing.T, env []string, clientID string, budget time.Duration, accept func(sessionRow) bool) sessionRow {
	t.Helper()
	deadline := time.Now().Add(budget)
	var last []sessionRow
	for time.Now().Before(deadline) {
		last = listSessions(t, env, clientID)
		for _, r := range last {
			if accept(r) {
				return r
			}
		}
		time.Sleep(500 * time.Millisecond)
	}
	t.Fatalf("no matching session for %q within %s; last seen: %+v", clientID, budget, last)
	return sessionRow{}
}

// waitTools polls the gateway's tools/list until want reports true.
func waitTools(t *testing.T, c *gatewayClient, budget time.Duration, what string, want func([]string) bool) {
	t.Helper()
	deadline := time.Now().Add(budget)
	var last []string
	for time.Now().Before(deadline) {
		last = c.listTools(15 * time.Second)
		if want(last) {
			return
		}
		time.Sleep(500 * time.Millisecond)
	}
	t.Fatalf("tools/list never satisfied %q within %s; last: %v", what, budget, last)
}

func hasTool(tools []string, name string) bool { return slices.Contains(tools, name) }

func TestDaemonRestartReregistersAndDropsOverlay(t *testing.T) {
	if testing.Short() {
		t.Skip("daemon restart e2e skipped in -short mode (30s re-register ladder)")
	}
	dataDir := t.TempDir()

	// Short socket path: t.TempDir on macOS can exceed sun_path.
	sockDir, err := os.MkdirTemp("", "ahe2e")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(sockDir) })
	socket := filepath.Join(sockDir, "ctl.sock")
	env := append(testEnv(dataDir), "AGENTHUB_SOCKET="+socket)

	script := `{"tools":[{"def":{"name":"echo","description":"echo","inputSchema":{"type":"object"}}}]}`
	scriptPath := filepath.Join(dataDir, "fakemcp-script.json")
	if err := os.WriteFile(scriptPath, []byte(script), 0o600); err != nil {
		t.Fatal(err)
	}
	runAgenthub(t, dataDir, "", "server", "add", "fake", "--cmd", fakemcpBin, "--args", scriptPath)
	enableServer(t, dataDir, "fake")

	h := &hitlEnv{dataDir: dataDir, socket: socket, env: env}
	runAgenthubEnv(t, env, "", "daemon", "start")
	t.Cleanup(func() { h.killDaemon(t) })

	c := startGatewayEnv(t, env, "e2e-restart")
	c.initialize()
	c.waitForTool("fake__echo", 30*time.Second)

	first := waitSession(t, env, "e2e-restart", 30*time.Second, func(r sessionRow) bool { return r.ID != "" })
	if first.OverlaySummary != "" {
		t.Fatalf("fresh session already carries an overlay: %+v", first)
	}

	// --- narrow the live session: the overlay hides the tool on both sides.
	runAgenthubEnv(t, env, "", "session", "scope", first.ID, "--disable-server", "fake")
	waitTools(t, c, overlayBudget, "fake__echo hidden by the overlay",
		func(tools []string) bool { return !hasTool(tools, "fake__echo") })
	narrowed := waitSession(t, env, "e2e-restart", overlayBudget,
		func(r sessionRow) bool { return r.ID == first.ID && r.OverlaySummary != "" })
	t.Logf("overlay in force: %q", narrowed.OverlaySummary)

	// --- kill -9: the overlay's authority died with the daemon, so the
	// gateway must WIDEN back to its static scope on its own. This is the
	// half that must not wait for the daemon to come back.
	h.killDaemonStrict(t, true)
	h.assertSocketRefuses(t)
	waitTools(t, c, 30*time.Second, "fake__echo visible again after the daemon died",
		func(tools []string) bool { return hasTool(tools, "fake__echo") })

	// The data plane is untouched by the daemon's death (A.3 #2).
	res := c.callTool("fake__echo", map[string]any{"marker": "orphaned"}, 30*time.Second)
	if text := c.textContent(res); !strings.Contains(text, "orphaned") {
		c.fatalf("call after daemon kill = %q", text)
	}

	// --- restart: the daemon comes back with an EMPTY session table (its
	// registry of live gateways died with it), and the gateway re-registers
	// into it on the next ladder tick.
	runAgenthubEnv(t, env, "", "daemon", "start")
	if rows := listSessions(t, env, "e2e-restart"); len(rows) != 0 {
		t.Fatalf("a restarted daemon already knows sessions: %+v — they cannot be real", rows)
	}
	fresh := waitSession(t, env, "e2e-restart", relinkBudget,
		func(r sessionRow) bool { return r.ID != "" })

	// The overlay must NOT come back: a session grant dies with its session
	// (docs/architecture.md §2), and nothing on disk may resurrect it.
	if fresh.OverlaySummary != "" {
		t.Fatalf("the restarted daemon resurrected an overlay: %+v", fresh)
	}
	// ... and the gateway still shows the widened surface.
	waitTools(t, c, 30*time.Second, "fake__echo still visible after re-registration",
		func(tools []string) bool { return hasTool(tools, "fake__echo") })

	// The registration is genuinely NEW, not a stale row: the human-facing id
	// is "client:seq" (ruling #7) and a fresh daemon restarts the sequence at
	// 1, so identity freshness cannot be proven from the string. It is proven
	// by BEHAVIOUR instead — a narrowing filed against the new id has to take
	// effect on the gateway, which is only possible over a live, freshly
	// authorized link.
	runAgenthubEnv(t, env, "", "session", "scope", fresh.ID, "--disable-server", "fake")
	waitTools(t, c, overlayBudget, "fake__echo hidden again over the re-established link",
		func(tools []string) bool { return !hasTool(tools, "fake__echo") })

	c.close()
}
