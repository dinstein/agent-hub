package e2e_test

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

// "Isolation a config claims must be delivered or refused: for fields like
// `runtime: docker`, fail closed rather than silently degrade to host
// execution" is one of AGENTS.md's named traps, and the DELIVERY half is the
// only half that had a test. TestDockerRuntimeDownstream proves a container
// runs — and it skips itself on any machine without a working Docker daemon,
// so on most runs nothing about this rule was checked at all.
//
// The refusal half needs no Docker, which is what makes it worth having: it
// is the direction that fails dangerously. A delivery bug is a server that
// does not work, noticed immediately. A refusal bug is a server that works
// perfectly while running on the host, which is exactly what the operator
// wrote `runtime: docker` to prevent, and nothing about the running system
// looks wrong.
//
// Every case here therefore asserts on a MARKER FILE the command touches if
// it is ever executed on the host. The absence of a tool is not evidence —
// a container that failed to start also exposes no tool — so the marker is
// what separates "the isolation was refused" from "the isolation was skipped
// and this happened to break anyway".

// hostMarker writes a command that records having run on the host and then
// becomes the fake server, and returns (commandPath, markerPath).
func hostMarker(t *testing.T) (string, string) {
	t.Helper()
	dir := t.TempDir()
	marker := filepath.Join(dir, "executed-on-the-host")
	cmd := filepath.Join(dir, "server.sh")
	body := "#!/bin/sh\ntouch " + marker + "\nexec " + fakemcpBin + "\n"
	if err := os.WriteFile(cmd, []byte(body), 0o755); err != nil {
		t.Fatalf("write command: %v", err)
	}
	return cmd, marker
}

// assertNeverRanOnHost is the assertion every case in this file turns on.
func assertNeverRanOnHost(t *testing.T, c *gatewayClient, marker, tool string) {
	t.Helper()
	if _, err := os.Stat(marker); err == nil {
		c.fatalf("the entry ran ON THE HOST: isolation it claimed was silently dropped")
	}
	if names := c.listTools(30 * time.Second); slices.Contains(names, tool) {
		c.fatalf("%s is being served: %v", tool, names)
	}
}

// TestADockerEntryThatCannotStartNeverFallsBackToTheHost is the core of the
// rule, and it is deterministic on every machine shape this suite runs on.
//
// The image name cannot exist, so the container path fails everywhere: on a
// runner with Docker the pull fails, and on one without it the CLI itself
// does. What must NOT differ between those is the outcome — a downstream that
// asked to be contained and could not be must not be run uncontained instead.
//
// This is why the case needs no Docker and does not skip. Every other test of
// this feature in the tree does.
func TestADockerEntryThatCannotStartNeverFallsBackToTheHost(t *testing.T) {
	dataDir := t.TempDir()
	cmd, marker := hostMarker(t)

	runAgenthub(t, dataDir, "", "server", "add", "boxed", "--cmd", cmd,
		"--image", "agenthub-e2e-no-such-image:0", "--json")
	enableServer(t, dataDir, "boxed")
	runAgenthub(t, dataDir, "", "server", "add", "plain", "--cmd", fakemcpBin, "--json")
	enableServer(t, dataDir, "plain")

	c := startGateway(t, dataDir, "runtimeclient")
	c.initialize()
	// The host server connecting is what puts the gateway demonstrably past
	// its dialling phase, so the boxed one's absence is a decision.
	c.waitForTool("plain__echo", 30*time.Second)

	// It TRIED the container path — without this the marker's absence would
	// also be satisfied by an entry nothing ever attempted.
	waitStderr(t, c, 30*time.Second, "server=boxed", "runtime=docker")
	assertNeverRanOnHost(t, c, marker, "boxed__echo")
	c.close()
}

// TestAMistypedRuntimeIsRefusedRatherThanTreatedAsHost covers the failure
// direction ValidateRuntime's own comment names: "a typo like \"dcoker\" must
// not quietly drop the isolation the operator asked for".
//
// It is written straight into registry/servers.json because the CLI refuses
// it — see the case below — and a rule enforced only at the CLI is not
// enforced at all for a file that arrives by hand edit, migration or a synced
// dotfile. An unknown runtime read at load time must disable that entry, not
// fall through to the default.
func TestAMistypedRuntimeIsRefusedRatherThanTreatedAsHost(t *testing.T) {
	dataDir := t.TempDir()
	cmd, marker := hostMarker(t)

	runAgenthub(t, dataDir, "", "server", "add", "plain", "--cmd", fakemcpBin, "--json")
	enableServer(t, dataDir, "plain")
	writeServersJSON(t, dataDir, map[string]any{
		"servers": map[string]any{
			"plain": map[string]any{
				"enabled": true, "source": "manual", "transport": "stdio", "command": fakemcpBin,
			},
			"typo": map[string]any{
				"enabled": true, "source": "manual", "transport": "stdio",
				"command": cmd, "runtime": "dcoker",
			},
		},
	})

	c := startGateway(t, dataDir, "typoclient")
	c.initialize()
	c.waitForTool("plain__echo", 30*time.Second)

	// The refusal is reported per entry and names the runtime it could not
	// honour: a silent skip would present as a server that simply never
	// appears, which is the same symptom as a typo in its id.
	waitStderr(t, c, 30*time.Second, "server=typo", "unknown runtime")
	assertNeverRanOnHost(t, c, marker, "typo__echo")

	// And the bystander is untouched — one unusable entry disables that entry
	// and nothing else, or a single bad line becomes an outage.
	if got := c.textContent(c.callTool("plain__echo",
		map[string]any{"marker": "unaffected"}, 30*time.Second)); !strings.Contains(got, "unaffected") {
		c.fatalf("the unusable entry took its neighbour down with it: %q", got)
	}
	c.close()
}

// TestARuntimeClaimTheCLICannotHonourIsRefusedAtAddTime is the early half of
// the same rule: the operator finds out while they can still fix it.
//
// All three refusals are E_USAGE rather than a stored entry, which is the
// point — an entry that reached the registry would be one the loader has to
// refuse later, and the operator would learn about it from a server that
// never connects instead of from the command that created it.
func TestARuntimeClaimTheCLICannotHonourIsRefusedAtAddTime(t *testing.T) {
	for _, tc := range []struct {
		name string
		args []string
		want string
	}{
		{
			// The typo, refused where it is cheapest to fix.
			name: "unknown runtime",
			args: []string{"--cmd", "/bin/true", "--runtime", "dcoker"},
			want: "unknown runtime",
		},
		{
			// Isolation with nothing to isolate into.
			name: "docker without an image",
			args: []string{"--cmd", "/bin/true", "--runtime", "docker"},
			want: "needs a docker image",
		},
		{
			// A container block on a transport that spawns no process: the
			// isolation would be silently ignored, which is the same failure
			// wearing the opposite shape.
			name: "container flags on an http transport",
			args: []string{"--url", "https://example.invalid/mcp", "--transport", "http",
				"--image", "alpine:3"},
			want: "stdio transport only",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dataDir := t.TempDir()
			args := append([]string{"server", "add", "target"}, tc.args...)
			code, out := runAgenthubExit(t, dataDir, "", append(args, "--json")...)
			if code == 0 {
				t.Fatalf("the entry was accepted: %s", out)
			}
			if !strings.Contains(out, tc.want) {
				t.Errorf("the refusal does not explain itself (want %q): %s", tc.want, out)
			}
			// Nothing was stored: a refusal that half-applied would leave the
			// loader to refuse it again on every start.
			ls, _ := runAgenthub(t, dataDir, "", "server", "ls", "--json")
			if strings.Contains(ls, "target") {
				t.Errorf("the refused entry was stored anyway: %s", ls)
			}
		})
	}
}

// TestAHostEntryIsRecordedAsHostInTheLog pins the one line that says which of
// the two runtimes a connection actually got — the host half; the docker half
// is asserted by the first case, which waits on `runtime=docker`.
//
// internal/downstream's comment calls `runtime` the load-bearing field for
// exactly this reason: it is the only place recording whether isolation was
// delivered. An operator has no other way to tell a contained downstream from
// a host one after the fact, and "it is configured for docker" is a statement
// about the file, not about the process that is running.
//
// Both values being pinned is what makes the field a discriminator. A log
// that said `runtime=docker` unconditionally would satisfy the first case and
// tell an operator nothing.
func TestAHostEntryIsRecordedAsHostInTheLog(t *testing.T) {
	dataDir := t.TempDir()
	runAgenthub(t, dataDir, "", "server", "add", "hostly", "--cmd", fakemcpBin, "--json")
	enableServer(t, dataDir, "hostly")

	c := startGateway(t, dataDir, "runtimelogclient")
	c.initialize()
	c.waitForTool("hostly__echo", 30*time.Second)
	waitStderr(t, c, 30*time.Second, "server=hostly", "runtime=host")
	c.close()
}
