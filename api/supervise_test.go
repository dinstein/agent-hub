package api

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// The supervised start appends the owner handshake to its child's argv, and
// the child here is this test binary re-execed. Registering the two flags is
// what lets it parse them — and it doubles as the assertion that the names
// are what the daemon's CLI expects, since a rename on one side leaves the
// re-exec failing to parse rather than quietly passing.
var (
	fakeOwnerPID   = flag.Int("owner-pid", 0, "owner handshake, read by the fake supervised daemon")
	fakeLifelineFD = flag.Int("owner-lifeline-fd", 0, "owner handshake, read by the fake supervised daemon")
)

// handshakeFileName is where the fake daemon records what it was told, for
// the parent test to read back.
const handshakeFileName = "handshake.json"

type handshake struct {
	OwnerPID   int `json:"ownerPid"`
	LifelineFD int `json:"lifelineFd"`
	// LifelineOpen reports whether the named descriptor was actually a live,
	// readable pipe rather than a number nobody wired up.
	LifelineOpen bool `json:"lifelineOpen"`
}

// TestFakeSupervisedDaemonProcess is not a test: it is the daemon binary for
// the supervision tests, entered by re-exec. It serves /v1/ping, records the
// handshake it received, and then waits to be stopped.
func TestFakeSupervisedDaemonProcess(t *testing.T) {
	if os.Getenv("AGENTHUB_TEST_FAKE_DAEMON") != "supervised" {
		t.Skip("helper process for the supervision tests")
	}
	sock := os.Getenv("AGENTHUB_SOCKET")
	runDir := os.Getenv("AGENTHUB_TEST_RUNDIR")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatalf("fake daemon: listen %s: %v", sock, err)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/ping", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"ok": true,
			"data": Hello{
				Version: "0.0.0-fake", Pid: os.Getpid(), Generation: 1,
				Owner: *fakeOwnerPID,
			},
		})
	})
	h := handshake{OwnerPID: *fakeOwnerPID, LifelineFD: *fakeLifelineFD}
	if h.LifelineFD > 0 {
		// A pipe with a live writer at the other end: reading returns EAGAIN
		// or blocks, and Stat succeeds. A number that was never wired fails
		// here instead.
		f := os.NewFile(uintptr(h.LifelineFD), "lifeline")
		if f != nil {
			if _, err := f.Stat(); err == nil {
				h.LifelineOpen = true
			}
		}
	}
	blob, _ := json.Marshal(h)
	if err := os.WriteFile(filepath.Join(runDir, handshakeFileName), blob, 0o600); err != nil {
		t.Fatalf("fake daemon: writing the handshake: %v", err)
	}
	info, _ := json.Marshal(daemonInfo{Endpoint: sock, Pid: os.Getpid(), Version: "0.0.0-fake"})
	if err := os.WriteFile(filepath.Join(runDir, "daemon.json"), info, 0o600); err != nil {
		t.Fatalf("fake daemon: writing daemon.json: %v", err)
	}

	// SERVED LAST, and that ordering is the whole point. StartSupervised
	// returns as soon as /v1/ping answers — awaitReady reads daemon.json only
	// to learn the endpoint, and falls back to the socket path when it is not
	// there yet — so anything this process is supposed to have recorded by the
	// time the parent is unblocked has to be on disk before the first response
	// goes out. Serving first left the parent free to read a handshake.json
	// that os.WriteFile had created and not yet filled, which fails as
	// "unexpected end of JSON input" on a loaded machine and passes two
	// hundred times in a row on an idle one.
	go http.Serve(ln, mux) //nolint:errcheck // the parent stops this process

	select {} // until the parent stops us
}

// startFake runs the fake daemon under supervision, in a sandbox of its own.
func startFake(t *testing.T) (*Supervised, string) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("the fake daemon listens on a unix socket")
	}
	dir := shortTempDir(t)
	sock := filepath.Join(dir, "ctl.sock")
	t.Setenv("AGENTHUB_TEST_FAKE_DAEMON", "supervised")
	t.Setenv("AGENTHUB_SOCKET", sock)
	t.Setenv("AGENTHUB_TEST_RUNDIR", dir)
	// The child's raw stderr follows the DATA directory, so pinning it keeps
	// this test out of the real user's log directory.
	t.Setenv("AGENTHUB_DATA_DIR", dir)

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	s, err := StartSupervised(ctx, StartOptions{
		SocketPath:   sock,
		RunDir:       dir,
		DaemonBinary: os.Args[0],
		DaemonArgs:   []string{"-test.run=^TestFakeSupervisedDaemonProcess$"},
		Deadline:     15 * time.Second,
		PollInterval: 25 * time.Millisecond,
	})
	if err != nil {
		// The child's raw stderr is the only account of why it never came up,
		// and without it a failure here says "deadline exceeded" and nothing
		// about the process that missed it. This fixture starts a real
		// subprocess under a real deadline, so it CAN lose to a loaded
		// machine — and a flake that reports nothing is one nobody can tell
		// from a regression.
		t.Fatalf("StartSupervised: %v\n--- child stderr ---\n%s", err, fakeChildStderr(dir))
	}
	t.Cleanup(func() { _ = s.Stop(context.Background()) })
	return s, dir
}

// fakeChildStderr reads back whatever the supervised child wrote before it
// gave up. A missing file is itself the answer — the process never got far
// enough to open one.
func fakeChildStderr(dir string) string {
	b, err := os.ReadFile(filepath.Join(dir, daemonStderrName))
	if err != nil {
		return "(no stderr file: " + err.Error() + ")"
	}
	if len(b) == 0 {
		return "(empty)"
	}
	return string(b)
}

func TestStartSupervisedRunsAndStopsTheHub(t *testing.T) {
	s, _ := startFake(t)

	h, err := s.Client.Ping(context.Background())
	if err != nil {
		t.Fatalf("Ping: %v", err)
	}
	if h.Version != "0.0.0-fake" {
		t.Fatalf("Hello.Version = %q", h.Version)
	}
	if h.Owner != os.Getpid() {
		t.Fatalf("the hub reports owner %d, want this process (%d)", h.Owner, os.Getpid())
	}
	if s.Pid() <= 0 {
		t.Fatal("a supervised hub must have a process handle to stop")
	}

	if err := s.Stop(context.Background()); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	select {
	case <-s.Exited():
	case <-time.After(10 * time.Second):
		t.Fatal("Stop returned but the hub is still running")
	}
	// Idempotent: a second stop (the cleanup hook runs one too) is a no-op.
	if err := s.Stop(context.Background()); err != nil {
		t.Fatalf("second Stop: %v", err)
	}
}

func TestStartSupervisedHandsOverTheOwnerHandshake(t *testing.T) {
	_, dir := startFake(t)

	blob, err := os.ReadFile(filepath.Join(dir, handshakeFileName))
	if err != nil {
		t.Fatalf("reading the handshake the child recorded: %v", err)
	}
	var h handshake
	if err := json.Unmarshal(blob, &h); err != nil {
		t.Fatal(err)
	}
	if h.OwnerPID != os.Getpid() {
		t.Errorf("child was told owner %d, want %d", h.OwnerPID, os.Getpid())
	}
	// fd 3 is ExtraFiles[0] by definition; the assertion that matters is that
	// something is actually open there. A lifeline that is only a number on a
	// command line is the failure mode this catches: the daemon would wait on
	// it forever and never notice the application had gone.
	if h.LifelineFD != 3 {
		t.Errorf("child was told lifeline fd %d, want 3", h.LifelineFD)
	}
	if !h.LifelineOpen {
		t.Error("the lifeline descriptor was not open in the child")
	}
}

func TestStartSupervisedRefusesAHubItDidNotStart(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/ping", func(w http.ResponseWriter, _ *http.Request) {
		writeOK(t, w, Hello{Version: "somebody else's"})
	})
	sock := newTestDaemon(t, mux)

	_, err := StartSupervised(context.Background(), StartOptions{
		SocketPath:   sock,
		DaemonBinary: filepath.Join(shortTempDir(t), "does-not-exist"),
	})
	if !errors.Is(err, ErrDaemonNotOurs) {
		t.Fatalf("err = %v, want ErrDaemonNotOurs — adopting a running hub as a child we may stop is the one thing this must refuse", err)
	}
}

func TestStartSupervisedReportsAChildThatDiedEarly(t *testing.T) {
	dir := shortTempDir(t)
	t.Setenv("AGENTHUB_DATA_DIR", dir)
	bin := writeScript(t, dir, `echo "boom: a hub belongs to the AgentHub application" >&2; exit 2`)

	_, err := StartSupervised(context.Background(), StartOptions{
		SocketPath:   filepath.Join(dir, "ctl.sock"),
		RunDir:       dir,
		DaemonBinary: bin,
		DaemonArgs:   []string{},
		// Must NOT be consumed: the exit is noticed as it happens, and the
		// refusal — not a timeout — is what the operator has to act on.
		Deadline:     10 * time.Second,
		PollInterval: 10 * time.Millisecond,
	})
	if err == nil {
		t.Fatal("want an error when the child exits before serving")
	}
	if !strings.Contains(err.Error(), "boom: a hub belongs to the AgentHub application") {
		t.Fatalf("error must carry the child's own stderr, got: %v", err)
	}
}
