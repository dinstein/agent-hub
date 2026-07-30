package api

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// TestFakeDaemonProcess is not a test: it is the fake daemon binary for
// the DialOrStart tests, entered via re-exec of the test binary with
// -test.run=^TestFakeDaemonProcess$. Guarded by env so a normal test run
// skips it.
func TestFakeDaemonProcess(t *testing.T) {
	if os.Getenv("AGENTHUB_TEST_FAKE_DAEMON") != "1" {
		t.Skip("helper process for DialOrStart tests; enabled via AGENTHUB_TEST_FAKE_DAEMON=1")
	}
	sock := os.Getenv("AGENTHUB_SOCKET")
	runDir := os.Getenv("AGENTHUB_TEST_RUNDIR")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatalf("fake daemon: listen %s: %v", sock, err)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/ping", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"ok":   true,
			"data": Hello{Version: "0.0.0-fake", Pid: os.Getpid(), Generation: 1},
		})
	})
	go http.Serve(ln, mux) //nolint:errcheck // process is killed by the parent
	// Readiness handshake: write daemon.json only after bind, as the real
	// daemon does (docs/architecture.md §10).
	info, _ := json.Marshal(daemonInfo{Endpoint: sock, Pid: os.Getpid(), Version: "0.0.0-fake"})
	if err := os.WriteFile(filepath.Join(runDir, "daemon.json"), info, 0o600); err != nil {
		t.Fatalf("fake daemon: writing daemon.json: %v", err)
	}
	select {} // serve until the parent kills us
}

func TestDialOrStartSpawnsDaemon(t *testing.T) {
	dir := shortTempDir(t)
	sock := filepath.Join(dir, "ctl.sock")
	t.Setenv("AGENTHUB_TEST_FAKE_DAEMON", "1")
	t.Setenv("AGENTHUB_SOCKET", sock)
	t.Setenv("AGENTHUB_TEST_RUNDIR", dir)
	t.Cleanup(func() { killDaemonFromInfo(t, filepath.Join(dir, "daemon.json")) })

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	c, err := DialOrStartWith(ctx, StartOptions{
		SocketPath:   sock,
		RunDir:       dir,
		DaemonBinary: os.Args[0], // re-exec this test binary as the daemon
		DaemonArgs:   []string{"-test.run=^TestFakeDaemonProcess$"},
		Deadline:     15 * time.Second,
		PollInterval: 25 * time.Millisecond,
	})
	if err != nil {
		t.Fatalf("DialOrStartWith: %v", err)
	}
	defer c.Close()
	h, err := c.Ping(ctx)
	if err != nil {
		t.Fatalf("Ping after DialOrStart: %v", err)
	}
	if h.Version != "0.0.0-fake" {
		t.Errorf("Hello.Version = %q, want 0.0.0-fake", h.Version)
	}
}

func killDaemonFromInfo(t *testing.T, path string) {
	t.Helper()
	info, err := readDaemonInfo(path)
	if err != nil || info.Pid == 0 {
		return // never started; nothing to kill
	}
	// os.FindProcess + Kill rather than syscall.Kill(pid, SIGKILL): the two are
	// the same call on Unix, and this one also compiles for GOOS=windows, which
	// `make cross-windows` vets. syscall.Kill does not exist there, so one line
	// of test cleanup used to fail the whole package's cross-platform vet.
	p, err := os.FindProcess(info.Pid)
	if err != nil {
		return
	}
	_ = p.Kill()
}

func TestDialOrStartAlreadyRunningSkipsExec(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/ping", func(w http.ResponseWriter, r *http.Request) {
		writeOK(t, w, Hello{Version: "live"})
	})
	sock := newTestDaemon(t, mux)

	c, err := DialOrStartWith(context.Background(), StartOptions{
		SocketPath:   sock,
		DaemonBinary: filepath.Join(shortTempDir(t), "does-not-exist"),
	})
	if err != nil {
		t.Fatalf("DialOrStartWith with live daemon must not exec: %v", err)
	}
	defer c.Close()
}

// writeScript writes an executable shell script standing in for the
// daemon binary.
func writeScript(t *testing.T, dir, body string) string {
	t.Helper()
	p := filepath.Join(dir, "fake-agenthub")
	if err := os.WriteFile(p, []byte("#!/bin/sh\n"+body+"\n"), 0o755); err != nil {
		t.Fatalf("writing script: %v", err)
	}
	return p
}

func TestDialOrStartChildFailureReportsRealError(t *testing.T) {
	dir := shortTempDir(t)
	bin := writeScript(t, dir, `echo "boom: config invalid" >&2; exit 3`)

	_, err := DialOrStartWith(context.Background(), StartOptions{
		SocketPath:   filepath.Join(dir, "ctl.sock"),
		RunDir:       dir,
		DaemonBinary: bin,
		DaemonArgs:   []string{},
		Deadline:     10 * time.Second, // must NOT be consumed: exit is detected early
		PollInterval: 10 * time.Millisecond,
	})
	if err == nil {
		t.Fatal("want error when the child exits non-zero")
	}
	if !strings.Contains(err.Error(), "boom: config invalid") {
		t.Errorf("error must carry the child's stderr, got: %v", err)
	}
	if !strings.Contains(err.Error(), "exited before daemon became ready") {
		t.Errorf("error must name the real cause, got: %v", err)
	}
}

func TestDialOrStartDeadlineWhenLauncherDetaches(t *testing.T) {
	dir := shortTempDir(t)
	bin := writeScript(t, dir, `exit 0`) // "daemonizes" but never serves

	_, err := DialOrStartWith(context.Background(), StartOptions{
		SocketPath:   filepath.Join(dir, "ctl.sock"),
		RunDir:       dir,
		DaemonBinary: bin,
		DaemonArgs:   []string{},
		// Generous on purpose: the assertion is that the message names the
		// detach, which requires the launcher's exit to be observed first.
		// On a loaded machine (the full suite runs ~30 race-instrumented
		// binaries at once) merely spawning a shell can take seconds, so a
		// tight deadline would test the scheduler rather than the behavior.
		Deadline:     8 * time.Second,
		PollInterval: 20 * time.Millisecond,
	})
	if err == nil {
		t.Fatal("want deadline error when the daemon never becomes ready")
	}
	if !strings.Contains(err.Error(), "did not become ready") {
		t.Errorf("want readiness-deadline error, got: %v", err)
	}
	if !strings.Contains(err.Error(), "launcher exited 0") {
		t.Errorf("error should note the launcher detached, got: %v", err)
	}
}

func TestDialOrStartContextCancellation(t *testing.T) {
	dir := shortTempDir(t)
	bin := writeScript(t, dir, `sleep 5`)

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(100 * time.Millisecond)
		cancel()
	}()
	_, err := DialOrStartWith(ctx, StartOptions{
		SocketPath:   filepath.Join(dir, "ctl.sock"),
		RunDir:       dir,
		DaemonBinary: bin,
		DaemonArgs:   []string{},
		Deadline:     20 * time.Second,
		PollInterval: 20 * time.Millisecond,
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("want context.Canceled, got %v", err)
	}
}

// The GUI ships beside the CLI and, launched from Finder or a dock icon,
// inherits none of a login shell's $PATH. Resolving the daemon only through
// $PATH meant it could not start the agenthub sitting in its own directory:
// the user got "socket missing" for a launch that never happened.
func TestDaemonBinaryPrefersSibling(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("execute-bit semantics differ; the sibling branch is unix-only here")
	}
	dir := t.TempDir()
	exe := filepath.Join(dir, "agenthub-gui")
	if err := os.WriteFile(exe, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatalf("write caller: %v", err)
	}

	// No sibling yet: fall back to the bare name for $PATH.
	if got := daemonBinaryNear(exe, "agenthub"); got != "agenthub" {
		t.Errorf("with no sibling: got %q, want the bare name", got)
	}

	// A sibling that is not executable must NOT win: returning it would
	// replace a working $PATH lookup with a path that cannot be spawned.
	sib := filepath.Join(dir, "agenthub")
	if err := os.WriteFile(sib, nil, 0o644); err != nil {
		t.Fatalf("write sibling: %v", err)
	}
	if got := daemonBinaryNear(exe, "agenthub"); got != "agenthub" {
		t.Errorf("with a non-executable sibling: got %q, want the bare name", got)
	}

	// Executable sibling: absolute path wins.
	if err := os.Chmod(sib, 0o755); err != nil {
		t.Fatalf("chmod sibling: %v", err)
	}
	// Compare against the symlink-resolved path: on macOS t.TempDir() sits
	// under /var, which is a link to /private/var, and resolving that is the
	// behaviour under test rather than an artefact of it.
	wantSib, err := filepath.EvalSymlinks(sib)
	if err != nil {
		t.Fatalf("resolve sibling: %v", err)
	}
	if got := daemonBinaryNear(exe, "agenthub"); got != wantSib {
		t.Errorf("with an executable sibling: got %q, want %q", got, wantSib)
	}

	// A directory named agenthub is not a binary.
	dir2 := t.TempDir()
	exe2 := filepath.Join(dir2, "agenthub-gui")
	if err := os.WriteFile(exe2, nil, 0o755); err != nil {
		t.Fatalf("write caller: %v", err)
	}
	if err := os.Mkdir(filepath.Join(dir2, "agenthub"), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if got := daemonBinaryNear(exe2, "agenthub"); got != "agenthub" {
		t.Errorf("with a directory sibling: got %q, want the bare name", got)
	}
}
