package e2e_test

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

// `--env` is how a stdio downstream is configured and, for most real MCP
// servers, how it is given its API key — and it had no end-to-end coverage at
// all. vault_test.go covers `${SECRET_X}` resolution, but through `--header`,
// which is the HTTP downstream's channel: a different substitution site
// reaching a different transport. The stdio side, which is the majority of
// what people install, was never driven.
//
// Nothing that reads a tool result can see a child's environment, so these
// cases spawn a wrapper script that dumps `printenv` to a file and then execs
// the fake server. The file is the observation, and it is the child's own
// account rather than the registry's — which is the whole point, since what
// is stored and what is delivered are exactly the two things that can
// disagree.
//
// The wrapper is a script FILE, not `sh -c`: spawnguard blocks inline eval,
// and rightly, so a fixture built that way would be testing the guard instead
// of the env block. That guard gets its own case below, where it belongs.

// envDumper writes a wrapper that records its environment and then becomes
// the fake MCP server, and returns (wrapperPath, dumpPath).
func envDumper(t *testing.T) (string, string) {
	t.Helper()
	dir := t.TempDir()
	dump := filepath.Join(dir, "child-env.txt")
	wrapper := filepath.Join(dir, "wrapper.sh")
	body := "#!/bin/sh\nprintenv > " + dump + "\nexec " + fakemcpBin + "\n"
	if err := os.WriteFile(wrapper, []byte(body), 0o755); err != nil {
		t.Fatalf("write wrapper: %v", err)
	}
	return wrapper, dump
}

// childEnv reads the dumped environment, waiting for the file: the wrapper
// writes it as the process starts, which races the tool call that proves the
// server is up.
func childEnv(t *testing.T, dump string, budget time.Duration) string {
	t.Helper()
	deadline := time.Now().Add(budget)
	for {
		data, err := os.ReadFile(dump)
		if err == nil && len(data) > 0 {
			return string(data)
		}
		if !time.Now().Before(deadline) {
			t.Fatalf("the downstream never recorded its environment at %s (err %v)", dump, err)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// waitStderr polls the gateway's stderr until one line carries every one of
// want. It is how a case waits for a failure the gateway reports and nothing
// else surfaces — a downstream that refused to start withdraws no tool,
// because it never had one to withdraw.
func waitStderr(t *testing.T, c *gatewayClient, budget time.Duration, want ...string) {
	t.Helper()
	deadline := time.Now().Add(budget)
	for time.Now().Before(deadline) {
		for _, line := range strings.Split(c.stderrTail(), "\n") {
			hits := 0
			for _, w := range want {
				if strings.Contains(line, w) {
					hits++
				}
			}
			if hits == len(want) {
				return
			}
		}
		time.Sleep(200 * time.Millisecond)
	}
	c.fatalf("no gateway log line carried all of %v within %s", want, budget)
}

// TestAnEnvBlockReachesTheDownstreamProcess is the plain case: what the entry
// says the child's environment is, the child's environment is.
func TestAnEnvBlockReachesTheDownstreamProcess(t *testing.T) {
	dataDir := t.TempDir()
	wrapper, dump := envDumper(t)

	runAgenthub(t, dataDir, "", "server", "add", "configured", "--cmd", wrapper,
		"--env", "AGENTHUB_E2E_PLAIN=plain-value",
		"--env", "AGENTHUB_E2E_SECOND=second-value", "--json")
	enableServer(t, dataDir, "configured")

	c := startGateway(t, dataDir, "envclient")
	c.initialize()
	c.waitForTool("configured__echo", 30*time.Second)

	got := childEnv(t, dump, 15*time.Second)
	for _, want := range []string{
		"AGENTHUB_E2E_PLAIN=plain-value",
		"AGENTHUB_E2E_SECOND=second-value",
	} {
		if !strings.Contains(got, want) {
			c.fatalf("the child's environment does not carry %q", want)
		}
	}
	c.close()
}

// TestASecretPlaceholderInEnvIsResolvedBeforeTheSpawn is the case that
// matters most, because it is how an MCP server is given an API key.
//
// Two assertions, and the second is the one a passing substitution cannot
// give you: the child must see the VALUE, and it must never see the literal
// `${SECRET_...}`. A resolver that failed open would deliver the placeholder
// verbatim, the downstream would treat it as its key, and the failure would
// surface as the downstream rejecting a credential nobody could find — which
// looks like a provider problem, not a hub one.
func TestASecretPlaceholderInEnvIsResolvedBeforeTheSpawn(t *testing.T) {
	dataDir := t.TempDir()
	env := vaultEnv(dataDir)
	wrapper, dump := envDumper(t)

	const value = "s3cr3t-api-key-from-the-vault"
	runAgenthubEnv(t, env, value+"\n", "secret", "set", "keyed", "API_KEY", "--stdin")
	out, _ := runAgenthubEnv(t, env, "", "server", "add", "keyed", "--cmd", wrapper,
		"--env", "SERVICE_API_KEY=${SECRET_API_KEY}", "--json")
	// The registry keeps the placeholder, not the value: `server add` echoing
	// the resolved secret back would put it in a shell history and a
	// screenshot before it ever reached a process.
	if strings.Contains(out, value) {
		t.Fatalf("server add printed the secret value: %s", out)
	}
	runAgenthubEnv(t, env, "", "server", "enable", "keyed", "--no-probe")
	runAgenthubEnv(t, env, "", "config", "set", "discovery", "full")

	c := startGatewayEnv(t, env, "secretenvclient")
	c.initialize()
	c.waitForTool("keyed__echo", 30*time.Second)

	got := childEnv(t, dump, 15*time.Second)
	if !strings.Contains(got, "SERVICE_API_KEY="+value) {
		c.fatalf("the child did not receive the resolved secret")
	}
	if strings.Contains(got, "${SECRET_") {
		c.fatalf("the child received an UNRESOLVED placeholder: the resolver failed open")
	}

	// And the stored entry still holds the placeholder rather than the value:
	// the vault is the only place the secret lives.
	ls, _ := runAgenthubEnv(t, env, "", "server", "ls", "--json")
	if strings.Contains(ls, value) {
		t.Errorf("`server ls` printed the secret value: %s", ls)
	}
	c.close()
}

// TestAnEnvSecretWithoutItsKeyFailsClosed is vault_test.go's fail-closed rule
// on the stdio channel: a placeholder that cannot be resolved must stop the
// spawn, not travel.
//
// The observation is the absence of the dump file. If the resolver had failed
// open the child would have started — and started holding the literal
// `${SECRET_API_KEY}` as its credential, which is the one outcome worse than
// not starting at all.
func TestAnEnvSecretWithoutItsKeyFailsClosed(t *testing.T) {
	dataDir := t.TempDir()
	env := vaultEnv(dataDir)
	wrapper, dump := envDumper(t)

	runAgenthubEnv(t, env, "value-that-will-be-unreachable\n",
		"secret", "set", "keyed", "API_KEY", "--stdin")
	runAgenthubEnv(t, env, "", "server", "add", "keyed", "--cmd", wrapper,
		"--env", "SERVICE_API_KEY=${SECRET_API_KEY}", "--json")
	runAgenthubEnv(t, env, "", "server", "enable", "keyed", "--no-probe")
	runAgenthubEnv(t, env, "", "config", "set", "discovery", "full")

	// The same data directory WITHOUT the passphrase: the vault is there and
	// cannot be opened, which is what a machine looks like after the key is
	// rotated or lost.
	c := startGatewayEnv(t, testEnv(dataDir), "nokeyclient")
	c.initialize()

	// Waited on POSITIVE evidence that the gateway tried and failed, rather
	// than on a window in which nothing happened: "no tools after N seconds"
	// is also what a gateway that never got as far as dialling looks like,
	// and that would make this pass for the wrong reason.
	waitStderr(t, c, 30*time.Second, "keyed", "connect failed")

	if _, err := os.Stat(dump); err == nil {
		c.fatalf("the child was SPAWNED without a resolvable secret; " +
			"an unresolved placeholder must stop the spawn, not travel with it")
	}
	if names := c.listTools(15 * time.Second); slices.Contains(names, "keyed__echo") {
		c.fatalf("the downstream came up without the key its env needed: %v", names)
	}
	c.close()
}

// TestSpawnguardRefusesAnEnvThatRedirectsWhatTheChildLoads covers the layer
// AGENTS.md puts outside the permission model — "netguard / spawnguard refuse
// destinations and processes regardless of who asked" — which had no
// end-to-end case at all.
//
// The shape is env smuggling: an innocuous-looking server entry whose env
// block makes the spawned process load something else. Note where it is
// caught. `server add` ACCEPTS it, deliberately — the registry records what
// the operator wrote — and the SPAWN is what refuses, because the guard vets
// the final command line after secret expansion and after any docker
// rewriting. A test that asserted at add time would be asserting the wrong
// half and would pass while the guard was unwired.
//
// The sibling is the other half of the rule: a rejected entry disables that
// entry and nothing else. A guard that took the whole gateway down with it
// would turn one bad line of configuration into an outage.
func TestSpawnguardRefusesAnEnvThatRedirectsWhatTheChildLoads(t *testing.T) {
	dataDir := t.TempDir()

	code, out := runAgenthubExit(t, dataDir, "", "server", "add", "smuggler",
		"--cmd", fakemcpBin, "--env", "LD_PRELOAD=/tmp/evil.so", "--json")
	if code != 0 {
		t.Fatalf("`server add` refused the entry; this rule is enforced at SPAWN, "+
			"and a test asserting here would pass with the guard unwired: %s", out)
	}
	enableServer(t, dataDir, "smuggler")
	runAgenthub(t, dataDir, "", "server", "add", "bystander", "--cmd", fakemcpBin, "--json")
	enableServer(t, dataDir, "bystander")

	c := startGateway(t, dataDir, "spawnguardclient")
	c.initialize()
	// The bystander connects, which is what puts the gateway demonstrably
	// past its dialling phase: after this, the smuggler's absence is a
	// refusal rather than a race.
	c.waitForTool("bystander__echo", 30*time.Second)
	if names := c.listTools(30 * time.Second); slices.Contains(names, "smuggler__echo") {
		c.fatalf("a downstream whose env sets LD_PRELOAD was spawned: %v", names)
	}

	// The refusal is legible, and names both the variable and the fix. A
	// guard that blocked silently would present as a server that simply never
	// connects, which is the hardest failure to diagnose from outside.
	stderr := c.stderrTail()
	for _, want := range []string{"spawnguard", "env_smuggling", "LD_PRELOAD"} {
		if !strings.Contains(stderr, want) {
			c.fatalf("the refusal does not mention %q:\n%s", want, stderr)
		}
	}
	c.close()
}
