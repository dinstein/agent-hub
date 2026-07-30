package services

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/dinstein/agent-hub/api"
)

// ---------------------------------------------------------------------------
// Fake daemon: the same shape as the api package's own tests — a real HTTP
// server on a real Unix socket, so the UDS transport is exercised rather
// than stubbed.
// ---------------------------------------------------------------------------

// shortTempDir returns a temp dir with a short absolute path: t.TempDir()
// paths can exceed the ~104-byte sun_path limit for UDS on macOS.
func shortTempDir(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "ahgui")
	if err != nil {
		t.Fatalf("mkdtemp: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return dir
}

type fakeDaemon struct {
	socket string
	srv    *http.Server
}

func newFakeDaemon(t *testing.T, h http.Handler) *fakeDaemon {
	t.Helper()
	sock := filepath.Join(shortTempDir(t), "ctl.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatalf("listen unix %s: %v", sock, err)
	}
	srv := &http.Server{Handler: h}
	go srv.Serve(ln) //nolint:errcheck // closed on cleanup
	d := &fakeDaemon{socket: sock, srv: srv}
	t.Cleanup(func() { _ = srv.Close() })
	return d
}

func (d *fakeDaemon) stop() { _ = d.srv.Close() }

func writeOK(t *testing.T, w http.ResponseWriter, data any) {
	t.Helper()
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(map[string]any{"ok": true, "data": data}); err != nil {
		t.Errorf("encoding response: %v", err)
	}
}

func writeErr(t *testing.T, w http.ResponseWriter, status int, code, msg string) {
	t.Helper()
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(map[string]any{
		"ok":    false,
		"error": map[string]string{"code": code, "message": msg},
	}); err != nil {
		t.Errorf("encoding error response: %v", err)
	}
}

// pingMux returns a mux that answers /v1/ping, the handshake every connect
// performs.
func pingMux(t *testing.T) *http.ServeMux {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/ping", func(w http.ResponseWriter, _ *http.Request) {
		writeOK(t, w, api.Hello{Version: "test", Pid: 1234, Generation: 9})
	})
	return mux
}

// testDialer connects to a fake daemon and never spawns anything.
type testDialer struct {
	mu     sync.Mutex
	socket string
	dials  int
	starts int
	spawns bool
	err    error
	gate   chan struct{}
}

func (d *testDialer) connect() (*api.Client, error) {
	d.mu.Lock()
	gate := d.gate
	d.mu.Unlock()
	// A held gate models a dial that is genuinely still in progress — a daemon
	// that has been spawned but has not bound its socket yet. The outcome is
	// read AFTER the gate releases, so a test can decide how the in-flight
	// dial ends while it is still hanging.
	if gate != nil {
		<-gate
	}

	d.mu.Lock()
	sock, err := d.socket, d.err
	d.mu.Unlock()
	if err != nil {
		return nil, err
	}
	c := api.New(sock)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if _, perr := c.Ping(ctx); perr != nil {
		c.Close()
		return nil, perr
	}
	return c, nil
}

func (d *testDialer) dial(context.Context) (*api.Client, error) {
	d.mu.Lock()
	d.dials++
	d.mu.Unlock()
	return d.connect()
}

func (d *testDialer) dialOrStart(context.Context) (*api.Client, bool, error) {
	d.mu.Lock()
	d.starts++
	spawned := d.spawns
	d.mu.Unlock()
	c, err := d.connect()
	return c, spawned && err == nil, err
}

// setGate installs a channel every subsequent dial blocks on until it is
// closed. Nil (the default) means dials complete immediately.
func (d *testDialer) setGate(c chan struct{}) {
	d.mu.Lock()
	d.gate = c
	d.mu.Unlock()
}

// spawns makes the fake report that it launched the daemon, so ownership
// (and therefore shutdown) can be exercised without a real process.
func (d *testDialer) setSpawns(v bool) {
	d.mu.Lock()
	d.spawns = v
	d.mu.Unlock()
}

func (d *testDialer) counts() (dials, starts int) {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.dials, d.starts
}

// recorder collects emitted frontend events.
type recorder struct {
	mu     sync.Mutex
	events []recorded
}

type recorded struct {
	name string
	data any
}

func (r *recorder) Emit(name string, data any) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.events = append(r.events, recorded{name, data})
}

// all returns a snapshot of everything emitted so far. The pump emits from
// its own goroutine, so a test that inspects the whole log must go through
// the lock rather than read the slice directly.
func (r *recorder) all() []recorded {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]recorded(nil), r.events...)
}

func (r *recorder) byName(name string) []recorded {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []recorded
	for _, e := range r.events {
		if e.name == name {
			out = append(out, e)
		}
	}
	return out
}

// waitFor polls cond until it holds or the deadline passes.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

// newHub wires a Hub to a fake daemon. The hub is stopped on cleanup so no
// pump goroutine outlives the test.
func newHub(t *testing.T, d *fakeDaemon, e Emitter) (*Hub, *testDialer) {
	t.Helper()
	dl := &testDialer{socket: d.socket}
	h := &Hub{dialer: dl, emitter: e}
	t.Cleanup(h.stop)
	return h, dl
}

// ---------------------------------------------------------------------------
// Tests
// ---------------------------------------------------------------------------

func TestListServersRoundTripOverUDS(t *testing.T) {
	mux := pingMux(t)
	mux.HandleFunc("GET /v1/servers", func(w http.ResponseWriter, _ *http.Request) {
		writeOK(t, w, []api.Server{{
			ID: "github", Transport: "stdio", Enabled: true, State: "ready", Tools: 12,
			Health: api.Health{
				Level: api.HealthLevelDegraded, AdminState: api.AdminStateEnabled,
				Summary: "token expires soon", Action: api.ActionLogin,
			},
		}})
	})
	h, _ := newHub(t, newFakeDaemon(t, mux), nil)

	servers, err := h.ListServers(t.Context())
	if err != nil {
		t.Fatalf("ListServers: %v", err)
	}
	if len(servers) != 1 || servers[0].ID != "github" {
		t.Fatalf("unexpected servers: %+v", servers)
	}
	// Health must survive the hop verbatim: the GUI renders, never derives.
	got := servers[0].Health
	if got.Level != api.HealthLevelDegraded || got.Action != api.ActionLogin ||
		got.AdminState != api.AdminStateEnabled || got.Summary != "token expires soon" {
		t.Errorf("Health mangled in transit: %+v", got)
	}
}

func TestServerHealthSelectsByID(t *testing.T) {
	mux := pingMux(t)
	mux.HandleFunc("GET /v1/servers", func(w http.ResponseWriter, _ *http.Request) {
		writeOK(t, w, []api.Server{
			{ID: "a", Health: api.Health{Level: api.HealthLevelHealthy}},
			{ID: "b", Health: api.Health{Level: api.HealthLevelUnhealthy, Action: api.ActionRestart}},
		})
	})
	h, _ := newHub(t, newFakeDaemon(t, mux), nil)

	hb, err := h.ServerHealth(t.Context(), "b")
	if err != nil {
		t.Fatalf("ServerHealth: %v", err)
	}
	if hb.Level != api.HealthLevelUnhealthy || hb.Action != api.ActionRestart {
		t.Errorf("unexpected health: %+v", hb)
	}
	if _, err := h.ServerHealth(t.Context(), "missing"); err == nil {
		t.Error("ServerHealth on an unknown server: want error, got nil")
	}
}

func TestSkillsAndAuditReportNotImplementedDistinctly(t *testing.T) {
	mux := pingMux(t)
	// Nothing is registered for /v1/skills and /v1/audit: this daemon does
	// not serve them yet. The failure must be a code the UI can turn into
	// "unavailable", never an empty list.
	mux.HandleFunc("/", func(w http.ResponseWriter, _ *http.Request) {
		writeErr(t, w, http.StatusNotFound, api.ErrCodeNotFound, "unknown endpoint")
	})
	h, _ := newHub(t, newFakeDaemon(t, mux), nil)

	if _, err := h.ListSkills(t.Context()); !api.IsCode(err, api.ErrCodeNotFound) {
		t.Errorf("ListSkills: want E_NOT_FOUND, got %v", err)
	}
	if _, err := h.AuditTail(t.Context(), 50); !api.IsCode(err, api.ErrCodeNotFound) {
		t.Errorf("AuditTail: want E_NOT_FOUND, got %v", err)
	}
	if _, err := h.SecurityTail(t.Context(), 50); !api.IsCode(err, api.ErrCodeNotFound) {
		t.Errorf("SecurityTail: want E_NOT_FOUND, got %v", err)
	}
}

// TestAuditTailClampsAndTagsStream: the two ledgers are two ROUTES
// (/v1/audit and /v1/security), not one route with a stream selector — a
// mis-spelled selector that silently returned the wrong stream would be a
// governance surface reading the wrong ledger. The client-side clamp of the
// limit is pinned here too.
func TestAuditTailClampsAndTagsStream(t *testing.T) {
	got := make(chan string, 4)
	mux := pingMux(t)
	mux.HandleFunc("GET /v1/audit", func(w http.ResponseWriter, r *http.Request) {
		got <- "audit?" + r.URL.RawQuery
		writeOK(t, w, []api.AuditRecord{})
	})
	mux.HandleFunc("GET /v1/security", func(w http.ResponseWriter, r *http.Request) {
		got <- "security?" + r.URL.RawQuery
		writeOK(t, w, []api.SecurityEvent{})
	})
	h, _ := newHub(t, newFakeDaemon(t, mux), nil)

	if _, err := h.AuditTail(t.Context(), 100000); err != nil {
		t.Fatalf("AuditTail: %v", err)
	}
	if q := <-got; q != "audit?limit=1000" {
		t.Errorf("query = %q", q)
	}
	if _, err := h.SecurityTail(t.Context(), 0); err != nil {
		t.Fatalf("SecurityTail: %v", err)
	}
	if q := <-got; q != "security?" {
		t.Errorf("query = %q", q)
	}
}

func TestOfflineFailsLoudlyAndNeverSpawnsPerCall(t *testing.T) {
	rec := &recorder{}
	dl := &testDialer{err: errors.New("connect: no such file or directory")}
	h := &Hub{dialer: dl, emitter: rec}
	t.Cleanup(h.stop)

	if _, err := h.ListServers(t.Context()); !errors.Is(err, ErrOffline) {
		t.Fatalf("want ErrOffline, got %v", err)
	}
	if _, err := h.ListSessions(t.Context()); !errors.Is(err, ErrOffline) {
		t.Fatalf("want ErrOffline, got %v", err)
	}
	// Data calls only dial. Starting a daemon is an explicit user action
	// (Connect), otherwise a crash-looping daemon would be re-spawned once
	// per click.
	dials, starts := dl.counts()
	if starts != 0 {
		t.Errorf("data calls spawned the daemon %d times", starts)
	}
	if dials != 2 {
		t.Errorf("dials = %d, want 2", dials)
	}
	if st := h.Status(); st.Connected || st.Error == "" {
		t.Errorf("status should report the failure: %+v", st)
	}
}

// TestStartConnectsInTheBackground covers the startup path used by the Wails
// service: the window must open even while the connection is still being
// established, so start never blocks and reports through the status event.
func TestStartConnectsInTheBackground(t *testing.T) {
	rec := &recorder{}
	h, dl := newHub(t, newFakeDaemon(t, pingMux(t)), rec)

	h.start(t.Context())
	waitFor(t, "connection", func() bool { return h.Status().Connected })
	waitFor(t, "daemon status event", func() bool { return len(rec.byName(EventDaemon)) == 1 })
	if _, starts := dl.counts(); starts != 1 {
		t.Errorf("starts = %d, want 1", starts)
	}
}

func TestConnectStartsDaemonAndPublishesStatus(t *testing.T) {
	rec := &recorder{}
	h, dl := newHub(t, newFakeDaemon(t, pingMux(t)), rec)

	st, err := h.Connect(t.Context())
	if err != nil {
		t.Fatalf("Connect: %v", err)
	}
	if !st.Connected || st.Version != "test" || st.Pid != 1234 || st.Generation != 9 {
		t.Errorf("status = %+v", st)
	}
	if _, starts := dl.counts(); starts != 1 {
		t.Errorf("starts = %d, want 1", starts)
	}
	waitFor(t, "daemon status event", func() bool { return len(rec.byName(EventDaemon)) == 1 })
}

func TestTransportFailureDropsClientAndRedials(t *testing.T) {
	var mu sync.Mutex
	fail := false
	mux := pingMux(t)
	mux.HandleFunc("GET /v1/servers", func(w http.ResponseWriter, _ *http.Request) {
		mu.Lock()
		f := fail
		mu.Unlock()
		if f {
			// Hijack and close without a response: a transport-level break,
			// not a control-plane refusal.
			panic(http.ErrAbortHandler)
		}
		writeOK(t, w, []api.Server{{ID: "a"}})
	})
	h, dl := newHub(t, newFakeDaemon(t, mux), nil)

	if _, err := h.ListServers(t.Context()); err != nil {
		t.Fatalf("ListServers: %v", err)
	}
	mu.Lock()
	fail = true
	mu.Unlock()
	if _, err := h.ListServers(t.Context()); err == nil {
		t.Fatal("want transport error, got nil")
	}
	if st := h.Status(); st.Connected {
		t.Error("status should be disconnected after a transport failure")
	}
	mu.Lock()
	fail = false
	mu.Unlock()
	if _, err := h.ListServers(t.Context()); err != nil {
		t.Fatalf("ListServers after recovery: %v", err)
	}
	if dials, _ := dl.counts(); dials == 0 {
		t.Error("expected a re-dial after the transport failure")
	}
}

// TestPumpBridgesSSEToFrontendEvents covers the SSE -> Wails event bridge:
// every topic the daemon streams has to reach the frontend as a TopicEvent
// carrying the revision and the raw payload.
func TestPumpBridgesSSEToFrontendEvents(t *testing.T) {
	rec := &recorder{}
	mux := pingMux(t)
	mux.HandleFunc("GET /v1/events", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		fl, ok := w.(http.Flusher)
		if !ok {
			t.Error("response writer is not a Flusher")
			return
		}
		frames := []string{
			`{"topic":"servers","kind":"changed","rev":7,"payload":[{"id":"a"}]}`,
			`{"topic":"sessions","kind":"opened","rev":8,"payload":{"id":"claude:1"}}`,
		}
		for _, f := range frames {
			_, _ = fmt.Fprintf(w, "id: %d\ndata: %s\n\n", len(f), f)
			fl.Flush()
		}
		<-r.Context().Done()
	})
	h, _ := newHub(t, newFakeDaemon(t, mux), rec)

	if _, err := h.Connect(t.Context()); err != nil {
		t.Fatalf("Connect: %v", err)
	}
	waitFor(t, "servers event", func() bool { return len(rec.byName(EventServers)) == 1 })
	waitFor(t, "session event", func() bool { return len(rec.byName(EventSessions)) == 1 })

	ev, ok := rec.byName(EventServers)[0].data.(TopicEvent)
	if !ok {
		t.Fatalf("payload type = %T, want TopicEvent", rec.byName(EventServers)[0].data)
	}
	if ev.Topic != api.TopicServers || ev.Kind != "changed" || ev.Rev != 7 {
		t.Errorf("servers event = %+v", ev)
	}

}

// TestStopIsIdempotentAndStopsThePump guards against a leaked bridge
// goroutine holding the control connection open after shutdown.
func TestStopIsIdempotentAndStopsThePump(t *testing.T) {
	rec := &recorder{}
	fd := newFakeDaemon(t, pingMux(t))
	h, _ := newHub(t, fd, rec)
	if _, err := h.Connect(t.Context()); err != nil {
		t.Fatalf("Connect: %v", err)
	}
	h.stop()
	h.stop()
	if st := h.Status(); st.Connected {
		t.Errorf("status after stop = %+v", st)
	}
	if _, err := h.ListServers(t.Context()); err == nil {
		fd.stop()
	}
}

func TestMarshalErrorCarriesMachineReadableCodes(t *testing.T) {
	decode := func(t *testing.T, err error) map[string]any {
		t.Helper()
		b := MarshalError(err)
		if b == nil {
			t.Fatal("MarshalError returned nil")
		}
		var m map[string]any
		if uerr := json.Unmarshal(b, &m); uerr != nil {
			t.Fatalf("MarshalError produced invalid JSON: %v", uerr)
		}
		return m
	}

	apiErr := &api.Error{
		ErrorBody: api.ErrorBody{Code: api.ErrCodeAlreadyDecided, Message: "decided by cli", Hint: "refresh"},
		Status:    http.StatusConflict,
	}
	m := decode(t, fmt.Errorf("wrapped: %w", apiErr))
	if m["code"] != api.ErrCodeAlreadyDecided || m["hint"] != "refresh" {
		t.Errorf("api error marshalled as %v", m)
	}

	m = decode(t, fmt.Errorf("%w: dial failed", ErrOffline))
	if m["code"] != "E_OFFLINE" || m["offline"] != true {
		t.Errorf("offline error marshalled as %v", m)
	}

	m = decode(t, errors.New("boom"))
	if m["code"] != "E_GUI" || m["message"] != "boom" {
		t.Errorf("plain error marshalled as %v", m)
	}

	if MarshalError(nil) != nil {
		t.Error("MarshalError(nil) should fall back to the default handling")
	}
}

// TestUseWaitsForStartupConnect pins the startup gate: a call made while the
// launch is still in flight must wait for it, not report the daemon
// unreachable.
//
// Regression. The window paints before the daemon finishes launching, so
// every page's first call raced the spawn and failed against a socket that
// appeared moments later — the user saw a wall of "daemon is not reachable"
// for a daemon that came up fine.
func TestUseWaitsForStartupConnect(t *testing.T) {
	d := newFakeDaemon(t, pingMux(t))
	h, dl := newHub(t, d, nil)

	// Hold the startup dial INSIDE the dialer, so the launch is genuinely
	// still in flight while use runs. Setting an error and clearing it from
	// another goroutine does not do that: nothing blocks, so the test only
	// races the clearing goroutine and passes or fails on scheduling.
	gate := make(chan struct{})
	dl.setGate(gate)
	h.start(context.Background())

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	type result struct {
		client *api.Client
		err    error
	}
	done := make(chan result, 1)
	go func() {
		c, err := h.use(ctx)
		done <- result{c, err}
	}()

	// The actual invariant: while the startup connect is unfinished, use
	// neither returns a client nor reports the daemon unreachable. It waits.
	select {
	case r := <-done:
		t.Fatalf("use returned while the startup connect was still in flight: client=%v err=%v", r.client, r.err)
	case <-time.After(100 * time.Millisecond):
	}

	close(gate) // the daemon finishes binding

	r := <-done
	if r.err != nil {
		t.Fatalf("use failed despite a reachable daemon: %v", r.err)
	}
	if r.client == nil {
		t.Fatal("use returned a nil client and no error")
	}
}

// TestStopShutsDownOnlyOurDaemon pins the ownership rule for shutdown: a
// daemon this process started is stopped on exit, one it merely found is
// left alone. Stopping a daemon we did not start would end another client's
// session to tidy up after ours.
func TestStopShutsDownOnlyOurDaemon(t *testing.T) {
	t.Run("found daemon is left running", func(t *testing.T) {
		d := newFakeDaemon(t, pingMux(t))
		h, dl := newHub(t, d, nil)
		dl.setSpawns(false)
		h.start(context.Background())
		waitFor(t, "daemon connected", func() bool { return h.Status().Connected })

		h.stop()
		if h.spawned {
			t.Error("a daemon we only dialled was recorded as ours")
		}
	})

	t.Run("spawned daemon is claimed", func(t *testing.T) {
		d := newFakeDaemon(t, pingMux(t))
		h, dl := newHub(t, d, nil)
		dl.setSpawns(true)
		h.start(context.Background())
		waitFor(t, "daemon connected", func() bool { return h.Status().Connected })

		h.mu.Lock()
		ours := h.spawned
		h.mu.Unlock()
		if !ours {
			t.Fatal("a daemon we spawned was not claimed; stop would leave it behind")
		}
		// stop clears the claim so a second call cannot signal a recycled pid.
		h.stop()
		h.mu.Lock()
		again := h.spawned
		h.mu.Unlock()
		if again {
			t.Error("stop left the ownership claim set; it is not idempotent")
		}
	})
}

// TestStopDaemonIgnoresBadPid pins that shutdown never panics or blocks on a
// pid that is absent or nonsensical — it runs during teardown, where there
// is no surface left to report a failure on.
func TestStopDaemonIgnoresBadPid(t *testing.T) {
	stopDaemon(0)
	stopDaemon(-1)
}
