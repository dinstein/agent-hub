package cli

import (
	"testing"
	"time"

	"github.com/dinstein/agent-hub/internal/ctlapi"
)

func newWatchState() *watchState {
	return &watchState{
		entries: map[int]ctlapi.ApprovalWire{},
		byToken: map[string]int{},
	}
}

func wire(token string) ctlapi.ApprovalWire {
	return ctlapi.ApprovalWire{
		Token: token, Server: "gh", Tool: "delete_repo",
		Deadline: time.Now().Add(time.Minute),
	}
}

// TestWatchStateKeepsNumbersStableAcrossReplays is the property `approval
// watch` is built on: the number printed next to a request is what the
// operator types to approve it, so it must name the same request for as long
// as it is on screen.
//
// The SSE subscription reconnects with backoff and the broker replays its
// pending queue, so the same request arrives again — routinely, not
// exceptionally. Renumbering on a replay would mean the operator types
// "a 2" for what the screen showed as [2] and approves whatever request
// inherited that number. For a HITL gate the failure is silent and in the
// permissive direction: a call gets approved, just not the one they read.
func TestWatchStateKeepsNumbersStableAcrossReplays(t *testing.T) {
	ws := newWatchState()

	first, ok := ws.add(wire("tok-a"))
	if !ok || first != 1 {
		t.Fatalf("first add = (%d, %v), want (1, true)", first, ok)
	}
	second, ok := ws.add(wire("tok-b"))
	if !ok || second != 2 {
		t.Fatalf("second add = (%d, %v), want (2, true)", second, ok)
	}

	// The replay: same tokens again, as a reconnect delivers them.
	for _, tok := range []string{"tok-a", "tok-b"} {
		if n, ok := ws.add(wire(tok)); ok {
			t.Fatalf("replay of %s was accepted as new request [%d]", tok, n)
		}
	}
	if len(ws.entries) != 2 {
		t.Fatalf("entries = %v, want the original two", ws.entries)
	}
	if ws.byToken["tok-a"] != 1 || ws.byToken["tok-b"] != 2 {
		t.Fatalf("numbering moved under replay: %v", ws.byToken)
	}
}

// TestWatchStateNeverReusesANumber: a number that has been resolved must not
// be handed to a later request. The operator may still have the old line on
// screen — or already be typing it — when a new request arrives, and reuse
// turns that keystroke into a decision about a different call.
func TestWatchStateNeverReusesANumber(t *testing.T) {
	ws := newWatchState()
	first, _ := ws.add(wire("tok-a"))

	n, ok := ws.resolve("tok-a")
	if !ok || n != first {
		t.Fatalf("resolve = (%d, %v), want (%d, true)", n, ok, first)
	}
	if len(ws.entries) != 0 || len(ws.byToken) != 0 {
		t.Fatalf("resolve left state behind: entries=%v byToken=%v", ws.entries, ws.byToken)
	}

	next, ok := ws.add(wire("tok-b"))
	if !ok {
		t.Fatal("add after resolve was rejected")
	}
	if next == first {
		t.Fatalf("number %d was reused for a different request", next)
	}

	// And the freed token may legitimately return (a re-raised approval)
	// without colliding with anything.
	again, ok := ws.add(wire("tok-a"))
	if !ok {
		t.Fatal("a resolved token could not be re-added")
	}
	if again == first || again == next {
		t.Fatalf("re-added token got a reused number %d", again)
	}
}

// TestWatchStateResolveIsHonestAboutUnknownTokens: `resolved` events arrive
// for requests this watcher never displayed (another frontend decided first),
// and reporting a bogus number for them would print a line about a request
// the operator never saw.
func TestWatchStateResolveIsHonestAboutUnknownTokens(t *testing.T) {
	ws := newWatchState()
	ws.add(wire("tok-a"))

	if n, ok := ws.resolve("never-seen"); ok || n != 0 {
		t.Fatalf("resolve of an unknown token = (%d, %v), want (0, false)", n, ok)
	}
	if len(ws.entries) != 1 {
		t.Fatalf("an unknown resolve disturbed the table: %v", ws.entries)
	}
	// Resolving twice is not an error either: the local drop in watchCommand
	// races the resolved event, so the second one must be a no-op.
	if _, ok := ws.resolve("tok-a"); !ok {
		t.Fatal("first resolve failed")
	}
	if _, ok := ws.resolve("tok-a"); ok {
		t.Fatal("second resolve reported success for an already-dropped request")
	}
}
