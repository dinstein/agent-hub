package services

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"

	"github.com/dinstein/agent-hub/api"
)

// ---------------------------------------------------------------------------
// Shared harness for the bound control-plane methods.
//
// Every bound method is one control-plane call, so each is covered along the
// same four axes: it reaches the right endpoint, it carries the precondition
// where one exists, a 409 stale precondition becomes something the page can
// act on, a validation refusal keeps the connection, and an unreachable
// daemon fails loudly instead of returning an empty result.
//
// The fake daemon is a real HTTP server on a real Unix socket (see
// hub_test.go), so the UDS transport is exercised rather than stubbed.
// ---------------------------------------------------------------------------

// Generations used throughout: the page read at readGen and the daemon
// answers a write at writtenGen. They differ so a test cannot pass by
// echoing the request back.
const (
	readGen    = 7
	writtenGen = 8
	// conflictGen is where the registry "really" stands when a write loses
	// its compare-and-swap.
	conflictGen = 42
)

// ctlReq is one recorded request.
type ctlReq struct {
	method string
	path   string
	query  string
	body   []byte
}

// ctlRecorder is a fake daemon that records every non-ping request and
// answers it according to the mode currently installed.
type ctlRecorder struct {
	mu   sync.Mutex
	reqs []ctlReq
	// respond writes the answer. Swapped between phases of a test.
	respond func(t *testing.T, w http.ResponseWriter, data any)
	// data is the success payload of the case being exercised.
	data any
}

// okMode answers 200 with the case's payload.
func okMode(t *testing.T, w http.ResponseWriter, data any) { writeOK(t, w, data) }

// conflictMode answers the optimistic-concurrency refusal: 409 with
// E_STALE_PRECONDITION and the generation the registry now stands at.
func conflictMode(t *testing.T, w http.ResponseWriter, _ any) {
	t.Helper()
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusConflict)
	if err := json.NewEncoder(w).Encode(map[string]any{
		"ok": false,
		"error": map[string]any{
			"code":       api.ErrCodeStalePrecondition,
			"message":    "the registry moved between your read and this write",
			"generation": conflictGen,
		},
	}); err != nil {
		t.Errorf("encoding conflict: %v", err)
	}
}

// badRequestMode answers the validation refusal. Validation lives in
// internal/confops, so this is the ONLY place a bad form is caught — the
// service layer must forward it, not pre-empt it.
func badRequestMode(t *testing.T, w http.ResponseWriter, _ any) {
	writeErr(t, w, http.StatusBadRequest, api.ErrCodeBadRequest, "id must not be empty")
}

func (rec *ctlRecorder) serve(t *testing.T) http.Handler {
	t.Helper()
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/ping":
			writeOK(t, w, api.Hello{Version: "test", Pid: 1234, Generation: readGen})
			return
		case "/v1/events":
			// The SSE bridge comes up with the connection. Hold it open and
			// record nothing: it is infrastructure, not a bound call.
			w.Header().Set("Content-Type", "text/event-stream")
			w.WriteHeader(http.StatusOK)
			<-r.Context().Done()
			return
		}
		body, _ := io.ReadAll(r.Body)
		rec.mu.Lock()
		rec.reqs = append(rec.reqs, ctlReq{
			method: r.Method, path: r.URL.Path, query: r.URL.RawQuery, body: body,
		})
		respond, data := rec.respond, rec.data
		rec.mu.Unlock()
		respond(t, w, data)
	})
}

func (rec *ctlRecorder) setMode(respond func(*testing.T, http.ResponseWriter, any), data any) {
	rec.mu.Lock()
	defer rec.mu.Unlock()
	rec.respond, rec.data = respond, data
}

// last returns the most recent recorded request.
func (rec *ctlRecorder) last(t *testing.T) ctlReq {
	t.Helper()
	rec.mu.Lock()
	defer rec.mu.Unlock()
	if len(rec.reqs) == 0 {
		t.Fatal("the daemon was never called")
	}
	return rec.reqs[len(rec.reqs)-1]
}

// ctlCase pins one bound method to the control-plane call behind it.
type ctlCase struct {
	name   string
	method string
	path   string
	// query is the expected raw query string ("" = none). Write cases get
	// the precondition appended automatically.
	query string
	// data is the daemon's success payload. Write cases may leave it nil:
	// the runner substitutes a WriteResult carrying writtenGen.
	data any
	// invoke calls the bound method. It returns the generation the answer
	// carried (0 when the surface has none).
	invoke func(ctx context.Context, h *Hub) (uint64, error)
	// guarded marks a write that carries expectedGeneration and can lose a
	// compare-and-swap.
	guarded bool
}

// wantQuery is the query the daemon should see for this case.
func (c ctlCase) wantQuery() string {
	if !c.guarded {
		return c.query
	}
	pre := "expected_generation=7"
	if c.query == "" {
		return pre
	}
	// url.Values.Encode sorts keys, and "expected_generation" sorts first
	// against every parameter this API uses.
	return pre + "&" + c.query
}

// writeAnswer is the generic success payload of a guarded write: only the
// WriteResult tail is asserted, and every *Write type embeds it.
func writeAnswer() any {
	return map[string]any{"generation": writtenGen, "changed": true}
}

// runCtlCases exercises every case along the four axes described at the top
// of this file.
func runCtlCases(t *testing.T, cases []ctlCase) {
	t.Helper()
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			data := tc.data
			if data == nil {
				data = writeAnswer()
			}
			rec := &ctlRecorder{respond: okMode, data: data}
			h, dl := newHub(t, newFakeDaemon(t, rec.serve(t)), nil)

			// 1. Success: the right endpoint, and the precondition on it.
			gen, err := tc.invoke(t.Context(), h)
			if err != nil {
				t.Fatalf("%s: %v", tc.name, err)
			}
			got := rec.last(t)
			if got.method != tc.method || got.path != tc.path {
				t.Errorf("called %s %s, want %s %s", got.method, got.path, tc.method, tc.path)
			}
			if got.query != tc.wantQuery() {
				t.Errorf("query = %q, want %q", got.query, tc.wantQuery())
			}
			if tc.guarded && gen != writtenGen {
				// Without this the page has no way to continue editing
				// except by re-reading, which is the poll §6 rules out.
				t.Errorf("answer generation = %d, want %d", gen, writtenGen)
			}

			// 2. Validation refusal: forwarded with its code, and the
			// connection survives it — the daemon answered, it just said no.
			rec.setMode(badRequestMode, nil)
			before, _ := dl.counts()
			if _, err := tc.invoke(t.Context(), h); !api.IsCode(err, api.ErrCodeBadRequest) {
				t.Errorf("validation failure: want E_BAD_REQUEST, got %v", err)
			}
			if dials, _ := dl.counts(); dials != before {
				t.Errorf("re-dialed %d times after a refusal", dials-before)
			}

			// 3. Stale precondition: only guarded writes can lose one.
			rec.setMode(conflictMode, nil)
			_, err = tc.invoke(t.Context(), h)
			assertConflict(t, err, tc.guarded)
			if dials, _ := dl.counts(); dials != before {
				t.Errorf("re-dialed %d times after a conflict", dials-before)
			}

			// 4. Daemon offline: loud, and never an empty result.
			off := &Hub{dialer: &testDialer{err: errors.New("connect: no such file or directory")}}
			t.Cleanup(off.stop)
			if _, err := tc.invoke(t.Context(), off); !errors.Is(err, ErrOffline) {
				t.Errorf("offline: want ErrOffline, got %v", err)
			}
		})
	}
}

// assertConflict checks the conflict contract the frontend depends on: the
// typed error, and the marshalled shape a page branches on.
func assertConflict(t *testing.T, err error, guarded bool) {
	t.Helper()
	if !guarded {
		// An unguarded surface has no generation to be stale against, so a
		// 409 from it must never be dressed up as one — "reload and retry"
		// would be advice that cannot work.
		if api.IsConflict(err) {
			t.Errorf("an unguarded call reported an optimistic-concurrency conflict: %v", err)
		}
		return
	}
	if !api.IsConflict(err) {
		t.Fatalf("want *api.ConflictError, got %v", err)
	}
	var m map[string]any
	if uerr := json.Unmarshal(MarshalError(err), &m); uerr != nil {
		t.Fatalf("MarshalError produced invalid JSON: %v", uerr)
	}
	if m["kind"] != ErrorKindConflict {
		t.Errorf("kind = %v, want %q", m["kind"], ErrorKindConflict)
	}
	if m["currentGeneration"] != float64(conflictGen) {
		t.Errorf("currentGeneration = %v, want %d", m["currentGeneration"], conflictGen)
	}
	if m["code"] != api.ErrCodeStalePrecondition {
		t.Errorf("code = %v", m["code"])
	}
	if m["hint"] == "" || m["hint"] == nil {
		t.Error("a conflict must arrive with a hint: the page has to tell the user what happened")
	}
}

// ---------------------------------------------------------------------------
// Registry-backed surfaces
// ---------------------------------------------------------------------------

func TestRegistryBoundMethods(t *testing.T) {
	spec := api.ServerSpec{ID: "github", Entry: api.ServerEntry{
		Transport: api.TransportStdio, Command: "npx", Enabled: true,
	}}
	runCtlCases(t, []ctlCase{
		// Servers.
		{
			name: "GetServer", method: http.MethodGet, path: "/v1/servers/github",
			data: api.ServerDetail{Generation: readGen, ID: "github"},
			invoke: func(ctx context.Context, h *Hub) (uint64, error) {
				d, err := h.GetServer(ctx, "github")
				return d.Generation, err
			},
		},
		{
			name: "CreateServer", method: http.MethodPost, path: "/v1/servers", guarded: true,
			invoke: func(ctx context.Context, h *Hub) (uint64, error) {
				w, err := h.CreateServer(ctx, spec, readGen)
				return w.Generation, err
			},
		},
		{
			name: "UpdateServer", method: http.MethodPatch, path: "/v1/servers/github", guarded: true,
			invoke: func(ctx context.Context, h *Hub) (uint64, error) {
				w, err := h.UpdateServer(ctx, spec, readGen)
				return w.Generation, err
			},
		},
		{
			name: "DeleteServer", method: http.MethodDelete, path: "/v1/servers/github", guarded: true,
			invoke: func(ctx context.Context, h *Hub) (uint64, error) {
				w, err := h.DeleteServer(ctx, "github", readGen)
				return w.Generation, err
			},
		},
		{
			name: "SetServerEnabled", method: http.MethodPatch, path: "/v1/servers/github", guarded: true,
			invoke: func(ctx context.Context, h *Hub) (uint64, error) {
				w, err := h.SetServerEnabled(ctx, "github", false, readGen)
				return w.Generation, err
			},
		},
		{
			name: "TestServer", method: http.MethodPost, path: "/v1/servers/github/test",
			data: api.ServerTestResult{Server: "github", ToolCount: 3},
			invoke: func(ctx context.Context, h *Hub) (uint64, error) {
				_, err := h.TestServer(ctx, "github", api.ServerTestRequest{Tool: "search"})
				return 0, err
			},
		},

		// Profiles.
		{
			name: "ListProfiles", method: http.MethodGet, path: "/v1/profiles",
			data: api.ProfileList{Generation: readGen, Active: "dev", ActiveKnown: true},
			invoke: func(ctx context.Context, h *Hub) (uint64, error) {
				l, err := h.ListProfiles(ctx)
				return l.Generation, err
			},
		},
		{
			name: "CreateProfile", method: http.MethodPost, path: "/v1/profiles", guarded: true,
			invoke: func(ctx context.Context, h *Hub) (uint64, error) {
				w, err := h.CreateProfile(ctx, "dev", &[]string{"github"}, readGen)
				return w.Generation, err
			},
		},
		{
			name: "RenameProfile", method: http.MethodPatch, path: "/v1/profiles/dev", guarded: true,
			invoke: func(ctx context.Context, h *Hub) (uint64, error) {
				w, err := h.RenameProfile(ctx, "dev", "work", readGen)
				return w.Generation, err
			},
		},
		{
			name: "SetProfileServers", method: http.MethodPatch, path: "/v1/profiles/dev", guarded: true,
			invoke: func(ctx context.Context, h *Hub) (uint64, error) {
				w, err := h.SetProfileServers(ctx, "dev",
					api.ServerSetEdit{Mode: api.ServerSetAdd, Servers: []string{"fs"}}, readGen)
				return w.Generation, err
			},
		},
		{
			name: "SetProfileTools", method: http.MethodPatch, path: "/v1/profiles/dev", guarded: true,
			invoke: func(ctx context.Context, h *Hub) (uint64, error) {
				w, err := h.SetProfileTools(ctx, "dev", "fs", api.OnlyTools("read_file"), readGen)
				return w.Generation, err
			},
		},
		{
			name: "SetActiveProfile", method: http.MethodPatch, path: "/v1/profiles/dev", guarded: true,
			invoke: func(ctx context.Context, h *Hub) (uint64, error) {
				w, err := h.SetActiveProfile(ctx, "dev", readGen)
				return w.Generation, err
			},
		},
		{
			name: "ClearActiveProfile", method: http.MethodPatch, path: "/v1/profiles/dev", guarded: true,
			invoke: func(ctx context.Context, h *Hub) (uint64, error) {
				w, err := h.ClearActiveProfile(ctx, "dev", readGen)
				return w.Generation, err
			},
		},
		{
			name: "DeleteProfile", method: http.MethodDelete, path: "/v1/profiles/dev", guarded: true,
			invoke: func(ctx context.Context, h *Hub) (uint64, error) {
				w, err := h.DeleteProfile(ctx, "dev", readGen)
				return w.Generation, err
			},
		},

		// Client scope bindings.
		{
			name: "GetScope", method: http.MethodGet, path: "/v1/scope/claude",
			data: api.ScopeDetail{Generation: readGen, Client: "claude", Exists: true},
			invoke: func(ctx context.Context, h *Hub) (uint64, error) {
				d, err := h.GetScope(ctx, "claude")
				return d.Generation, err
			},
		},
		{
			name: "SetScope", method: http.MethodPut, path: "/v1/scope/claude", guarded: true,
			invoke: func(ctx context.Context, h *Hub) (uint64, error) {
				w, err := h.SetScope(ctx, "claude",
					api.ClientBinding{Servers: &[]string{"github"}}, readGen)
				return w.Generation, err
			},
		},
		{
			name: "ClearScope", method: http.MethodDelete, path: "/v1/scope/claude", guarded: true,
			invoke: func(ctx context.Context, h *Hub) (uint64, error) {
				w, err := h.ClearScope(ctx, "claude", readGen)
				return w.Generation, err
			},
		},

		// Governance switches.
		{
			name: "ConfigKeys", method: http.MethodGet, path: "/v1/config",
			data: api.GovernanceList{Generation: readGen},
			invoke: func(ctx context.Context, h *Hub) (uint64, error) {
				l, err := h.ConfigKeys(ctx)
				return l.Generation, err
			},
		},
		{
			name: "SetConfig", method: http.MethodPut, path: "/v1/config/denyDestructive", guarded: true,
			invoke: func(ctx context.Context, h *Hub) (uint64, error) {
				w, err := h.SetConfig(ctx, "denyDestructive", "true", readGen)
				return w.Generation, err
			},
		},

		// Tool-level governance and the quarantine.
		{
			name: "ListTools", method: http.MethodGet, path: "/v1/tools", query: "server=github",
			data: api.ToolList{Generation: readGen},
			invoke: func(ctx context.Context, h *Hub) (uint64, error) {
				l, err := h.ListTools(ctx, "github")
				return l.Generation, err
			},
		},
		{
			name:   "SetToolEnabled",
			method: http.MethodPut, path: "/v1/tools/github/create_issue", guarded: true,
			invoke: func(ctx context.Context, h *Hub) (uint64, error) {
				w, err := h.SetToolEnabled(ctx, "github", "create_issue", false, readGen)
				return w.Generation, err
			},
		},
		{
			name:   "SetToolOverride",
			method: http.MethodPut, path: "/v1/tools/github/create_issue", guarded: true,
			invoke: func(ctx context.Context, h *Hub) (uint64, error) {
				desc := "neutralized"
				w, err := h.SetToolOverride(ctx, "github", "create_issue",
					api.ToolOverride{Description: &desc}, readGen)
				return w.Generation, err
			},
		},
		{
			name: "ListQuarantine", method: http.MethodGet, path: "/v1/quarantine",
			data: api.QuarantineList{Generation: readGen},
			invoke: func(ctx context.Context, h *Hub) (uint64, error) {
				l, err := h.ListQuarantine(ctx)
				return l.Generation, err
			},
		},
		{
			name:   "ReleaseQuarantine",
			method: http.MethodDelete, path: "/v1/quarantine/github__create_issue", guarded: true,
			invoke: func(ctx context.Context, h *Hub) (uint64, error) {
				w, err := h.ReleaseQuarantine(ctx, "github__create_issue", readGen)
				return w.Generation, err
			},
		},
	})
}

// TestUnguardedWriteSendsNoPrecondition pins the "0 means do not check"
// spelling: absence, not "expected_generation=0". The two are opposite
// instructions and a page that sends the wrong one would either overwrite a
// concurrent edit or never be able to write at all.
func TestUnguardedWriteSendsNoPrecondition(t *testing.T) {
	rec := &ctlRecorder{respond: okMode, data: writeAnswer()}
	h, _ := newHub(t, newFakeDaemon(t, rec.serve(t)), nil)

	if _, err := h.SetConfig(t.Context(), "discovery", "grouped", 0); err != nil {
		t.Fatalf("SetConfig: %v", err)
	}
	if q := rec.last(t).query; q != "" {
		t.Errorf("query = %q, want empty: 0 is spelled by absence", q)
	}
}

// TestThreeStateBodiesSurviveTheBinding covers the values a naive form would
// collapse. "block everything" and "no rule" are opposite instructions that
// share a Go zero value, so they are carried by pointers all the way to the
// wire; if the service layer ever flattened one, the fail-open direction is
// the one that would win silently.
func TestThreeStateBodiesSurviveTheBinding(t *testing.T) {
	rec := &ctlRecorder{respond: okMode, data: writeAnswer()}
	h, _ := newHub(t, newFakeDaemon(t, rec.serve(t)), nil)
	ctx := t.Context()

	// A profile created with an explicit empty membership is block-all.
	if _, err := h.CreateProfile(ctx, "locked", &[]string{}, readGen); err != nil {
		t.Fatalf("CreateProfile: %v", err)
	}
	if body := string(rec.last(t).body); !strings.Contains(body, `"servers":[]`) {
		t.Errorf("block-all collapsed into no-narrowing: %s", body)
	}
	// A profile created without an answer to the question has no narrowing.
	if _, err := h.CreateProfile(ctx, "open", nil, readGen); err != nil {
		t.Fatalf("CreateProfile(nil): %v", err)
	}
	if body := string(rec.last(t).body); strings.Contains(body, `"servers"`) {
		t.Errorf("no-narrowing sent a servers key: %s", body)
	}

	// The same distinction on a client binding.
	if _, err := h.SetScope(ctx, "claude", api.ClientBinding{Servers: &[]string{}}, readGen); err != nil {
		t.Fatalf("SetScope: %v", err)
	}
	if body := string(rec.last(t).body); !strings.Contains(body, `"servers":[]`) {
		t.Errorf("block-all collapsed in the binding: %s", body)
	}
	// Amending only the discovery mode must not mention the server rule at
	// all: an absent field is "leave it alone".
	mode := "grouped"
	if _, err := h.SetScope(ctx, "claude", api.ClientBinding{Discovery: &mode}, readGen); err != nil {
		t.Fatalf("SetScope(discovery): %v", err)
	}
	if body := string(rec.last(t).body); strings.Contains(body, `"servers"`) {
		t.Errorf("an amend reset the rule it never mentioned: %s", body)
	}

	// Profile tool selectors: "none" must not encode as "all".
	if _, err := h.SetProfileTools(ctx, "dev", "fs", api.NoTools(), readGen); err != nil {
		t.Fatalf("SetProfileTools: %v", err)
	}
	if body := string(rec.last(t).body); !strings.Contains(body, `"mode":"none"`) {
		t.Errorf("NoTools did not survive: %s", body)
	}
}

// TestConflictRetryUsesTheReportedGeneration walks the loop a page is meant
// to run: write with a stale generation, lose, take currentGeneration out of
// the marshalled error, retry with it, win. Nothing was written by the
// losing attempt, and the retry is not blind.
func TestConflictRetryUsesTheReportedGeneration(t *testing.T) {
	rec := &ctlRecorder{respond: conflictMode}
	h, _ := newHub(t, newFakeDaemon(t, rec.serve(t)), nil)

	_, err := h.SetConfig(t.Context(), "denyDestructive", "false", readGen)
	if !api.IsConflict(err) {
		t.Fatalf("want a conflict, got %v", err)
	}
	var m map[string]any
	if uerr := json.Unmarshal(MarshalError(err), &m); uerr != nil {
		t.Fatalf("MarshalError: %v", uerr)
	}
	next, ok := m["currentGeneration"].(float64)
	if !ok {
		t.Fatal("the page has nothing to retry with")
	}

	rec.setMode(okMode, writeAnswer())
	w, err := h.SetConfig(t.Context(), "denyDestructive", "false", uint64(next))
	if err != nil {
		t.Fatalf("retry: %v", err)
	}
	if q := rec.last(t).query; q != "expected_generation=42" {
		t.Errorf("retry query = %q, want the generation the conflict reported", q)
	}
	if w.Generation != writtenGen {
		t.Errorf("retry answered generation %d, want %d", w.Generation, writtenGen)
	}
}

// TestWritesEmitNoFrontendEvents: the only thing that reaches the webview as
// an event is what came off the SSE stream. A write answers its own caller
// and stays out of the event bus — otherwise "I changed it" and "someone
// else changed it" would arrive on two different paths with two different
// shapes, which is exactly what docs/modules/controlplane.md rules out.
func TestWritesEmitNoFrontendEvents(t *testing.T) {
	r := &recorder{}
	rec := &ctlRecorder{respond: okMode, data: writeAnswer()}
	h, _ := newHub(t, newFakeDaemon(t, rec.serve(t)), r)
	ctx := t.Context()

	if _, err := h.SetConfig(ctx, "denyDestructive", "true", readGen); err != nil {
		t.Fatalf("SetConfig: %v", err)
	}
	if _, err := h.DeleteServer(ctx, "github", readGen); err != nil {
		t.Fatalf("DeleteServer: %v", err)
	}
	for _, name := range []string{EventServers, EventSessions, EventApprovals, EventActivity, EventSkills} {
		if got := r.byName(name); len(got) != 0 {
			t.Errorf("a write emitted %d %s events", len(got), name)
		}
	}
}
