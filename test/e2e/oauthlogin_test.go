package e2e_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

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

// oauthProvider is a resource server and its authorization server on one
// loopback origin, the same arrangement internal/oauthflow's own fixture uses.
// It answers only what a login needs: the RFC 9728 pointer, RFC 8414 metadata,
// dynamic client registration, the device grant, and the MCP endpoint that
// starts refusing and ends up answering.
type oauthProvider struct {
	srv  *httptest.Server
	base string
	// mcp is the MCP half, reused from the http downstream fixture so that
	// only the authorization is new here.
	mcp *fakeHTTPServer
	// granted is the access token this provider issues and the only value
	// its MCP endpoint accepts.
	granted string

	mu           sync.Mutex
	registered   int
	deviceGrants int
	lastGrant    string
	// challenged counts MCP requests answered 401: it is what proves the
	// login started from a real refusal rather than from configuration.
	challenged int
}

func newOAuthProvider(t *testing.T) *oauthProvider {
	t.Helper()
	p := &oauthProvider{
		mcp:     &fakeHTTPServer{script: fakemcp.Minimal()},
		granted: "granted-access-token",
	}
	// Unstarted, because the documents this provider serves name its own
	// address: writing the URL into the handler after Start would be a race
	// against the requests it is already able to answer.
	p.srv = httptest.NewUnstartedServer(http.HandlerFunc(p.route))
	p.base = "http://" + p.srv.Listener.Addr().String()
	p.srv.Start()
	t.Cleanup(p.srv.Close)
	return p
}

func (p *oauthProvider) mcpURL() string { return p.base + "/mcp" }

// prmURL is where the 401 challenge points. Naming it explicitly, rather than
// relying on the candidate search, is what makes a failure here a failure of
// the login and not of discovery — which has its own coverage upstream.
func (p *oauthProvider) prmURL() string { return p.base + "/.well-known/oauth-protected-resource/mcp" }

func (p *oauthProvider) route(w http.ResponseWriter, r *http.Request) {
	switch {
	case strings.Contains(r.URL.Path, "/.well-known/oauth-protected-resource"):
		writeJSONDoc(w, map[string]any{
			"resource":              p.mcpURL(),
			"authorization_servers": []string{p.base},
			"scopes_supported":      []string{"mcp.read"},
		})
	case strings.Contains(r.URL.Path, "/.well-known/"):
		writeJSONDoc(w, map[string]any{
			"issuer":                                         p.base,
			"authorization_endpoint":                         p.base + "/authorize",
			"token_endpoint":                                 p.base + "/token",
			"registration_endpoint":                          p.base + "/register",
			"device_authorization_endpoint":                  p.base + "/device",
			"code_challenge_methods_supported":               []string{"S256"},
			"authorization_response_iss_parameter_supported": true,
		})
	case r.URL.Path == "/register":
		p.mu.Lock()
		p.registered++
		p.mu.Unlock()
		w.WriteHeader(http.StatusCreated)
		writeJSONBody(w, map[string]any{
			"client_id":           "e2e-registered-client",
			"client_id_issued_at": 1,
		})
	case r.URL.Path == "/device":
		writeJSONDoc(w, map[string]any{
			"device_code":      "e2e-device-code",
			"user_code":        "WDJB-MJHT",
			"verification_uri": p.base + "/activate",
			"expires_in":       300,
			"interval":         1,
		})
	case r.URL.Path == "/token":
		p.serveToken(w, r)
	case r.URL.Path == "/mcp":
		p.serveMCP(w, r)
	default:
		http.NotFound(w, r)
	}
}

// serveToken answers the device grant. The grant type is asserted rather than
// ignored: a token handed out for the wrong grant would let this test pass
// against a flow that never ran the one it claims to.
func (p *oauthProvider) serveToken(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}
	p.mu.Lock()
	p.deviceGrants++
	p.lastGrant = r.Form.Get("grant_type")
	p.mu.Unlock()
	if r.Form.Get("grant_type") != "urn:ietf:params:oauth:grant-type:device_code" {
		writeJSONStatus(w, http.StatusBadRequest, map[string]any{"error": "unsupported_grant_type"})
		return
	}
	writeJSONDoc(w, map[string]any{
		"access_token":  p.granted,
		"token_type":    "Bearer",
		"expires_in":    3600,
		"refresh_token": "e2e-refresh-token",
		"scope":         "mcp.read",
	})
}

// serveMCP refuses until it is shown the token this provider issued, and the
// refusal carries the RFC 9728 pointer that starts a login.
func (p *oauthProvider) serveMCP(w http.ResponseWriter, r *http.Request) {
	if r.Header.Get("Authorization") != "Bearer "+p.granted {
		p.mu.Lock()
		p.challenged++
		p.mu.Unlock()
		w.Header().Set("WWW-Authenticate",
			`Bearer realm="agenthub-e2e", resource_metadata="`+p.prmURL()+`"`)
		w.WriteHeader(http.StatusUnauthorized)
		return
	}
	p.mcp.handle(w, r)
}

func (p *oauthProvider) counts() (registered, grants, challenged int, lastGrant string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.registered, p.deviceGrants, p.challenged, p.lastGrant
}

func writeJSONDoc(w http.ResponseWriter, doc map[string]any) {
	writeJSONStatus(w, http.StatusOK, doc)
}

func writeJSONBody(w http.ResponseWriter, doc map[string]any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(doc)
}

func writeJSONStatus(w http.ResponseWriter, status int, doc map[string]any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(doc)
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
		"--url", p.mcpURL(), "--transport", "http", "--local", "--json")

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

	registered, grants, challenged, lastGrant := p.counts()
	if challenged == 0 {
		t.Fatal("the login never saw a 401; it cannot have started from a real challenge")
	}
	if registered == 0 {
		t.Fatal("no dynamic client registration reached the provider")
	}
	if grants == 0 || lastGrant != "urn:ietf:params:oauth:grant-type:device_code" {
		t.Fatalf("the token was not obtained through the device grant (%d grants, last %q)", grants, lastGrant)
	}

	// The credential is never printed, by any of these commands.
	if strings.Contains(out+stderr, p.granted) {
		t.Fatal("auth login printed the access token")
	}
	out, _ = runAgenthubEnv(t, env, "", "auth", "status", "--json")
	if !strings.Contains(out, "guarded") {
		t.Fatalf("auth status does not know about the server just signed in to: %s", out)
	}
	if strings.Contains(out, p.granted) {
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
