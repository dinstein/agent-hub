package e2e_test

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// The hub belongs to the application that started it. Two halves, and only
// this suite can check either one: both are about real processes ending.
//
//	admission     — a start that names no owner is refused, so a hub nothing
//	                is responsible for cannot come into existence.
//	the watch     — a hub whose owner dies stops on its own, having been sent
//	                nothing at all.

// ownerBudget is how long a daemon may take to notice its owner is gone. The
// poll runs every two seconds, so this is several ticks plus the drain.
const ownerBudget = 20 * time.Second

// sandbox returns a data directory and the environment pointing at it, with a
// short socket path (t.TempDir can exceed sun_path on macOS).
func sandbox(t *testing.T) (dataDir, socket string, env []string) {
	t.Helper()
	dataDir = t.TempDir()
	sockDir, err := os.MkdirTemp("", "ahowner")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(sockDir) })
	socket = filepath.Join(sockDir, "ctl.sock")
	return dataDir, socket, append(testEnv(dataDir), "AGENTHUB_SOCKET="+socket)
}

// daemonPid reads the pid out of the readiness handshake file. It fails hard:
// a test that cannot find the daemon it just started must not quietly pass.
func daemonPid(t *testing.T, dataDir string) int {
	t.Helper()
	path := filepath.Join(dataDir, "run", "daemon.json")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading %s: %v", path, err)
	}
	var info struct {
		Pid   int `json:"pid"`
		Owner int `json:"owner"`
	}
	if err := json.Unmarshal(raw, &info); err != nil {
		t.Fatalf("parsing %s: %v", path, err)
	}
	if info.Pid == 0 {
		t.Fatalf("%s names no pid: %s", path, raw)
	}
	return info.Pid
}

func TestDaemonStartRefusesAHubThatBelongsToNobody(t *testing.T) {
	dataDir, _, _ := sandbox(t)

	code, out := runAgenthubExit(t, dataDir, "", "daemon", "start", "--json")
	if code != 2 {
		t.Fatalf("exit = %d, want 2 (usage): %s", code, out)
	}
	if !strings.Contains(out, "E_DAEMON_UNOWNED") {
		t.Fatalf("the refusal must be identifiable by code: %s", out)
	}
	// And it must say how to get a hub, in both of the two ways there are.
	if !strings.Contains(out, "AgentHub") || !strings.Contains(out, "--headless") {
		t.Fatalf("the refusal names no way forward: %s", out)
	}
	// Nothing was started: a refused start that left a process behind would
	// be the exact outcome the rule exists to prevent.
	if _, err := os.Stat(filepath.Join(dataDir, "run", "daemon.json")); err == nil {
		t.Fatal("a refused start still wrote the readiness handshake")
	}
}

func TestAHubDiesWithItsOwner(t *testing.T) {
	dataDir, _, env := sandbox(t)

	// A stand-in for the desktop application: a process that does nothing but
	// exist, so that killing it is the only thing this test does to the hub.
	owner := exec.Command("sh", "-c", "sleep 120")
	if err := owner.Start(); err != nil {
		t.Fatalf("starting the stand-in owner: %v", err)
	}
	ownerPID := owner.Process.Pid
	t.Cleanup(func() {
		_ = owner.Process.Kill()
		_ = owner.Wait()
	})

	runAgenthubEnv(t, env, "", "daemon", "start", "--owner-pid", strconv.Itoa(ownerPID))
	pid := daemonPid(t, dataDir)
	h := &daemonEnv{dataDir: dataDir, socket: envValue(t, env, "AGENTHUB_SOCKET"), env: env}
	t.Cleanup(func() { h.killDaemon(t) })

	// The hub records who it belongs to, so an operator can tell a hub that
	// will go away from one that will not.
	raw, err := os.ReadFile(filepath.Join(dataDir, "run", "daemon.json"))
	if err != nil {
		t.Fatalf("reading daemon.json: %v", err)
	}
	var info struct {
		Owner int `json:"owner"`
	}
	if err := json.Unmarshal(raw, &info); err != nil {
		t.Fatal(err)
	}
	if info.Owner != ownerPID {
		t.Fatalf("daemon.json owner = %d, want the owning process %d", info.Owner, ownerPID)
	}

	// Kill the owner outright. The hub is sent nothing: no signal, no
	// shutdown request, no goodbye — exactly what a crash or a force-quit
	// leaves it with.
	if err := owner.Process.Kill(); err != nil {
		t.Fatalf("killing the stand-in owner: %v", err)
	}
	_ = owner.Wait()

	deadline := time.Now().Add(ownerBudget)
	for processAlive(pid) {
		if time.Now().After(deadline) {
			t.Fatalf("hub pid %d outlived its owner by more than %s; nothing on this machine would ever stop it", pid, ownerBudget)
		}
		time.Sleep(200 * time.Millisecond)
	}
	h.assertSocketRefuses(t)
}

func TestAHeadlessHubOutlivesEveryone(t *testing.T) {
	dataDir, _, env := sandbox(t)

	runAgenthubEnv(t, env, "", "daemon", "start", "--headless")
	pid := daemonPid(t, dataDir)
	h := &daemonEnv{dataDir: dataDir, socket: envValue(t, env, "AGENTHUB_SOCKET"), env: env}
	t.Cleanup(func() { h.killDaemon(t) })

	// Long enough for several owner-watch ticks. A headless hub watches
	// nothing, and the failure this guards against is a watch that armed
	// itself anyway and shut down every hub an operator starts.
	time.Sleep(5 * time.Second)
	if !processAlive(pid) {
		t.Fatalf("the headless hub (pid %d) stopped on its own", pid)
	}
	out, _ := runAgenthubEnv(t, env, "", "daemon", "status", "--json")
	if !strings.Contains(out, `"running":true`) {
		t.Fatalf("daemon status does not report a running headless hub: %s", out)
	}
}

// envValue reads one variable out of a prepared environment slice.
func envValue(t *testing.T, env []string, key string) string {
	t.Helper()
	for _, kv := range env {
		if name, value, ok := strings.Cut(kv, "="); ok && name == key {
			return value
		}
	}
	t.Fatalf("%s is not in the prepared environment", key)
	return ""
}

// processAlive reports whether pid still exists. Signal 0 is the existence
// probe; any error reads as gone, which is the direction that makes the wait
// loops above terminate rather than hang.
func processAlive(pid int) bool {
	p, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	return p.Signal(syscall.Signal(0)) == nil
}
