package e2e_test

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestFakeDownstreamRoundTrip is the always-on e2e case: a scripted fake
// downstream (the standalone fakemcp binary with its default echo tool) is
// registered offline, then a real spawned gateway serves it end to end:
// initialize -> tools/list (polled until the live catalog is up) ->
// tools/call fake__echo -> clean EOF exit.
func TestFakeDownstreamRoundTrip(t *testing.T) {
	dataDir := t.TempDir()
	out, _ := runAgenthub(t, dataDir, "", "server", "add", "fake", "--cmd", fakemcpBin, "--json")
	enableServer(t, dataDir, "fake")
	if !strings.Contains(out, `"ok":true`) {
		t.Fatalf("server add envelope: %s", out)
	}

	c := startGateway(t, dataDir, "e2e")
	c.initialize()
	c.waitForTool("fake__echo", 30*time.Second)

	res := c.callTool("fake__echo", map[string]any{"marker": "e2e-fake-roundtrip"}, 30*time.Second)
	text := c.textContent(res)
	if !strings.Contains(text, "e2e-fake-roundtrip") {
		c.fatalf("echo result does not contain the marker: %q", text)
	}
	t.Logf("fake__echo answered: %s", text)
	c.close()
}

// TestRealNpxFilesystemServer is the acceptance-standard case
// (docs/conventions.md, "current state"): a
// real @modelcontextprotocol/server-filesystem downstream spawned via npx,
// its list_directory tool called through the gateway, the result naming a
// known file. Skipped only when npx is unavailable or explicitly disabled.
func TestRealNpxFilesystemServer(t *testing.T) {
	if os.Getenv("AGENTHUB_E2E_SKIP_NPX") == "1" {
		t.Skip("AGENTHUB_E2E_SKIP_NPX=1")
	}
	if _, err := exec.LookPath("npx"); err != nil {
		t.Skip("npx not found in PATH")
	}

	dataDir := t.TempDir()
	workDir, err := filepath.EvalSymlinks(t.TempDir()) // macOS: /var -> /private/var
	if err != nil {
		t.Fatal(err)
	}
	const marker = "agenthub-e2e-marker.txt"
	if err := os.WriteFile(filepath.Join(workDir, marker), []byte("hello from agenthub e2e\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	spec, err := json.Marshal(map[string]any{
		"mcpServers": map[string]any{
			"fs": map[string]any{
				"command": "npx",
				"args":    []string{"-y", "@modelcontextprotocol/server-filesystem", workDir},
			},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	runAgenthub(t, dataDir, string(spec), "server", "add", "--stdin")
	enableServer(t, dataDir, "fs")

	c := startGateway(t, dataDir, "e2e-npx")
	c.initialize()
	// First npx run may download the package: give the downstream connect
	// the full 120s budget.
	c.waitForTool("fs__list_directory", 120*time.Second)
	tools := c.listTools(30 * time.Second)
	t.Logf("filesystem tools exposed: %v", tools)

	res := c.callTool("fs__list_directory", map[string]any{"path": workDir}, 60*time.Second)
	text := c.textContent(res)
	if !strings.Contains(text, marker) {
		c.fatalf("list_directory of %s does not name %q: %q", workDir, marker, text)
	}
	t.Logf("fs__list_directory answered: %s", text)
	c.close()
}

// TestDockerRuntimeDownstream is the container-isolation acceptance case:
// a `runtime: docker` downstream spawned as a real container by a real
// gateway, its tool called end to end.
//
// Every other docker test in the tree drives a shell stand-in for the docker
// CLI, which pins the argv but can never prove a container ran. This one
// closes that gap, and it is the regression test for a specific way the
// feature can rot: if the connection layer ever loses the runtime dimension
// again, the entry spawns ON THE HOST and the isolation is silently gone.
//
// That failure is caught structurally rather than by inspection. The
// downstream binary is mounted at a path that exists ONLY inside the
// container, so a host spawn cannot answer at all — a passing tool call is
// itself the proof the container ran.
func TestDockerRuntimeDownstream(t *testing.T) {
	if os.Getenv("AGENTHUB_E2E_SKIP_DOCKER") == "1" {
		t.Skip("AGENTHUB_E2E_SKIP_DOCKER=1")
	}
	if _, err := exec.LookPath("docker"); err != nil {
		t.Skip("docker not found in PATH")
	}
	// A daemon that does not answer is a skip, not a failure: the suite must
	// stay green on a machine with the CLI but no running Docker.
	arch, err := dockerServerArch(t)
	if err != nil {
		t.Skipf("docker daemon does not answer: %v", err)
	}

	// fakemcp is built for the CONTAINER's os/arch, not the host's: on macOS
	// the daemon runs linux, so the host binary would not execute.
	mountDir := t.TempDir()
	guest := filepath.Join(mountDir, "fakemcp")
	build := exec.Command("go", "build", "-o", guest, "./internal/testutil/fakemcp/cmd/fakemcp")
	build.Dir = repoRootDir
	build.Env = append(os.Environ(), "GOOS=linux", "GOARCH="+arch, "CGO_ENABLED=0")
	if combined, buildErr := build.CombinedOutput(); buildErr != nil {
		t.Fatalf("cross-build fakemcp for linux/%s: %v\n%s", arch, buildErr, combined)
	}

	// Pull up front so the image download cannot be mistaken for a slow
	// handshake: after this, a downstream that does not appear is a bug and
	// not a cold cache.
	pull, cancelPull := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancelPull()
	if combined, pullErr := exec.CommandContext(pull, "docker", "pull", dockerE2EImage).
		CombinedOutput(); pullErr != nil {
		t.Skipf("cannot pull %s (no registry access?): %v\n%s", dockerE2EImage, pullErr, combined)
	}

	dataDir := t.TempDir()
	// alpine only has to provide a filesystem: fakemcp is static.
	runAgenthub(t, dataDir, "", "server", "add", "boxed", "--json",
		"--cmd", "/opt/fakemcp",
		"--image", dockerE2EImage,
		"--mount", guest+":/opt/fakemcp:ro")
	enableServer(t, dataDir, "boxed")

	c := startGateway(t, dataDir, "e2e-docker")
	c.initialize()
	// The image is pulled BEFORE the gateway starts (see above), so the
	// generous npx-style budget is not needed here: a host-spawn regression
	// fails immediately, and waiting two minutes to report it only makes the
	// diagnosis slower.
	c.waitForTool("boxed__echo", 60*time.Second)

	res := c.callTool("boxed__echo", map[string]any{"marker": "docker-runtime-e2e"}, 60*time.Second)
	text := c.textContent(res)
	if !strings.Contains(text, "docker-runtime-e2e") {
		c.fatalf("echo result does not contain the marker: %q", text)
	}
	t.Logf("boxed__echo answered from inside a container: %s", text)
	c.close()

	// --rm plus Close must leave nothing behind; a leak here is what
	// `agenthub doctor` reports as a stray container.
	if out := dockerManagedContainers(t); out != "" {
		t.Errorf("managed containers outlived the gateway:\n%s", out)
	}
}

// dockerE2EImage is the container the docker-runtime case runs in. It needs
// nothing but a filesystem, because the downstream is a static binary
// mounted in from the host.
const dockerE2EImage = "alpine:3"

// dockerServerArch returns the DAEMON's architecture (not the host's), which
// is what the downstream binary must be built for.
func dockerServerArch(t *testing.T) (string, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "docker", "version", "--format", "{{.Server.Arch}}").Output()
	if err != nil {
		return "", err
	}
	arch := strings.TrimSpace(string(out))
	if arch == "" {
		return "", fmt.Errorf("empty server arch")
	}
	return arch, nil
}

// dockerManagedContainers lists the containers agenthub labels as its own.
func dockerManagedContainers(t *testing.T) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "docker", "ps", "-a",
		"--filter", "label=agenthub.managed=true",
		"--format", "{{.Names}}\t{{.Status}}").Output()
	if err != nil {
		t.Logf("docker ps: %v", err) // diagnosis only; not the assertion
		return ""
	}
	return strings.TrimSpace(string(out))
}

// TestClientConnectWritesConfig covers the CLI leg end to end with the real
// binary: `client connect --path` writes a .mcp.json whose entry spawns
// this exact binary, and `client disconnect` removes it again.
func TestClientConnectWritesConfig(t *testing.T) {
	dataDir := t.TempDir()
	path := filepath.Join(t.TempDir(), ".mcp.json")

	runAgenthub(t, dataDir, "", "client", "connect", "claude-code", "--path", path)

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	var cfg struct {
		Servers map[string]struct {
			Command string   `json:"command"`
			Args    []string `json:"args"`
		} `json:"mcpServers"`
	}
	if err := json.Unmarshal(data, &cfg); err != nil {
		t.Fatalf("written config: %v\n%s", err, data)
	}
	entry, ok := cfg.Servers["agenthub"]
	if !ok {
		t.Fatalf("no agenthub entry in %s:\n%s", path, data)
	}
	// The command must point back at the very binary that ran the CLI
	// (os.Executable), modulo symlink resolution of the temp dir.
	wantBin, err := filepath.EvalSymlinks(agenthubBin)
	if err != nil {
		t.Fatal(err)
	}
	gotBin, err := filepath.EvalSymlinks(entry.Command)
	if err != nil {
		t.Fatalf("entry command %q: %v", entry.Command, err)
	}
	if gotBin != wantBin {
		t.Errorf("entry command = %q (resolved %q), want %q", entry.Command, gotBin, wantBin)
	}
	wantArgs := []string{"connect", "--client", "claude-code"}
	if len(entry.Args) != len(wantArgs) {
		t.Fatalf("entry args = %v, want %v", entry.Args, wantArgs)
	}
	for i := range wantArgs {
		if entry.Args[i] != wantArgs[i] {
			t.Errorf("entry args = %v, want %v", entry.Args, wantArgs)
		}
	}

	// Disconnect removes the entry from the same file.
	runAgenthub(t, dataDir, "", "client", "disconnect", "claude-code", "--path", path)
	data, err = os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var after struct {
		Servers map[string]json.RawMessage `json:"mcpServers"`
	}
	if err := json.Unmarshal(data, &after); err != nil {
		t.Fatalf("config after disconnect: %v\n%s", err, data)
	}
	if _, ok := after.Servers["agenthub"]; ok {
		t.Errorf("agenthub entry survived disconnect:\n%s", data)
	}
}
