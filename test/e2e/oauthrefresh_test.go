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
// This file covers what that machinery does for a downstream on THIS machine,
// and the answer is that it does not run. It is a deliberate answer, and
// finding it is what this file is for.
//
// `agenthub auth login --allow-local` signs in to a loopback provider, and
// `server add --local` records that judgement permanently as
// `provenance=local` — which the login paths honour (internal/cli/auth.go
// and internal/ctlapi/nonreglogin.go both derive AllowLoopback from it). The
// refresh paths have no equivalent. internal/gateway/auth.go builds
// `oauthflow.NewClient(oauthflow.Config{})` with AllowLoopback unset and the
// factory never sees the entry, and the daemon's OAuthAllowLoopback is a test
// seam nothing in production sets. So the oauthflow screen refuses the
// renewal before it makes a request, and it refuses it for both renewers.
//
// The consequence is user-visible and worth a test rather than a comment: a
// self-hosted OAuth MCP server works after login and stops when its token
// expires, permanently, with a WARN in a log nobody is reading. See
// docs/modules/oauth.md, "Known gaps".
//
// It also bounds this suite. No e2e can demonstrate a SUCCESSFUL refresh,
// because the only authorization server a test may run is on loopback and
// TLS does not help — the carve-out is gated on AllowLoopback whatever the
// scheme. What is testable is the refusal, and the refusal is a fail-closed
// security property that deserves to be enforced rather than incidental.

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

// TestALoopbackProvidersCredentialIsNeverRenewed pins the refusal, at the
// altitude where a user meets it.
//
// The shape is a revocation rather than an expiry, because it is faster and
// because it reaches the same code: the passive renewer fires on the
// downstream's 401 (trigger=rejection) with no waiting, while the proactive
// one would not look again for RefreshRetryBackoff after storing a
// short-lived token — `scheduleFrom` floors the next look at now+15s so that
// a provider issuing tokens shorter than the grace cannot turn every request
// into a renewal. Both end at the same screen.
//
// The assertion that carries it is that the provider's token endpoint is
// never reached AT ALL. oauthflow screens the destination before making a
// request, so "refused" here means the hub declined to talk to the
// authorization server, not that it tried and was turned away — a
// distinction the counter can see and a log line could not.
func TestALoopbackProvidersCredentialIsNeverRenewed(t *testing.T) {
	dataDir := t.TempDir()
	env := vaultEnv(dataDir)
	p := newOAuthProvider(t)

	loginTo(t, env, p, "selfhosted")

	// The login itself DID reach the token endpoint: --allow-local and
	// provenance=local are honoured there. That is what makes the absence of
	// a second one below a statement about the refresh path specifically, and
	// not about a provider this hub could never talk to.
	grantsAfterLogin, _, first := p.renewals()

	c := startGatewayEnv(t, env, "selfhostedclient")
	c.initialize()
	c.waitForTool("selfhosted__echo", 45*time.Second)
	if got := c.textContent(c.callTool("selfhosted__echo",
		map[string]any{"marker": "while-valid"}, 45*time.Second)); !strings.Contains(got, "while-valid") {
		c.fatalf("the stored credential did not work: %q", got)
	}

	// Revoke out from under the client, the way a provider ending a session
	// does. Nothing in the stored state says this happened.
	p.mu.Lock()
	p.granted = "rotated-by-the-provider"
	p.mu.Unlock()

	// The call now fails, and it stays failed: the 401 reaches the passive
	// renewer, which is refused before it can ask for a new token.
	rpcErr := c.callToolRefused("selfhosted__echo",
		map[string]any{"marker": "after-revoke"}, 45*time.Second)
	if !strings.Contains(rpcErr.Message, "401") {
		c.fatalf("the revoked credential failed for some other reason: %v", rpcErr)
	}

	refreshes, _, accepted := p.renewals()
	if refreshes != grantsAfterLogin {
		t.Errorf("the refresh reached the provider's token endpoint (%d grants, was %d); "+
			"the loopback screen is supposed to refuse before making a request",
			refreshes, grantsAfterLogin)
	}
	if accepted == first {
		t.Fatal("the fixture never revoked anything")
	}
	c.close()
}
