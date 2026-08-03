package confops

import (
	"context"
	"errors"
	"testing"

	"github.com/dinstein/agent-hub/internal/registry"
)

// newStore opens a fresh registry in a temp directory.
func newStore(t *testing.T) *registry.Store {
	t.Helper()
	st, _ := newStoreDir(t)
	return st
}

// newStoreDir also returns the directory, for tests that write a document by
// hand — the shape an installation upgrading from an older build arrives in.
func newStoreDir(t *testing.T) (*registry.Store, string) {
	t.Helper()
	dir := t.TempDir()
	st, err := registry.Open(dir)
	if err != nil {
		t.Fatalf("open registry: %v", err)
	}
	return st, dir
}

// stdio is a minimal valid stdio entry.
func stdio(cmd string) registry.ServerEntry {
	return registry.ServerEntry{Command: cmd, Enabled: true, Source: "test"}
}

// seedServers registers ids so the reference checks have something to hit.
func seedServers(t *testing.T, st *registry.Store, ids ...string) {
	t.Helper()
	for _, id := range ids {
		if _, err := AddServer(context.Background(), st,
			ServerSpec{ID: id, Entry: stdio(id + "-bin")}, Precondition{}); err != nil {
			t.Fatalf("seed %s: %v", id, err)
		}
	}
}

// wantErrorKind asserts the typed failure class and machine code.
func wantErrorKind(t *testing.T, err error, kind Kind, code string) {
	t.Helper()
	if err == nil {
		t.Fatalf("want a %s error, got nil", kind)
	}
	e, ok := AsError(err)
	if !ok {
		t.Fatalf("error %v is not a *confops.Error", err)
	}
	if e.Kind != kind || e.Code != code {
		t.Fatalf("error = {kind:%s code:%s}, want {kind:%s code:%s} (%v)", e.Kind, e.Code, kind, code, err)
	}
}

// wantStale asserts an optimistic-concurrency refusal that reports the
// generation actually on disk.
func wantStale(t *testing.T, err error, wantGot uint64) {
	t.Helper()
	if !errors.Is(err, ErrStalePrecondition) {
		t.Fatalf("error = %v, want ErrStalePrecondition", err)
	}
	se, ok := AsStale(err)
	if !ok {
		t.Fatalf("error %v carries no *StaleError", err)
	}
	if se.Got != wantGot {
		t.Errorf("stale.Got = %d, want %d", se.Got, wantGot)
	}
}

// TestPreconditionZeroMeansUncheckedAndNonZeroIsCheckedInLock pins the two
// halves of the optimistic-concurrency contract: generation 0 never refuses,
// and a mismatch refuses WITHOUT writing (the check is inside the lock,
// before the mutation).
func TestPreconditionZeroMeansUncheckedAndNonZeroIsCheckedInLock(t *testing.T) {
	ctx := context.Background()
	st := newStore(t)

	res, err := AddServer(ctx, st, ServerSpec{ID: "a", Entry: stdio("a-bin")}, Precondition{})
	if err != nil {
		t.Fatalf("add with no precondition: %v", err)
	}
	if !res.Changed || res.Generation == 0 {
		t.Fatalf("result = %+v, want a bumped generation", res.Result)
	}
	current := res.Generation

	// Matching precondition: accepted.
	if _, err := AddServer(ctx, st, ServerSpec{ID: "b", Entry: stdio("b-bin")},
		Precondition{Generation: current}); err != nil {
		t.Fatalf("add with a matching precondition: %v", err)
	}

	// Stale precondition: refused, and nothing was written.
	_, err = AddServer(ctx, st, ServerSpec{ID: "c", Entry: stdio("c-bin")},
		Precondition{Generation: current})
	wantStale(t, err, current+1)
	if _, ok := st.Snapshot().Servers.V.Servers["c"]; ok {
		t.Error("a refused precondition still wrote the entry")
	}
	if got := st.Snapshot().Generation; got != current+1 {
		t.Errorf("generation = %d, want %d (a refusal must not bump it)", got, current+1)
	}
}

// TestResultChangedFollowsTheNoOpGuard: re-applying an identical value must
// not report a change, because the registry's no-op guard skips both the
// write and the generation bump.
func TestResultChangedFollowsTheNoOpGuard(t *testing.T) {
	ctx := context.Background()
	st := newStore(t)
	seedServers(t, st, "a")

	first, err := SetServerEnabled(ctx, st, "a", false, Precondition{})
	if err != nil {
		t.Fatal(err)
	}
	if !first.Changed {
		t.Error("disabling an enabled server must report a change")
	}
	again, err := SetServerEnabled(ctx, st, "a", false, Precondition{})
	if err != nil {
		t.Fatal(err)
	}
	if again.Changed {
		t.Errorf("re-applying the same value reported a change: %+v", again.Result)
	}
	if again.Generation != first.Generation {
		t.Errorf("generation moved on a no-op: %d -> %d", first.Generation, again.Generation)
	}
}
