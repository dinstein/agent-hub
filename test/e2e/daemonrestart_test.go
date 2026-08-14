package e2e_test

import (
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"
)

// The daemon-lifecycle halves no other e2e covers:
//
//	A.3 #2 "gateway degradation / re-registration path" — after a daemon kill -9 AND a restart, the
//	       gateway re-registers and receives a BRAND NEW session identity.
//
// The gateway re-registers on a 30s ladder (docs/architecture.md#the-processes), so this test is
// slow by construction and skips itself in -short mode.

const (
	// relinkBudget is how long the gateway may take to notice the restarted
	// daemon: two re-register intervals plus slack.
	relinkBudget = 75 * time.Second
	// killBudget is how long the daemon may take to drop a killed session
	// from its table.
	killBudget = 20 * time.Second
)

// sessionRow is the slice of api.SessionInfo this test reads.
type sessionRow struct {
	ID       string `json:"id"`
	ClientID string `json:"client_id"`
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

func TestDaemonRestartReregistersTheGateway(t *testing.T) {
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

	h := &daemonEnv{dataDir: dataDir, socket: socket, env: env}
	// --headless is how a hub with no desktop application behind it is
	// started, and the suite goes through the same door an operator does: a
	// hub the tests were exempted from admitting is one nothing has verified.
	runAgenthubEnv(t, env, "", "daemon", "start", "--headless")
	t.Cleanup(func() { h.killDaemon(t) })

	c := startGatewayEnv(t, env, "e2e-restart")
	c.initialize()
	c.waitForTool("fake__echo", 30*time.Second)

	waitSession(t, env, "e2e-restart", 30*time.Second, func(r sessionRow) bool { return r.ID != "" })

	// --- kill -9. The data plane must not notice: a stdio session's scope
	// comes from the registry files, not from the daemon, so a gateway whose
	// daemon died keeps serving exactly what it served before.
	h.killDaemonStrict(t, true)
	h.assertSocketRefuses(t)
	waitTools(t, c, 30*time.Second, "fake__echo still visible after the daemon died",
		func(tools []string) bool { return hasTool(tools, "fake__echo") })

	res := c.callTool("fake__echo", map[string]any{"marker": "orphaned"}, 30*time.Second)
	if text := c.textContent(res); !strings.Contains(text, "orphaned") {
		c.fatalf("call after daemon kill = %q", text)
	}

	// --- restart: the daemon comes back with an EMPTY session table (its
	// registry of live gateways died with it), and the gateway re-registers
	// into it on the next ladder tick.
	runAgenthubEnv(t, env, "", "daemon", "start", "--headless")
	if rows := listSessions(t, env, "e2e-restart"); len(rows) != 0 {
		t.Fatalf("a restarted daemon already knows sessions: %+v — they cannot be real", rows)
	}
	fresh := waitSession(t, env, "e2e-restart", relinkBudget,
		func(r sessionRow) bool { return r.ID != "" })

	waitTools(t, c, 30*time.Second, "fake__echo still visible after re-registration",
		func(tools []string) bool { return hasTool(tools, "fake__echo") })

	// The registration is genuinely NEW, not a stale row. The human-facing id
	// is "client:seq" (ruling #7) and a fresh daemon restarts the sequence at
	// 1, so freshness cannot be read off the string. It is proven by
	// BEHAVIOUR: `session kill` reaches into the daemon's live handle on this
	// gateway, and a row the daemon merely remembered could not be killed.
	runAgenthubEnv(t, env, "", "session", "kill", fresh.ID)
	deadline := time.Now().Add(killBudget)
	for len(listSessions(t, env, "e2e-restart")) != 0 {
		if !time.Now().Before(deadline) {
			t.Fatalf("session %s survived `session kill`", fresh.ID)
		}
		time.Sleep(500 * time.Millisecond)
	}

	c.close()
}

// daemonEnv is a test's forked daemon: the data directory it serves, the
// control socket it listens on, and the child environment that reaches it.
//
// It used to live in the HITL end-to-end file, which was the only case that
// needed a real daemon. Removing human approval removed that case, but not
// the need: the daemon still owns session registration, and the restart
// behaviour below is the one thing no in-process test can show.
type daemonEnv struct {
	dataDir string
	socket  string
	env     []string
}

// killDaemon SIGKILLs the forked daemon if it is still running. Used as
// cleanup, where an already-dead daemon is the normal case.
func (h *daemonEnv) killDaemon(t *testing.T) {
	t.Helper()
	h.killDaemonStrict(t, false)
}

// killDaemonStrict SIGKILLs the daemon and waits for the process to be gone.
// With require set, a daemon that cannot be found is a test failure rather
// than a no-op — a test that means to kill one must not silently skip it.
//
// The wait is on the PROCESS, not on the socket. A socket probe is not a
// death test: a dial can fail transiently (a full listen backlog answers
// EAGAIN on Linux) while the daemon is very much alive, and the caller would
// then race a daemon it believes it has killed.
func (h *daemonEnv) killDaemonStrict(t *testing.T, require bool) {
	t.Helper()
	infoPath := filepath.Join(h.dataDir, "run", "daemon.json")
	raw, err := os.ReadFile(infoPath)
	if err != nil {
		if require {
			t.Fatalf("cannot kill daemon: reading %s: %v", infoPath, err)
		}
		return // cleanup path: already gone
	}
	var info struct {
		Pid int `json:"pid"`
	}
	if json.Unmarshal(raw, &info) != nil || info.Pid <= 0 {
		if require {
			t.Fatalf("cannot kill daemon: bad %s: %s", infoPath, raw)
		}
		return
	}
	_ = syscall.Kill(info.Pid, syscall.SIGKILL)
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		if err := syscall.Kill(info.Pid, 0); err != nil {
			return // ESRCH: reaped by init (the daemon is not our child)
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Errorf("daemon pid %d still alive 15s after SIGKILL", info.Pid)
}

// assertSocketRefuses waits until the control socket stops accepting, which
// is what a client observes when the daemon is gone.
func (h *daemonEnv) assertSocketRefuses(t *testing.T) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		conn, err := net.DialTimeout("unix", h.socket, time.Second)
		if err != nil {
			return
		}
		_ = conn.Close()
		if time.Now().After(deadline) {
			t.Fatalf("control socket %s still accepts connections after the daemon died", h.socket)
		}
		time.Sleep(100 * time.Millisecond)
	}
}
