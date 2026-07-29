package ctlapi

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/dinstein/agent-hub/api"
	"github.com/dinstein/agent-hub/internal/audit"
	"github.com/dinstein/agent-hub/internal/event"
	"github.com/dinstein/agent-hub/internal/registry"
	"github.com/dinstein/agent-hub/internal/session"
)

// capAuditor captures audit records for assertions.
type capAuditor struct {
	mu   sync.Mutex
	recs []audit.Record
}

func (c *capAuditor) Append(r audit.Record) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.recs = append(c.recs, r)
}

func (c *capAuditor) records() []audit.Record {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]audit.Record, len(c.recs))
	copy(out, c.recs)
	return out
}

// fakeStates is a ServerStateSource backed by a map.
type fakeStates map[string]ServerRuntime

func (f fakeStates) ServerRuntime(id string) (ServerRuntime, bool) {
	rt, ok := f[id]
	return rt, ok
}

type testEnv struct {
	reg  *registry.Store
	mgr  *session.MemoryManager
	bus  *event.Bus
	aud  *capAuditor
	srv  *Server
	sock string
}

// startServer boots a full control-plane server on a real UDS (through
// Listen, i.e. through the peer-cred gate) and returns an api client
// dialing it. Every request in these tests therefore exercises the
// same-uid acceptance branch of the credential check.
func startServer(t *testing.T, mutate func(*Options)) (*api.Client, *testEnv) {
	t.Helper()
	requireUnixy(t)

	reg, err := registry.Open(filepath.Join(t.TempDir(), "registry"))
	if err != nil {
		t.Fatal(err)
	}
	bus := event.NewBus()
	mgr := session.NewMemoryManager(session.Options{Bus: bus})
	aud := &capAuditor{}

	opts := Options{
		Version:   "test-1.0",
		Registry:  reg,
		Sessions:  mgr,
		Bus:       bus,
		Audit:     aud,
		KeepAlive: -1, // no keep-alives in tests
	}
	if mutate != nil {
		mutate(&opts)
	}
	srv, err := NewServer(opts)
	if err != nil {
		t.Fatal(err)
	}

	sock := shortSocketPath(t)
	l, err := Listen(sock)
	if err != nil {
		t.Fatal(err)
	}
	go func() { _ = srv.Serve(l) }()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = srv.Shutdown(ctx)
	})

	client := api.New(sock)
	t.Cleanup(client.Close)
	return client, &testEnv{reg: reg, mgr: mgr, bus: bus, aud: aud, srv: srv, sock: sock}
}

// rawClient returns a plain *http.Client over the UDS for wire-level
// assertions the typed client hides.
func rawClient(sock string) *http.Client {
	return &http.Client{Transport: &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, "unix", sock)
		},
	}}
}

func seedServer(t *testing.T, reg *registry.Store, id string, enabled bool) {
	t.Helper()
	err := reg.Update(context.Background(), func(tx *registry.Tx) error {
		tx.Servers.V.Servers[id] = registry.Doc[registry.ServerEntry]{V: registry.ServerEntry{
			Transport: "stdio",
			Command:   "fake",
			Enabled:   enabled,
			Source:    "manual",
		}}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestPing(t *testing.T) {
	client, env := startServer(t, nil)
	seedServer(t, env.reg, "github", true) // bump generation to 1

	h, err := client.Ping(t.Context())
	if err != nil {
		t.Fatalf("Ping: %v", err)
	}
	if h.Version != "test-1.0" {
		t.Errorf("version = %q", h.Version)
	}
	if h.Pid <= 0 {
		t.Errorf("pid = %d", h.Pid)
	}
	if h.Generation != env.reg.Snapshot().Generation || h.Generation == 0 {
		t.Errorf("generation = %d, want %d (non-zero)", h.Generation, env.reg.Snapshot().Generation)
	}
}

func TestServersList(t *testing.T) {
	states := fakeStates{
		"github": {Conn: ConnConnected, Tools: 26},
		"broken": {Conn: ConnError, ConnDetail: "spawn failed"},
	}
	client, env := startServer(t, func(o *Options) { o.States = states })
	seedServer(t, env.reg, "github", true)
	seedServer(t, env.reg, "broken", true)
	seedServer(t, env.reg, "paused", false)

	servers, err := client.Servers.List(t.Context())
	if err != nil {
		t.Fatalf("Servers.List: %v", err)
	}
	if len(servers) != 3 {
		t.Fatalf("got %d servers: %+v", len(servers), servers)
	}
	// Sorted by ID: broken, github, paused.
	broken, github, paused := servers[0], servers[1], servers[2]

	if github.ID != "github" || github.State != "connected" || github.Tools != 26 {
		t.Errorf("github = %+v", github)
	}
	if github.Health.Level != api.HealthLevelHealthy || github.Health.AdminState != api.AdminStateEnabled {
		t.Errorf("github health = %+v", github.Health)
	}
	if broken.Health.Level != api.HealthLevelUnhealthy || broken.Health.Action != api.ActionRestart {
		t.Errorf("broken health = %+v", broken.Health)
	}
	if paused.State != "unknown" || paused.Enabled {
		t.Errorf("paused = %+v", paused)
	}
	if paused.Health.Level != api.HealthLevelHealthy || paused.Health.Action != api.ActionEnable ||
		paused.Health.AdminState != api.AdminStateDisabled {
		t.Errorf("paused health = %+v", paused.Health)
	}
	if github.Transport != "stdio" || github.Source != "manual" || !github.Enabled {
		t.Errorf("github static fields = %+v", github)
	}
}

func TestServersQuarantineOutranksEnabled(t *testing.T) {
	client, env := startServer(t, func(o *Options) {
		o.States = fakeStates{"github": {Quarantined: true, Conn: ConnConnected}}
	})
	seedServer(t, env.reg, "github", true)

	servers, err := client.Servers.List(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	h := servers[0].Health
	if h.AdminState != api.AdminStateQuarantined || h.Action != api.ActionApprove || h.Level != api.HealthLevelHealthy {
		t.Errorf("health = %+v", h)
	}
}

func openSession(t *testing.T, mgr *session.MemoryManager, clientID string) *session.Session {
	t.Helper()
	s, err := mgr.OpenHTTP(context.Background(), session.SessionHello{ClientID: clientID, Roots: []string{"/w"}})
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestSessionsList(t *testing.T) {
	client, env := startServer(t, nil)
	s := openSession(t, env.mgr, "cursor")

	list, err := client.Sessions.List(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 {
		t.Fatalf("got %d sessions", len(list))
	}
	got := list[0]
	if got.ID != string(s.ID) || got.ClientID != "cursor" || got.Origin != "http" || got.Root != "/w" {
		t.Errorf("session = %+v", got)
	}
	if got.OverlaySummary != "" {
		t.Errorf("unexpected overlay summary %q", got.OverlaySummary)
	}
}

func TestScopeNarrowThenWidenRejected(t *testing.T) {
	client, env := startServer(t, nil)
	s := openSession(t, env.mgr, "cursor")
	id := string(s.ID)

	// Narrow: allowed.
	err := client.Sessions.SetScope(t.Context(), id, api.ScopeNarrow{
		Tools: map[string][]string{"github": {"get_issue", "list_prs"}},
	})
	if err != nil {
		t.Fatalf("narrow: %v", err)
	}
	ov := env.mgr.Overlay(s.ID)
	if ov == nil || ov.Tools["github"] == nil {
		t.Fatalf("overlay not applied: %+v", ov)
	}
	if got := ov.Tools["github"].Allow; len(got) != 2 {
		t.Errorf("allow = %v", got)
	}

	// Widen: rejected 403 with the tighten-only code; overlay unchanged.
	err = client.Sessions.SetScope(t.Context(), id, api.ScopeNarrow{
		Tools: map[string][]string{"github": {"get_issue", "list_prs", "merge_pr"}},
	})
	var apiErr *api.Error
	if !errors.As(err, &apiErr) {
		t.Fatalf("widen err = %v", err)
	}
	if apiErr.Status != http.StatusForbidden || apiErr.Code != CodeTightenOnly {
		t.Errorf("widen: status %d code %s", apiErr.Status, apiErr.Code)
	}
	if apiErr.RequestID == "" {
		t.Error("error carries no request id")
	}
	if got := env.mgr.Overlay(s.ID).Tools["github"].Allow; len(got) != 2 {
		t.Errorf("overlay changed on rejected mutation: %v", got)
	}

	// Disable a server: pure narrowing, allowed.
	if err := client.Sessions.SetScope(t.Context(), id, api.ScopeNarrow{
		DisableServers: []string{"github"},
	}); err != nil {
		t.Fatalf("disable: %v", err)
	}
	sel := env.mgr.Overlay(s.ID).Tools["github"]
	if sel == nil || sel.Allow == nil || len(sel.Allow) != 0 {
		t.Errorf("disable selector = %+v, want block-all (empty allow)", sel)
	}

	// Reset after narrowing = loosening without a grant: rejected.
	err = client.Sessions.SetScope(t.Context(), id, api.ScopeNarrow{Reset: true})
	if !errors.As(err, &apiErr) || apiErr.Code != CodeTightenOnly {
		t.Errorf("reset err = %v", err)
	}

	// Audit trail: one record per write, allowed/denied as it happened.
	recs := env.aud.records()
	if len(recs) != 4 {
		t.Fatalf("audit records = %d, want 4", len(recs))
	}
	wantDecisions := []audit.Decision{
		audit.DecisionAllowed, audit.DecisionDenied, audit.DecisionAllowed, audit.DecisionDenied,
	}
	for i, r := range recs {
		if r.Decision != wantDecisions[i] {
			t.Errorf("rec[%d].Decision = %s, want %s", i, r.Decision, wantDecisions[i])
		}
		if r.Session != id || r.Client != "cursor" || r.Actor != "cli" {
			t.Errorf("rec[%d] identity = %+v", i, r)
		}
		if r.RequestID == "" || r.ArgsHash == "" {
			t.Errorf("rec[%d] missing request id or args hash: %+v", i, r)
		}
		if r.Tool != "sessions/scope" {
			t.Errorf("rec[%d].Tool = %q", i, r.Tool)
		}
	}
}

func TestScopeUnknownSessionUniform404(t *testing.T) {
	client, _ := startServer(t, nil)
	err := client.Sessions.SetScope(t.Context(), "ghost:9", api.ScopeNarrow{Reset: true})
	var apiErr *api.Error
	if !errors.As(err, &apiErr) {
		t.Fatalf("err = %v", err)
	}
	if apiErr.Status != http.StatusNotFound || apiErr.Code != CodeNotFound || apiErr.Message != notFoundMessage {
		t.Errorf("got status=%d code=%s message=%q", apiErr.Status, apiErr.Code, apiErr.Message)
	}
}

// TestUniform404 asserts the anti-probing invariant: unknown route, wrong
// method and unknown resource id all produce byte-identical bodies (with
// the request id pinned).
func TestUniform404(t *testing.T) {
	_, env := startServer(t, nil)
	hc := rawClient(env.sock)

	fetch := func(method, path string) (int, string) {
		t.Helper()
		// The scope route decodes its body before resolving the session, so
		// give every probe a valid JSON body: the 404s under comparison are
		// then route-miss, method-miss and resource-miss respectively.
		var body io.Reader
		if method == http.MethodPost {
			body = strings.NewReader("{}")
		}
		req, err := http.NewRequestWithContext(t.Context(), method, "http://agenthub"+path, body)
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set(api.HeaderRequestID, "pin-404-test")
		resp, err := hc.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = resp.Body.Close() }()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(b)
	}

	stUnknown, bodyUnknown := fetch(http.MethodGet, "/v1/definitely-not-a-route")
	stMethod, bodyMethod := fetch(http.MethodPost, "/v1/ping")
	stResource, bodyResource := fetch(http.MethodPost, "/v1/sessions/nope:1/scope")
	stRoot, bodyRoot := fetch(http.MethodGet, "/")

	for _, st := range []int{stUnknown, stMethod, stResource, stRoot} {
		if st != http.StatusNotFound {
			t.Errorf("status = %d, want 404", st)
		}
	}
	if bodyUnknown != bodyMethod || bodyUnknown != bodyResource || bodyUnknown != bodyRoot {
		t.Errorf("404 bodies differ:\n%q\n%q\n%q\n%q", bodyUnknown, bodyMethod, bodyResource, bodyRoot)
	}
	if !strings.Contains(bodyUnknown, notFoundMessage) || !strings.Contains(bodyUnknown, CodeNotFound) {
		t.Errorf("404 body = %q", bodyUnknown)
	}
}

var hexID = regexp.MustCompile(`^[0-9a-f]{32}$`)

func TestRequestIDEchoGenerateAndBody(t *testing.T) {
	_, env := startServer(t, nil)
	hc := rawClient(env.sock)

	get := func(header string) (*http.Response, map[string]any) {
		t.Helper()
		req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, "http://agenthub/v1/nope", nil)
		if err != nil {
			t.Fatal(err)
		}
		if header != "" {
			req.Header.Set(api.HeaderRequestID, header)
		}
		resp, err := hc.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = resp.Body.Close() }()
		var env map[string]any
		if err := json.NewDecoder(resp.Body).Decode(&env); err != nil {
			t.Fatal(err)
		}
		return resp, env
	}

	// Echo: a well-formed id comes back on the header AND in the error body.
	resp, body := get("my-id-42")
	if got := resp.Header.Get(api.HeaderRequestID); got != "my-id-42" {
		t.Errorf("echo header = %q", got)
	}
	errObj, _ := body["error"].(map[string]any)
	if errObj == nil || errObj["requestId"] != "my-id-42" {
		t.Errorf("error body = %v", body)
	}

	// Generate: no header -> a fresh 32-hex id.
	resp, _ = get("")
	if got := resp.Header.Get(api.HeaderRequestID); !hexID.MatchString(got) {
		t.Errorf("generated id = %q", got)
	}

	// Replace: an unverifiable id (invalid charset / spaces) is never
	// echoed back. (CR/LF injection is already unrepresentable: net/http
	// rejects such header values outright on both ends.)
	hostile := "id with spaces & $(shell)"
	resp, _ = get(hostile)
	if got := resp.Header.Get(api.HeaderRequestID); got == hostile || !hexID.MatchString(got) {
		t.Errorf("hostile id echoed or not replaced: %q", got)
	}
}

func TestAPIVersionRejected(t *testing.T) {
	_, env := startServer(t, nil)
	hc := rawClient(env.sock)
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, "http://agenthub/v1/ping", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set(api.HeaderAPIVersion, "999")
	resp, err := hc.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("status = %d", resp.StatusCode)
	}
	b, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(b), CodeAPIVersion) {
		t.Errorf("body = %s", b)
	}
}

func TestActorHeaderValidation(t *testing.T) {
	_, env := startServer(t, nil)
	s := openSession(t, env.mgr, "cursor")
	hc := rawClient(env.sock)

	post := func(actor string) {
		t.Helper()
		req, err := http.NewRequestWithContext(t.Context(), http.MethodPost,
			"http://agenthub/v1/sessions/"+string(s.ID)+"/scope",
			strings.NewReader(`{"tools":{"gh":["a"]}}`))
		if err != nil {
			t.Fatal(err)
		}
		if actor != "" {
			req.Header.Set(HeaderActor, actor)
		}
		resp, err := hc.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
	}

	post("gui")
	post("gateway:cursor:1")
	post("../../etc/passwd") // invalid -> "cli"
	post("")                 // absent -> "cli"

	recs := env.aud.records()
	if len(recs) != 4 {
		t.Fatalf("records = %d", len(recs))
	}
	want := []string{"gui", "gateway:cursor:1", "cli", "cli"}
	for i, r := range recs {
		if r.Actor != want[i] {
			t.Errorf("rec[%d].Actor = %q, want %q", i, r.Actor, want[i])
		}
	}
}

func TestScopeBadBody(t *testing.T) {
	_, env := startServer(t, nil)
	s := openSession(t, env.mgr, "cursor")
	hc := rawClient(env.sock)
	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost,
		"http://agenthub/v1/sessions/"+string(s.ID)+"/scope", strings.NewReader("{not json"))
	if err != nil {
		t.Fatal(err)
	}
	resp, err := hc.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("status = %d", resp.StatusCode)
	}
}

func TestNewServerValidation(t *testing.T) {
	if _, err := NewServer(Options{}); err == nil {
		t.Error("NewServer accepted empty options")
	}
}
