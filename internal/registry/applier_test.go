package registry

import (
	"errors"
	"sync"
	"testing"
)

func TestApplierAdoptsGreaterOrEqual(t *testing.T) {
	var a Applier

	// Nothing applied yet: anything (including generation 0) is adopted.
	if !a.ShouldApply(0) {
		t.Fatal("fresh applier must adopt generation 0")
	}
	a.MarkApplied(3)

	if a.ShouldApply(2) {
		t.Error("gen 2 < applied 3: must not adopt")
	}
	if !a.ShouldApply(3) {
		t.Error("gen 3 == applied 3: must adopt (>= criterion, idempotent)")
	}
	if !a.ShouldApply(4) {
		t.Error("gen 4 > applied 3: must adopt")
	}
}

// TestApplierRapidWritesDoNotStrand replays the failure mode the >= criterion
// exists for (canonical.md §5c #2): the consumer handles an event for Rev 1,
// but by the time it re-reads, writes 2 and 3 have landed and their events
// coalesced away. An ==-Rev criterion would reject the read and wait forever;
// >= must adopt it immediately.
func TestApplierRapidWritesDoNotStrand(t *testing.T) {
	var a Applier
	a.MarkApplied(0)

	// Event{Rev:1} arrives; the re-read observes generation 3.
	readGen := uint64(3)
	if readGen != 1 && !a.ShouldApply(readGen) {
		t.Fatal("re-read newer than event Rev must be adopted")
	}
	ran, err := a.Apply(readGen, func() error { return nil })
	if err != nil || !ran {
		t.Fatalf("Apply(3) = (%v, %v), want (true, nil)", ran, err)
	}

	// Late event{Rev:2} arrives; the re-read observes generation 3 again:
	// idempotent re-adoption, never a rejection that strands us on gen 2.
	ran, err = a.Apply(3, func() error { return nil })
	if err != nil || !ran {
		t.Fatalf("re-Apply(3) = (%v, %v), want (true, nil)", ran, err)
	}
	// A genuinely stale read (torn interleaving) is rejected.
	ran, err = a.Apply(2, func() error { t.Fatal("apply ran for stale gen"); return nil })
	if err != nil || ran {
		t.Fatalf("Apply(2) = (%v, %v), want (false, nil)", ran, err)
	}
	if got, _ := a.Applied(); got != 3 {
		t.Fatalf("applied = %d, want 3", got)
	}
}

func TestApplierFailedApplyRecordsNothing(t *testing.T) {
	var a Applier
	boom := errors.New("boom")
	ran, err := a.Apply(5, func() error { return boom })
	if ran || !errors.Is(err, boom) {
		t.Fatalf("Apply = (%v, %v), want (false, boom)", ran, err)
	}
	if _, ok := a.Applied(); ok {
		t.Fatal("failed apply must not mark anything applied")
	}
	if !a.ShouldApply(5) {
		t.Fatal("same generation must be re-attemptable after a failed apply")
	}
}

func TestApplierMarkAppliedNeverRegresses(t *testing.T) {
	var a Applier
	a.MarkApplied(7)
	a.MarkApplied(4) // late out-of-order apply
	if got, _ := a.Applied(); got != 7 {
		t.Fatalf("applied = %d, want 7 (no regression)", got)
	}
}

func TestApplierConcurrentAppliesEndOnNewest(t *testing.T) {
	var a Applier
	var wg sync.WaitGroup
	for gen := uint64(1); gen <= 50; gen++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _ = a.Apply(gen, func() error { return nil })
		}()
	}
	wg.Wait()
	got, ok := a.Applied()
	if !ok || got != 50 {
		t.Fatalf("applied = (%d, %v), want (50, true)", got, ok)
	}
}
