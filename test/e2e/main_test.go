// Package e2e_test is the end-to-end regression suite: it
// builds the real agenthub binary, drives it exactly like an AI client does
// (a spawned `agenthub connect --client <id>` child spoken to over
// newline-delimited JSON-RPC on stdio), and pins the full path
// client -> gateway -> downstream server -> tool result.
//
// The suite is self-contained: TestMain compiles the agenthub and fakemcp
// binaries into a temp directory, and every test runs against its own
// AGENTHUB_DATA_DIR. It runs under plain `go test ./...` (and thus `make
// test` / CI); only the real-npx case skips itself when npx is unavailable
// or AGENTHUB_E2E_SKIP_NPX=1.
package e2e_test

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// Binaries built once by TestMain.
var (
	agenthubBin string
	fakemcpBin  string
	// repoRootDir is the resolved repository root, kept for the cases that
	// need to run `go build` again themselves (the docker case cross-builds
	// the downstream for the container's architecture).
	repoRootDir string
)

func TestMain(m *testing.M) {
	os.Exit(runMain(m))
}

func runMain(m *testing.M) int {
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		fmt.Fprintf(os.Stderr, "e2e: resolve repo root: %v\n", err)
		return 1
	}
	repoRootDir = root
	dir, err := os.MkdirTemp("", "agenthub-e2e-")
	if err != nil {
		fmt.Fprintf(os.Stderr, "e2e: temp dir: %v\n", err)
		return 1
	}
	defer func() { _ = os.RemoveAll(dir) }()

	agenthubBin = filepath.Join(dir, "agenthub")
	fakemcpBin = filepath.Join(dir, "fakemcp")
	if err := goBuild(root, agenthubBin, "./cmd/agenthub"); err != nil {
		fmt.Fprintf(os.Stderr, "e2e: %v\n", err)
		return 1
	}
	if err := goBuild(root, fakemcpBin, "./internal/testutil/fakemcp/cmd/fakemcp"); err != nil {
		fmt.Fprintf(os.Stderr, "e2e: %v\n", err)
		return 1
	}
	return m.Run()
}

func goBuild(root, out, pkg string) error {
	cmd := exec.Command("go", "build", "-o", out, pkg)
	cmd.Dir = root
	if combined, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("go build %s: %v\n%s", pkg, err, combined)
	}
	return nil
}

// testEnv is the child environment: the ambient environment minus every
// AGENTHUB_* variable, plus AGENTHUB_DATA_DIR pointed at the test's own
// data directory — no test may ever touch the real user registry.
//
// XDG_RUNTIME_DIR is deliberately INHERITED, not stripped. It used to be
// stripped, because on Linux it decided the run directory outright: every
// concurrent e2e daemon then shared one $XDG_RUNTIME_DIR/AgentHub/ctl.sock
// however carefully their data directories had been separated. That is now a
// property of the product rather than of this harness — AGENTHUB_DATA_DIR
// moves the run directory with it (platform.(*Resolver).RunDir) — so passing
// the variable through is what proves the rule end to end on a CI runner,
// which always sets it. Stripping it would hide the one environment shape
// where the rule is load-bearing.
func testEnv(dataDir string) []string {
	env := make([]string, 0, len(os.Environ())+1)
	for _, kv := range os.Environ() {
		if strings.HasPrefix(kv, "AGENTHUB_") {
			continue
		}
		env = append(env, kv)
	}
	return append(env, "AGENTHUB_DATA_DIR="+dataDir)
}

// envWith returns env with every key named in kv replaced rather than
// repeated. It is how a test redirects a variable the ambient environment
// already carries — HOME, for the cases that plant fake AI-client
// configuration files.
//
// Appending alone would in fact work: os/exec deduplicates the child
// environment and keeps the LAST occurrence of each key. But a test whose
// isolation from the developer's real home directory rests on that detail
// reads, at the call site, exactly like one that has no isolation at all —
// and the failure mode is a suite that rewrites the real ~/.cursor/mcp.json.
func envWith(t *testing.T, env []string, kv ...string) []string {
	t.Helper()
	replaced := make(map[string]bool, len(kv))
	for _, e := range kv {
		key, _, ok := strings.Cut(e, "=")
		if !ok || key == "" {
			t.Fatalf("envWith: %q is not KEY=VALUE", e)
		}
		replaced[key] = true
	}
	out := make([]string, 0, len(env)+len(kv))
	for _, e := range env {
		if key, _, ok := strings.Cut(e, "="); ok && replaced[key] {
			continue
		}
		out = append(out, e)
	}
	return append(out, kv...)
}

// runAgenthub executes one offline CLI invocation (server add, client
// connect, ...) against dataDir and fails the test on a non-zero exit,
// printing both output streams for diagnosis.
func runAgenthub(t *testing.T, dataDir, stdin string, args ...string) (stdout, stderr string) {
	t.Helper()
	return runAgenthubEnv(t, testEnv(dataDir), stdin, args...)
}

// enableServer puts an added server into service. `server add` writes the
// definition only and leaves the entry disabled, so every fixture that
// expects its tools to show up needs this second step.
//
// --no-probe because these fixtures are not reachable until the gateway
// spawns them: probing here would add a guaranteed-failing dial to every
// setup. A test that means to exercise the probe calls `server enable`
// itself.
func enableServer(t *testing.T, dataDir, id string) {
	t.Helper()
	runAgenthub(t, dataDir, "", "server", "enable", id, "--no-probe")
	// The same fixtures then look for the server's tools by name, and only
	// full mode puts those names in tools/list — the default is lazy, whose
	// list is the five meta-tools. `config set` is a read-modify-write, so
	// it keeps whatever else a test already put in governance.json. A test
	// that means to exercise another mode writes it afterwards and wins.
	runAgenthub(t, dataDir, "", "config", "set", "discovery", "full")
}

// runAgenthubEnv is runAgenthub with an explicit child environment (tests
// that pin AGENTHUB_SOCKET build it via testEnv + append).
func runAgenthubEnv(t *testing.T, env []string, stdin string, args ...string) (stdout, stderr string) {
	t.Helper()
	cmd := exec.Command(agenthubBin, args...)
	cmd.Env = env
	cmd.Stdin = strings.NewReader(stdin)
	var out, errOut bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &errOut
	if err := cmd.Run(); err != nil {
		t.Fatalf("agenthub %s: %v\nstdout:\n%s\nstderr:\n%s",
			strings.Join(args, " "), err, out.String(), errOut.String())
	}
	return out.String(), errOut.String()
}

// runAgenthubExit is runAgenthub for cases that EXPECT a failure: it returns
// the exit code and the combined stdout instead of failing the test.
func runAgenthubExit(t *testing.T, dataDir, stdin string, args ...string) (int, string) {
	t.Helper()
	return runAgenthubExitEnv(t, testEnv(dataDir), stdin, args...)
}

// runAgenthubExitEnv is runAgenthubExit with an explicit child environment.
// The failure cases that start a daemon need one: sandbox puts the control
// socket outside the data directory, because t.TempDir can exceed sun_path.
func runAgenthubExitEnv(t *testing.T, env []string, stdin string, args ...string) (int, string) {
	t.Helper()
	cmd := exec.Command(agenthubBin, args...)
	cmd.Env = env
	cmd.Stdin = strings.NewReader(stdin)
	var out, errOut bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &errOut
	err := cmd.Run()
	code := 0
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		code = ee.ExitCode()
	} else if err != nil {
		t.Fatalf("agenthub %s: %v\n%s", strings.Join(args, " "), err, errOut.String())
	}
	return code, out.String() + errOut.String()
}
