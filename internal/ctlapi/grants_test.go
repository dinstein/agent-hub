package ctlapi

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"slices"
	"testing"
	"time"

	"github.com/dinstein/agent-hub/internal/scope"
	"github.com/dinstein/agent-hub/internal/session"
)

// narrowSession applies an initial (agent-legal) narrowing overlay so the
// grant has something to widen.
func narrowSession(t *testing.T, mgr *session.MemoryManager, sid session.SessionID, server string, allow, deny []string) {
	t.Helper()
	err := mgr.Mutate(context.Background(), sid, func(ov *scope.Overlay) {
		if ov.Tools == nil {
			ov.Tools = map[string]*scope.ToolSelector{}
		}
		ov.Tools[server] = &scope.ToolSelector{Allow: allow, Deny: deny}
	})
	if err != nil {
		t.Fatalf("narrow: %v", err)
	}
}

func createGrant(t *testing.T, sock string, req GrantRequestWire) GrantWire {
	t.Helper()
	status, body := postJSON(t, sock, "/v1/grants", req)
	if status != http.StatusOK {
		t.Fatalf("grant create status = %d body=%s", status, body)
	}
	var env struct {
		Data GrantWire `json:"data"`
	}
	if err := json.Unmarshal(body, &env); err != nil {
		t.Fatal(err)
	}
	if env.Data.ID == "" || env.Data.Status != GrantPending {
		t.Fatalf("created grant = %+v", env.Data)
	}
	return env.Data
}

func decideGrant(t *testing.T, sock, id string, approve bool) (int, GrantWire, []byte) {
	t.Helper()
	status, body := postJSON(t, sock, "/v1/grants/"+id, GrantDecideWire{Approve: approve})
	var env struct {
		Data GrantWire `json:"data"`
	}
	_ = json.Unmarshal(body, &env)
	return status, env.Data, body
}

func TestGrantApproveWidensAndExpiryReverts(t *testing.T) {
	_, env := startServer(t, nil)
	s := openSession(t, env.mgr, "cursor")
	narrowSession(t, env.mgr, s.ID, "github", []string{"read_file"}, []string{"rm_rf"})

	g := createGrant(t, env.sock, GrantRequestWire{
		SessionID:  string(s.ID),
		Server:     "github",
		Tools:      []string{"write_file", "rm_rf"},
		Reason:     "one migration",
		TTLSeconds: 1, // fast reaper for the test
	})

	// The direct (agent-reachable) scope path must NOT be able to widen —
	// this is exactly what the grant path exists for.
	status, body := postJSON(t, env.sock, "/v1/sessions/"+string(s.ID)+"/scope",
		map[string]any{"reset": true})
	if status != http.StatusForbidden || !bytes.Contains(body, []byte(CodeTightenOnly)) {
		t.Fatalf("agent widen: status=%d body=%s (want 403 tighten-only)", status, body)
	}

	status, wire, body := decideGrant(t, env.sock, g.ID, true)
	if status != http.StatusOK || wire.Status != GrantApproved || wire.ExpiresAt == nil {
		t.Fatalf("approve: status=%d wire=%+v body=%s", status, wire, body)
	}

	// The overlay now carries the widening: allow gained write_file+rm_rf,
	// deny lost rm_rf.
	ov := env.mgr.Overlay(s.ID)
	if ov == nil {
		t.Fatal("no overlay after grant")
	}
	sel := ov.Tools["github"]
	if sel == nil || !slices.Contains(sel.Allow, "write_file") || !slices.Contains(sel.Allow, "rm_rf") {
		t.Fatalf("allow after grant = %+v", sel)
	}
	if slices.Contains(sel.Deny, "rm_rf") {
		t.Fatalf("deny still lists rm_rf after grant: %+v", sel)
	}

	// TTL reaper: the widening is reverted element-wise (volatile grant,
	// A.1 #8) and the grant flips to expired.
	waitFor(t, "grant expiry revert", func() bool {
		ov := env.mgr.Overlay(s.ID)
		if ov == nil {
			return false
		}
		sel := ov.Tools["github"]
		return sel != nil &&
			!slices.Contains(sel.Allow, "write_file") &&
			!slices.Contains(sel.Allow, "rm_rf") &&
			slices.Contains(sel.Deny, "rm_rf") &&
			slices.Contains(sel.Allow, "read_file")
	})
	var hist []GrantWire
	getJSON(t, env.sock, "/v1/grants?history=1", &hist)
	if len(hist) != 1 || hist[0].Status != GrantExpired {
		t.Fatalf("history = %+v", hist)
	}
}

func TestGrantDenyChangesNothing(t *testing.T) {
	_, env := startServer(t, nil)
	s := openSession(t, env.mgr, "cursor")
	narrowSession(t, env.mgr, s.ID, "github", []string{"read_file"}, nil)
	before := env.mgr.Overlay(s.ID).Version

	g := createGrant(t, env.sock, GrantRequestWire{
		SessionID: string(s.ID), Server: "github", Tools: []string{"write_file"},
	})
	status, wire, body := decideGrant(t, env.sock, g.ID, false)
	if status != http.StatusOK || wire.Status != GrantDenied || wire.DecidedBy != "cli" {
		t.Fatalf("deny: status=%d wire=%+v body=%s", status, wire, body)
	}
	if got := env.mgr.Overlay(s.ID).Version; got != before {
		t.Errorf("overlay version moved %d -> %d on a DENIED grant", before, got)
	}

	// Second decision is an idempotent 409 naming the first decider.
	status, _, body = decideGrant(t, env.sock, g.ID, true)
	if status != http.StatusConflict || !bytes.Contains(body, []byte(CodeAlreadyDecided)) {
		t.Fatalf("late decide: status=%d body=%s", status, body)
	}
}

func TestGrantStdioSessionPushesOverlay(t *testing.T) {
	_, env := startServer(t, nil)
	fg := registerFakeGateway(t, env.sock, "claude")
	defer fg.closeLink()
	fg.openLink()

	// Auto-ack pump: the daemon commits a grant overlay only after the
	// gateway acked it (push-then-commit).
	go func() {
		for f := range fg.frames {
			if f.event != LinkEventOverlay {
				continue
			}
			var frame OverlayFrame
			if json.Unmarshal(f.data, &frame) != nil {
				continue
			}
			body, _ := json.Marshal(GatewayAck{ID: frame.ID, OK: true})
			resp, err := fg.hc.Post("http://d/v1/gateway/"+fg.sid+"/ack", "application/json", bytes.NewReader(body))
			if err == nil {
				_ = resp.Body.Close()
			}
		}
	}()

	sid := session.SessionID(fg.sid)
	narrowSession(t, env.mgr, sid, "github", []string{}, nil) // block-all
	g := createGrant(t, env.sock, GrantRequestWire{
		SessionID: fg.sid, Server: "github", Tools: []string{"read_file"},
	})
	status, wire, body := decideGrant(t, env.sock, g.ID, true)
	if status != http.StatusOK || wire.Status != GrantApproved {
		t.Fatalf("approve: status=%d wire=%+v body=%s", status, wire, body)
	}
	ov := env.mgr.Overlay(sid)
	sel := ov.Tools["github"]
	if sel == nil || !slices.Contains(sel.Allow, "read_file") {
		t.Fatalf("stdio overlay after grant = %+v", sel)
	}
}

func TestGrantCreateValidation(t *testing.T) {
	_, env := startServer(t, nil)
	s := openSession(t, env.mgr, "cursor")

	// Unknown session: uniform 404.
	status, body := postJSON(t, env.sock, "/v1/grants",
		GrantRequestWire{SessionID: "ghost:9", Server: "github", Tools: []string{"x"}})
	if status != http.StatusNotFound {
		t.Errorf("unknown session: status=%d body=%s", status, body)
	}
	// Empty tools: 400 (a grant names explicit tools).
	status, body = postJSON(t, env.sock, "/v1/grants",
		GrantRequestWire{SessionID: string(s.ID), Server: "github"})
	if status != http.StatusBadRequest {
		t.Errorf("empty tools: status=%d body=%s", status, body)
	}
	// Unknown grant id: uniform 404.
	status, _, _ = decideGrant(t, env.sock, "deadbeef00000000", true)
	if status != http.StatusNotFound {
		t.Errorf("unknown grant: status=%d", status)
	}
}

func TestGrantTTLCapAndDefault(t *testing.T) {
	mutate := func(o *Options) { o.GrantTTL = 2 * time.Second }
	_, env := startServer(t, mutate)
	s := openSession(t, env.mgr, "cursor")

	g := createGrant(t, env.sock, GrantRequestWire{
		SessionID: string(s.ID), Server: "github", Tools: []string{"x"},
	})
	if g.TTLSeconds != 2 {
		t.Errorf("default ttl = %ds, want 2", g.TTLSeconds)
	}
	over := createGrant(t, env.sock, GrantRequestWire{
		SessionID: string(s.ID), Server: "github", Tools: []string{"x"},
		TTLSeconds: int64((48 * time.Hour) / time.Second),
	})
	if over.TTLSeconds != int64((24*time.Hour)/time.Second) {
		t.Errorf("capped ttl = %ds, want 24h", over.TTLSeconds)
	}
}
