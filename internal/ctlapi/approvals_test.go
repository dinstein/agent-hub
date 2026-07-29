package ctlapi

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/dinstein/agent-hub/internal/approval"
)

// approvalTestTTL keeps human-deadline waits short in tests.
const approvalTestTTL = 5 * time.Second

// withBroker mutates startServer options to wire a broker (and optionally
// an allowlist under dir).
func withBroker(t *testing.T, allowlistDir string) (func(*Options), *approval.MemBroker) {
	t.Helper()
	var al *approval.Allowlist
	if allowlistDir != "" {
		var err error
		al, err = approval.OpenAllowlist(allowlistDir)
		if err != nil {
			t.Fatal(err)
		}
	}
	broker := approval.NewMemBroker(approval.Options{Allowlist: al, DefaultTTL: approvalTestTTL})
	return func(o *Options) { o.Approvals = broker }, broker
}

// postJSON posts one enveloped request over the raw UDS client.
func postJSON(t *testing.T, sock, path string, body any) (int, []byte) {
	t.Helper()
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := rawClient(sock).Post("http://d"+path, "application/json", bytes.NewReader(raw))
	if err != nil {
		t.Fatalf("POST %s: %v", path, err)
	}
	defer func() { _ = resp.Body.Close() }()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return resp.StatusCode, b
}

func getJSON(t *testing.T, sock, path string, out any) int {
	t.Helper()
	resp, err := rawClient(sock).Get("http://d" + path)
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
	if out != nil && env.OK {
		if err := json.Unmarshal(env.Data, out); err != nil {
			t.Fatalf("GET %s data: %v", path, err)
		}
	}
	return resp.StatusCode
}

// askAsync fires POST /v1/approvals/ask on a goroutine and returns a channel
// with the resulting decision string.
func askAsync(t *testing.T, sock string, ask ApprovalAskWire) <-chan string {
	t.Helper()
	out := make(chan string, 1)
	go func() {
		status, body := postJSON(t, sock, "/v1/approvals/ask", ask)
		var env struct {
			OK   bool                 `json:"ok"`
			Data ApprovalDecisionWire `json:"data"`
		}
		if err := json.Unmarshal(body, &env); err != nil || !env.OK || status != http.StatusOK {
			out <- "decode-error:" + string(body)
			return
		}
		out <- env.Data.Decision
	}()
	return out
}

// waitPendingToken polls the ls endpoint until one pending request appears.
func waitPendingToken(t *testing.T, sock string) ApprovalWire {
	t.Helper()
	deadline := time.Now().Add(gwTestTimeout)
	for time.Now().Before(deadline) {
		var list []ApprovalWire
		getJSON(t, sock, "/v1/approvals", &list)
		if len(list) > 0 {
			return list[0]
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("no pending approval appeared")
	return ApprovalWire{}
}

func TestApprovalAskWithoutFrontendIsUnreachable(t *testing.T) {
	mutate, _ := withBroker(t, "")
	_, env := startServer(t, mutate)

	dec := <-askAsync(t, env.sock, ApprovalAskWire{Server: "srv", Tool: "boom"})
	if dec != "unreachable" {
		t.Fatalf("decision = %q, want unreachable (fail-closed with zero frontends)", dec)
	}
}

func TestApprovalApproveFlow(t *testing.T) {
	mutate, _ := withBroker(t, "")
	client, env := startServer(t, mutate)

	// A subscribed frontend makes FrontendCount > 0 and receives the pending
	// frame WITH argument bytes (SSE is the one channel args may travel).
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	events, err := client.Events.Subscribe(ctx, TopicApprovals)
	if err != nil {
		t.Fatal(err)
	}

	args := json.RawMessage(`{"path":"/tmp/x"}`)
	decCh := askAsync(t, env.sock, ApprovalAskWire{
		Server: "srv", Tool: "boom", Args: args, ArgsHash: "h1",
		GateReason: "destructive", Client: "cli-test", SessionID: "cli-test:1",
	})

	ev, ok := recvEvent(t, events, 2*time.Second)
	if !ok || ev.Kind != "pending" {
		t.Fatalf("pending frame = %+v ok=%v", ev, ok)
	}
	var pend ApprovalWire
	if err := json.Unmarshal(ev.Payload, &pend); err != nil {
		t.Fatal(err)
	}
	if string(pend.Args) != string(args) {
		t.Errorf("SSE pending args = %s, want %s", pend.Args, args)
	}
	if pend.Token == "" || pend.Deadline.IsZero() {
		t.Fatalf("pending frame incomplete: %+v", pend)
	}

	// The poll listing must NOT leak argument bytes.
	listed := waitPendingToken(t, env.sock)
	if len(listed.Args) != 0 {
		t.Errorf("GET /v1/approvals leaked args: %s", listed.Args)
	}
	if listed.Token != pend.Token {
		t.Errorf("ls token = %q, SSE token = %q", listed.Token, pend.Token)
	}

	status, body := postJSON(t, env.sock, "/v1/approvals/"+pend.Token, ApprovalDecideWire{Approve: true})
	if status != http.StatusOK {
		t.Fatalf("decide status = %d body=%s", status, body)
	}
	if dec := <-decCh; dec != "approved" {
		t.Fatalf("ask decision = %q, want approved", dec)
	}

	// The frontends hear the resolution.
	ev, ok = recvEvent(t, events, 2*time.Second)
	if !ok || ev.Kind != "resolved" {
		t.Fatalf("resolved frame = %+v ok=%v", ev, ok)
	}
	var res ApprovalResolved
	if err := json.Unmarshal(ev.Payload, &res); err != nil {
		t.Fatal(err)
	}
	if res.Token != pend.Token || res.Decision != "approved" || res.DecidedBy != "cli" {
		t.Errorf("resolved = %+v", res)
	}

	// Second decision: idempotent 409 naming the first decider.
	status, body = postJSON(t, env.sock, "/v1/approvals/"+pend.Token, ApprovalDecideWire{Approve: false})
	if status != http.StatusConflict {
		t.Fatalf("late decide status = %d body=%s", status, body)
	}
	if !bytes.Contains(body, []byte(CodeAlreadyDecided)) || !bytes.Contains(body, []byte("by cli")) {
		t.Errorf("late decide body = %s", body)
	}

	// History listing shows the decision; audit recorded both actions.
	var hist []ApprovalWire
	getJSON(t, env.sock, "/v1/approvals?history=1", &hist)
	if len(hist) != 1 || hist[0].Decision != "approved" || hist[0].DecidedBy != "cli" {
		t.Errorf("history = %+v", hist)
	}
	var sawAsk, sawDecide bool
	for _, r := range env.aud.records() {
		if strings.HasPrefix(r.Tool, "approvals/ask:") {
			sawAsk = true
			if r.ArgsHash != "h1" || r.Decision != "allowed" {
				t.Errorf("ask audit = %+v", r)
			}
		}
		if strings.HasPrefix(r.Tool, "approvals/decide:") {
			sawDecide = true
		}
	}
	if !sawAsk || !sawDecide {
		t.Errorf("audit missing records: ask=%v decide=%v", sawAsk, sawDecide)
	}
}

func TestApprovalDenyFlow(t *testing.T) {
	mutate, _ := withBroker(t, "")
	client, env := startServer(t, mutate)

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	if _, err := client.Events.Subscribe(ctx, TopicApprovals); err != nil {
		t.Fatal(err)
	}

	decCh := askAsync(t, env.sock, ApprovalAskWire{Server: "srv", Tool: "boom"})
	pend := waitPendingToken(t, env.sock)
	status, body := postJSON(t, env.sock, "/v1/approvals/"+pend.Token, ApprovalDecideWire{Approve: false})
	if status != http.StatusOK {
		t.Fatalf("deny status = %d body=%s", status, body)
	}
	if dec := <-decCh; dec != "denied" {
		t.Fatalf("ask decision = %q, want denied", dec)
	}
}

func TestApprovalRememberForeverSkipsHuman(t *testing.T) {
	mutate, broker := withBroker(t, t.TempDir())
	client, env := startServer(t, mutate)

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	if _, err := client.Events.Subscribe(ctx, TopicApprovals); err != nil {
		t.Fatal(err)
	}

	ask := ApprovalAskWire{Server: "srv", Tool: "boom", ArgsHash: "h", Fingerprint: "v1:abc"}
	decCh := askAsync(t, env.sock, ask)
	pend := waitPendingToken(t, env.sock)
	status, body := postJSON(t, env.sock, "/v1/approvals/"+pend.Token,
		ApprovalDecideWire{Approve: true, Remember: "forever"})
	if status != http.StatusOK {
		t.Fatalf("decide status = %d body=%s", status, body)
	}
	if dec := <-decCh; dec != "approved" {
		t.Fatalf("first ask = %q", dec)
	}
	cancel() // drop the frontend: FrontendCount returns to 0

	waitFor(t, "frontend detach", func() bool { return broker.FrontendCount() == 0 })

	// Same fingerprint: auto-approved from the allowlist, no human needed.
	if dec := <-askAsync(t, env.sock, ask); dec != "approved" {
		t.Fatalf("remembered ask = %q, want approved", dec)
	}
	// Different fingerprint (drifted tool): falls out of the allowlist and,
	// with no frontend, fails Unreachable — never silently approved.
	drifted := ask
	drifted.Fingerprint = "v1:other"
	if dec := <-askAsync(t, env.sock, drifted); dec != "unreachable" {
		t.Fatalf("drifted ask = %q, want unreachable", dec)
	}
}

func TestApprovalDecideErrors(t *testing.T) {
	mutate, _ := withBroker(t, "")
	_, env := startServer(t, mutate)

	// Unknown token: uniform 404.
	status, body := postJSON(t, env.sock, "/v1/approvals/NOSUCH", ApprovalDecideWire{Approve: true})
	if status != http.StatusNotFound || !bytes.Contains(body, []byte(CodeNotFound)) {
		t.Errorf("unknown token: status=%d body=%s", status, body)
	}
	// Unknown remember scope: 400, never guessed.
	status, body = postJSON(t, env.sock, "/v1/approvals/NOSUCH",
		ApprovalDecideWire{Approve: true, Remember: "eternal"})
	if status != http.StatusBadRequest || !bytes.Contains(body, []byte(CodeBadRequest)) {
		t.Errorf("bad remember: status=%d body=%s", status, body)
	}
	// Ask without server/tool: 400.
	status, body = postJSON(t, env.sock, "/v1/approvals/ask", ApprovalAskWire{Tool: "x"})
	if status != http.StatusBadRequest {
		t.Errorf("bad ask: status=%d body=%s", status, body)
	}
}

func TestApprovalEndpointsDisabledWithoutBroker(t *testing.T) {
	_, env := startServer(t, nil)
	if status := getJSON(t, env.sock, "/v1/approvals", nil); status != http.StatusNotFound {
		t.Errorf("ls without broker: status=%d, want uniform 404", status)
	}
	status, _ := postJSON(t, env.sock, "/v1/approvals/ask", ApprovalAskWire{Server: "s", Tool: "t"})
	if status != http.StatusNotFound {
		t.Errorf("ask without broker: status=%d, want uniform 404", status)
	}
}
