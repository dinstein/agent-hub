package transport

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/dinstein/agent-hub/internal/mcp"
)

func requirePOSIX(t *testing.T) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("stdio spawn tests use /bin/sh (Windows is M2)")
	}
}

// spawnShellServer spawns /bin/sh running script as a real child process.
func spawnShellServer(t *testing.T, script string) Transport {
	t.Helper()
	tr, err := SpawnStdio(StdioConfig{Command: "/bin/sh", Args: []string{"-c", script}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = tr.Close() })
	return tr
}

// A real handshake against a spawned process: initialize is answered with a
// downgraded version, stderr noise lands in the 4 KiB tail window.
func TestSpawnStdioInitializeAndStderr(t *testing.T) {
	requirePOSIX(t)
	const script = `
echo "boot noise on stderr" >&2
read line
printf '%s\n' '{"jsonrpc":"2.0","id":1,"result":{"protocolVersion":"2025-06-18","capabilities":{"tools":{}},"serverInfo":{"name":"shfake","version":"0.1"}}}'
read line2
`
	tr := spawnShellServer(t, script)
	res, err := initializeLegacy(testCtx(t), tr, mcp.Implementation{Name: "agenthub", Version: "test"})
	if err != nil {
		t.Fatalf("initialize: %v (stderr: %q)", err, tr.Stderr())
	}
	if res.ProtocolVersion != "2025-06-18" {
		t.Fatalf("negotiated %q", res.ProtocolVersion)
	}
	if res.ServerInfo.Name != "shfake" {
		t.Fatalf("serverInfo %+v", res.ServerInfo)
	}
	// The exec stderr copier is asynchronous; poll briefly.
	deadline := time.Now().Add(5 * time.Second)
	for !strings.Contains(tr.Stderr(), "boot noise on stderr") {
		if time.Now().After(deadline) {
			t.Fatalf("stderr tail = %q, want boot noise", tr.Stderr())
		}
		time.Sleep(10 * time.Millisecond)
	}
	if err := tr.Close(); err != nil {
		t.Fatal(err)
	}
}

// Child exits (crash during handshake): the pending call must end with
// ClassUnavailable, not hang.
func TestSpawnStdioProcessExitFailsPending(t *testing.T) {
	requirePOSIX(t)
	tr := spawnShellServer(t, `read line; exit 3`)
	_, err := tr.Call(testCtx(t), mcp.MethodInitialize, mcp.InitializeParams{
		ProtocolVersion: mcp.ProtocolVersion,
		ClientInfo:      mcp.Implementation{Name: "x", Version: "1"},
	})
	var te *Error
	if !errors.As(err, &te) || te.Class != ClassUnavailable {
		t.Fatalf("err = %v, want ClassUnavailable after process exit", err)
	}
	// The transport stays terminally failed.
	if _, err := tr.Call(testCtx(t), mcp.MethodPing, nil); err == nil {
		t.Fatal("call after process exit succeeded")
	}
}

// A child that emits garbage: typed malformed-frame error, no panic, no
// process crash.
func TestSpawnStdioMalformedOutput(t *testing.T) {
	requirePOSIX(t)
	tr := spawnShellServer(t, `read line; echo 'this is not json'; sleep 5`)
	_, err := tr.Call(testCtx(t), mcp.MethodPing, nil)
	if !errors.Is(err, mcp.ErrMalformedFrame) {
		t.Fatalf("err = %v, want ErrMalformedFrame", err)
	}
	var te *Error
	if !errors.As(err, &te) || te.Class != ClassUnavailable {
		t.Fatalf("err = %v, want ClassUnavailable", err)
	}
}

// Cwd and Env must reach the child verbatim.
func TestSpawnStdioCwdAndEnv(t *testing.T) {
	requirePOSIX(t)
	dir := t.TempDir()
	resolved, err := filepath.EvalSymlinks(dir) // macOS /var vs /private/var
	if err != nil {
		t.Fatal(err)
	}
	tr, err := SpawnStdio(StdioConfig{
		Command: "/bin/sh",
		Args: []string{"-c",
			`read line; printf '{"jsonrpc":"2.0","id":1,"result":{"cwd":"%s","probe":"%s"}}\n' "$(pwd)" "$AGENTHUB_TEST_PROBE"; read line2`},
		Env: append(os.Environ(), "AGENTHUB_TEST_PROBE=probe-value"),
		Cwd: resolved,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = tr.Close() })
	raw, err := tr.Call(testCtx(t), mcp.MethodPing, nil)
	if err != nil {
		t.Fatal(err)
	}
	got := string(raw)
	if !strings.Contains(got, resolved) || !strings.Contains(got, "probe-value") {
		t.Fatalf("child saw %s, want cwd %q and env probe", got, resolved)
	}
}

func TestSpawnStdioEmptyCommand(t *testing.T) {
	if _, err := SpawnStdio(StdioConfig{}); err == nil {
		t.Fatal("want error for empty command")
	}
}

func TestSpawnStdioSpawnFailureIsUnavailable(t *testing.T) {
	requirePOSIX(t)
	_, err := SpawnStdio(StdioConfig{Command: "/nonexistent/binary-xyz"})
	var te *Error
	if !errors.As(err, &te) || te.Class != ClassUnavailable {
		t.Fatalf("err = %v, want ClassUnavailable spawn failure", err)
	}
}
