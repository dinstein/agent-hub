package confops

import (
	"context"
	"strings"
	"testing"
)

func TestGovernanceKeyTableIsShared(t *testing.T) {
	keys := GovernanceKeys()
	if len(keys) == 0 {
		t.Fatal("the governance key table is empty")
	}
	// Mutating the returned slice must not touch the source of truth.
	keys[0].Name = "tampered"
	if GovernanceKeys()[0].Name == "tampered" {
		t.Error("GovernanceKeys returned the live table")
	}
	for _, want := range []string{"discovery"} {
		if _, ok := LookupGovernanceKey(want); !ok {
			t.Errorf("key %q is missing", want)
		}
	}
	// snake_case aliases resolve to the canonical key.
	k, ok := LookupGovernanceKey("discovery_mode")
	if !ok || k.Name != "discovery" {
		t.Errorf("alias resolved to %+v", k)
	}
}

func TestSetGovernance(t *testing.T) {
	ctx := context.Background()
	st := newStore(t)

	// An unset key reads as its zero value, not as an error.
	entry, err := GetGovernance(st.Snapshot().Governance.V, "discovery")
	if err != nil || entry.Value != "" {
		t.Fatalf("unset key = %+v, %v", entry, err)
	}

	res, err := SetGovernance(ctx, st, "discovery_mode", "lazy", Precondition{})
	if err != nil {
		t.Fatalf("set: %v", err)
	}
	if res.Key != "discovery" || res.Value != "lazy" || !res.Changed {
		t.Fatalf("result = %+v", res)
	}

	again, err := SetGovernance(ctx, st, "discovery", "lazy", Precondition{})
	if err != nil {
		t.Fatal(err)
	}
	if again.Changed {
		t.Errorf("re-setting the same value reported a change: %+v", again)
	}
}

func TestSetGovernanceValidation(t *testing.T) {
	ctx := context.Background()
	st := newStore(t)

	// Failure direction: a typo must never read as "off".
	_, err := SetGovernance(ctx, st, "discovery", "maybe", Precondition{})
	wantErrorKind(t, err, KindUsage, CodeUsage)
	if st.Snapshot().Governance.V.Discovery == "maybe" {
		t.Error("a rejected value was applied")
	}

	_, err = SetGovernance(ctx, st, "discovery", "bogus", Precondition{})
	wantErrorKind(t, err, KindUsage, CodeUsage)

	_, err = SetGovernance(ctx, st, "colour", "blue", Precondition{})
	wantErrorKind(t, err, KindUsage, CodeConfigKeyUnknown)

	_, err = GetGovernance(st.Snapshot().Governance.V, "colour")
	wantErrorKind(t, err, KindUsage, CodeConfigKeyUnknown)

	e, _ := AsError(UnknownGovernanceKey("colour"))
	if !strings.Contains(e.Hint, "discovery") || !strings.Contains(e.Hint, ResultBudgetPrefix) {
		t.Errorf("hint = %q, want the full key list", e.Hint)
	}
}

func TestSetGovernanceResultBudget(t *testing.T) {
	ctx := context.Background()
	st := newStore(t)

	res, err := SetGovernance(ctx, st, ResultBudgetPrefix+"*", "65536", Precondition{})
	if err != nil || res.Value != "65536" {
		t.Fatalf("budget = %+v, %v", res, err)
	}
	// The forced marker must be visible: it merges by MIN instead of
	// most-specific-wins, which is a different rule.
	forced, err := SetGovernance(ctx, st, ResultBudgetPrefix+"github", "1024!", Precondition{})
	if err != nil || !strings.Contains(forced.Value, "forced") {
		t.Fatalf("forced budget = %+v, %v", forced, err)
	}

	entries := ListGovernance(st.Snapshot().Governance.V)
	budgets := 0
	for _, e := range entries {
		if strings.HasPrefix(e.Key, ResultBudgetPrefix) {
			budgets++
		}
	}
	if budgets != 2 {
		t.Errorf("listed %d budget keys, want 2: %+v", budgets, entries)
	}

	if _, err := SetGovernance(ctx, st, ResultBudgetPrefix+"github", "-", Precondition{}); err != nil {
		t.Fatal(err)
	}
	cleared, err := GetGovernance(st.Snapshot().Governance.V, ResultBudgetPrefix+"github")
	if err != nil || cleared.Value != "" {
		t.Errorf("cleared budget = %+v, %v", cleared, err)
	}

	_, err = SetGovernance(ctx, st, ResultBudgetPrefix+"github", "-5", Precondition{})
	wantErrorKind(t, err, KindUsage, CodeUsage)
	_, err = SetGovernance(ctx, st, ResultBudgetPrefix, "1024", Precondition{})
	wantErrorKind(t, err, KindUsage, CodeUsage)
}

func TestSetGovernancePreconditionConflict(t *testing.T) {
	ctx := context.Background()
	st := newStore(t)
	if _, err := SetGovernance(ctx, st, "discovery", "full", Precondition{}); err != nil {
		t.Fatal(err)
	}
	if _, err := SetGovernance(ctx, st, "discovery", "lazy", Precondition{}); err != nil {
		t.Fatal(err)
	}
	gen := st.Snapshot().Generation

	_, err := SetGovernance(ctx, st, "discovery", "full", Precondition{Generation: gen - 1})
	wantStale(t, err, gen)
	if st.Snapshot().Governance.V.Discovery == "full" {
		t.Error("a stale set was applied anyway")
	}
}
