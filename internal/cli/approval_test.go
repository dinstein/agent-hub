package cli

import (
	"context"
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/dinstein/agent-hub/internal/approval"
	"github.com/dinstein/agent-hub/internal/ctlapi"
	"github.com/dinstein/agent-hub/internal/event"
	"github.com/dinstein/agent-hub/internal/platform"
	"github.com/dinstein/agent-hub/internal/registry"
	"github.com/dinstein/agent-hub/internal/scope"
	"github.com/dinstein/agent-hub/internal/session"
)

// ctlHarness is a live control-plane server for CLI tests: real UDS, real
// broker, real session manager. AGENTHUB_SOCKET points the CLI at it.
type ctlHarness struct {
	broker *approval.MemBroker
	mgr    *session.MemoryManager
	socket string
}

func startCtl(t *testing.T) *ctlHarness {
	t.Helper()
	setDataDir(t)
	sockDir, err := os.MkdirTemp("", "ahcli")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(sockDir) })
	socket := filepath.Join(sockDir, "ctl.sock")
	t.Setenv(platform.EnvSocket, socket)

	reg, err := registry.Open(filepath.Join(t.TempDir(), "reg"))
	if err != nil {
		t.Fatal(err)
	}
	bus := event.NewBus()
	mgr := session.NewMemoryManager(session.Options{Bus: bus})
	broker := approval.NewMemBroker(approval.Options{DefaultTTL: 5 * time.Second})
	srv, err := ctlapi.NewServer(ctlapi.Options{
		Version:   "cli-test",
		Registry:  reg,
		Sessions:  mgr,
		Bus:       bus,
		Approvals: broker,
		GrantTTL:  time.Hour,
		Logger:    slog.New(slog.DiscardHandler),
		KeepAlive: -1,
	})
	if err != nil {
		t.Fatal(err)
	}
	l, err := ctlapi.Listen(socket)
	if err != nil {
		t.Fatal(err)
	}
	go func() { _ = srv.Serve(l) }()
	t.Cleanup(func() { _ = srv.Close() })
	return &ctlHarness{broker: broker, mgr: mgr, socket: socket}
}

// pendingAsk parks one request in the broker (with a subscribed frontend so
// it fans out instead of failing Unreachable) and returns its token plus
// the decision channel.
func (h *ctlHarness) pendingAsk(t *testing.T) (string, <-chan approval.Decision) {
	t.Helper()
	ch, cancel := h.broker.Subscribe("test-frontend")
	t.Cleanup(cancel)
	dec := make(chan approval.Decision, 1)
	go func() {
		dec <- h.broker.Ask(context.Background(), approval.Request{
			Server: "srv", Tool: "boom", ArgsJSON: json.RawMessage(`{}`),
			ArgsHash: "h", Fingerprint: "v1:x",
			GateReason: approval.ReasonDestructive, Client: "c", SessionID: "c:1",
		})
	}()
	select {
	case req := <-ch:
		return req.Token, dec
	case <-time.After(5 * time.Second):
		t.Fatal("request never fanned out")
		return "", nil
	}
}

func TestApprovalLsRequiresDaemon(t *testing.T) {
	setDataDir(t)
	sockDir, err := os.MkdirTemp("", "ahcli")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(sockDir) })
	t.Setenv(platform.EnvSocket, filepath.Join(sockDir, "nope.sock"))

	code, out, _ := runCLI(t, "", "approval", "ls", "--json")
	if code != ExitDaemonDown {
		t.Fatalf("exit = %d, want %d (daemon offline)\n%s", code, ExitDaemonDown, out)
	}
	env := decodeEnvelope(t, out)
	if env.Error == nil || env.Error.Code != CodeDaemonDown {
		t.Errorf("envelope = %s", out)
	}
}

func TestApprovalLsApproveFlow(t *testing.T) {
	h := startCtl(t)
	token, dec := h.pendingAsk(t)

	code, out, _ := runCLI(t, "", "approval", "ls", "--json")
	if code != ExitOK {
		t.Fatalf("ls exit = %d\n%s", code, out)
	}
	env := decodeEnvelope(t, out)
	var data struct {
		Approvals []ctlapi.ApprovalWire `json:"approvals"`
	}
	if err := json.Unmarshal(env.Data, &data); err != nil {
		t.Fatal(err)
	}
	if len(data.Approvals) != 1 || data.Approvals[0].Token != token {
		t.Fatalf("ls = %s", out)
	}
	if len(data.Approvals[0].Args) != 0 {
		t.Error("ls leaked argument bytes")
	}

	code, out, _ = runCLI(t, "", "approval", "approve", token, "--json")
	if code != ExitOK {
		t.Fatalf("approve exit = %d\n%s", code, out)
	}
	select {
	case d := <-dec:
		if d != approval.Approved {
			t.Fatalf("broker decision = %v", d)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("ask never resolved")
	}

	// Idempotent repeat: 409 from the daemon renders as success info.
	code, out, _ = runCLI(t, "", "approval", "deny", token, "--json")
	if code != ExitOK {
		t.Fatalf("late deny exit = %d\n%s", code, out)
	}
	env = decodeEnvelope(t, out)
	var res ApprovalDecideResult
	if err := json.Unmarshal(env.Data, &res); err != nil {
		t.Fatal(err)
	}
	if !res.AlreadyDecided || !strings.Contains(res.Note, "by cli") {
		t.Errorf("late deny = %s", out)
	}
}

func TestApprovalDenyAndUnknownToken(t *testing.T) {
	h := startCtl(t)
	token, dec := h.pendingAsk(t)

	code, out, _ := runCLI(t, "", "approval", "deny", token, "--json")
	if code != ExitOK {
		t.Fatalf("deny exit = %d\n%s", code, out)
	}
	if d := <-dec; d != approval.Denied {
		t.Fatalf("broker decision = %v", d)
	}

	code, out, _ = runCLI(t, "", "approval", "approve", "NOSUCH", "--json")
	if code != ExitNotFound {
		t.Fatalf("unknown token exit = %d\n%s", code, out)
	}
}

func TestGrantCLIRoundTrip(t *testing.T) {
	h := startCtl(t)
	s, err := h.mgr.OpenHTTP(context.Background(), session.SessionHello{ClientID: "cursor"})
	if err != nil {
		t.Fatal(err)
	}
	// Narrow first so the grant has something to widen.
	err = h.mgr.Mutate(context.Background(), s.ID, func(ov *scope.Overlay) {
		ov.Tools = map[string]*scope.ToolSelector{"github": {Allow: []string{"read_file"}}}
	})
	if err != nil {
		t.Fatal(err)
	}

	code, out, _ := runCLI(t, "", "grant", "request",
		"--session", string(s.ID), "--server", "github", "--tool", "write_file",
		"--reason", "test", "--json")
	if code != ExitOK {
		t.Fatalf("request exit = %d\n%s", code, out)
	}
	env := decodeEnvelope(t, out)
	var created GrantResult
	if err := json.Unmarshal(env.Data, &created); err != nil {
		t.Fatal(err)
	}
	if created.Grant.Status != ctlapi.GrantPending {
		t.Fatalf("created = %s", out)
	}

	code, out, _ = runCLI(t, "", "grant", "ls", "--json")
	if code != ExitOK || !strings.Contains(out, created.Grant.ID) {
		t.Fatalf("ls exit=%d out=%s", code, out)
	}

	code, out, _ = runCLI(t, "", "grant", "approve", created.Grant.ID, "--json")
	if code != ExitOK {
		t.Fatalf("approve exit = %d\n%s", code, out)
	}
	ov := h.mgr.Overlay(s.ID)
	if ov == nil || ov.Tools["github"] == nil ||
		!strings.Contains(strings.Join(ov.Tools["github"].Allow, ","), "write_file") {
		t.Fatalf("overlay after approve = %+v", ov)
	}

	// Idempotent repeat.
	code, out, _ = runCLI(t, "", "grant", "approve", created.Grant.ID, "--json")
	if code != ExitOK || !strings.Contains(out, "already") {
		t.Fatalf("late approve exit=%d out=%s", code, out)
	}

	// Unknown id: exit 3.
	code, _, _ = runCLI(t, "", "grant", "deny", "deadbeef00000000", "--json")
	if code != ExitNotFound {
		t.Fatalf("unknown grant exit = %d", code)
	}
}

func TestGrantRequestUsage(t *testing.T) {
	setDataDir(t)
	code, _, _ := runCLI(t, "", "grant", "request", "--server", "github")
	if code != ExitUsage {
		t.Fatalf("exit = %d, want usage", code)
	}
}
