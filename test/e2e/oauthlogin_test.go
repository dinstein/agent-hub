package e2e_test

import (
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/dinstein/agent-hub/internal/testutil/fakeas"
	"github.com/dinstein/agent-hub/internal/testutil/fakemcp"
)

// `agenthub auth login` is the command a user reaches for the moment `server
// test` reports that a downstream will not answer until it is authorized, and
// it had no end-to-end coverage at all. internal/oauthflow is tested against a
// scriptable fake authorization server in nineteen files, but all of it runs
// in one process against an in-package fixture: what none of it can show is
// that the CLI wires the flow up, that the token it obtains is written where a
// LATER process looks for it, and that the connection a client uses picks it
// up without being told.
//
// The flow driven here is the device grant (RFC 8628), for one practical
// reason: it is the only interaction mode that finishes without a browser or a
// human pasting a URL, so it is the only one this suite can run. It is also
// the mode agenthub selects on its own whenever an authorization server
// advertises device_authorization_endpoint, which makes it a real path rather
// than a convenient one — but the test asks for it explicitly, because a test
// that let mode selection decide could open a browser on a developer's machine
// the day the selection rule changes.

// newOAuthProvider starts the shared fake provider (internal/testutil/fakeas)
// with this suite's MCP fixture behind it. The provider used to be a second,
// weaker copy living here, so the suite that tests what a USER meets could not
// reach the failures the protocol tests could; opts lets a caller turn the
// knobs it needs.
func newOAuthProvider(t *testing.T, opts ...func(*fakeas.Options)) *fakeas.Server {
	t.Helper()
	mcp := &fakeHTTPServer{script: fakemcp.Minimal()}
	o := fakeas.Options{MCP: http.HandlerFunc(mcp.handle)}
	for _, f := range opts {
		f(&o)
	}
	return fakeas.New(t, o)
}

// TestAuthLoginStoresATokenLaterConnectionsUse walks the whole authorization
// story in the order a user meets it: a server that refuses, a login that
// obtains a credential, and a client that never learns any of it happened.
//
// The last step is the one worth the fixture. `auth login` succeeding proves
// the flow ran; it says nothing about whether what it stored is what a
// connection reads. Those are different packages, keyed by a composite this
// test never spells out, and the failure mode — a login that reports success
// while every later connect keeps getting a 401 — is indistinguishable from a
// provider problem when it happens on a real machine.
func TestAuthLoginStoresATokenLaterConnectionsUse(t *testing.T) {
	dataDir := t.TempDir()
	// The vault is pinned to the encrypted file for the reason vault_test.go
	// gives: an OAuth token is a credential like any other, and without this
	// the store would reach for the developer's real OS keychain.
	env := vaultEnv(dataDir)
	p := newOAuthProvider(t)

	runAgenthubEnv(t, env, "", "server", "add", "guarded",
		"--url", p.MCPURL(), "--transport", "http", "--local", "--json")

	// Before the login the server does not work, and that is how a user finds
	// out they need one. Asserting it here also means the success later cannot
	// come from a provider that would have answered anyway.
	code, out := runAgenthubExitEnv(t, env, "", "server", "test", "guarded", "--json")
	if code == 0 {
		t.Fatalf("the guarded server answered before any login: %s", out)
	}

	// --allow-local because the authorization server is on this machine;
	// --device because it is the one mode that completes without a human.
	out, stderr := runAgenthubEnv(t, env, "", "auth", "login", "guarded",
		"--device", "--allow-local", "--json")
	if e := lastEnvelope(t, out); !e.OK {
		t.Fatalf("auth login failed: %s\nstderr: %s", out, stderr)
	}

	observed := p.Counts()
	if observed.Challenges == 0 {
		t.Fatal("the login never saw a 401; it cannot have started from a real challenge")
	}
	if observed.Registrations == 0 {
		t.Fatal("no dynamic client registration reached the provider")
	}
	if observed.DeviceGrants == 0 || observed.LastGrant != "urn:ietf:params:oauth:grant-type:device_code" {
		t.Fatalf("the token was not obtained through the device grant (%d grants, last %q)",
			observed.DeviceGrants, observed.LastGrant)
	}

	// The credential is never printed, by any of these commands.
	if strings.Contains(out+stderr, observed.Accepted) {
		t.Fatal("auth login printed the access token")
	}
	out, _ = runAgenthubEnv(t, env, "", "auth", "status", "--json")
	if !strings.Contains(out, "guarded") {
		t.Fatalf("auth status does not know about the server just signed in to: %s", out)
	}
	if strings.Contains(out, observed.Accepted) {
		t.Fatalf("auth status printed the access token: %s", out)
	}

	// And the payoff: a separate process connects and is served, having been
	// told nothing except the server's name.
	if out, _ = runAgenthubEnv(t, env, "", "server", "test", "guarded", "--json"); !lastEnvelope(t, out).OK {
		t.Fatalf("the stored sign-in did not reach the connection: %s", out)
	}

	// A spawned gateway is a further process again, and the one a real client
	// talks to. It reads the same store from its own assembly.
	runAgenthubEnv(t, env, "", "server", "enable", "guarded", "--no-probe")
	runAgenthubEnv(t, env, "", "config", "set", "discovery", "full")
	c := startGatewayEnv(t, env, "authclient")
	c.initialize()
	c.waitForTool("guarded__echo", 45*time.Second)
	if got := c.textContent(c.callTool("guarded__echo", map[string]any{"marker": "after-login"}, 45*time.Second)); !strings.Contains(got, "after-login") {
		t.Fatalf("the gateway could not use the stored sign-in: %q", got)
	}
	c.close()

	// Signing out takes it away again, in the same direction `server disable`
	// does: a logout that left a working connection behind would be the one
	// outcome a user performing it is trying to prevent.
	runAgenthubEnv(t, env, "", "auth", "logout", "guarded")
	if code, out = runAgenthubExitEnv(t, env, "", "server", "test", "guarded", "--json"); code == 0 {
		t.Fatalf("the server still answers after logout: %s", out)
	}
}
