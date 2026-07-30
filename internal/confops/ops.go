package confops

import (
	"context"
	"errors"
	"maps"
	"slices"
	"strings"

	"github.com/dinstein/agent-hub/internal/registry"
)

// Precondition is the optimistic-concurrency guard of every operation.
//
// Generation == 0 means "do not check" — the non-interactive path used by
// scripts and by the CLI, whose behaviour is therefore unchanged. A non-zero
// value is the generation the caller last read; the operation refuses with
// *StaleError if the registry has moved on.
type Precondition struct {
	Generation uint64
}

// check compares the caller's expectation against the generation observed
// under the lock. It is called INSIDE registry.Store.Update, before any
// mutation, so there is no window between the comparison and the write.
func (p Precondition) check(current uint64) error {
	if p.Generation == 0 || p.Generation == current {
		return nil
	}
	return &StaleError{Want: p.Generation, Got: current}
}

// Result is what every operation returns alongside its domain-specific
// payload: the generation the registry now stands at, the healed-quarantine
// reports (a document was unreadable, got quarantined and reset — usable,
// but the operator must hear about it), and whether the write actually
// changed anything.
//
// Changed is derived from the generation, which the registry bumps only for
// a real state change (its no-op guard compares parsed JSON values). A
// re-set of an identical value therefore reports Changed == false without
// the operation having to diff anything itself.
type Result struct {
	Generation uint64
	Warnings   []string
	Changed    bool
}

// apply is the shared write path: lock → reload → precondition → mutate →
// atomic commit → generation bump, with quarantine reports demoted to
// warnings.
//
// Failure direction: fn returning an error aborts the transaction with
// nothing written, and a failed precondition is exactly such an error.
func apply(ctx context.Context, st *registry.Store, pre Precondition, fn func(tx *registry.Tx) error) (Result, error) {
	if st == nil {
		return Result{}, usagef("no registry store")
	}
	var before uint64
	uerr := st.Update(ctx, func(tx *registry.Tx) error {
		before = tx.Generation()
		if err := pre.check(before); err != nil {
			return err
		}
		return fn(tx)
	})
	warnings, fatal := splitQuarantine(uerr)
	if fatal != nil {
		return Result{Warnings: warnings}, fatal
	}
	after := st.Snapshot().Generation
	return Result{Generation: after, Warnings: warnings, Changed: after != before}, nil
}

// splitQuarantine separates healed-quarantine reports (a document was
// unreadable, was preserved under a .unreadable-<ts> name and reset to its
// default, and the store is fully usable) from fatal errors.
//
// Failure direction: healed corruption is a WARNING, never a silent success
// and never a hard failure — the operator must be told, and the operation
// must still go through, because refusing would leave them unable to repair
// the configuration through the very tool that reported it.
func splitQuarantine(err error) (warnings []string, fatal error) {
	var fatals []error
	var walk func(error)
	walk = func(e error) {
		if e == nil {
			return
		}
		if multi, ok := e.(interface{ Unwrap() []error }); ok {
			for _, c := range multi.Unwrap() {
				walk(c)
			}
			return
		}
		var ue *registry.UnreadableError
		if errors.As(e, &ue) {
			warnings = append(warnings, ue.Error())
			return
		}
		fatals = append(fatals, e)
	}
	walk(err)
	return warnings, errors.Join(fatals...)
}

// dedupSorted trims, de-duplicates and sorts a string slice while keeping
// the nil/empty distinction that the three-state selectors depend on: nil in
// stays nil (no intervention), an all-blank input becomes the EMPTY slice
// (block-all), never nil. Collapsing empty to nil would turn block-all into
// allow-all, which is the fail-open direction.
func dedupSorted(in []string) []string {
	if in == nil {
		return nil
	}
	out := make([]string, 0, len(in))
	for _, s := range in {
		if s = strings.TrimSpace(s); s != "" {
			out = append(out, s)
		}
	}
	slices.Sort(out)
	return slices.Compact(out)
}

// sortedKeys returns a map's keys in ascending order. Deterministic order is
// a contract here: it decides the order of reported side effects (repointed
// clients, dangling references) that callers render and tests pin.
func sortedKeys[V any](m map[string]V) []string {
	return slices.Sorted(maps.Keys(m))
}
