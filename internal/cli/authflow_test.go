package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/dinstein/agent-hub/api"
	"github.com/dinstein/agent-hub/internal/oauthflow"
	"github.com/dinstein/agent-hub/internal/secrets"
)

// fakeAuthServer is a loopback OAuth 2.1 authorization server: RFC 9728
// protected-resource metadata, RFC 8414 AS metadata, RFC 7591 dynamic
// registration and a token endpoint that rotates its refresh token.
//
// It exists to pin the CLI wiring (which flags reach which flow, what is
// persisted, what is rendered) end to end. The protocol details themselves
// are internal/oauthflow's own tests.
type fakeAuthServer struct {
	srv       *httptest.Server
	resource  string // the MCP server URL this AS protects
	issued    atomic.Int64
	tokenHits atomic.Int64
	regHits   atomic.Int64
}

func newFakeAuthServer(t *testing.T, resource string) *fakeAuthServer {
	t.Helper()
	as := &fakeAuthServer{resource: resource}
	mux := http.NewServeMux()

	mux.HandleFunc("/.well-known/oauth-authorization-server", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, map[string]any{
			"issuer":                                as.srv.URL,
			"authorization_endpoint":                as.srv.URL + "/authorize",
			"token_endpoint":                        as.srv.URL + "/token",
			"registration_endpoint":                 as.srv.URL + "/register",
			"code_challenge_methods_supported":      []string{"S256"},
			"grant_types_supported":                 []string{"authorization_code", "refresh_token"},
			"response_types_supported":              []string{"code"},
			"token_endpoint_auth_methods_supported": []string{"none"},
		})
	})
	mux.HandleFunc("/register", func(w http.ResponseWriter, _ *http.Request) {
		as.regHits.Add(1)
		w.WriteHeader(http.StatusCreated)
		writeJSON(w, map[string]any{"client_id": "dcr-client-1"})
	})
	mux.HandleFunc("/token", func(w http.ResponseWriter, _ *http.Request) {
		as.tokenHits.Add(1)
		n := as.issued.Add(1)
		writeJSON(w, map[string]any{
			"access_token":  fmt.Sprintf("access-%d", n),
			"refresh_token": fmt.Sprintf("refresh-%d", n),
			"token_type":    "Bearer",
			"expires_in":    3600,
			"scope":         "read",
		})
	})
	as.srv = httptest.NewServer(mux)
	t.Cleanup(as.srv.Close)
	return as
}

// protectedResourceHandler serves the RFC 9728 document of the MCP server
// that this AS protects. It is mounted on the resource server, not here.
func (as *fakeAuthServer) protectedResourceHandler(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, map[string]any{
		"resource":              as.resource,
		"authorization_servers": []string{as.srv.URL},
		"scopes_supported":      []string{"read"},
	})
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

// newProtectedMCPServer starts a loopback MCP endpoint that publishes
// protected-resource metadata pointing at as.
func newProtectedMCPServer(t *testing.T) (*httptest.Server, *fakeAuthServer) {
	t.Helper()
	mux := http.NewServeMux()
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	as := newFakeAuthServer(t, srv.URL+"/mcp")
	mux.HandleFunc("/.well-known/oauth-protected-resource/mcp", as.protectedResourceHandler)
	mux.HandleFunc("/.well-known/oauth-protected-resource", as.protectedResourceHandler)
	mux.HandleFunc("/mcp", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("WWW-Authenticate", `Bearer realm="mcp"`)
		w.WriteHeader(http.StatusUnauthorized)
	})
	return srv, as
}

// TestAuthLoginManualRoundTrip runs the whole headless manual flow through
// the CLI: discovery → DCR → authorize URL printed as a progress event →
// pasted code → token exchange → vault. Then `auth status` reports it, and
// `auth refresh` renews it.
//
// The vault is pinned to secrets.enc via AGENTHUB_SECRET_KEY so the test
// never touches the OS keyring.
func TestAuthLoginManualRoundTrip(t *testing.T) {
	setDataDir(t)
	t.Setenv("AGENTHUB_SECRET_KEY", "test-passphrase-for-cli-auth")
	mcpSrv, as := newProtectedMCPServer(t)

	if code, out, _ := runCLI(t, "", "server", "add", "remote",
		"--url", mcpSrv.URL+"/mcp", "--local", "--json"); code != ExitOK {
		t.Fatalf("server add exit = %d\n%s", code, out)
	}

	// A bare code is accepted (docs/modules/oauth.md: users often strip the URL);
	// PKCE still binds the exchange to this process.
	code, out, stderr := runCLI(t, "auth-code-xyz\n",
		"auth", "login", "remote", "--manual", "--allow-local", "--json")
	if code != ExitOK {
		t.Fatalf("auth login exit = %d\n%s\n%s", code, out, stderr)
	}
	lines := strings.Split(strings.TrimSpace(out), "\n")
	env := decodeEnvelope(t, lines[len(lines)-1])
	if !env.OK {
		t.Fatalf("login envelope: %s", out)
	}
	var res AuthLoginResult
	if err := json.Unmarshal(env.Data, &res); err != nil {
		t.Fatal(err)
	}
	if res.Mode != "manual" || !res.HasRefresh || res.ExpiresIn <= 0 {
		t.Fatalf("login result = %+v", res)
	}
	if as.regHits.Load() != 1 || as.tokenHits.Load() != 1 {
		t.Fatalf("register/token hits = %d/%d, want 1/1", as.regHits.Load(), as.tokenHits.Load())
	}
	// No credential may appear anywhere in the output, in either stream.
	for _, s := range []string{out, stderr} {
		for _, secret := range []string{"access-1", "refresh-1", "auth-code-xyz"} {
			if strings.Contains(s, secret) {
				t.Fatalf("output leaked %q:\n%s", secret, s)
			}
		}
	}
	// The progress stream must have carried the authorization URL.
	if !strings.Contains(out, "awaiting_paste") || !strings.Contains(out, "/authorize") {
		t.Errorf("no authorization URL in the progress stream:\n%s", out)
	}

	// status reports the stored state without rendering it.
	code, out, _ = runCLI(t, "", "auth", "status", "remote", "--json")
	if code != ExitOK {
		t.Fatalf("auth status exit = %d\n%s", code, out)
	}
	var rows AuthStatusList
	if err := json.Unmarshal(decodeEnvelope(t, out).Data, &rows); err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].State != "authorized" || !rows[0].HasRefreshToken {
		t.Fatalf("status rows = %+v", rows)
	}
	if strings.Contains(out, "access-1") || strings.Contains(out, "refresh-1") {
		t.Fatalf("auth status leaked a credential:\n%s", out)
	}

	// The two commands reading this one vault must agree about it. Their wire
	// shapes stay separate on purpose — different domains, different costs —
	// but the lifecycle in the middle is one function now, and this is what
	// says so from OUTSIDE: same server, both commands, field by field.
	assertAuthCommandsAgree(t, "remote")

	// refresh spends the rotated refresh token exactly once.
	code, out, stderr = runCLI(t, "", "auth", "refresh", "remote", "--json")
	if code != ExitOK {
		t.Fatalf("auth refresh exit = %d\n%s\n%s", code, out, stderr)
	}
	if as.tokenHits.Load() != 2 {
		t.Fatalf("token endpoint hits = %d, want 2", as.tokenHits.Load())
	}

	// A second login reuses the DCR registration instead of re-registering
	// (providers rate-limit DCR, and re-registering litters their client
	// list).
	if code, out, _ = runCLI(t, "auth-code-2\n",
		"auth", "login", "remote", "--manual", "--allow-local", "--json"); code != ExitOK {
		t.Fatalf("second login exit = %d\n%s", code, out)
	}
	if as.regHits.Load() != 1 {
		t.Fatalf("register hits = %d, want the registration to be reused", as.regHits.Load())
	}

	// logout removes both vault entries: status falls back to "none".
	if code, out, _ = runCLI(t, "", "auth", "logout", "remote", "--json"); code != ExitOK {
		t.Fatalf("auth logout exit = %d\n%s", code, out)
	}
	code, out, _ = runCLI(t, "", "auth", "status", "remote", "--json")
	if code != ExitOK {
		t.Fatalf("auth status after logout exit = %d\n%s", code, out)
	}
	rows = nil
	if err := json.Unmarshal(decodeEnvelope(t, out).Data, &rows); err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].State != "none" {
		t.Fatalf("status after logout = %+v", rows)
	}
}

// TestAuthLoginManualRejectsMismatchedState pins the manual-mode protection
// most tools drop: a pasted callback URL from a DIFFERENT authorization
// request must be refused, not exchanged.
func TestAuthLoginManualRejectsMismatchedState(t *testing.T) {
	setDataDir(t)
	t.Setenv("AGENTHUB_SECRET_KEY", "test-passphrase-for-cli-auth")
	mcpSrv, as := newProtectedMCPServer(t)

	if code, _, _ := runCLI(t, "", "server", "add", "remote",
		"--url", mcpSrv.URL+"/mcp", "--local", "--json"); code != ExitOK {
		t.Fatal("server add failed")
	}
	code, out, _ := runCLI(t, "http://127.0.0.1:8642/callback?code=x&state=someone-elses\n",
		"auth", "login", "remote", "--manual", "--allow-local", "--json")
	if code != ExitAuth {
		t.Fatalf("exit = %d, want %d (auth failure)\n%s", code, ExitAuth, out)
	}
	if as.tokenHits.Load() != 0 {
		t.Fatal("a code from a foreign authorization request was exchanged")
	}
}

// assertAuthCommandsAgree compares what `auth status` and `server ls` say
// about one server. Everything both of them carry must match, because both
// read one vault through one lifecycle function; the fields only one of them
// carries (kind, missing secrets on one side, the registrar on neither
// exclusively) are not this assertion's business.
//
// It exists because the drift it forbids already happened once: the refresh
// action was added to `server ls` alone, so the same expired token was
// `refresh` in one command and `login` in the other.
func assertAuthCommandsAgree(t *testing.T, id string) {
	t.Helper()

	var rows AuthStatusList
	decodeInto(t, mustRun(t, "", "auth", "status", id, "--json"), &rows)
	if len(rows) != 1 {
		t.Fatalf("auth status returned %d rows for %q", len(rows), id)
	}
	status := rows[0]

	var servers ServerList
	decodeInto(t, mustRun(t, "", "server", "ls", "--json"), &servers)
	var ls *ServerAuth
	for _, s := range servers {
		if s.ID == id {
			ls = s.Auth
		}
	}
	if ls == nil {
		t.Fatalf("server ls carried no auth for %q: %+v", id, servers)
	}

	for _, f := range []struct {
		name      string
		got, want any
	}{
		{"state", ls.State, status.State},
		{"action", ls.Action, status.Action},
		{"hint", ls.Hint, status.Hint},
		{"hasRefreshToken", ls.HasRefreshToken, status.HasRefreshToken},
		{"issuer", ls.Issuer, status.Issuer},
		{"scope", ls.Scope, status.Scope},
		{"clientRegistrar", ls.ClientRegistrar, status.ClientRegistrar},
		{"expiresAt", ls.ExpiresAt, status.ExpiresAt},
		{"detail", ls.Detail, status.Detail},
	} {
		if f.got != f.want {
			t.Errorf("%s: server ls says %#v, auth status says %#v", f.name, f.got, f.want)
		}
	}
}

// TestAuthCommandsAgreeOnAnExpiredToken is the same agreement in the state
// that matters most: expired, with a refresh token, which is where the two
// commands drifted apart. The round-trip test above reaches only `authorized`,
// where action and hint are both empty and the comparison is nearly vacuous.
func TestAuthCommandsAgreeOnAnExpiredToken(t *testing.T) {
	dir := setDataDir(t)
	t.Setenv("AGENTHUB_SECRET_KEY", "test-passphrase-for-cli-auth")
	mustRun(t, "", "server", "add", "remote", "--url", "https://mcp.example.com/mcp", "--json")

	// Seed the vault directly: no fake provider can be made to hand out a
	// token that is already expired.
	store := oauthflow.NewStore(secrets.NewChain(secrets.ChainConfig{Dir: filepath.Join(dir, "secrets")}))
	now := time.Now()
	err := store.Save(context.Background(), "remote", &oauthflow.State{
		Issuer:        "https://idp.example.com",
		Scope:         "read write",
		RegistrarKind: "dcr",
		RefreshToken:  "r",
		IssuedAt:      now.Add(-2 * time.Hour).Unix(),
		ExpiresAt:     now.Add(-time.Hour).Unix(),
	}, "expired-access-token")
	if err != nil {
		t.Fatal(err)
	}

	assertAuthCommandsAgree(t, "remote")

	// And the agreement is on the values this change is about, not on two
	// empty strings.
	var rows AuthStatusList
	decodeInto(t, mustRun(t, "", "auth", "status", "remote", "--json"), &rows)
	if len(rows) != 1 {
		t.Fatalf("auth status rows = %+v", rows)
	}
	switch got := rows[0]; {
	case got.State != api.AuthStateExpired:
		t.Errorf("state = %q, want expired", got.State)
	case got.Action != api.ActionRefresh:
		t.Errorf("action = %q, want refresh — a refresh token is stored", got.Action)
	case !strings.Contains(got.Hint, "agenthub auth refresh remote"):
		t.Errorf("hint = %q, want the refresh command", got.Hint)
	case got.Scope != "read write" || got.ClientRegistrar != "dcr":
		t.Errorf("scope/registrar = %q/%q", got.Scope, got.ClientRegistrar)
	}
	if strings.Contains(mustRun(t, "", "server", "ls", "--json"), "expired-access-token") {
		t.Fatal("server ls rendered the access token")
	}
}
