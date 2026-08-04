package e2e_test

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"
)

// The two `server` verbs whose contract is about a RUNNING gateway rather
// than about the registry: trace/logs (turn wire capture on under a live
// client) and disable (take a server away from one).
//
// Both are claims the CLI's own help makes and nothing verifies. `server
// trace` says "a running client picks the change up without being
// restarted"; `server disable` says it "takes one away from everybody at
// once". A registry-only test can prove neither.

// serverLogs is the slice of cli.ServerLogs these tests read back.
type serverLogs struct {
	Server string `json:"server"`
	Path   string `json:"path"`
	Frames []struct {
		Dir    string `json:"dir"`
		Method string `json:"method"`
		CallID string `json:"callId"`
		Cause  string `json:"cause"`
		Seq    int    `json:"seq"`
		Bytes  int    `json:"bytes"`
	} `json:"frames"`
	Note string `json:"note"`
}

// readServerLogs reads `server logs <id> --json` with no limit, so a later
// frame can never be cut off by the default window.
func readServerLogs(t *testing.T, dataDir, id string) serverLogs {
	t.Helper()
	out, _ := runAgenthub(t, dataDir, "", "server", "logs", id, "--limit", "0", "--json")
	env := lastEnvelope(t, out)
	if !env.OK {
		t.Fatalf("server logs %s: %s", id, out)
	}
	var logs serverLogs
	if err := json.Unmarshal(env.Data, &logs); err != nil {
		t.Fatalf("server logs data: %v\n%s", err, env.Data)
	}
	return logs
}

// setTrace runs `server trace <id> on|off` and returns the file the frames
// land in.
func setTrace(t *testing.T, dataDir, id, state string) string {
	t.Helper()
	out, _ := runAgenthub(t, dataDir, "", "server", "trace", id, state, "--json")
	env := lastEnvelope(t, out)
	if !env.OK {
		t.Fatalf("server trace %s %s: %s", id, state, out)
	}
	var res struct {
		Trace bool   `json:"trace"`
		Path  string `json:"path"`
	}
	if err := json.Unmarshal(env.Data, &res); err != nil {
		t.Fatalf("server trace data: %v\n%s", err, env.Data)
	}
	if want := state == "on"; res.Trace != want {
		t.Fatalf("trace = %v after `trace %s`", res.Trace, state)
	}
	if res.Path == "" {
		t.Fatal("`server trace` named no file — switching it off still has to " +
			"say which file holds what was already captured")
	}
	return res.Path
}

// recordedWithin reports whether a frame carrying marker has reached the
// trace log within grace.
//
// The grace is for the writer, not for the switch: frames are enqueued and
// flushed asynchronously (audit.Writer never blocks a tool call), so reading
// the file the instant a call returns can miss a frame that is on its way.
func recordedWithin(t *testing.T, dataDir, id, marker string, grace time.Duration) bool {
	t.Helper()
	deadline := time.Now().Add(grace)
	for {
		for _, f := range readServerLogs(t, dataDir, id).Frames {
			if f.CallID == "" || !strings.Contains(showCall(t, dataDir, f.CallID), marker) {
				continue
			}
			return true
		}
		if !time.Now().Before(deadline) {
			return false
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// showCall reads one call's stored bodies back. It is what makes the frame
// assertions possible now that a body lives in the ledger's encrypted pack
// rather than in a plaintext file: the frame line says WHICH call, and this
// says what that call carried.
func showCall(t *testing.T, dataDir, callID string) string {
	t.Helper()
	// --payloads: the frame bodies are the point of the assertion, and they
	// are encrypted at rest, so reading them back is what proves the capture
	// happened at the connection, before anything could filter it.
	out, _ := runAgenthub(t, dataDir, "", "calls", "show", callID, "--payloads", "--json")
	return out
}

// waitTraceState calls the tool repeatedly, with a fresh marker each time,
// until a call is recorded (want=true) or not recorded (want=false).
//
// Retrying is the point rather than an accommodation. `server trace` writes
// the registry and returns; the gateway picks the change up through its
// debounced watch some tens of milliseconds later. A single call issued
// straight after the CLI exits therefore proves nothing in either direction
// — it usually lands BEFORE the reload, which is how the first draft of this
// test managed to report that hot reload was broken when it was the test
// that was racing.
//
// So the assertion is the one the feature actually makes: within the budget,
// calls start (or stop) being recorded. A switch that never takes effect
// times out here; one that takes effect on the third call passes, and that
// is a fair reading of "a running client picks the change up".
func waitTraceState(t *testing.T, c *gatewayClient, dataDir, id, tool string, want bool, budget time.Duration) {
	t.Helper()
	const grace = 2 * time.Second
	deadline := time.Now().Add(budget)
	for i := 0; ; i++ {
		marker := fmt.Sprintf("probe-%v-%d", want, i)
		c.callTool(tool, map[string]any{"marker": marker}, 30*time.Second)
		if recordedWithin(t, dataDir, id, marker, grace) == want {
			return
		}
		if !time.Now().Before(deadline) {
			still := "not "
			if want {
				still = ""
			}
			raw, err := os.ReadFile(readServerLogs(t, dataDir, id).Path)
			c.fatalf("calls were still %srecorded %s after trace was set to %v\n"+
				"log on disk (err=%v):\n%s", still, budget, want, err, raw)
		}
	}
}

// TestServerTraceCapturesFramesForALiveGateway drives the wire recorder the
// way an operator does: a client is already running and misbehaving, and
// the recording has to start without restarting it.
//
// Three claims, all of them from `server trace --help`, none of them
// checkable without a real gateway process:
//
//   - it is OFF by default (a debugging aid that recorded by default would
//     leave every server's traffic on disk on every install);
//   - a running client picks the change up, in both directions;
//   - frames are captured at the connection, before anything filters them,
//     so what is recorded is what the server actually said.
//
// The last one is what the assertion on the ARGUMENT is for. A frame body is
// the one place the exact bytes of an exchange are kept, and a test that only
// counted frames would pass just as happily if the capture had quietly moved
// to somewhere the payload had already been rewritten.
func TestServerTraceCapturesFramesForALiveGateway(t *testing.T) {
	dataDir := t.TempDir()
	runAgenthub(t, dataDir, "", "server", "add", "traced", "--cmd", fakemcpBin, "--json")
	enableServer(t, dataDir, "traced")

	// The bodies live in the ledger's encrypted pack, so capturing them is
	// the evidence tier's job — one key, one place, one protection, instead
	// of a debugging switch writing unredacted traffic to a plaintext file
	// beside an encrypted ledger holding the same bytes.
	runAgenthub(t, dataDir, "", "calls", "enable", "--json")

	c := startGateway(t, dataDir, "traceclient")
	c.initialize()
	c.waitForTool("traced__echo", 30*time.Second)

	// Off by default. This one needs no retry loop: the switch was never on,
	// so there is no reload to wait for, and a frame appearing here is a
	// failure however long one waits.
	c.callTool("traced__echo", map[string]any{"marker": "before-trace"}, 30*time.Second)
	if logs := readServerLogs(t, dataDir, "traced"); len(logs.Frames) != 0 {
		t.Fatalf("frames were recorded with trace off: %+v", logs.Frames)
	}

	path := setTrace(t, dataDir, "traced", "on")
	waitTraceState(t, c, dataDir, "traced", "traced__echo", true, 30*time.Second)

	// What is captured is the real conversation, in both directions, with
	// the argument in it verbatim.
	const marker = "verbatim-argument"
	c.callTool("traced__echo", map[string]any{"marker": marker}, 30*time.Second)
	if !recordedWithin(t, dataDir, "traced", marker, 5*time.Second) {
		c.fatalf("a call made with tracing established was not recorded")
	}
	var sawRequest, sawResponse bool
	for _, f := range readServerLogs(t, dataDir, "traced").Frames {
		if f.Method != "tools/call" || f.CallID == "" ||
			!strings.Contains(showCall(t, dataDir, f.CallID), marker) {
			continue
		}
		// The frame's own kind is its direction, and both halves of the
		// exchange must be there: one direction alone is a conversation with
		// nobody answering.
		switch f.Dir {
		case "sent":
			sawRequest = true
		case "recv":
			sawResponse = true
		}
		if f.Cause != "call" {
			t.Errorf("a client call produced a frame with cause %q", f.Cause)
		}
	}
	if !sawRequest {
		c.fatalf("no outbound tools/call frame carried the argument verbatim")
	}
	if !sawResponse {
		c.fatalf("the downstream's answer was not recorded; only one direction " +
			"of the conversation is in the file")
	}
	if _, err := os.Stat(path); err != nil {
		t.Errorf("`server trace` named %s but the frames went elsewhere: %v", path, err)
	}
	// The join key is what the move into the ledger bought: every frame of a
	// client call names the call, so "what did this call actually do" is one
	// question rather than two streams and a guess.
	for _, f := range readServerLogs(t, dataDir, "traced").Frames {
		if f.Cause == "call" && f.CallID == "" {
			t.Fatalf("a call frame has no call id: %+v", f)
		}
	}

	// And off again: the switch has to release as well as engage, or "turn
	// it on to diagnose something, and off again afterwards" leaves
	// unredacted traffic accumulating on disk for as long as the client runs.
	setTrace(t, dataDir, "traced", "off")
	waitTraceState(t, c, dataDir, "traced", "traced__echo", false, 30*time.Second)
	c.close()
}

// TestServerDisableWithdrawsToolsFromALiveGateway is the live half of
// `server disable`: "takes one away from everybody at once, and no profile
// can put it back".
//
// Withdrawal is the direction that matters. An operator disables a server
// because they want it gone NOW — a credential leaked, a downstream started
// misbehaving — and a change that only reaches clients started afterwards
// leaves every session that is already open still calling it, which is
// exactly the set of sessions the operator was worried about.
func TestServerDisableWithdrawsToolsFromALiveGateway(t *testing.T) {
	dataDir := t.TempDir()
	runAgenthub(t, dataDir, "", "server", "add", "alpha", "--cmd", fakemcpBin, "--json")
	enableServer(t, dataDir, "alpha")
	runAgenthub(t, dataDir, "", "server", "add", "beta", "--cmd", fakemcpBin, "--json")
	enableServer(t, dataDir, "beta")

	c := startGateway(t, dataDir, "disableclient")
	c.initialize()
	c.waitForTool("alpha__echo", 30*time.Second)
	c.waitForTool("beta__echo", 30*time.Second)

	runAgenthub(t, dataDir, "", "server", "disable", "alpha", "--json")
	waitTools(t, c, 30*time.Second, "alpha__echo withdrawn", func(names []string) bool {
		return !hasTool(names, "alpha__echo")
	})
	if names := c.listTools(30 * time.Second); !hasTool(names, "beta__echo") {
		c.fatalf("disabling alpha took beta with it: %v", names)
	}

	// And back: re-enabling restores it to the same live session, so the
	// disable is a switch rather than a one-way door.
	runAgenthub(t, dataDir, "", "server", "enable", "alpha", "--no-probe", "--json")
	c.waitForTool("alpha__echo", 30*time.Second)
	c.close()
}
