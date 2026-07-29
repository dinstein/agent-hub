package ctlapi

import (
	"encoding/json"
	"net/http"
	"net/url"
	"testing"

	"github.com/dinstein/agent-hub/internal/scope"
	"github.com/dinstein/agent-hub/internal/session"
)

// envelopeData decodes a success envelope body into out.
func envelopeData(t *testing.T, raw []byte, out any) {
	t.Helper()
	var env struct {
		OK   bool            `json:"ok"`
		Data json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(raw, &env); err != nil {
		t.Fatalf("response is not an envelope: %v\n%s", err, raw)
	}
	if !env.OK {
		t.Fatalf("envelope not ok: %s", raw)
	}
	if err := json.Unmarshal(env.Data, out); err != nil {
		t.Fatalf("data = %s: %v", env.Data, err)
	}
}

// TestKillSession covers POST /v1/sessions/{id}/kill: the session is gone
// afterwards, and the action is audited like every other control-plane
// write.
func TestKillSession(t *testing.T) {
	client, env := startServer(t, nil)
	s := openSession(t, env.mgr, "cursor")

	status, body := postJSON(t, env.sock, "/v1/sessions/"+url.PathEscape(string(s.ID))+"/kill", struct{}{})
	if status != http.StatusOK {
		t.Fatalf("status = %d, body %v", status, body)
	}
	var res KillResult
	envelopeData(t, body, &res)
	if !res.Killed || res.SessionID != string(s.ID) || res.ClientID != "cursor" {
		t.Errorf("result = %+v", res)
	}

	list, err := client.Sessions.List(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 0 {
		t.Errorf("session survived the kill: %+v", list)
	}

	var audited bool
	for _, r := range env.aud.records() {
		if r.Tool == "sessions/kill" && r.Session == string(s.ID) && r.Client == "cursor" {
			audited = true
		}
	}
	if !audited {
		t.Errorf("kill was not audited: %+v", env.aud.records())
	}
}

// TestKillUnknownSessionIs404 pins the anti-probing rule: an unknown id
// reads exactly like an unknown route. Without the existence check Close
// would be a silent no-op and a typo would report success.
func TestKillUnknownSessionIs404(t *testing.T) {
	_, env := startServer(t, nil)
	status, _ := postJSON(t, env.sock, "/v1/sessions/ghost:9/kill", struct{}{})
	if status != http.StatusNotFound {
		t.Errorf("status = %d, want 404", status)
	}
}

// TestScopeDiscoveryOverride: discovery is an EXPERIENCE field, so it moves
// freely (the tighten-only check does not apply to it) and it survives a
// request that also carries Reset.
func TestScopeDiscoveryOverride(t *testing.T) {
	_, env := startServer(t, nil)
	s := openSession(t, env.mgr, "cursor")
	path := "/v1/sessions/" + url.PathEscape(string(s.ID)) + "/scope"

	status, body := postJSON(t, env.sock, path, ScopeNarrowWire{Discovery: "lazy"})
	if status != http.StatusOK {
		t.Fatalf("status = %d, body %v", status, body)
	}
	ov := env.mgr.Overlay(session.SessionID(s.ID))
	if ov == nil || ov.Discovery == nil || *ov.Discovery != scope.DiscoveryLazy {
		t.Fatalf("overlay discovery = %+v, want lazy", ov)
	}

	// Reset drops the narrowing but must NOT wipe a discovery override sent
	// in the same request (reset runs first, the override after).
	req := ScopeNarrowWire{Discovery: "grouped"}
	req.Reset = true
	status, body = postJSON(t, env.sock, path, req)
	if status != http.StatusOK {
		t.Fatalf("reset+discovery status = %d, body %v", status, body)
	}
	ov = env.mgr.Overlay(session.SessionID(s.ID))
	if ov == nil || ov.Discovery == nil || *ov.Discovery != scope.DiscoveryGrouped {
		t.Errorf("overlay discovery after reset = %+v, want grouped", ov)
	}
}

// TestScopeUnknownDiscoveryIsRefused: silently keeping the old mode would
// tell the operator an override took effect when it did not.
func TestScopeUnknownDiscoveryIsRefused(t *testing.T) {
	_, env := startServer(t, nil)
	s := openSession(t, env.mgr, "cursor")
	status, _ := postJSON(t, env.sock,
		"/v1/sessions/"+url.PathEscape(string(s.ID))+"/scope",
		ScopeNarrowWire{Discovery: "sideways"})
	if status != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", status)
	}
	if ov := env.mgr.Overlay(session.SessionID(s.ID)); ov != nil && ov.Discovery != nil {
		t.Errorf("a refused request still mutated the overlay: %+v", ov)
	}
}
