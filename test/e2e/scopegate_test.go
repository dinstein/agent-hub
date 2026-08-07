package e2e_test

import (
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Every scope case in this suite until now asserted that a withheld tool is
// absent from tools/list. None of them called it.
//
// That is the cheaper half of the claim, and it is not the half that
// protects anything. The gateway connects EVERY enabled downstream whatever
// the profile says (internal/gateway/hotreload.go, specsFromSnapshot), and a
// name the scope hides therefore stays in the router's catalog and stays
// routable: internal/gateway/upstream.go dispatches it into the pipeline on
// purpose, with the comment "the scope gate is the enforcement point and
// owns that rejection". So the process serving a hidden tool is running, its
// tools are aggregated, and the only thing between the client and them is
// one gate — which no test in this file's absence ever fired.
//
// Delete that gate and the suite stayed green: listing is a projection
// (discovery.Visible) and would go on hiding the name while every client
// that guessed it got a result. These cases are the other half.
//
// Both run the same shape, and the second half of each is what makes the
// first mean anything: refuse, then WIDEN and call successfully. A refusal
// alone is also what a broken fixture, a dead downstream or a typo'd tool
// name produces.

// wantScopeDenied asserts a refusal is the scope gate's, by its stable code.
//
// The code is written out rather than imported from internal/pipeline, for
// the reason lazyMetaTools is: this suite drives the gateway from the
// OUTSIDE, and E_SCOPE_DENIED is ABI the moment it ships
// (internal/pipeline/gates.go, "They are ABI once emitted; do not rename").
// An external client tells a refusal apart from a failure by reading it, so
// a rename must fail here — importing the constant would rename the
// assertion along with the code and prove nothing.
//
// The JSON-RPC code is checked too, and it is the more consequential half:
// -32600 (Invalid Request) is terminal, and an agent that retries a refusal
// forever is the behaviour that number exists to prevent. Anything in the
// retryable band would be a silent regression the message text cannot show.
func wantScopeDenied(t *testing.T, c *gatewayClient, what string, err *rpcError) {
	t.Helper()
	const codeScopeDenied = "E_SCOPE_DENIED"
	if !strings.Contains(err.Message, codeScopeDenied) {
		c.fatalf("%s was refused, but not by the scope gate: %v", what, err)
	}
	if err.Code != -32600 {
		c.fatalf("%s refusal carries JSON-RPC code %d, want -32600 (terminal): %v",
			what, err.Code, err)
	}
}

// TestAProfileHiddenServerRefusesTheCallAndNotOnlyTheListing covers the
// coarser altitude: a profile that names one of two enabled servers.
//
// beta is enabled, connected and serving — the profile only decides that
// this client may not see it. Guessing its exposed name is trivial (the
// routing prefix is the server id), so "it is not in the list" is not a
// boundary; the gate is.
func TestAProfileHiddenServerRefusesTheCallAndNotOnlyTheListing(t *testing.T) {
	dataDir := t.TempDir()
	twoServerProfile(t, dataDir, "narrow", "gateclient")

	c := startGateway(t, dataDir, "gateclient")
	c.initialize()
	c.waitForTool("alpha__echo", 30*time.Second)
	if names := c.listTools(30 * time.Second); hasTool(names, "beta__echo") {
		c.fatalf("a profile listing only alpha exposed beta: %v", names)
	}

	// The name the client was never shown, called anyway.
	err := c.callToolRefused("beta__echo", map[string]any{"marker": "should-not-run"}, 30*time.Second)
	wantScopeDenied(t, c, "beta__echo under a profile that hides beta", err)

	// The refusal was the gate and not a broken beta: widen the profile and
	// the very same call answers.
	runProfile(t, dataDir, "server", "add", "narrow", "beta")
	c.waitForTool("beta__echo", 30*time.Second)
	if got := c.textContent(c.callTool("beta__echo", map[string]any{"marker": "now-allowed"},
		30*time.Second)); !strings.Contains(got, "now-allowed") {
		c.fatalf("beta__echo did not answer once the profile allowed it: %q", got)
	}
	c.close()
}

// TestAServerToolAllowListRefusesTheCallItWithholds covers the finer
// altitude, and the one that fails worse: `server tool allow` is the
// MACHINE's offer, so a hole in it is open to every client at once rather
// than to one profile's.
//
// The withheld tool is a sibling of an allowed one on the same connected
// downstream, which is what leaves nothing else to explain a refusal: the
// server is up, its process is answering, and the two names differ only in
// what an operator wrote down.
func TestAServerToolAllowListRefusesTheCallItWithholds(t *testing.T) {
	dataDir := t.TempDir()

	script := filepath.Join(t.TempDir(), "two.json")
	writeScript(t, script, "read_thing", "write_thing")
	runAgenthub(t, dataDir, "", "server", "add", "alpha", "--cmd", fakemcpBin, "--args", script, "--json")
	enableServer(t, dataDir, "alpha")
	runAgenthub(t, dataDir, "", "server", "tool", "allow", "alpha", "--only", "read_thing")

	c := startGateway(t, dataDir, "allowclient")
	c.initialize()
	c.waitForTool("alpha__read_thing", 30*time.Second)
	if names := c.listTools(30 * time.Second); hasTool(names, "alpha__write_thing") {
		c.fatalf("a server allow list of read_thing exposed write_thing: %v", names)
	}

	// The allowed sibling answers, so the downstream is demonstrably serving
	// both — whatever happens to the other name is a decision, not an outage.
	if got := c.textContent(c.callTool("alpha__read_thing", map[string]any{"marker": "allowed"},
		30*time.Second)); !strings.Contains(got, "allowed") {
		c.fatalf("alpha__read_thing did not echo: %q", got)
	}

	err := c.callToolRefused("alpha__write_thing", map[string]any{"marker": "should-not-run"}, 30*time.Second)
	wantScopeDenied(t, c, "alpha__write_thing under a server allow list that omits it", err)

	// And back: dropping the rule makes the same call work, which is what
	// proves write_thing was a real tool the whole time.
	runAgenthub(t, dataDir, "", "server", "tool", "allow", "alpha", "--all")
	c.waitForTool("alpha__write_thing", 30*time.Second)
	if got := c.textContent(c.callTool("alpha__write_thing", map[string]any{"marker": "now-offered"},
		30*time.Second)); !strings.Contains(got, "now-offered") {
		c.fatalf("alpha__write_thing did not answer once the server offered it: %q", got)
	}
	c.close()
}
