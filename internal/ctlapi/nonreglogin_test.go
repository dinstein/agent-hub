package ctlapi

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/dinstein/agent-hub/internal/oauthflow"
	"github.com/dinstein/agent-hub/internal/oauthlogin"
	"github.com/dinstein/agent-hub/internal/registry"
)

// nrLogins is a LoginSessions over a canned answer, recording what it was
// asked for. The flow itself is internal/oauthlogin's subject; what this file
// tests is the registry lookup, the routing and the rendering.
type nrLogins struct {
	mu        sync.Mutex
	started   []oauthlogin.Request
	session   oauthlogin.Session
	startErr  error
	cancelled []string
}

func (l *nrLogins) Start(req oauthlogin.Request) (oauthlogin.Session, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.started = append(l.started, req)
	if l.startErr != nil {
		return oauthlogin.Session{}, l.startErr
	}
	s := l.session
	s.Server = req.ServerID
	return s, nil
}

func (l *nrLogins) Get(id string) (oauthlogin.Session, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if id != l.session.ID {
		return oauthlogin.Session{}, oauthlogin.ErrNoSession
	}
	return l.session, nil
}

func (l *nrLogins) Cancel(id string) (oauthlogin.Session, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if id != l.session.ID {
		return oauthlogin.Session{}, oauthlogin.ErrNoSession
	}
	l.cancelled = append(l.cancelled, id)
	s := l.session
	s.Phase = oauthlogin.PhaseFailed
	s.Err = context.Canceled
	return s, nil
}

func (l *nrLogins) request(t *testing.T) oauthlogin.Request {
	t.Helper()
	l.mu.Lock()
	defer l.mu.Unlock()
	if len(l.started) == 0 {
		t.Fatal("no login was started")
	}
	return l.started[len(l.started)-1]
}

// seedHTTPServer stores an http entry with optional OAuth hints. seedServer
// writes a stdio entry, which has no url to authorize against.
func seedHTTPServer(t *testing.T, reg *registry.Store, id, url, provenance string, hint *registry.OAuthHint) {
	t.Helper()
	err := reg.Update(context.Background(), func(tx *registry.Tx) error {
		tx.Servers.V.Servers[id] = registry.Doc[registry.ServerEntry]{V: registry.ServerEntry{
			Transport:  "http",
			URL:        url,
			Enabled:    true,
			Source:     "manual",
			Provenance: provenance,
			OAuth:      hint,
		}}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func pendingSession() oauthlogin.Session {
	return oauthlogin.Session{
		ID:       "sess-abc",
		Phase:    oauthlogin.PhasePending,
		Deadline: time.Now().Add(10 * time.Minute),
	}
}

func TestLoginStartAnswersBeforeThereIsAnythingToShow(t *testing.T) {
	logins := &nrLogins{session: pendingSession()}
	env := nrStart(t, func(d *NonRegistryDeps) {
		d.OAuth = &nrOAuth{states: map[string]*oauthflow.State{}}
		d.Logins = logins
	})
	seedHTTPServer(t, env.reg, "github", "https://api.example/mcp", "", nil)

	status, body := nrDo(t, env.sock, http.MethodPost, "/v1/auth/github/login", nil)
	// 202: the session exists, the work does not. Choosing between the device
	// and loopback flows needs the authorization server's metadata, and
	// holding the response until then puts a discovery timeout inside a
	// button press.
	if status != http.StatusAccepted {
		t.Fatalf("status = %d, want 202: %s", status, body)
	}
	var out AuthLoginWire
	nrData(t, body, &out)
	if out.ID != "sess-abc" || out.Server != "github" {
		t.Fatalf("session = %+v", out)
	}
	if out.Phase != string(oauthlogin.PhasePending) {
		t.Errorf("phase = %q, want pending", out.Phase)
	}
	if out.Mode != "" {
		t.Errorf("mode = %q; it cannot be known before discovery", out.Mode)
	}
	if out.Deadline == 0 {
		t.Error("no deadline reported: a poller cannot tell a slow login from a dead one")
	}
}

func TestLoginStartForwardsTheEntrysOwnHints(t *testing.T) {
	logins := &nrLogins{session: pendingSession()}
	env := nrStart(t, func(d *NonRegistryDeps) {
		d.OAuth = &nrOAuth{states: map[string]*oauthflow.State{
			// A previous login registered port 8040.
			"github": {CallbackPort: 8040},
		}}
		d.Logins = logins
	})
	seedHTTPServer(t, env.reg, "github", "https://api.example/mcp", "", &registry.OAuthHint{
		Issuer:                "https://as.example",
		Scopes:                []string{"repo", "read:org"},
		ResourceMetadataURL:   "https://api.example/.well-known/oauth-protected-resource",
		AuthorizationEndpoint: "https://as.example/authorize/tenant1",
	})

	if status, body := nrDo(t, env.sock, http.MethodPost, "/v1/auth/github/login", nil); status != http.StatusAccepted {
		t.Fatalf("status = %d: %s", status, body)
	}
	got := logins.request(t)
	if got.ResourceURL != "https://api.example/mcp" {
		t.Errorf("ResourceURL = %q", got.ResourceURL)
	}
	if got.Issuer != "https://as.example" {
		t.Errorf("Issuer = %q", got.Issuer)
	}
	if len(got.Scopes) != 2 || got.Scopes[0] != "repo" {
		t.Errorf("Scopes = %v; hints are sent verbatim", got.Scopes)
	}
	if got.ResourceMetadataURL == "" || got.AuthorizationEndpoint == "" {
		t.Errorf("pinned endpoints dropped: %+v", got)
	}
	// The stored callback port has to come back, or every provider that
	// matches redirect_uri byte for byte breaks on the second login.
	if got.CallbackPort != 8040 {
		t.Errorf("CallbackPort = %d, want 8040", got.CallbackPort)
	}
}

// The loopback carve-out is a property of the STORED entry, never of the
// caller: there is no request field that can ask for it.
func TestLoopbackExemptionFollowsTheEntrysProvenance(t *testing.T) {
	for _, tc := range []struct {
		name       string
		provenance string
		want       bool
	}{
		{"remote entries stay screened", registry.ProvenanceRemote, false},
		{"an unset provenance stays screened", "", false},
		{"a local entry is exempt", registry.ProvenanceLocal, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			logins := &nrLogins{session: pendingSession()}
			env := nrStart(t, func(d *NonRegistryDeps) {
				d.OAuth = &nrOAuth{states: map[string]*oauthflow.State{}}
				d.Logins = logins
			})
			seedHTTPServer(t, env.reg, "s", "https://api.example/mcp", tc.provenance, nil)
			if status, body := nrDo(t, env.sock, http.MethodPost, "/v1/auth/s/login", nil); status != http.StatusAccepted {
				t.Fatalf("status = %d: %s", status, body)
			}
			if got := logins.request(t).AllowLoopback; got != tc.want {
				t.Errorf("AllowLoopback = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestLoginStartRefusesAServerItCannotAddress(t *testing.T) {
	logins := &nrLogins{session: pendingSession()}
	env := nrStart(t, func(d *NonRegistryDeps) {
		d.OAuth = &nrOAuth{states: map[string]*oauthflow.State{}}
		d.Logins = logins
	})
	// A stdio server: no url, and no issuer hint to fall back on.
	seedServer(t, env.reg, "local", true)

	status, body := nrDo(t, env.sock, http.MethodPost, "/v1/auth/local/login", nil)
	if status != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400: %s", status, body)
	}
	if !strings.Contains(string(body), "no url to authorize against") {
		t.Errorf("body does not say what is wrong: %s", body)
	}
}

func TestLoginStartForAnUnknownServerIsTheUniform404(t *testing.T) {
	env := nrStart(t, func(d *NonRegistryDeps) {
		d.OAuth = &nrOAuth{states: map[string]*oauthflow.State{}}
		d.Logins = &nrLogins{session: pendingSession()}
	})
	if status, _ := nrDo(t, env.sock, http.MethodPost, "/v1/auth/ghost/login", nil); status != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", status)
	}
}

// A daemon assembled without a login manager must say so and name the command
// that still works. A bare 404 here is indistinguishable from a typo in the
// server id, and sends the user looking for the wrong bug.
func TestLoginStartWithoutAManagerNamesTheCLI(t *testing.T) {
	env := nrStart(t, func(d *NonRegistryDeps) {
		d.OAuth = &nrOAuth{states: map[string]*oauthflow.State{}}
	})
	seedHTTPServer(t, env.reg, "github", "https://api.example/mcp", "", nil)
	status, body := nrDo(t, env.sock, http.MethodPost, "/v1/auth/github/login", nil)
	if status != http.StatusNotFound {
		t.Fatalf("status = %d, want 404: %s", status, body)
	}
	if !strings.Contains(string(body), "agenthub auth login github") {
		t.Errorf("the refusal does not name the command that works: %s", body)
	}
}

func TestLoginPollReportsTheDeviceCodeAndNotTheSecret(t *testing.T) {
	logins := &nrLogins{session: oauthlogin.Session{
		ID:                      "sess-abc",
		Server:                  "linear",
		Phase:                   oauthlogin.PhasePending,
		Mode:                    string(oauthflow.ModeDevice),
		UserCode:                "WDJB-MJHT",
		VerificationURI:         "https://as.example/device",
		VerificationURIComplete: "https://as.example/device?code=WDJB-MJHT",
		Deadline:                time.Now().Add(time.Minute),
	}}
	env := nrStart(t, func(d *NonRegistryDeps) {
		d.OAuth = &nrOAuth{states: map[string]*oauthflow.State{}}
		d.Logins = logins
	})

	status, body := nrDo(t, env.sock, http.MethodGet, "/v1/logins/sess-abc", nil)
	if status != http.StatusOK {
		t.Fatalf("status = %d: %s", status, body)
	}
	var out AuthLoginWire
	nrData(t, body, &out)
	if out.UserCode != "WDJB-MJHT" || out.VerificationURI == "" {
		t.Fatalf("device fields missing: %+v", out)
	}
	// The user code is meant to be read aloud; the device code polled with is
	// the secret. Asserting on the KEY SET is what makes this test able to
	// fail: oauthlogin.Session has no device-code field to leak today, so a
	// value-based check would pass for as long as that stays true and say
	// nothing on the day it does not.
	assertNoCredentialKeys(t, body)
}

// assertNoCredentialKeys fails if the wire grew a field that could carry a
// secret. It names the shapes rather than the values because that is the
// mistake this guards: someone adding a field, not someone leaking a
// particular string.
func assertNoCredentialKeys(t *testing.T, body []byte) {
	t.Helper()
	var raw map[string]json.RawMessage
	nrData(t, body, &raw)
	for _, banned := range []string{
		"access_token", "refresh_token", "device_code", "code", "client_secret", "token",
	} {
		if _, ok := raw[banned]; ok {
			t.Errorf("wire carries %q", banned)
		}
	}
}

func TestLoginPollReportsTheAuthorizationURLForTheCallerToOpen(t *testing.T) {
	logins := &nrLogins{session: oauthlogin.Session{
		ID:               "sess-abc",
		Server:           "github",
		Phase:            oauthlogin.PhasePending,
		Mode:             string(oauthflow.ModeLoopback),
		AuthorizationURL: "https://as.example/authorize?state=xyz",
		Deadline:         time.Now().Add(time.Minute),
	}}
	env := nrStart(t, func(d *NonRegistryDeps) {
		d.OAuth = &nrOAuth{states: map[string]*oauthflow.State{}}
		d.Logins = logins
	})
	_, body := nrDo(t, env.sock, http.MethodGet, "/v1/logins/sess-abc", nil)
	var out AuthLoginWire
	nrData(t, body, &out)
	if out.AuthorizationURL != "https://as.example/authorize?state=xyz" {
		t.Fatalf("the caller was given nothing to open: %+v", out)
	}
}

// A failed login is a SUCCESSFUL read of a failed thing. The poller needs the
// reason far more than it needs an HTTP status to branch on, and only an id
// that names nothing is a 404.
func TestAFailedLoginIsA200WithItsReason(t *testing.T) {
	fe := &oauthflow.FlowError{
		Type:       oauthflow.ErrorTypeDiscovery,
		Err:        errors.New("no authorization server metadata"),
		Suggestion: "pass an explicit issuer with --issuer",
	}
	logins := &nrLogins{session: oauthlogin.Session{
		ID: "sess-abc", Server: "github", Phase: oauthlogin.PhaseFailed, Err: fe,
	}}
	env := nrStart(t, func(d *NonRegistryDeps) {
		d.OAuth = &nrOAuth{states: map[string]*oauthflow.State{}}
		d.Logins = logins
	})
	status, body := nrDo(t, env.sock, http.MethodGet, "/v1/logins/sess-abc", nil)
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", status, body)
	}
	var out AuthLoginWire
	nrData(t, body, &out)
	if out.Phase != string(oauthlogin.PhaseFailed) || out.Error == "" {
		t.Fatalf("failure not reported: %+v", out)
	}
	// The suggestion is oauthflow's own, so this surface and the CLI answer
	// one failure with one sentence instead of two vocabularies.
	if out.Hint != "pass an explicit issuer with --issuer" {
		t.Errorf("hint = %q, want the flow's own suggestion", out.Hint)
	}
}

func TestCompletedLoginReportsWhatWasStoredAndNoToken(t *testing.T) {
	logins := &nrLogins{session: oauthlogin.Session{
		ID: "sess-abc", Server: "github", Phase: oauthlogin.PhaseComplete,
		Mode: string(oauthflow.ModeDevice), Issuer: "https://as.example",
		Scope: "repo", ExpiresAt: 1893456000, HasRefreshToken: true,
	}}
	env := nrStart(t, func(d *NonRegistryDeps) {
		d.OAuth = &nrOAuth{states: map[string]*oauthflow.State{}}
		d.Logins = logins
	})
	_, body := nrDo(t, env.sock, http.MethodGet, "/v1/logins/sess-abc", nil)
	var out AuthLoginWire
	nrData(t, body, &out)
	if out.Phase != string(oauthlogin.PhaseComplete) {
		t.Fatalf("phase = %q", out.Phase)
	}
	if out.Issuer != "https://as.example" || out.Scope != "repo" || out.TokenExpiresAt != 1893456000 {
		t.Errorf("stored facts not reported: %+v", out)
	}
	if !out.HasRefreshToken {
		t.Error("HasRefreshToken lost — it is the boolean, never the token")
	}
	assertNoCredentialKeys(t, body)
}

// A manager that cannot start a login must surface it, not answer 202 with a
// session id nobody can poll.
func TestLoginStartFailureIsReported(t *testing.T) {
	logins := &nrLogins{session: pendingSession(), startErr: errors.New("entropy unavailable")}
	env := nrStart(t, func(d *NonRegistryDeps) {
		d.OAuth = &nrOAuth{states: map[string]*oauthflow.State{}}
		d.Logins = logins
	})
	seedHTTPServer(t, env.reg, "github", "https://api.example/mcp", "", nil)
	status, body := nrDo(t, env.sock, http.MethodPost, "/v1/auth/github/login", nil)
	if status != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500: %s", status, body)
	}
	if !strings.Contains(string(body), "entropy unavailable") {
		t.Errorf("the reason was dropped: %s", body)
	}
}

func TestLoginPollForAnUnknownSessionIs404(t *testing.T) {
	env := nrStart(t, func(d *NonRegistryDeps) {
		d.OAuth = &nrOAuth{states: map[string]*oauthflow.State{}}
		d.Logins = &nrLogins{session: pendingSession()}
	})
	if status, _ := nrDo(t, env.sock, http.MethodGet, "/v1/logins/nope", nil); status != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", status)
	}
}

func TestCancelStopsTheWait(t *testing.T) {
	logins := &nrLogins{session: pendingSession()}
	env := nrStart(t, func(d *NonRegistryDeps) {
		d.OAuth = &nrOAuth{states: map[string]*oauthflow.State{}}
		d.Logins = logins
	})
	status, body := nrDo(t, env.sock, http.MethodDelete, "/v1/logins/sess-abc", nil)
	if status != http.StatusOK {
		t.Fatalf("status = %d: %s", status, body)
	}
	var out AuthLoginWire
	nrData(t, body, &out)
	if out.Phase != string(oauthlogin.PhaseFailed) {
		t.Errorf("phase after cancel = %q", out.Phase)
	}
	if !strings.Contains(out.Hint, "cancelled") || !strings.Contains(out.Hint, "nothing was stored") {
		t.Errorf("hint = %q; a cancelled sign-in must say so and say nothing was stored", out.Hint)
	}
	logins.mu.Lock()
	defer logins.mu.Unlock()
	if len(logins.cancelled) != 1 {
		t.Errorf("cancel not forwarded: %v", logins.cancelled)
	}
}

// A daemon with no login manager must not serve the session routes either:
// they would 404 anyway, but going through the nil interface would panic.
func TestSessionRoutesAreOffWithoutAManager(t *testing.T) {
	env := nrStart(t, func(d *NonRegistryDeps) {
		d.OAuth = &nrOAuth{states: map[string]*oauthflow.State{}}
	})
	for _, m := range []string{http.MethodGet, http.MethodDelete} {
		if status, _ := nrDo(t, env.sock, m, "/v1/logins/sess-abc", nil); status != http.StatusNotFound {
			t.Errorf("%s /v1/logins/{id} = %d, want 404", m, status)
		}
	}
}
