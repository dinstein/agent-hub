package ctlapi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/dinstein/agent-hub/internal/confops"
	"github.com/dinstein/agent-hub/internal/event"
	"github.com/dinstein/agent-hub/internal/registry"
	"github.com/dinstein/agent-hub/internal/session"
)

// adminResp is one decoded control-plane response.
type adminResp struct {
	status int
	OK     bool            `json:"ok"`
	Data   json.RawMessage `json:"data"`
	Error  *struct {
		Code       string `json:"code"`
		Message    string `json:"message"`
		Hint       string `json:"hint"`
		RequestID  string `json:"requestId"`
		Generation uint64 `json:"generation"`
	} `json:"error"`
	raw []byte
}

// decode unmarshals the success payload into v.
func (a adminResp) decode(t *testing.T, v any) {
	t.Helper()
	if !a.OK {
		t.Fatalf("response was not ok: %s", a.raw)
	}
	if err := json.Unmarshal(a.Data, v); err != nil {
		t.Fatalf("decoding data %s: %v", a.Data, err)
	}
}

// doAdmin issues one request over the control socket. body may be nil (no
// body), a []byte / string (sent verbatim) or any JSON-marshalable value.
// Every request carries the GUI actor header, which is what the audit
// assertions below check for.
func doAdmin(t *testing.T, sock, method, path string, body any) adminResp {
	t.Helper()
	var rdr io.Reader
	switch b := body.(type) {
	case nil:
	case []byte:
		rdr = bytes.NewReader(b)
	case string:
		rdr = bytes.NewReader([]byte(b))
	default:
		raw, err := json.Marshal(b)
		if err != nil {
			t.Fatal(err)
		}
		rdr = bytes.NewReader(raw)
	}
	req, err := http.NewRequestWithContext(t.Context(), method, "http://unix"+path, rdr)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set(HeaderActor, "gui")
	resp, err := rawClient(sock).Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	out := adminResp{status: resp.StatusCode, raw: raw}
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("%s %s: undecodable envelope %q: %v", method, path, raw, err)
	}
	return out
}

// wantErr asserts a failure envelope with the given status and code.
func (a adminResp) wantErr(t *testing.T, status int, code string) {
	t.Helper()
	if a.status != status || a.Error == nil || a.Error.Code != code {
		t.Fatalf("got status %d body %s, want status %d code %s", a.status, a.raw, status, code)
	}
}

// adminServer boots a control plane with the state and logs directories
// wired, which is what the daemon does in production.
func adminServer(t *testing.T, mutate func(*Options)) (*testEnv, string, string) {
	t.Helper()
	dir := t.TempDir()
	stateDir := filepath.Join(dir, "state")
	logsDir := filepath.Join(dir, "logs")
	_, env := startServer(t, func(o *Options) {
		o.StateDir = stateDir
		o.LogsDir = logsDir
		if mutate != nil {
			mutate(o)
		}
	})
	return env, stateDir, logsDir
}

// stdioEntry is a minimal valid stdio server entry.
func stdioEntry() map[string]any {
	return map[string]any{"transport": "stdio", "command": "fake", "enabled": true}
}

// TestAdminErrorCodeContract pins the cross-package agreement stated in
// admin.go: the wire code for a lost compare-and-swap is the SAME string
// confops freezes, so the CLI's envelope and the control plane's error body
// cannot drift apart.
func TestAdminErrorCodeContract(t *testing.T) {
	if CodeStalePrecondition != confops.CodeStalePrecondition {
		t.Errorf("ctlapi %q != confops %q", CodeStalePrecondition, confops.CodeStalePrecondition)
	}
}

func TestServerCreateReadPatchDelete(t *testing.T) {
	env, _, _ := adminServer(t, nil)

	created := doAdmin(t, env.sock, http.MethodPost, "/v1/servers", map[string]any{
		"id":    "github",
		"entry": stdioEntry(),
	})
	if created.status != http.StatusOK {
		t.Fatalf("create: %d %s", created.status, created.raw)
	}
	var cw struct {
		Generation uint64 `json:"generation"`
		Changed    bool   `json:"changed"`
		ID         string `json:"id"`
	}
	created.decode(t, &cw)
	if cw.ID != "github" || !cw.Changed || cw.Generation == 0 {
		t.Fatalf("create result = %+v", cw)
	}
	if _, ok := env.reg.Snapshot().Servers.V.Servers["github"]; !ok {
		t.Fatal("server not in registry")
	}

	got := doAdmin(t, env.sock, http.MethodGet, "/v1/servers/github", nil)
	var detail struct {
		Generation uint64               `json:"generation"`
		ID         string               `json:"id"`
		Entry      registry.ServerEntry `json:"entry"`
	}
	got.decode(t, &detail)
	if detail.Entry.Command != "fake" || detail.ID != "github" {
		t.Fatalf("detail = %+v", detail)
	}
	if detail.Generation != cw.Generation {
		t.Errorf("detail generation %d, want %d", detail.Generation, cw.Generation)
	}

	patched := doAdmin(t, env.sock, http.MethodPatch, "/v1/servers/github", map[string]any{
		"expected_generation": detail.Generation,
		"entry":               map[string]any{"enabled": false},
	})
	if patched.status != http.StatusOK {
		t.Fatalf("patch: %d %s", patched.status, patched.raw)
	}
	entry := env.reg.Snapshot().Servers.V.Servers["github"].V
	if entry.Enabled {
		t.Error("patch did not disable the server")
	}
	if entry.Command != "fake" {
		t.Errorf("patch dropped an unmentioned field: %+v", entry)
	}

	deleted := doAdmin(t, env.sock, http.MethodDelete, "/v1/servers/github", nil)
	if deleted.status != http.StatusOK {
		t.Fatalf("delete: %d %s", deleted.status, deleted.raw)
	}
	if _, ok := env.reg.Snapshot().Servers.V.Servers["github"]; ok {
		t.Error("server still in registry after delete")
	}
}

// A patch that mentions a map field REPLACES it. Merging would make a leaked
// environment variable impossible to remove through this endpoint.
func TestServerPatchReplacesMapsWholesale(t *testing.T) {
	env, _, _ := adminServer(t, nil)
	entry := stdioEntry()
	entry["env"] = map[string]string{"A": "1", "B": "2"}
	doAdmin(t, env.sock, http.MethodPost, "/v1/servers", map[string]any{"id": "s", "entry": entry})

	res := doAdmin(t, env.sock, http.MethodPatch, "/v1/servers/s", map[string]any{
		"entry": map[string]any{"env": map[string]string{"A": "3"}},
	})
	if res.status != http.StatusOK {
		t.Fatalf("patch: %d %s", res.status, res.raw)
	}
	got := env.reg.Snapshot().Servers.V.Servers["s"].V.Env
	if len(got) != 1 || got["A"] != "3" {
		t.Errorf("env = %v, want only A=3", got)
	}
}

// The confops validation chain is reached, not re-implemented: an unknown
// transport is refused with the code confops freezes.
func TestServerCreateValidationFailures(t *testing.T) {
	env, _, _ := adminServer(t, nil)

	doAdmin(t, env.sock, http.MethodPost, "/v1/servers", map[string]any{
		"id": "bad", "entry": map[string]any{"transport": "carrier-pigeon"},
	}).wantErr(t, http.StatusBadRequest, confops.CodeUnsupportedTransport)

	doAdmin(t, env.sock, http.MethodPost, "/v1/servers", map[string]any{
		"id": "", "entry": stdioEntry(),
	}).wantErr(t, http.StatusBadRequest, confops.CodeUsage)

	doAdmin(t, env.sock, http.MethodPost, "/v1/servers", map[string]any{"id": "dup", "entry": stdioEntry()})
	doAdmin(t, env.sock, http.MethodPost, "/v1/servers", map[string]any{"id": "dup", "entry": stdioEntry()}).
		wantErr(t, http.StatusConflict, confops.CodeServerExists)
}

// A PATCH on a server nobody registered is the uniform 404, byte-identical
// to an unknown route (anti-probing).
func TestAdminUnknownResourcesAreUniform404(t *testing.T) {
	env, _, _ := adminServer(t, nil)
	reference := doAdmin(t, env.sock, http.MethodGet, "/v1/definitely-not-a-route", nil)
	reference.wantErr(t, http.StatusNotFound, CodeNotFound)

	cases := []struct {
		method, path string
		body         any
	}{
		{http.MethodGet, "/v1/servers/ghost", nil},
		{http.MethodPatch, "/v1/servers/ghost", map[string]any{"entry": map[string]any{"enabled": false}}},
		{http.MethodDelete, "/v1/servers/ghost", nil},
		{http.MethodPatch, "/v1/profiles/ghost", map[string]any{"rename": "other"}},
		{http.MethodDelete, "/v1/profiles/ghost", nil},
		{http.MethodDelete, "/v1/scope/ghost", nil},
		{http.MethodDelete, "/v1/quarantine/ghost", nil},
		{http.MethodPut, "/v1/servers/ghost", map[string]any{}},
		{http.MethodGet, "/v1/config/discovery", nil},
	}
	for _, tc := range cases {
		got := doAdmin(t, env.sock, tc.method, tc.path, tc.body)
		got.wantErr(t, http.StatusNotFound, CodeNotFound)
		if got.Error.Message != reference.Error.Message || got.Error.Hint != reference.Error.Hint {
			t.Errorf("%s %s: body %q differs from the uniform 404 %q",
				tc.method, tc.path, got.raw, reference.raw)
		}
	}
}

// A path segment carrying an escaped slash must not smuggle an extra
// segment past the router.
func TestAdminPathEscapingIsNotSmuggling(t *testing.T) {
	env, _, _ := adminServer(t, nil)
	doAdmin(t, env.sock, http.MethodGet, "/v1/servers/a%2Fb", nil).
		wantErr(t, http.StatusNotFound, CodeNotFound)
}

func TestStalePreconditionConflicts(t *testing.T) {
	env, _, _ := adminServer(t, nil)
	seedServer(t, env.reg, "seed", true) // moves the generation
	current := env.reg.Snapshot().Generation

	stale := doAdmin(t, env.sock, http.MethodPost, "/v1/servers", map[string]any{
		"id": "github", "entry": stdioEntry(), "expected_generation": current + 99,
	})
	stale.wantErr(t, http.StatusConflict, CodeStalePrecondition)
	if stale.Error.Generation != current {
		t.Errorf("409 body generation = %d, want the current %d", stale.Error.Generation, current)
	}
	if _, ok := env.reg.Snapshot().Servers.V.Servers["github"]; ok {
		t.Error("a stale write must not land")
	}

	// The same guard on the query string, which is the only way a DELETE can
	// carry one.
	doAdmin(t, env.sock, http.MethodDelete,
		"/v1/servers/seed?expected_generation=999", nil).
		wantErr(t, http.StatusConflict, CodeStalePrecondition)

	// And the matching generation goes through.
	ok := doAdmin(t, env.sock, http.MethodPost, "/v1/servers", map[string]any{
		"id": "github", "entry": stdioEntry(), "expectedGeneration": current,
	})
	if ok.status != http.StatusOK {
		t.Fatalf("matching precondition rejected: %d %s", ok.status, ok.raw)
	}
}

// The two accepted spellings are aliases of ONE value. Disagreeing numbers
// are refused rather than resolved by a guess about a concurrency guard.
func TestPreconditionSpellingsMustAgree(t *testing.T) {
	env, _, _ := adminServer(t, nil)
	doAdmin(t, env.sock, http.MethodPost, "/v1/servers", map[string]any{
		"id": "x", "entry": stdioEntry(), "expected_generation": 1, "expectedGeneration": 2,
	}).wantErr(t, http.StatusBadRequest, CodeBadRequest)

	doAdmin(t, env.sock, http.MethodDelete, "/v1/servers/x?expected_generation=nope", nil).
		wantErr(t, http.StatusBadRequest, CodeBadRequest)
}

// The daemon watcher suppresses this process's own registry writes, so the
// control plane announces them itself — otherwise a GUI edit would be the
// one change nobody is told about.
func TestWritePublishesRegistryChange(t *testing.T) {
	env, _, _ := adminServer(t, nil)
	sub := env.bus.Subscribe(event.DefaultBuffer)
	defer sub.Close()

	doAdmin(t, env.sock, http.MethodPost, "/v1/servers", map[string]any{"id": "s", "entry": stdioEntry()})

	var sawRegistry, sawServers bool
	deadline := time.After(3 * time.Second)
	for range 2 {
		select {
		case ev := <-sub.Events():
			switch ev.Topic {
			case TopicRegistry:
				sawRegistry = true
				ch, ok := ev.Payload.(registry.Change)
				if !ok || ch.Kind != registry.DocServers || ch.Rev == 0 {
					t.Errorf("registry frame = %+v", ev.Payload)
				}
			case "server.registry":
				sawServers = true
			}
		case <-deadline:
			t.Fatal("no registry change event after a write")
		}
	}
	if !sawRegistry || !sawServers {
		t.Errorf("registry=%v servers=%v", sawRegistry, sawServers)
	}
}

func TestProfileLifecycle(t *testing.T) {
	env, _, _ := adminServer(t, nil)
	seedServer(t, env.reg, "github", true)

	if got := doAdmin(t, env.sock, http.MethodPost, "/v1/profiles",
		map[string]any{"name": "dev"}); got.status != http.StatusOK {
		t.Fatalf("create: %d %s", got.status, got.raw)
	}
	doAdmin(t, env.sock, http.MethodPost, "/v1/profiles", map[string]any{"name": "dev"}).
		wantErr(t, http.StatusConflict, confops.CodeProfileExists)

	// Membership.
	res := doAdmin(t, env.sock, http.MethodPatch, "/v1/profiles/dev", map[string]any{
		"servers": map[string]any{"mode": "add", "servers": []string{"github"}},
	})
	if res.status != http.StatusOK {
		t.Fatalf("servers add: %d %s", res.status, res.raw)
	}
	// A server nobody registered narrows nothing, so it is refused.
	doAdmin(t, env.sock, http.MethodPatch, "/v1/profiles/dev", map[string]any{
		"servers": map[string]any{"mode": "add", "servers": []string{"ghost"}},
	}).wantErr(t, http.StatusNotFound, CodeNotFound)

	// Tool selector.
	res = doAdmin(t, env.sock, http.MethodPatch, "/v1/profiles/dev", map[string]any{
		"tools": map[string]any{"server": "github", "mode": "only", "tools": []string{"read_file"}},
	})
	if res.status != http.StatusOK {
		t.Fatalf("tools: %d %s", res.status, res.raw)
	}
	sel := env.reg.Snapshot().Profiles.V.Profiles["dev"].V.Tools["github"].V
	if len(sel.Allow) != 1 || sel.Allow[0] != "read_file" {
		t.Errorf("selector = %+v", sel)
	}
	// An unset mode is refused rather than defaulted to the loose case.
	doAdmin(t, env.sock, http.MethodPatch, "/v1/profiles/dev", map[string]any{
		"tools": map[string]any{"server": "github", "mode": ""},
	}).wantErr(t, http.StatusBadRequest, confops.CodeUsage)

	// Rename repoints references.
	if err := env.reg.Update(context.Background(), func(tx *registry.Tx) error {
		tx.Clients.V.Clients["cursor"] = registry.Doc[registry.ClientEntry]{
			V: registry.ClientEntry{Profile: "dev"},
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	res = doAdmin(t, env.sock, http.MethodPatch, "/v1/profiles/dev", map[string]any{"rename": "work"})
	var pw struct {
		Name      string   `json:"name"`
		OldName   string   `json:"old_name"`
		Repointed []string `json:"repointed"`
	}
	res.decode(t, &pw)
	if pw.Name != "work" || pw.OldName != "dev" || len(pw.Repointed) != 1 || pw.Repointed[0] != "cursor" {
		t.Fatalf("rename result = %+v", pw)
	}

	// Delete reports the clients it fail-closed.
	res = doAdmin(t, env.sock, http.MethodDelete, "/v1/profiles/work", nil)
	var dw struct {
		Dangling []string `json:"dangling"`
		Warnings []string `json:"warnings"`
		Deleted  bool     `json:"deleted"`
	}
	res.decode(t, &dw)
	if !dw.Deleted || len(dw.Dangling) != 1 || len(dw.Warnings) == 0 {
		t.Fatalf("delete result = %+v", dw)
	}
}

// A patch is exactly one operation: combining two would leave a request that
// half-applied when the second confops call failed.
func TestProfilePatchIsOneOperation(t *testing.T) {
	env, _, _ := adminServer(t, nil)
	doAdmin(t, env.sock, http.MethodPost, "/v1/profiles", map[string]any{"name": "dev"})

	doAdmin(t, env.sock, http.MethodPatch, "/v1/profiles/dev", map[string]any{
		"rename":  "work",
		"servers": map[string]any{"mode": "replace"},
	}).wantErr(t, http.StatusBadRequest, CodeBadRequest)

	doAdmin(t, env.sock, http.MethodPatch, "/v1/profiles/dev", map[string]any{}).
		wantErr(t, http.StatusBadRequest, CodeBadRequest)
}

func TestProfileListAndActiveMarker(t *testing.T) {
	env, _, _ := adminServer(t, nil)
	doAdmin(t, env.sock, http.MethodPost, "/v1/profiles", map[string]any{"name": "dev"})
	doAdmin(t, env.sock, http.MethodPost, "/v1/profiles", map[string]any{"name": "ops"})

	res := doAdmin(t, env.sock, http.MethodPatch, "/v1/profiles/ops", map[string]any{"active": true})
	if res.status != http.StatusOK {
		t.Fatalf("active: %d %s", res.status, res.raw)
	}
	active, err := confops.ActiveProfile(env.reg)
	if err != nil || active != "ops" {
		t.Fatalf("marker = %q (%v)", active, err)
	}

	var list struct {
		Generation  uint64 `json:"generation"`
		Active      string `json:"active"`
		ActiveKnown bool   `json:"active_known"`
		Profiles    []struct {
			Name string `json:"name"`
		} `json:"profiles"`
	}
	doAdmin(t, env.sock, http.MethodGet, "/v1/profiles", nil).decode(t, &list)
	if len(list.Profiles) != 2 || list.Profiles[0].Name != "dev" || list.Profiles[1].Name != "ops" {
		t.Fatalf("profiles = %+v", list.Profiles)
	}
	if list.Active != "ops" || !list.ActiveKnown || list.Generation == 0 {
		t.Errorf("list = %+v", list)
	}
}

func TestScopeBinding(t *testing.T) {
	env, _, _ := adminServer(t, nil)
	seedServer(t, env.reg, "github", true)
	doAdmin(t, env.sock, http.MethodPost, "/v1/profiles", map[string]any{"name": "dev"})

	// An unbound client is a 200 with exists=false, not a 404: a frontend
	// needs that state to offer creating the binding.
	var got struct {
		Exists bool `json:"exists"`
	}
	doAdmin(t, env.sock, http.MethodGet, "/v1/scope/cursor", nil).decode(t, &got)
	if got.Exists {
		t.Fatal("unbound client reported as existing")
	}

	res := doAdmin(t, env.sock, http.MethodPut, "/v1/scope/cursor", map[string]any{
		"profile": map[string]any{"kind": "named", "name": "dev"},
	})
	if res.status != http.StatusOK {
		t.Fatalf("put: %d %s", res.status, res.raw)
	}
	entry := env.reg.Snapshot().Clients.V.Clients["cursor"].V
	if entry.Binding().Name != "dev" {
		t.Fatalf("entry = %+v", entry)
	}

	// Rebinding replaces the reference. The entry holds nothing else, so
	// there is no second field a rebind could leave stale.
	doAdmin(t, env.sock, http.MethodPost, "/v1/profiles", map[string]any{"name": "other"})
	if res = doAdmin(t, env.sock, http.MethodPut, "/v1/scope/cursor", map[string]any{
		"profile": map[string]any{"kind": "named", "name": "other"},
	}); res.status != http.StatusOK {
		t.Fatalf("rebind: %d %s", res.status, res.raw)
	}
	if got := env.reg.Snapshot().Clients.V.Clients["cursor"].V.Binding().Name; got != "other" {
		t.Fatalf("rebind = %q, want other", got)
	}
	if res = doAdmin(t, env.sock, http.MethodPut, "/v1/scope/cursor", map[string]any{
		"profile": map[string]any{"kind": "named", "name": "dev"},
	}); res.status != http.StatusOK {
		t.Fatalf("rebind back: %d %s", res.status, res.raw)
	}

	// A retired narrowing field is REFUSED, never accepted-and-dropped. The
	// caller was asking to narrow; binding the client while silently
	// discarding that half would report success for a WIDER surface than it
	// asked for.
	for _, field := range []string{"servers", "tools", "discovery"} {
		body := map[string]any{"profile": map[string]any{"kind": "named", "name": "dev"}}
		switch field {
		case "servers":
			body[field] = []string{"github"}
		case "tools":
			body[field] = map[string]any{"github": map[string]any{"mode": "none"}}
		default:
			body[field] = "lazy"
		}
		if res = doAdmin(t, env.sock, http.MethodPut, "/v1/scope/cursor", body); res.status != http.StatusBadRequest {
			t.Errorf("%s: status = %d, want 400 — a retired field must fail loudly", field, res.status)
		}
	}

	// A dangling profile is reported loudly rather than shown as an empty set.
	doAdmin(t, env.sock, http.MethodDelete, "/v1/profiles/dev", nil)
	var read struct {
		Dangling        bool   `json:"dangling"`
		DanglingProfile string `json:"dangling_profile"`
	}
	doAdmin(t, env.sock, http.MethodGet, "/v1/scope/cursor", nil).decode(t, &read)
	if !read.Dangling || read.DanglingProfile != "dev" {
		t.Errorf("dangling not reported: %+v", read)
	}

	if res = doAdmin(t, env.sock, http.MethodDelete, "/v1/scope/cursor", nil); res.status != http.StatusOK {
		t.Fatalf("delete: %d %s", res.status, res.raw)
	}
	if _, ok := env.reg.Snapshot().Clients.V.Clients["cursor"]; ok {
		t.Error("binding still present")
	}
	// Clearing it twice is a miss, not a cheerful success.
	doAdmin(t, env.sock, http.MethodDelete, "/v1/scope/cursor", nil).
		wantErr(t, http.StatusNotFound, CodeNotFound)
}

func TestScopeRejectsUnknownValues(t *testing.T) {
	env, _, _ := adminServer(t, nil)
	doAdmin(t, env.sock, http.MethodPut, "/v1/scope/cursor",
		map[string]any{"profile": map[string]any{"kind": "whatever"}}).
		wantErr(t, http.StatusBadRequest, confops.CodeUsage)
	// A named binding with no name is a typo. "No profile" is spelled
	// followActive, so resolving the typo to it would be a silent widening.
	doAdmin(t, env.sock, http.MethodPut, "/v1/scope/cursor",
		map[string]any{"profile": map[string]any{"kind": "named"}}).
		wantErr(t, http.StatusBadRequest, confops.CodeUsage)
	// A retired narrowing field is a usage error, not a quietly dropped one.
	doAdmin(t, env.sock, http.MethodPut, "/v1/scope/cursor",
		map[string]any{"servers": []string{"ghost"}}).
		wantErr(t, http.StatusBadRequest, confops.CodeUsage)
	doAdmin(t, env.sock, http.MethodPut, "/v1/scope/cursor", map[string]any{}).
		wantErr(t, http.StatusBadRequest, confops.CodeUsage)
}

func TestGovernanceListAndSet(t *testing.T) {
	env, _, _ := adminServer(t, nil)

	var list struct {
		Generation uint64 `json:"generation"`
		Entries    []struct {
			Key   string `json:"key"`
			Value string `json:"value"`
			Kind  string `json:"kind"`
		} `json:"entries"`
	}
	doAdmin(t, env.sock, http.MethodGet, "/v1/config", nil).decode(t, &list)
	// The listing IS confops' frozen table — same keys, same order.
	keys := confops.GovernanceKeys()
	if len(list.Entries) != len(keys) {
		t.Fatalf("listed %d keys, confops has %d", len(list.Entries), len(keys))
	}
	for i, k := range keys {
		if list.Entries[i].Key != k.Name {
			t.Errorf("entry %d = %q, want %q", i, list.Entries[i].Key, k.Name)
		}
	}

	res := doAdmin(t, env.sock, http.MethodPut, "/v1/config/discovery",
		map[string]any{"value": "lazy"})
	var cw struct {
		Key      string `json:"key"`
		Value    string `json:"value"`
		Previous string `json:"previous"`
		Changed  bool   `json:"changed"`
	}
	res.decode(t, &cw)
	if cw.Key != "discovery" || cw.Value != "lazy" || !cw.Changed {
		t.Fatalf("set result = %+v", cw)
	}
	if env.reg.Snapshot().Governance.V.Discovery != "lazy" {
		t.Error("key not written")
	}

	// Relaxing a governance value is ALLOWED here — this is the only place
	// that can.
	res = doAdmin(t, env.sock, http.MethodPut, "/v1/config/discovery_mode",
		map[string]any{"value": "full"})
	if res.status != http.StatusOK {
		t.Fatalf("relax: %d %s", res.status, res.raw)
	}
	if env.reg.Snapshot().Governance.V.Discovery != "full" {
		t.Error("key not relaxed")
	}

	// An unparseable value leaves the switch untouched: a typo must never
	// read as "false" and silently turn a gate off.
	doAdmin(t, env.sock, http.MethodPut, "/v1/config/discovery",
		map[string]any{"value": "maybe"}).
		wantErr(t, http.StatusBadRequest, confops.CodeUsage)
	doAdmin(t, env.sock, http.MethodPut, "/v1/config/nonsense",
		map[string]any{"value": "true"}).
		wantErr(t, http.StatusBadRequest, confops.CodeConfigKeyUnknown)
	doAdmin(t, env.sock, http.MethodPut, "/v1/config/discovery",
		map[string]any{"value": map[string]any{"nested": true}}).
		wantErr(t, http.StatusBadRequest, CodeBadRequest)

	// The dynamic budget family goes through the same table.
	if res = doAdmin(t, env.sock, http.MethodPut, "/v1/config/resultBudget.github",
		map[string]any{"value": "4096"}); res.status != http.StatusOK {
		t.Fatalf("budget: %d %s", res.status, res.raw)
	}
	if env.reg.Snapshot().Governance.V.ResultBudget["github"].V.Bytes != 4096 {
		t.Error("budget not written")
	}
}

func TestGovernanceSetHonoursPrecondition(t *testing.T) {
	env, _, _ := adminServer(t, nil)
	current := env.reg.Snapshot().Generation
	doAdmin(t, env.sock, http.MethodPut, "/v1/config/discovery", map[string]any{
		"value": "grouped", "expected_generation": current + 5,
	}).wantErr(t, http.StatusConflict, CodeStalePrecondition)
	if env.reg.Snapshot().Governance.V.Discovery == "grouped" {
		t.Error("a stale governance write must not land")
	}
}

// Bodies are bounded; a malformed one is a client error, never a partial
// write.
func TestAdminRejectsMalformedBodies(t *testing.T) {
	env, _, _ := adminServer(t, nil)
	doAdmin(t, env.sock, http.MethodPost, "/v1/servers", "{not json").
		wantErr(t, http.StatusBadRequest, CodeBadRequest)
	doAdmin(t, env.sock, http.MethodPost, "/v1/servers", nil).
		wantErr(t, http.StatusBadRequest, CodeBadRequest)
	big := bytes.Repeat([]byte("a"), maxBodyBytes+1)
	doAdmin(t, env.sock, http.MethodPost, "/v1/servers", big).
		wantErr(t, http.StatusBadRequest, CodeBadRequest)
}

// TestConfopsErrorMappingMatrix pins the whole Kind -> status table in one
// place, including the two kinds the endpoint tests above cannot reach
// through a request (a guard refusal and a corrupt state file).
//
// The uniform 404 is part of the matrix on purpose: a KindNotFound loses its
// specific code on the way out, so a prober cannot tell an unknown id from
// an unknown route.
func TestConfopsErrorMappingMatrix(t *testing.T) {
	srv, err := NewServer(Options{
		Registry: mustRegistry(t), Sessions: session.NewMemoryManager(session.Options{}), Bus: event.NewBus(),
	})
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name   string
		err    error
		status int
		code   string
	}{
		{"usage", &confops.Error{Kind: confops.KindUsage, Code: confops.CodeUsage, Message: "m"},
			http.StatusBadRequest, confops.CodeUsage},
		{"not found", &confops.Error{Kind: confops.KindNotFound, Code: confops.CodeServerNotFound, Message: "m"},
			http.StatusNotFound, CodeNotFound},
		{"conflict", &confops.Error{Kind: confops.KindConflict, Code: confops.CodeProfileExists, Message: "m"},
			http.StatusConflict, confops.CodeProfileExists},
		{"denied", &confops.Error{Kind: confops.KindDenied, Code: confops.CodeDenied, Message: "m"},
			http.StatusForbidden, confops.CodeDenied},
		{"state", &confops.Error{Kind: confops.KindState, Code: confops.CodeStateCorrupt, Message: "m"},
			http.StatusInternalServerError, confops.CodeStateCorrupt},
		{"stale", &confops.StaleError{Want: 7, Got: 9},
			http.StatusConflict, CodeStalePrecondition},
		{"lock timeout", &registry.LockTimeoutError{Path: "p", Timeout: time.Second},
			http.StatusInternalServerError, CodeInternal},
		{"unclassified", errors.New("boom"), http.StatusInternalServerError, CodeInternal},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodPost, "/v1/servers", nil)
			srv.writeOpsError(rec, req, tc.err)
			if rec.Code != tc.status {
				t.Errorf("status = %d, want %d", rec.Code, tc.status)
			}
			var env struct {
				Error struct {
					Code       string `json:"code"`
					Message    string `json:"message"`
					Generation uint64 `json:"generation"`
				} `json:"error"`
			}
			if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
				t.Fatalf("body %q: %v", rec.Body.String(), err)
			}
			if env.Error.Code != tc.code {
				t.Errorf("code = %q, want %q", env.Error.Code, tc.code)
			}
			if tc.code == CodeNotFound && env.Error.Message != notFoundMessage {
				t.Errorf("a not-found must render the uniform body, got %q", env.Error.Message)
			}
			if tc.name == "stale" && env.Error.Generation != 9 {
				t.Errorf("stale body generation = %d, want the current 9", env.Error.Generation)
			}
		})
	}
}

// mustRegistry opens an empty registry for the handler-level tests.
func mustRegistry(t *testing.T) *registry.Store {
	t.Helper()
	st, err := registry.Open(filepath.Join(t.TempDir(), "registry"))
	if err != nil {
		t.Fatal(err)
	}
	return st
}
