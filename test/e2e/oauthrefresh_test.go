package e2e_test

import (
	"strings"
	"testing"
	"time"
)

// The OAuth suite covered getting a credential and not keeping one:
// oauthlogin_test.go issues an hour-long token, so nothing in it reaches the
// renewal machinery. Renewal is where the failure a user actually meets
// lives — a login is verified the minute it is performed, while a refresh
// that does not work presents days later as a server that worked yesterday.
//
// This file covers renewal for a downstream on THIS machine, and until
// recently the answer was that it did not happen at all. `server add --local`
// records the operator's judgement as `provenance=local`, and the login paths
// honoured it while the refresh paths had no equivalent: the gateway built
// `oauthflow.NewClient(oauthflow.Config{})` with AllowLoopback unset, and the
// daemon's OAuthAllowLoopback was a test seam nothing in production set. The
// SSRF screen then refused the renewal before making a request — for both
// renewers — so a self-hosted OAuth MCP server worked after login and stopped
// when its token expired, permanently, with a WARN in a log nobody reads.
//
// The carve-out is now decided per server from that same declaration
// (oauthflow.CoordinatorConfig.AllowLoopback), which is what makes this file
// possible at all: the only authorization server a test may run is on
// loopback, and TLS does not help, because the screen is about the address
// rather than the scheme. The refusal for everything else is unchanged and is
// pinned where it can be stated precisely, next to the code —
// internal/oauthflow's TestCoordinatorRefusesALoopbackTokenEndpoint.

// loginTo runs the shared setup: register the provider's MCP endpoint, sign
// in through the device grant, and put the server into service.
func loginTo(t *testing.T, env []string, p *oauthProvider, serverID string) {
	t.Helper()
	runAgenthubEnv(t, env, "", "server", "add", serverID,
		"--url", p.mcpURL(), "--transport", "http", "--local", "--json")
	out, stderr := runAgenthubEnv(t, env, "", "auth", "login", serverID,
		"--device", "--allow-local", "--json")
	if e := lastEnvelope(t, out); !e.OK {
		t.Fatalf("auth login failed: %s\nstderr: %s", out, stderr)
	}
	runAgenthubEnv(t, env, "", "server", "enable", serverID, "--no-probe")
	runAgenthubEnv(t, env, "", "config", "set", "discovery", "full")
}

// TestALocalProvidersCredentialIsRenewedOnRejection is the passive renewal
// path end to end, on the only kind of provider a test can run.
//
// The shape is a rotation rather than a wait, because it is faster and
// reaches the same code: the passive renewer fires on the downstream's 401
// (trigger=rejection) immediately, while the proactive one would not look
// again for RefreshRetryBackoff after storing a short-lived token —
// `scheduleFrom` floors the next look at now+15s so a provider issuing tokens
// shorter than the grace cannot turn every request into a renewal.
//
// What carries it is that the SECOND call succeeds. The fixture rotates the
// token it accepts on every refresh grant, so the bearer that worked a moment
// ago is dead and only a credential obtained after the rejection can work —
// no test has to reach into the vault to check, and no log line has to be
// trusted.
func TestALocalProvidersCredentialIsRenewedOnRejection(t *testing.T) {
	dataDir := t.TempDir()
	env := vaultEnv(dataDir)
	p := newOAuthProvider(t)

	loginTo(t, env, p, "selfhosted")
	grantsAfterLogin, _, first := p.renewals()

	c := startGatewayEnv(t, env, "selfhostedclient")
	defer c.close()
	c.initialize()
	c.waitForTool("selfhosted__echo", 45*time.Second)
	if got := c.textContent(c.callTool("selfhosted__echo",
		map[string]any{"marker": "while-valid"}, 45*time.Second)); !strings.Contains(got, "while-valid") {
		c.fatalf("the stored credential did not work: %q", got)
	}

	// Rotate out from under the client, the way a provider ending a session
	// does. Nothing in the stored state says this happened.
	p.mu.Lock()
	p.granted = "rotated-by-the-provider"
	p.mu.Unlock()

	if got := c.textContent(c.callTool("selfhosted__echo",
		map[string]any{"marker": "after-rotation"}, 45*time.Second)); !strings.Contains(got, "after-rotation") {
		c.fatalf("the call was not recovered by a renewal: %q", got)
	}

	refreshes, _, accepted := p.renewals()
	if refreshes != grantsAfterLogin+1 {
		t.Errorf("refresh grants = %d, want exactly one more than the %d the login left; "+
			"the passive renewer must ask once, not per request", refreshes, grantsAfterLogin)
	}
	if accepted == first || accepted == "rotated-by-the-provider" {
		t.Errorf("the provider still accepts %q; the call cannot have carried a renewed credential", accepted)
	}
}
