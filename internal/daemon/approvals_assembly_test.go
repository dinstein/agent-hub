package daemon_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"testing"
	"time"

	"github.com/dinstein/agent-hub/api"
	"github.com/dinstein/agent-hub/internal/approval"
	"github.com/dinstein/agent-hub/internal/ctlapi"
	"github.com/dinstein/agent-hub/internal/daemon"
)

// udsClient is a raw HTTP client over the daemon socket.
func udsClient(socket string) *http.Client {
	return &http.Client{Transport: &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, "unix", socket)
		},
	}}
}

// TestDaemonAssemblesApprovalBroker pins the M1-C assembly: a fresh daemon
// serves the approvals surface (broker wired), fails an ask closed with no
// frontend, and completes the ask -> SSE pending -> decide -> approved loop
// once a frontend subscribes. It also proves the allowlist landed in
// <state>/approvals-allowlist.json via remember=forever.
func TestDaemonAssemblesApprovalBroker(t *testing.T) {
	t.Parallel()
	h := startDaemon(t, func(cfg *daemon.Config) { cfg.ApprovalTTL = 5 * time.Second })
	hc := udsClient(h.socket)

	// Broker wired: the listing answers (200 + empty array), not the
	// uniform 404 of a broker-less server.
	var list []ctlapi.ApprovalWire
	status := daemonGetJSON(t, hc, "/v1/approvals", &list)
	if status != http.StatusOK || len(list) != 0 {
		t.Fatalf("GET /v1/approvals = %d %v", status, list)
	}

	// No frontend: fail-closed unreachable.
	if dec := daemonAsk(t, hc, "srv", "boom"); dec != "unreachable" {
		t.Fatalf("ask without frontend = %q", dec)
	}

	// Frontend attached over the real SSE surface: the loop closes.
	client := api.New(h.socket)
	defer client.Close()
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	events, err := client.Events.Subscribe(ctx, "approvals")
	if err != nil {
		t.Fatal(err)
	}
	decCh := make(chan string, 1)
	go func() { decCh <- daemonAsk(t, hc, "srv", "boom") }()

	var pend ctlapi.ApprovalWire
	select {
	case ev := <-events:
		if ev.Kind != "pending" {
			t.Errorf("event kind = %q", ev.Kind)
		}
		if err := json.Unmarshal(ev.Payload, &pend); err != nil {
			t.Fatal(err)
		}
	case <-time.After(testTimeout):
		t.Fatal("no pending SSE frame")
	}
	daemonDecide(t, hc, pend.Token, true, "forever")
	select {
	case dec := <-decCh:
		if dec != "approved" {
			t.Fatalf("ask decision = %q", dec)
		}
	case <-time.After(testTimeout):
		t.Fatal("ask never returned")
	}

	// remember=forever persisted the fingerprint-bound allowlist under the
	// daemon's state dir.
	stateDir, err := h.resolver.StateDir()
	if err != nil {
		t.Fatal(err)
	}
	al, err := approval.OpenAllowlist(stateDir)
	if err != nil {
		t.Fatalf("allowlist after remember=forever: %v", err)
	}
	if entries := al.Entries(); len(entries) != 1 || entries[0].Fingerprint != "v1:test" {
		t.Fatalf("allowlist entries = %+v", entries)
	}
}

func daemonAsk(t *testing.T, hc *http.Client, server, tool string) string {
	t.Helper()
	body, _ := json.Marshal(ctlapi.ApprovalAskWire{
		Server: server, Tool: tool,
		Args: json.RawMessage(`{"a":1}`), ArgsHash: "h", Fingerprint: "v1:test",
	})
	resp, err := hc.Post("http://d/v1/approvals/ask", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Errorf("ask: %v", err)
		return "transport-error"
	}
	defer func() { _ = resp.Body.Close() }()
	var env struct {
		Data ctlapi.ApprovalDecisionWire `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&env); err != nil {
		t.Errorf("ask decode: %v", err)
		return "decode-error"
	}
	return env.Data.Decision
}

func daemonDecide(t *testing.T, hc *http.Client, token string, approve bool, remember string) {
	t.Helper()
	body, _ := json.Marshal(ctlapi.ApprovalDecideWire{Approve: approve, Remember: remember})
	resp, err := hc.Post("http://d/v1/approvals/"+token, "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("decide: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("decide status = %d body=%s", resp.StatusCode, b)
	}
}

func daemonGetJSON(t *testing.T, hc *http.Client, path string, out any) int {
	t.Helper()
	resp, err := hc.Get("http://d" + path)
	if err != nil {
		t.Fatalf("GET %s: %v", path, err)
	}
	defer func() { _ = resp.Body.Close() }()
	var env struct {
		OK   bool            `json:"ok"`
		Data json.RawMessage `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&env); err != nil {
		t.Fatalf("GET %s decode: %v", path, err)
	}
	if env.OK && out != nil {
		if err := json.Unmarshal(env.Data, out); err != nil {
			t.Fatal(err)
		}
	}
	return resp.StatusCode
}
