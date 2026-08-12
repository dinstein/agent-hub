package e2e_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/dinstein/agent-hub/internal/testutil/fakeas"
)

// What happens to a sign-in AFTER it is performed, end to end.
//
// oauthlogin_test.go covers obtaining a credential and oauthrefresh_test.go
// covers renewing one on rejection. This file covers the two outcomes that
// take days to appear on a real machine and are therefore the ones nobody
// finds until a user does: the provider ending the session for good, and the
// daemon renewing a credential nobody is watching.
//
// Every assertion here is a COUNT of what the provider was asked, because
// every property worth pinning is an absence. "Stopped asking" and "backed
// off for a day" produce the same log line and the same command output; only
// the number of requests separates them. Same for a credential with no expiry
// that must never be renewed, and for a provider that never answers 401.
//
// Not covered here, and deliberately: the loopback interaction mode. It opens
// a real browser through the platform handler, with no seam the CLI exposes,
// so an e2e for it would open a tab on whoever ran the suite. Its own
// coverage is internal/oauthflow's loopback tests, which inject a browser.

// authStatusRows reads `auth status --json` for one server.
func authStatusRow(t *testing.T, env []string, serverID string) map[string]any {
	t.Helper()
	out, _ := runAgenthubEnv(t, env, "", "auth", "status", serverID, "--json")
	var rows []map[string]any
	if err := json.Unmarshal(lastEnvelope(t, out).Data, &rows); err != nil {
		t.Fatalf("auth status --json: %v\n%s", err, out)
	}
	if len(rows) != 1 {
		t.Fatalf("auth status rows = %d, want 1:\n%s", len(rows), out)
	}
	return rows[0]
}

// TestARefusedGrantStopsAndAsksForALogin is the failure this whole area
// exists for, at the altitude a user meets it: a server that worked
// yesterday, a provider that has since ended the session, and every surface
// that is supposed to explain it.
//
// Before this, the hub answered by offering the dead credential to the
// provider again on every renewer's schedule — forever, with a WARN nobody
// reads — while `server ls` said the sign-in had merely expired and pointed
// at a renewal that could not work.
func TestARefusedGrantStopsAndAsksForALogin(t *testing.T) {
	dataDir := t.TempDir()
	env := vaultEnv(dataDir)
	p := newOAuthProvider(t)

	loginTo(t, env, p, "guarded")

	c := startGatewayEnv(t, env, "revokeclient")
	defer c.close()
	c.initialize()
	c.waitForTool("guarded__echo", 45*time.Second)
	if got := c.textContent(c.callTool("guarded__echo",
		map[string]any{"marker": "before"}, 45*time.Second)); !strings.Contains(got, "before") {
		c.fatalf("the stored credential did not work to begin with: %q", got)
	}

	// The user withdraws consent in a browser tab somewhere. The provider
	// stops accepting the bearer AND stops honouring the grant behind it;
	// nothing in the vault says any of it happened.
	p.RotateAccessToken()
	p.RefuseGrants(true)
	before := p.Counts()

	// The next call takes a 401, the passive renewer asks once, and is
	// refused. The call fails — there is nothing left to call with.
	rpcErr := c.callToolRefused("guarded__echo", map[string]any{"marker": "after"}, 45*time.Second)
	if !strings.Contains(rpcErr.Message, "401") {
		c.fatalf("the call failed for some other reason than the rejected credential: %v", rpcErr)
	}
	asked := p.Counts()
	if asked.TokenRequests != before.TokenRequests+1 {
		t.Fatalf("token requests = %d, want exactly one ask", asked.TokenRequests)
	}

	// What the operator is told. Three surfaces, one answer, and none of
	// them offers a renewal that cannot work.
	row := authStatusRow(t, env, "guarded")
	if row["state"] != "revoked" {
		t.Errorf("auth status state = %v, want revoked", row["state"])
	}
	if row["hasRefreshToken"] == true {
		t.Error("auth status advertises an unattended repair for a refused grant")
	}
	if detail, _ := row["detail"].(string); !strings.Contains(detail, "consent withdrawn") {
		t.Errorf("auth status detail = %q, want the provider's own words", detail)
	}
	out, _ := runAgenthubEnv(t, env, "", "server", "ls", "--json")
	if !strings.Contains(out, `"revoked"`) || !strings.Contains(out, "auth login guarded") {
		t.Errorf("server ls does not name the refusal or its repair:\n%s", out)
	}

	// And the property the counter exists for: further calls, and further
	// commands, do not go back to the provider.
	for range 3 {
		_ = c.callToolRefused("guarded__echo", map[string]any{"marker": "again"}, 45*time.Second)
	}
	// `server test` fails here, and is included precisely because it is the
	// command a user runs next: it must not ask the provider either.
	if code, out := runAgenthubExitEnv(t, env, "", "server", "test", "guarded", "--json"); code == 0 {
		t.Fatalf("the server answered with a refused credential: %s", out)
	}
	if got := p.Counts().TokenRequests; got != asked.TokenRequests {
		t.Fatalf("token requests = %d after three more calls and a server test, want %d — "+
			"a grant the provider refused must never be offered again", got, asked.TokenRequests)
	}

	// Recovery: a fresh login, and the same client is served again without
	// being told anything happened.
	p.RefuseGrants(false)
	out, stderr := runAgenthubEnv(t, env, "", "auth", "login", "guarded",
		"--device", "--allow-local", "--json")
	if e := lastEnvelope(t, out); !e.OK {
		t.Fatalf("re-login failed: %s\n%s", out, stderr)
	}
	if row := authStatusRow(t, env, "guarded"); row["state"] != "authorized" {
		t.Fatalf("after a fresh login, auth status = %v", row["state"])
	}
	if got := c.textContent(c.callTool("guarded__echo",
		map[string]any{"marker": "recovered"}, 45*time.Second)); !strings.Contains(got, "recovered") {
		c.fatalf("the client was not recovered by the new sign-in: %q", got)
	}
}

// TestTheDaemonRenewsACredentialNobodyIsWatching is the proactive path end to
// end, and it is the half no gateway can stand in for: there is no client, no
// connection and no 401 anywhere in it. A hub that only renews on rejection
// looks identical until the morning somebody opens their laptop to a server
// that has been dead all night.
//
// Two servers, one daemon. The second one advertises no expiry at all, which
// means "never expires" rather than "expired" — refreshing it on a timer
// would be a permanent storm against the provider, and that is the failure
// this asserts the absence of.
func TestTheDaemonRenewsACredentialNobodyIsWatching(t *testing.T) {
	if testing.Short() {
		t.Skip("daemon proactive refresh e2e skipped in -short mode (waits for a scan)")
	}
	dataDir := t.TempDir()

	// Short socket path: t.TempDir on macOS can exceed sun_path.
	sockDir, err := os.MkdirTemp("", "ahoauth")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(sockDir) })
	socket := filepath.Join(sockDir, "ctl.sock")
	env := append(vaultEnv(dataDir), "AGENTHUB_SOCKET="+socket)

	// A two-second token: short enough that the scan finds it due almost at
	// once, and short enough to be past the grace floor, so what is measured
	// is the daemon's schedule rather than the test's patience.
	short := newOAuthProvider(t, func(o *fakeas.Options) { o.AccessTTL = 2 })
	// No expires_in at all. Nothing may renew this one, ever.
	eternal := newOAuthProvider(t, func(o *fakeas.Options) { o.NoExpiry = true })

	loginTo(t, env, short, "shortlived")
	loginTo(t, env, eternal, "eternal")
	shortAfterLogin := short.Counts()
	eternalAfterLogin := eternal.Counts()

	h := &daemonEnv{dataDir: dataDir, socket: socket, env: env}
	runAgenthubEnv(t, env, "", "daemon", "start", "--headless")
	t.Cleanup(func() { h.killDaemon(t) })

	// No client, no connection: the only thing that can produce a refresh
	// grant here is the daemon's own scan.
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		if short.Counts().Refreshes > shortAfterLogin.Refreshes {
			break
		}
		time.Sleep(200 * time.Millisecond)
	}
	renewed := short.Counts()
	if renewed.Refreshes <= shortAfterLogin.Refreshes {
		t.Fatalf("the daemon never renewed an expiring credential (%d refresh grants); "+
			"a hub that only renews on rejection is dead by morning", renewed.Refreshes)
	}
	if renewed.Accepted == shortAfterLogin.Accepted {
		t.Error("the provider still accepts the pre-renewal bearer; nothing was actually rotated")
	}

	// The never-expiring one was left alone throughout.
	if got := eternal.Counts(); got.TokenRequests != eternalAfterLogin.TokenRequests {
		t.Errorf("a credential with no advertised expiry was renewed %d time(s): "+
			"\"no expires_in\" means never expires, and refreshing it on a timer is a storm",
			got.TokenRequests-eternalAfterLogin.TokenRequests)
	}
}
