package ctlapi

import (
	"testing"

	"github.com/dinstein/agent-hub/internal/downstream"
)

// TestDownstreamConnStatesAreKnownHere pins the cross-package contract
// internal/downstream/probe.go states: its ConnState values ARE the wire
// strings of this package's ConnState. downstream must not import the control
// plane, so the two enums are separate declarations kept in agreement by
// their string values — and probe.go names a contract test on this side as
// what keeps them that way. There was none until this one.
//
// The failure it prevents is quiet on the producing side and loud on the
// wrong one: rename "connecting" to "pending" in downstream and every gateway
// keeps reporting happily, while connSeverity's default ranks the value above
// every known state and ComputeHealth surfaces "unknown connection state" for
// a server that is merely starting up. Fail-toward-visibility is the right
// direction for a state source bug, and it is still an operator chasing a
// health alert that means nothing.
//
// WHAT IT DOES NOT DO: enumerate downstream's constants. Go offers no
// exhaustiveness check over string constants, so a NEW state added there is
// not caught here — the list below has to grow with it, which is what the
// failure message says. What is covered is the likelier drift by far: one of
// these three values changing under a name that still compiles on both sides.
func TestDownstreamConnStatesAreKnownHere(t *testing.T) {
	for _, tc := range []struct {
		name string
		from downstream.ConnState
		want ConnState
	}{
		{"connecting", downstream.ConnConnecting, ConnConnecting},
		{"connected", downstream.ConnConnected, ConnConnected},
		{"error", downstream.ConnError, ConnError},
	} {
		if got := ConnState(tc.from); got != tc.want {
			t.Errorf("downstream reports %q for %s; this package spells it %q. "+
				"The two enums are kept in agreement by their string values — change both, "+
				"or the state arrives here unrecognized.", tc.from, tc.name, tc.want)
			continue
		}
		if sev := connSeverity(ConnState(tc.from)); sev == connSeverity(ConnState("a value nothing declares")) {
			t.Errorf("downstream's %s state (%q) falls into connSeverity's unrecognized branch, "+
				"so ComputeHealth reports \"unknown connection state\" for it. If downstream has "+
				"grown a state, add it to this package's ConnState and to the table above.",
				tc.name, tc.from)
		}
	}
}
