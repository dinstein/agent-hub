package ctlapi

import (
	"encoding/json"
	"net/http"
	"net/url"
	"testing"
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
// afterwards.
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
