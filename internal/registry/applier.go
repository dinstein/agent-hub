package registry

import "sync"

// Applier implements the adoption criterion for registry reloads
// (docs/conventions.md#the-hot-reload-path #2).
//
// A Change event is a notification, not a snapshot: on receipt the consumer
// re-reads the registry itself. The criterion for adopting what it read is
//
//	read generation >= last applied generation
//
// and never "read generation == event Rev". With the equality test, rapid
// consecutive writes coalesce so that the consumer reads a generation newer
// than the event it is handling, rejects it, and then waits forever for an
// event matching what it already read — stuck on the old version. The >=
// test adopts any state at least as new as what is currently applied;
// re-applying the same generation is idempotent by construction (the read
// state is the applied state).
type Applier struct {
	mu      sync.Mutex
	applied uint64
	seeded  bool
}

// ShouldApply reports whether a snapshot read at gen must be adopted:
// true iff nothing has been applied yet or gen >= the last applied
// generation. It does not record anything — pair it with MarkApplied, or use
// Apply for the combined operation.
func (a *Applier) ShouldApply(gen uint64) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.shouldApplyLocked(gen)
}

func (a *Applier) shouldApplyLocked(gen uint64) bool {
	return !a.seeded || gen >= a.applied
}

// MarkApplied records gen as applied. The applied generation never regresses:
// marking an older generation than the current one is a no-op, so a late
// out-of-order apply cannot roll the criterion backwards.
func (a *Applier) MarkApplied(gen uint64) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.markAppliedLocked(gen)
}

func (a *Applier) markAppliedLocked(gen uint64) {
	if !a.seeded || gen > a.applied {
		a.applied = gen
	}
	a.seeded = true
}

// Applied returns the last applied generation and whether anything has been
// applied yet.
func (a *Applier) Applied() (uint64, bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.applied, a.seeded
}

// Apply runs apply and records gen iff the criterion admits gen. It returns
// whether apply ran and its error; a failed apply records nothing, so the
// same state is re-attempted on the next trigger.
//
// The lock is held across apply: concurrent Apply calls serialize, which
// keeps "check criterion" and "apply state" atomic — without it two racing
// reloads could interleave and finish with the older state applied last.
func (a *Applier) Apply(gen uint64, apply func() error) (bool, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if !a.shouldApplyLocked(gen) {
		return false, nil
	}
	if err := apply(); err != nil {
		return false, err
	}
	a.markAppliedLocked(gen)
	return true, nil
}
