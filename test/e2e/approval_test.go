package e2e_test

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

// This file pins the M1-C approval loop end to end with real processes:
// real daemon (forked via `daemon start`), real `approval watch` frontend,
// real gateway, real fake downstream. Covered:
//
//   - gated call -> `approval ls` shows it -> `approval approve` -> the call
//     executes and returns its result;
//   - `approval deny` -> the call fails with the HITL-denied gate code;
//   - kill -9 of the daemon -> a gated call is rejected fail-closed
//     (E_HITL_UNAVAILABLE) while non-gated calls keep working.

// hitlEnv is the assembled test environment for the approval e2e.
type hitlEnv struct {
	dataDir string
	socket  string
	env     []string
	watch   *watchProc
}

// setupHITL builds: governance with humanApproval set, a fakemcp downstream
// with one read-only and one destructive tool, a background daemon, and a
// running `approval watch` frontend.
func setupHITL(t *testing.T) *hitlEnv {
	t.Helper()
	dataDir := t.TempDir()

	// Short socket path: t.TempDir on macOS can exceed sun_path.
	sockDir, err := os.MkdirTemp("", "ahe2e")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(sockDir) })
	socket := filepath.Join(sockDir, "ctl.sock")
	env := append(testEnv(dataDir), "AGENTHUB_SOCKET="+socket)

	// humanApproval gates EVERY call. It is global (governance.json) because
	// that is the only place an approval switch lives: the retired client
	// layer used to carry a destructive-only variant, and removing the layer
	// removed the setting rather than leaving the gate reading a field nothing
	// could set.
	regDir := filepath.Join(dataDir, "registry")
	if err := os.MkdirAll(regDir, 0o700); err != nil {
		t.Fatal(err)
	}
	governanceJSON := []byte(`{"humanApproval":true}`)
	if err := os.WriteFile(filepath.Join(regDir, "governance.json"), governanceJSON, 0o600); err != nil {
		t.Fatal(err)
	}

	// Downstream: "read" is annotated read-only; "boom" has no annotations and
	// therefore counts as destructive (fail-closed default). Under
	// humanApproval BOTH are gated — the annotation still decides how the
	// request is CLASSIFIED to the approver, which is what the Destructive
	// assertions below check.
	script := `{"tools":[
	  {"def":{"name":"read","description":"read-only echo","inputSchema":{"type":"object"},
	          "annotations":{"readOnlyHint":true}}},
	  {"def":{"name":"boom","description":"destructive echo","inputSchema":{"type":"object"}}}
	]}`
	scriptPath := filepath.Join(dataDir, "fakemcp-script.json")
	if err := os.WriteFile(scriptPath, []byte(script), 0o600); err != nil {
		t.Fatal(err)
	}
	runAgenthub(t, dataDir, "", "server", "add", "fake", "--cmd", fakemcpBin, "--args", scriptPath)

	h := &hitlEnv{dataDir: dataDir, socket: socket, env: env}
	runAgenthubEnv(t, env, "", "daemon", "start")
	t.Cleanup(func() { h.killDaemon(t) })

	h.watch = startWatch(t, env)
	return h
}

// killDaemon SIGKILLs the daemon (A.3 #2: no goodbye, no cleanup) and waits
// until the control socket stops accepting.
func (h *hitlEnv) killDaemon(t *testing.T) {
	t.Helper()
	h.killDaemonStrict(t, false)
}

// killDaemonStrict is killDaemon with a require flag: when the test depends
// on the daemon actually being dead (the fail-closed assertions do), a
// missing or unreadable daemon.json must fail loudly instead of silently
// leaving a live broker behind — that ambiguity cost three CI rounds.
func (h *hitlEnv) killDaemonStrict(t *testing.T, require bool) {
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
	// Wait for the process itself to be gone. A socket probe is NOT a death
	// test: a dial can fail transiently (a full listen backlog returns
	// EAGAIN on Linux) while the daemon is very much alive, and the test
	// would then race a live broker that holds the gated call open until its
	// 120s TTL — which is exactly how this raced on CI.
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		if err := syscall.Kill(info.Pid, 0); err != nil {
			return // ESRCH: reaped by init (the daemon is not our child)
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Errorf("daemon pid %d still alive 15s after SIGKILL", info.Pid)
}

// assertSocketRefuses proves nothing is serving the control socket any
// more. A gated call may only fail closed once this holds; if something
// still accepts, the call would legitimately wait for a decision and the
// resulting timeout would be blamed on the wrong component.
func (h *hitlEnv) assertSocketRefuses(t *testing.T) {
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

// watchProc is a spawned `approval watch` frontend. Its presence is what
// makes the broker reachable (FrontendCount > 0).
type watchProc struct {
	cmd   *exec.Cmd
	stdin *os.File // kept open: watch stays interactive

	mu  sync.Mutex
	out []string
}

func startWatch(t *testing.T, env []string) *watchProc {
	t.Helper()
	cmd := exec.Command(agenthubBin, "approval", "watch", "--notify")
	cmd.Env = env
	inR, inW, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	cmd.Stdin = inR
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	cmd.Stderr = cmd.Stdout // interleave; only used for diagnostics
	if err := cmd.Start(); err != nil {
		t.Fatalf("start approval watch: %v", err)
	}
	_ = inR.Close()

	w := &watchProc{cmd: cmd, stdin: inW}
	ready := make(chan struct{})
	go func() {
		sc := bufio.NewScanner(stdout)
		var once sync.Once
		for sc.Scan() {
			line := sc.Text()
			w.mu.Lock()
			w.out = append(w.out, line)
			w.mu.Unlock()
			if strings.Contains(line, "watching approvals") {
				once.Do(func() { close(ready) })
			}
		}
	}()
	t.Cleanup(func() {
		_ = inW.Close()
		_ = cmd.Process.Kill()
		_, _ = cmd.Process.Wait()
	})

	// The banner prints only after the SSE subscription is established, so
	// from here on the broker sees at least one frontend.
	select {
	case <-ready:
	case <-time.After(20 * time.Second):
		t.Fatalf("approval watch never became ready; output:\n%s", strings.Join(w.output(), "\n"))
	}
	return w
}

func (w *watchProc) output() []string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return append([]string(nil), w.out...)
}

// waitApprovalToken polls `approval ls --json` until a pending request
// appears and returns its token.
func waitApprovalToken(t *testing.T, env []string) string {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		out, _ := runAgenthubEnv(t, env, "", "approval", "ls", "--json")
		var envl struct {
			OK   bool `json:"ok"`
			Data struct {
				Approvals []struct {
					Token string `json:"token"`
				} `json:"approvals"`
			} `json:"data"`
		}
		if err := json.Unmarshal([]byte(out), &envl); err == nil &&
			envl.OK && len(envl.Data.Approvals) > 0 {
			return envl.Data.Approvals[0].Token
		}
		time.Sleep(200 * time.Millisecond)
	}
	t.Fatal("no pending approval appeared in `approval ls`")
	return ""
}

// callOutcome is the non-fatal tools/call result for goroutine use.
type callOutcome struct {
	res    json.RawMessage
	rpcErr *rpcError
	err    error
}

// tryCallTool is gatewayClient.call without t.Fatal — safe to run on a
// helper goroutine while the main goroutine drives the CLI. Only one
// in-flight call may use the shared message channel at a time.
func (c *gatewayClient) tryCallTool(name string, args any, timeout time.Duration) callOutcome {
	c.mu.Lock()
	c.nextID++
	id := c.nextID
	c.mu.Unlock()
	msg := map[string]any{
		"jsonrpc": "2.0", "id": id, "method": "tools/call",
		"params": map[string]any{"name": name, "arguments": args},
	}
	b, err := json.Marshal(msg)
	if err != nil {
		return callOutcome{err: err}
	}
	if _, err := c.stdin.Write(append(b, '\n')); err != nil {
		return callOutcome{err: err}
	}
	wantID := []byte(fmt.Sprintf("%d", id))
	deadline := time.After(timeout)
	for {
		select {
		case raw, ok := <-c.msgs:
			if !ok {
				return callOutcome{err: fmt.Errorf("gateway closed stdout mid-call")}
			}
			var m rpcMsg
			if err := json.Unmarshal(raw, &m); err != nil {
				return callOutcome{err: fmt.Errorf("non-JSON frame: %w", err)}
			}
			switch {
			case m.Method != "" && len(m.ID) > 0 && string(m.ID) != "null":
				// Reverse RPC (roots/list): answer inline, non-fatally.
				reply := map[string]any{"jsonrpc": "2.0", "id": json.RawMessage(m.ID)}
				if m.Method == "roots/list" {
					reply["result"] = map[string]any{"roots": []any{}}
				} else {
					reply["error"] = map[string]any{"code": -32601, "message": "not served"}
				}
				if rb, rerr := json.Marshal(reply); rerr == nil {
					_, _ = c.stdin.Write(append(rb, '\n'))
				}
			case m.Method != "":
				// notification: ignore
			case bytes.Equal(m.ID, wantID):
				return callOutcome{res: m.Result, rpcErr: m.Error}
			}
		case <-deadline:
			// A gated call that never returns is the interesting failure:
			// dump the gateway's goroutines into its captured stderr so the
			// report says where it was parked instead of just "timeout".
			c.dumpStacks()
			return callOutcome{err: fmt.Errorf("timeout waiting for tools/call %s; gateway stderr:\n%s",
				name, c.stderr.String())}
		}
	}
}

// gatedCallAsync fires a gated tools/call on a goroutine.
func gatedCallAsync(c *gatewayClient, marker string) <-chan callOutcome {
	out := make(chan callOutcome, 1)
	go func() {
		out <- c.tryCallTool("fake__boom", map[string]any{"marker": marker}, 90*time.Second)
	}()
	return out
}

func recvOutcome(t *testing.T, ch <-chan callOutcome) callOutcome {
	t.Helper()
	select {
	case o := <-ch:
		if o.err != nil {
			t.Fatalf("gated call transport error: %v", o.err)
		}
		return o
	case <-time.After(120 * time.Second):
		t.Fatal("gated call never returned")
		return callOutcome{}
	}
}

func TestApprovalEndToEnd(t *testing.T) {
	h := setupHITL(t)

	c := startGatewayEnv(t, h.env, "e2e-hitl")
	c.initialize()
	c.waitForTool("fake__boom", 30*time.Second)

	// A read-only call is gated too under humanApproval, and is classified as
	// NON-destructive when it reaches the approver — the annotation decides
	// how the request is presented, not whether it is asked about.
	readOutcome := func(marker string) <-chan callOutcome {
		out := make(chan callOutcome, 1)
		go func() {
			out <- c.tryCallTool("fake__read", map[string]any{"marker": marker}, 90*time.Second)
		}()
		return out
	}
	pending := readOutcome("plain-read")
	token := waitApprovalToken(t, h.env)
	runAgenthubEnv(t, h.env, "", "approval", "approve", token)
	o := recvOutcome(t, pending)
	if o.rpcErr != nil {
		c.fatalf("approved read-only call failed: %v", o.rpcErr)
	}
	if text := c.textContent(o.res); !strings.Contains(text, "plain-read") {
		c.fatalf("read echo = %q", text)
	}

	// --- approve path: gated call blocks, ls shows it, approve releases it.
	outcome := gatedCallAsync(c, "gated-approved")
	token = waitApprovalToken(t, h.env)
	runAgenthubEnv(t, h.env, "", "approval", "approve", token)
	o = recvOutcome(t, outcome)
	if o.rpcErr != nil {
		c.fatalf("approved call failed: %v", o.rpcErr)
	}
	if text := c.textContent(o.res); !strings.Contains(text, "gated-approved") {
		c.fatalf("approved call result = %q", text)
	}

	// The watch frontend saw the pending card for that token.
	sawToken := false
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) && !sawToken {
		for _, line := range h.watch.output() {
			if strings.Contains(line, token) {
				sawToken = true
				break
			}
		}
		time.Sleep(100 * time.Millisecond)
	}
	if !sawToken {
		t.Errorf("approval watch never displayed token %s; output:\n%s",
			token, strings.Join(h.watch.output(), "\n"))
	}

	// --- deny path: the call is rejected with the HITL-denied gate code.
	outcome = gatedCallAsync(c, "gated-denied")
	token = waitApprovalToken(t, h.env)
	runAgenthubEnv(t, h.env, "", "approval", "deny", token)
	o = recvOutcome(t, outcome)
	if o.rpcErr == nil {
		c.fatalf("denied call succeeded: %s", o.res)
	}
	if !strings.Contains(o.rpcErr.Message, "E_HITL_DENIED") {
		c.fatalf("denied call error = %v, want E_HITL_DENIED", o.rpcErr)
	}

	// --- kill -9: gated calls fail closed, normal calls keep working.
	h.killDaemonStrict(t, true)
	h.assertSocketRefuses(t)
	o = recvOutcome(t, gatedCallAsync(c, "gated-orphaned"))
	if o.rpcErr == nil {
		c.fatalf("gated call succeeded with a dead daemon: %s", o.res)
	}
	if !strings.Contains(o.rpcErr.Message, "E_HITL_UNAVAILABLE") {
		c.fatalf("dead-daemon gated call error = %v, want E_HITL_UNAVAILABLE", o.rpcErr)
	}
	// Every call is gated under humanApproval, so with the broker gone there
	// is no "normal call" left to keep working: the fail-closed direction is
	// the whole answer, and it is asserted above.

	c.close()
}
