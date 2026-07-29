package gateway

import (
	"log/slog"
	"testing"
	"time"

	"github.com/dinstein/agent-hub/internal/downstream"
)

// newCredGateway assembles the smallest gateway that can react to a
// credential announcement: the epoch counter plus the maps the re-dial
// ladder keeps its state in. It is the same struct-literal shape
// TestRedialClimbsExactlyOneRungPerAttempt uses, and for the same reason —
// the reaction is a decision about state, and a real dial would only add
// timing to a question that has none.
func newCredGateway(specIDs ...string) *gateway {
	specs := make([]downstream.Spec, 0, len(specIDs))
	for _, id := range specIDs {
		specs = append(specs, downstream.Spec{ID: id})
	}
	return &gateway{
		log:         slog.New(slog.DiscardHandler),
		redial:      newRedialParams(0),
		redialAt:    map[string]time.Time{},
		redialTries: map[string]int{},
		dialing:     map[string]struct{}{},
		connErr:     map[string]string{},
		servers:     map[string]*downstream.Server{},
		specs:       specs,
		credEpochs:  newCredEpochs(),
	}
}

// TestCredentialChangeWakesAFailedServer is the case the whole announcement
// plane was built for: `agenthub auth login` on a server whose handshake had
// already been rejected. The ladder alone would make that login wait out a
// backoff earned BEFORE the credential existed, which is exactly the "why is
// it still broken" the plane exists to end.
func TestCredentialChangeWakesAFailedServer(t *testing.T) {
	t.Parallel()
	g := newCredGateway("alpha")

	// Four failures in: the next rung is 135s away, far past anything a test
	// would sit through and far past a user's patience.
	for range 4 {
		g.noteConnectResult("alpha", "401 unauthorized")
	}
	if due := g.redialAt["alpha"]; !due.After(time.Now().Add(time.Minute)) {
		t.Fatalf("setup: the ladder is due at %v, want a rung well into the future", due)
	}

	g.onCredentialChanged("alpha")

	// Due next tick, and back at the bottom of the ladder: a server whose
	// credential just changed has not earned the rung it climbed on the old
	// one.
	if got := g.redialTries["alpha"]; got != 0 {
		t.Errorf("ladder is at rung %d after an announcement, want it reset to 0", got)
	}
	claimed := g.claimDue(time.Now())
	if len(claimed) != 1 || claimed[0].ID != "alpha" {
		t.Fatalf("claimDue returned %v, want alpha due immediately", claimed)
	}
}

// TestCredentialChangeOnAConnectedServerDoesNotReconnect pins the other half.
// The daemon's refresher rewrites the vault every 60s, so reconnecting on
// each announcement would be a reconnect storm rather than a fix — the epoch
// bump is the whole reaction, and the next request re-reads the vault
// through the connection that is already up.
func TestCredentialChangeOnAConnectedServerDoesNotReconnect(t *testing.T) {
	t.Parallel()
	g := newCredGateway("alpha")
	g.servers["alpha"] = &downstream.Server{} // connected

	before := g.credEpochs.get("alpha")
	g.onCredentialChanged("alpha")

	if got := g.credEpochs.get("alpha"); got == before {
		t.Errorf("epoch stayed at %d; a connected server's cached bearer would never be dropped", got)
	}
	if _, armed := g.redialAt["alpha"]; armed {
		t.Error("a connected server was armed for re-dial by a credential change")
	}
	if claimed := g.claimDue(time.Now()); len(claimed) != 0 {
		t.Errorf("claimDue returned %v, want nothing: the server is connected", claimed)
	}
}

// TestCredentialChangeLeavesAnUndialedServerAlone. wakeLocked is guarded on a
// RECORDED failure, and that guard is load-bearing in both directions a
// server can be absent from g.servers: one still making its first attempt is
// "connecting", not broken, and dialing it a second time underneath itself is
// the race the single dial slot exists to prevent.
func TestCredentialChangeLeavesAnUndialedServerAlone(t *testing.T) {
	t.Parallel()
	g := newCredGateway("alpha")
	// Not in g.servers and no connErr: the first dial is in flight.

	g.onCredentialChanged("alpha")

	if _, armed := g.redialAt["alpha"]; armed {
		t.Error("a server whose first dial has not landed yet was armed for re-dial")
	}
	// The epoch still moves. It is free, and it is what makes the in-flight
	// dial's own credential read see the value that was just stored.
	if got := g.credEpochs.get("alpha"); got == 0 {
		t.Error("epoch did not move; the dial in flight could cache a superseded credential")
	}
}

// TestCredentialEpochsAreKeyedByServerNotScope. A derived instance inherits
// its base server's login, so one announcement has to invalidate every
// instance. Keying the counter by server id is what makes that automatic;
// the alternative is remembering to bump each derivation, and forgetting one
// leaves it on a dead token with nothing to say so.
func TestCredentialEpochsAreKeyedByServerNotScope(t *testing.T) {
	t.Parallel()
	e := newCredEpochs()
	e.bump("alpha")
	e.bump("alpha")

	if got := e.get("alpha"); got != 2 {
		t.Errorf("alpha epoch = %d, want 2", got)
	}
	if got := e.get("beta"); got != 0 {
		t.Errorf("beta epoch = %d, want 0: one server's login is not another's", got)
	}
}
