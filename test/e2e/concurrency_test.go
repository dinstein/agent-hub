package e2e_test

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// "One hub, shared by every AI client" is the product claim, and until the
// dispatcher landed the suite could not make a single statement about it: the
// client held one request at a time, so nothing here ever had two calls in
// flight, let alone two clients.
//
// What that left unexercised is not obscure. The gateway runs every
// tools/call on its own goroutine, and its own log comment explains that the
// request id is what tells six concurrent calls apart. internal/downstream
// serializes per server through an owner queue — a design decision whose
// whole point is that it serializes ONE server and not the hub.
// notifications/cancelled reaches a per-request cancel map that only exists
// because requests overlap. Every one of those is a property that a serial
// client would report as working no matter how it was broken.

// writeSlowScript writes a fakemcp script whose tools/call sleeps before
// answering, so a call to this server stays in flight long enough for
// another one to be observed finishing first.
//
// The JSON is hand-rolled like writeScript's, rather than built from
// fakemcp.Script: this suite drives the product from outside, and the script
// file is part of that outside — a struct literal would stop proving the
// on-disk shape is what the binary reads.
func writeSlowScript(t *testing.T, path string, delay time.Duration, toolNames ...string) {
	t.Helper()
	type toolDef struct {
		Name        string          `json:"name"`
		Description string          `json:"description"`
		InputSchema json.RawMessage `json:"inputSchema"`
	}
	tools := make([]map[string]any, 0, len(toolNames))
	for _, n := range toolNames {
		tools = append(tools, map[string]any{"def": toolDef{
			Name:        n,
			Description: "echoes its arguments back as text, slowly",
			InputSchema: json.RawMessage(`{"type":"object"}`),
		}})
	}
	data, err := json.Marshal(map[string]any{
		"tools": tools,
		"rules": []map[string]any{{
			"method": "tools/call",
			"actions": []map[string]any{
				{"kind": "sleep", "delay": delay.String()},
				{"kind": "respond"},
			},
		}},
	})
	if err != nil {
		t.Fatalf("marshal slow fakemcp script: %v", err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatalf("write slow fakemcp script: %v", err)
	}
}

// TestASlowServerDoesNotBlockAnother is the property the owner queue exists
// to bound: calls to ONE downstream are serialized, and that must not
// serialize the hub.
//
// A hub fronting a dozen servers where one slow tool stalls the other eleven
// is the failure this shape is chosen to avoid, and it is invisible from a
// serial client — which would observe the calls completing in the order it
// sent them either way, because it could only send them in order.
func TestASlowServerDoesNotBlockAnother(t *testing.T) {
	dataDir := t.TempDir()

	slow := filepath.Join(t.TempDir(), "slow.json")
	writeSlowScript(t, slow, 5*time.Second, "echo")
	runAgenthub(t, dataDir, "", "server", "add", "slow", "--cmd", fakemcpBin, "--args", slow, "--json")
	enableServer(t, dataDir, "slow")
	runAgenthub(t, dataDir, "", "server", "add", "fast", "--cmd", fakemcpBin, "--json")
	enableServer(t, dataDir, "fast")

	c := startGateway(t, dataDir, "concclient")
	c.initialize()
	c.waitForTool("slow__echo", 30*time.Second)
	c.waitForTool("fast__echo", 30*time.Second)

	slowCall := c.begin("tools/call", map[string]any{
		"name": "slow__echo", "arguments": map[string]any{"marker": "slow"},
	})
	// The fast call is sent while the slow one is demonstrably unanswered,
	// and must come back without waiting for it.
	fastStarted := time.Now()
	fast := c.callTool("fast__echo", map[string]any{"marker": "fast"}, 30*time.Second)
	fastTook := time.Since(fastStarted)

	if !strings.Contains(c.textContent(fast), "fast") {
		c.fatalf("fast__echo did not echo while slow__echo was in flight")
	}
	if slowCall.settled() {
		c.fatalf("the 5s call had already answered; the fixture proves nothing")
	}
	// Generous, because the assertion is "did not wait for the slow one", not
	// a latency budget: the slow server sleeps 5s, so anything under half of
	// that settles the question on the slowest CI runner.
	if fastTook > 2500*time.Millisecond {
		c.fatalf("the fast call took %s while a 5s call was in flight; the hub serialized them", fastTook)
	}

	// The slow one still completes correctly — it was overtaken, not lost.
	res, rpcErr := slowCall.await(30 * time.Second)
	if rpcErr != nil {
		c.fatalf("the overtaken slow call failed: %v", rpcErr)
	}
	if !strings.Contains(c.textContent(res), "slow") {
		c.fatalf("the slow call's answer is not its own: %s", res)
	}
	c.close()
}

// TestConcurrentCallsEachGetTheirOwnAnswer sends many calls at once and
// checks that every answer went back to the request that asked for it.
//
// Serializing per server is correct; CROSS-WIRING two answers is not, and
// the two are indistinguishable to any test that never has two calls
// outstanding. Each call carries a distinct marker, and the echo tool
// returns its own arguments, so a swap is detectable rather than merely
// improbable.
func TestConcurrentCallsEachGetTheirOwnAnswer(t *testing.T) {
	dataDir := t.TempDir()
	runAgenthub(t, dataDir, "", "server", "add", "alpha", "--cmd", fakemcpBin, "--json")
	enableServer(t, dataDir, "alpha")

	c := startGateway(t, dataDir, "manycalls")
	c.initialize()
	c.waitForTool("alpha__echo", 30*time.Second)

	const n = 12
	calls := make([]*inflight, n)
	for i := range calls {
		calls[i] = c.begin("tools/call", map[string]any{
			"name":      "alpha__echo",
			"arguments": map[string]any{"marker": fmt.Sprintf("call-%d", i)},
		})
	}
	// Awaited in REVERSE order, so the test cannot pass by accident on a
	// client that simply reads answers off the wire in arrival order.
	for i := n - 1; i >= 0; i-- {
		res, rpcErr := calls[i].await(60 * time.Second)
		if rpcErr != nil {
			c.fatalf("concurrent call %d failed: %v", i, rpcErr)
		}
		want := fmt.Sprintf("call-%d", i)
		got := c.textContent(res)
		if !strings.Contains(got, want) {
			c.fatalf("call %d got an answer belonging to another request: want %q, got %q", i, want, got)
		}
	}
	c.close()
}

// TestCancellingOneCallLeavesTheOthersAlone drives notifications/cancelled,
// which is the only reason the gateway keeps a per-request cancel map at all.
//
// Two halves, and the second is the one worth the fixture. A cancelled
// request must get NO response — receivers of a cancellation must not expect
// one — and cancelling it must not take down the calls sharing the
// connection. A cancel that cancelled the wrong request, or the whole
// session, is exactly what a map keyed by request id is there to prevent, and
// nothing without overlapping calls can tell the difference.
func TestCancellingOneCallLeavesTheOthersAlone(t *testing.T) {
	dataDir := t.TempDir()

	slow := filepath.Join(t.TempDir(), "slow.json")
	writeSlowScript(t, slow, 10*time.Second, "echo")
	runAgenthub(t, dataDir, "", "server", "add", "slow", "--cmd", fakemcpBin, "--args", slow, "--json")
	enableServer(t, dataDir, "slow")
	runAgenthub(t, dataDir, "", "server", "add", "fast", "--cmd", fakemcpBin, "--json")
	enableServer(t, dataDir, "fast")

	c := startGateway(t, dataDir, "cancelclient")
	c.initialize()
	c.waitForTool("slow__echo", 30*time.Second)
	c.waitForTool("fast__echo", 30*time.Second)

	doomed := c.begin("tools/call", map[string]any{
		"name": "slow__echo", "arguments": map[string]any{"marker": "doomed"},
	})
	// A 10s downstream sleep, so the cancellation lands mid-flight rather
	// than racing a call that already finished.
	c.notify("notifications/cancelled", map[string]any{
		"requestId": doomed.id,
		"reason":    "e2e cancellation",
	})

	// The rest of the session is unaffected: a call on the other server
	// answers normally afterwards.
	if got := c.textContent(c.callTool("fast__echo", map[string]any{"marker": "survivor"},
		30*time.Second)); !strings.Contains(got, "survivor") {
		c.fatalf("cancelling one call broke the session: %q", got)
	}

	// And the cancelled one is never answered. Waited out past the
	// downstream's own sleep, so this is "no reply ever" and not "no reply
	// yet" — if the gateway ignored the cancellation, the result would have
	// arrived within that window.
	select {
	case <-doomed.ch:
		c.fatalf("the cancelled request was answered; a cancelled call must get no response")
	case <-time.After(12 * time.Second):
	}
	doomed.abandon()
	c.close()
}

// TestTwoClientsShareTheHubConcurrently is the claim in the repository's
// first sentence, exercised: two AI clients, one hub, calls overlapping.
//
// Two clients are two whole gateway PROCESSES here, which is why this cannot
// be reduced to the case above. Each has its own catalog, its own scope
// resolution and its own downstream connections, and everything they share is
// on disk — so a leak between them shows up as one client's call being
// answered by the other's server, or as one blocking the other.
func TestTwoClientsShareTheHubConcurrently(t *testing.T) {
	dataDir := t.TempDir()
	runAgenthub(t, dataDir, "", "server", "add", "alpha", "--cmd", fakemcpBin, "--json")
	enableServer(t, dataDir, "alpha")

	first := startGateway(t, dataDir, "clientone")
	second := startGateway(t, dataDir, "clienttwo")
	for _, c := range []*gatewayClient{first, second} {
		c.initialize()
		c.waitForTool("alpha__echo", 45*time.Second)
	}

	// Every request of both clients is sent before any answer is collected,
	// so all sixteen are outstanding across two processes at once. No
	// goroutine is needed for that — begin does not block — and none is
	// wanted: await fails the test on timeout, and t.Fatalf is only legal on
	// the test's own goroutine.
	const each = 8
	type tagged struct {
		call *inflight
		want string
	}
	var sent []tagged
	for _, tc := range []struct {
		c      *gatewayClient
		marker string
	}{{first, "one"}, {second, "two"}} {
		for i := range each {
			want := fmt.Sprintf("%s-%d", tc.marker, i)
			sent = append(sent, tagged{
				call: tc.c.begin("tools/call", map[string]any{
					"name":      "alpha__echo",
					"arguments": map[string]any{"marker": want},
				}),
				want: want,
			})
		}
	}
	for _, s := range sent {
		res, rpcErr := s.call.await(60 * time.Second)
		if rpcErr != nil {
			t.Errorf("call %q failed: %v", s.want, rpcErr)
			continue
		}
		if !strings.Contains(string(res), s.want) {
			t.Errorf("call %q was answered with %s", s.want, res)
		}
	}

	first.close()
	second.close()
}
