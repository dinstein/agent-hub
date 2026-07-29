package gateway

import (
	"context"
	"encoding/json"
	"log/slog"
	"path/filepath"
	"testing"
	"time"

	"github.com/dinstein/agent-hub/internal/approval"
	"github.com/dinstein/agent-hub/internal/ctlapi"
	"github.com/dinstein/agent-hub/internal/event"
	"github.com/dinstein/agent-hub/internal/integrity"
	"github.com/dinstein/agent-hub/internal/pipeline"
	"github.com/dinstein/agent-hub/internal/registry"
	"github.com/dinstein/agent-hub/internal/session"
)

// startApprovalDaemon boots a real control-plane server WITH a broker on
// socket and returns the broker (the test plays the human frontend).
func startApprovalDaemon(t *testing.T, socket string) (*approval.MemBroker, func()) {
	t.Helper()
	reg, err := registry.Open(filepath.Join(t.TempDir(), "reg"))
	if err != nil {
		t.Fatal(err)
	}
	bus := event.NewBus()
	broker := approval.NewMemBroker(approval.Options{DefaultTTL: 5 * time.Second})
	srv, err := ctlapi.NewServer(ctlapi.Options{
		Version:   "test",
		Registry:  reg,
		Sessions:  session.NewMemoryManager(session.Options{Bus: bus}),
		Bus:       bus,
		Approvals: broker,
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
	stop := func() { _ = srv.Close() }
	t.Cleanup(stop)
	return broker, stop
}

// newAskerUnderTest builds the minimal gateway state gwAsker touches: the
// client id, logger and control link.
func newAskerUnderTest(t *testing.T, socket string) *gwAsker {
	t.Helper()
	g := &gateway{
		cfg: Config{ClientID: "asker-test"},
		log: slog.New(slog.DiscardHandler),
	}
	if socket != "" {
		g.ctl = newCtlLink(g, socket, time.Second)
	}
	return &gwAsker{g: g}
}

// answerFirst subscribes as a frontend and answers the first fanned-out
// request. Returns the received request for assertions.
func answerFirst(t *testing.T, broker *approval.MemBroker, approve bool) <-chan approval.Request {
	t.Helper()
	got := make(chan approval.Request, 1)
	ch, cancel := broker.Subscribe("test-frontend")
	go func() {
		defer cancel()
		req, ok := <-ch
		if !ok {
			return
		}
		_ = broker.AnswerAs(req.Token, approve, approval.RememberNone, "test-frontend")
		got <- req
	}()
	return got
}

func TestGwAskerApproveDeny(t *testing.T) {
	t.Parallel()
	_, socket := linkResolver(t, t.TempDir())
	broker, _ := startApprovalDaemon(t, socket)
	a := newAskerUnderTest(t, socket)

	args := json.RawMessage(`{"x":1}`)
	ctx := withCallMeta(context.Background(), callMeta{
		args: args,
		snap: integrity.ToolSnapshot{Name: "boom", Description: "d", InputSchema: json.RawMessage(`{"type":"object"}`)},
	})

	reqCh := answerFirst(t, broker, true)
	dec, err := a.Ask(ctx, pipeline.ApprovalRequest{
		ServerID: "srv", RawTool: "boom", ArgsHash: "h1", Destructive: true,
	})
	if err != nil || dec != pipeline.DecisionApproved {
		t.Fatalf("approve: dec=%q err=%v", dec, err)
	}
	req := <-reqCh
	// The wire request carried everything the frontend needs: raw args over
	// the socket, the pipeline's args binding, the live-definition
	// fingerprint, the destructive gate reason and the caller identity.
	if string(req.ArgsJSON) != string(args) || req.ArgsHash != "h1" {
		t.Errorf("wire args = %s hash=%q", req.ArgsJSON, req.ArgsHash)
	}
	wantFP, err := integrity.Fingerprint(integrity.ToolSnapshot{
		Name: "boom", Description: "d", InputSchema: json.RawMessage(`{"type":"object"}`)})
	if err != nil || req.Fingerprint != wantFP {
		t.Errorf("wire fingerprint = %q, want %q (err=%v)", req.Fingerprint, wantFP, err)
	}
	if req.GateReason != approval.ReasonDestructive || req.Client != "asker-test" {
		t.Errorf("wire req = %+v", req)
	}

	answerFirst(t, broker, false)
	dec, err = a.Ask(ctx, pipeline.ApprovalRequest{ServerID: "srv", RawTool: "boom", ArgsHash: "h2"})
	if err != nil || dec != pipeline.DecisionDenied {
		t.Fatalf("deny: dec=%q err=%v", dec, err)
	}
}

func TestGwAskerFailClosed(t *testing.T) {
	t.Parallel()
	// No control link at all (socket path unresolvable at startup).
	a := newAskerUnderTest(t, "")
	dec, err := a.Ask(context.Background(), pipeline.ApprovalRequest{ServerID: "s", RawTool: "t"})
	if err != nil || dec != pipeline.DecisionUnavailable {
		t.Fatalf("nil link: dec=%q err=%v, want unavailable", dec, err)
	}

	// Daemon never started: connection refused -> Unreachable -> blocked.
	_, socket := linkResolver(t, t.TempDir())
	a = newAskerUnderTest(t, socket)
	dec, err = a.Ask(context.Background(), pipeline.ApprovalRequest{ServerID: "s", RawTool: "t"})
	if err != nil || dec != pipeline.DecisionUnavailable {
		t.Fatalf("no daemon: dec=%q err=%v, want unavailable", dec, err)
	}

	// Daemon up but zero frontends: broker answers unreachable immediately.
	_, socket2 := linkResolver(t, t.TempDir())
	_, _ = startApprovalDaemon(t, socket2)
	a = newAskerUnderTest(t, socket2)
	dec, err = a.Ask(context.Background(), pipeline.ApprovalRequest{ServerID: "s", RawTool: "t"})
	if err != nil || dec != pipeline.DecisionUnavailable {
		t.Fatalf("no frontend: dec=%q err=%v, want unavailable", dec, err)
	}
}

// TestGwAskerDaemonDiesMidFlight: the daemon force-closes every connection
// (in-process kill -9, A.3 #2) while an ask is blocked waiting for a human.
// The gateway must observe a rejection, never an approval. (The real
// kill -9 across processes is exercised in test/e2e.)
func TestGwAskerDaemonDiesMidFlight(t *testing.T) {
	t.Parallel()
	_, socket := linkResolver(t, t.TempDir())
	broker, stop := startApprovalDaemon(t, socket)
	a := newAskerUnderTest(t, socket)

	sub, cancel := broker.Subscribe("victim") // a frontend exists but never answers
	defer cancel()
	go func() {
		<-sub // the request reached the broker: now the daemon dies
		stop()
	}()
	dec, err := a.Ask(context.Background(), pipeline.ApprovalRequest{ServerID: "s", RawTool: "t"})
	if err != nil || dec != pipeline.DecisionUnavailable {
		t.Fatalf("mid-flight kill: dec=%q err=%v, want unavailable", dec, err)
	}
}
