package gateway

import (
	"time"

	"github.com/dinstein/agent-hub/internal/downstream"
	"github.com/dinstein/agent-hub/internal/logx"
)

// This file is the gateway's re-dial plane: the answer to "a downstream that
// failed to connect is never dialed again".
//
// Before it, connectOne recorded WHY a dial failed (connErr) and stopped
// there, so every recovery cost a client restart whatever the cause — the
// server was slower to come up than the gateway, the network was briefly
// gone, a stdio server crashed on its first launch, or (the case that
// motivated this) the credential was stored AFTER the gateway had already
// been rejected with a 401.
//
// The passive recovery in internal/downstream/httpauth.go cannot cover that
// last one, and the distinction is the whole reason this file exists: that
// path hangs off a LIVE connection's round tripper, and a handshake that
// failed leaves no connection for any request to travel through. It repairs
// a credential that expired under a working connection; it can do nothing
// for a connection that never opened.
//
// Discovery mode decides nothing here, and must not: in lazy mode a failed
// server's tools are absent from the catalog, so no call can ever arrive to
// trigger a dial on demand. Recovery therefore has to be driven from a
// timer, not from traffic.
// The ladder: 5s, 15s, 45s, 135s, then 5min forever. Slow at the top on
// purpose — a server that is genuinely gone must not be dialed in a tight
// loop for the life of the process, and a stdio entry re-dialed too eagerly
// is a process spawn every time.
//
// Only the base is configurable (Config.RedialBase); the tick and the cap are
// derived from it so a test that shrinks the ladder shrinks all of it. Two
// independent knobs would let a caller set a base above the cap, and the
// first reader to hit that would be debugging arithmetic rather than
// reconnects.
const (
	defaultRedialBase = 5 * time.Second
	redialFactor      = 3
	// redialTickDiv derives the consult interval from the base. It bounds
	// only the granularity of a due time, never the dial rate.
	redialTickDiv = 2
	// redialCapMul derives the ceiling: 60 × 5s = the 5 minutes above.
	redialCapMul = 60
	// minRedialTick keeps a shrunken test ladder from spinning the CPU.
	minRedialTick = 10 * time.Millisecond
)

// redialParams is the resolved ladder of one gateway.
type redialParams struct {
	base time.Duration
	tick time.Duration
	cap  time.Duration
}

func newRedialParams(base time.Duration) redialParams {
	if base <= 0 {
		base = defaultRedialBase
	}
	tick := base / redialTickDiv
	if tick < minRedialTick {
		tick = minRedialTick
	}
	return redialParams{base: base, tick: tick, cap: base * redialCapMul}
}

// startRedial attaches the re-dial loop. It ends on lifeCtx cancellation;
// shutdown joins it through redialWG.
func (g *gateway) startRedial() {
	g.redialWG.Add(1)
	go func() {
		defer g.redialWG.Done()
		g.redialLoop()
	}()
}

func (g *gateway) redialLoop() {
	t := time.NewTicker(g.redial.tick)
	defer t.Stop()
	for {
		select {
		case <-g.lifeCtx.Done():
			return
		case <-t.C:
			g.redialDue(time.Now())
		}
	}
}

// redialDue dials every enabled server whose backoff has elapsed.
func (g *gateway) redialDue(now time.Time) {
	for _, spec := range g.claimDue(now) {
		g.log.Info("re-dialing a downstream that failed to connect",
			logx.Server(spec.ID), "attempt", g.attempts(spec.ID))
		go g.connectOne(spec)
	}
}

// claimDue picks the specs whose rung has come up and CLAIMS each dial in
// the same critical section, so two ticks — or a tick racing the hot-reload
// path — can never dial one server twice.
func (g *gateway) claimDue(now time.Time) []downstream.Spec {
	g.mu.Lock()
	defer g.mu.Unlock()

	var due []downstream.Spec
	for _, spec := range g.specs {
		if _, live := g.servers[spec.ID]; live {
			continue // connected: nothing to recover
		}
		at, armed := g.redialAt[spec.ID]
		if !armed || now.Before(at) {
			// Not armed means no dial has FAILED yet: a server still making
			// its first attempt is "connecting", not broken, and must not be
			// dialed a second time underneath itself.
			continue
		}
		if !g.beginDialLocked(spec.ID) {
			continue // a dial is already in flight under someone else's accounting
		}
		// Claiming does NOT advance the rung. The dial about to start ends in
		// exactly one of two places, and both already own the ladder:
		// noteConnectResult arms the next rung on failure and clears it on
		// success. Arming here as well climbed two rungs per attempt, which
		// turned the documented 5s/15s/45s/135s into 5s/45s/cap — a backoff
		// that looks like it is working while skipping every other rung.
		//
		// The dial slot, not the due time, is what stops the next tick from
		// dialing the same server again while this one is in flight.
		due = append(due, spec)
	}
	return due
}

// armLocked advances one server to its next rung. Callers hold g.mu.
func (g *gateway) armLocked(id string, now time.Time) {
	tries := g.redialTries[id]
	delay := g.redial.base
	for i := 0; i < tries && delay < g.redial.cap; i++ {
		delay *= redialFactor
	}
	if delay > g.redial.cap {
		delay = g.redial.cap
	}
	g.redialTries[id] = tries + 1
	g.redialAt[id] = now.Add(delay)
}

// resetLadderLocked forgets a server's backoff so its next failure starts at
// the base delay again. Callers hold g.mu.
//
// Called on a SUCCESSFUL connect and whenever the entry's definition changes:
// a server the operator just edited is a different server as far as the
// ladder is concerned, and making the fix wait out a rung earned by the
// previous definition is the behaviour this whole file exists to remove.
func (g *gateway) resetLadderLocked(id string) {
	delete(g.redialAt, id)
	delete(g.redialTries, id)
}

// wakeLocked drops a server to the front of the queue: the next tick dials
// it regardless of the rung it had reached. Callers hold g.mu.
//
// This is what a credential announcement turns into (credwatch.go) — the
// stored credential just changed, so the reason for the last rejection may
// be gone, and waiting out a backoff earned before the fix would be exactly
// the "why is it still broken" the ladder is meant to end.
func (g *gateway) wakeLocked(id string) {
	if _, failed := g.connErr[id]; !failed {
		return // connected, or never dialed: nothing to wake
	}
	g.redialTries[id] = 0
	g.redialAt[id] = time.Time{} // zero is always in the past: due next tick
}

// attempts reports how many rungs a server has climbed (for logging).
func (g *gateway) attempts(id string) int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.redialTries[id]
}

// beginDialLocked claims the single in-flight dial slot of one server and
// accounts it as pending. false means a dial is already in flight and the
// caller must not start another. Callers hold g.mu.
//
// One slot per server is what lets three independent dial paths (startup,
// hot reload, re-dial) coexist: without it, a reload landing next to a due
// rung produces two connections, one of which connectOne then has to close
// as the loser of a race it should never have entered.
func (g *gateway) beginDialLocked(id string) bool {
	if _, busy := g.dialing[id]; busy {
		return false
	}
	g.dialing[id] = struct{}{}
	g.pending++
	return true
}

// finishDial releases the slot claimed by beginDialLocked. Every connectOne exit
// path runs it (deferred), including the ones that drop a stale definition.
func (g *gateway) finishDial(id string) {
	g.mu.Lock()
	delete(g.dialing, id)
	g.pending--
	g.mu.Unlock()
}
