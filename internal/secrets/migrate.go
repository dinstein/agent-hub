package secrets

import (
	"context"
	"fmt"
)

// MigrateResult records the outcome for one ref.
type MigrateResult struct {
	Ref Ref
	// Migrated is true when the value was copied to the new store and the
	// old entry was deleted. A ref absent from the old store reports
	// Migrated=false with Err=nil.
	Migrated bool
	Err      error
}

// Migrate moves refs between stores with read-old → write-new →
// read-back-verify → delete-old semantics. The old entry
// is deleted only after the new store returns the identical value
// (fail-closed: on any verification failure both copies are kept and the
// ref is reported failed — a duplicated secret is recoverable, a dropped
// one is not).
//
// Pass backend-level stores: a *Chain's Get consults environment levels
// first, which would let an env var satisfy the read-back verification
// without proving the new backend actually holds the value.
func Migrate(ctx context.Context, from, to Store, refs []Ref) []MigrateResult {
	out := make([]MigrateResult, 0, len(refs))
	for _, ref := range refs {
		out = append(out, migrateOne(ctx, from, to, ref))
	}
	return out
}

func migrateOne(ctx context.Context, from, to Store, ref Ref) MigrateResult {
	r := MigrateResult{Ref: ref}
	val, ok, err := from.Get(ctx, ref)
	if err != nil {
		r.Err = fmt.Errorf("secrets: migrate %s: read old: %w", ref.StorageKey(), err)
		return r
	}
	if !ok {
		return r // absent: nothing to migrate, not an error
	}
	if err := to.Set(ctx, ref, val); err != nil {
		r.Err = fmt.Errorf("secrets: migrate %s: write new: %w", ref.StorageKey(), err)
		return r
	}
	got, ok, err := to.Get(ctx, ref)
	if err != nil || !ok || got != val {
		// Old entry deliberately kept (fail-closed).
		r.Err = fmt.Errorf("secrets: migrate %s: read-back verification failed (err=%v ok=%v)",
			ref.StorageKey(), err, ok)
		return r
	}
	if err := from.Delete(ctx, ref); err != nil {
		// Value now lives in both stores — safe but dirty; surface it.
		r.Err = fmt.Errorf("secrets: migrate %s: delete old (value duplicated in old and new): %w",
			ref.StorageKey(), err)
		return r
	}
	r.Migrated = true
	return r
}
