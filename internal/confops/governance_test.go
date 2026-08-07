package confops

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dinstein/agent-hub/internal/registry"
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
	for _, want := range []string{"discovery", "calls.enabled", "calls.durability", "calls.results", "calls.resultBytes", "calls.retentionDays", "calls.maxBytes", "calls.minFreeBytes"} {
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

func TestCallsGovernanceDefaultsValidationAndEnable(t *testing.T) {
	ctx := context.Background()
	st := newStore(t)
	p := st.Snapshot().Governance.V.ResolvedCalls()
	if p.Enabled || p.Durability != "sync" || p.ResultMode != "truncated" || p.RetentionDays <= 0 || p.MaxBytes <= 0 {
		t.Fatalf("defaults = %+v", p)
	}
	if _, err := SetGovernance(ctx, st, "calls.enabled", "true", Precondition{}); err == nil {
		t.Fatal("enabled without a key id")
	}

	res, err := SetCallsEnabled(ctx, st, true, "key-1", Precondition{})
	if err != nil || !res.Changed || !res.Policy.Enabled || res.Policy.KeyID != "key-1" {
		t.Fatalf("enable = %+v, %v", res, err)
	}
	if doc := st.Snapshot().Governance.V.CallsDoc(); doc == nil || doc.V.RetentionDays == 0 {
		t.Fatal("enable did not materialize bounded defaults")
	}
	if _, err := SetGovernance(ctx, st, "calls.enabled", "false", Precondition{}); err != nil {
		t.Fatal(err)
	}
	if _, err := SetGovernance(ctx, st, "calls.results", "full", Precondition{}); err != nil {
		t.Fatal(err)
	}
	if got := st.Snapshot().Governance.V.ResolvedCalls(); got.Enabled || got.ResultMode != "full" || got.KeyID != "key-1" {
		t.Fatalf("updated policy = %+v", got)
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

	var e *Error
	_ = errors.As(UnknownGovernanceKey("colour"), &e)
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

// The events switch is tri-state, and the third state has to survive a round
// trip: "nobody set this" and "someone chose the default value" are different
// facts, and only the first may render as unset.
func TestEventsEnabledIsTriState(t *testing.T) {
	var g registry.GovernanceDoc
	if !g.EventsEnabled() {
		t.Fatal("absent must resolve to the default, which is on")
	}
	key, ok := LookupGovernanceKey("events.enabled")
	if !ok {
		t.Fatal("events.enabled is not a governance key")
	}
	if err := key.set(&g, "false"); err != nil {
		t.Fatal(err)
	}
	if g.Events == nil || g.EventsEnabled() {
		t.Fatalf("set false = %v", g.Events)
	}
	if got := key.Get(g); got != "false" {
		t.Errorf("Get = %q after set false", got)
	}
	if err := key.set(&g, "-"); err != nil {
		t.Fatal(err)
	}
	if g.Events != nil {
		t.Fatal(`"-" must clear the field, not write the default's value`)
	}
	if err := key.set(&g, "maybe"); err == nil {
		t.Fatal("a non-boolean was accepted")
	}
}

// A governance.json written before the rename must keep its policy AND its
// key id: without the key id the days already on disk cannot be decrypted at
// all, so a silent reset here is not a lost setting but lost evidence.
func TestPreRenameGovernanceKeyIsReadAndFoldedForward(t *testing.T) {
	ctx := context.Background()
	st, dir := newStoreDir(t)
	legacy := `{"audit":{"enabled":true,"keyId":"key-old","results":"full"}}` + "\n"
	if err := os.WriteFile(filepath.Join(dir, "governance.json"), []byte(legacy), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := st.Reload(ctx); err != nil {
		t.Fatal(err)
	}

	got := st.Snapshot().Governance.V.ResolvedCalls()
	if !got.Enabled || got.KeyID != "key-old" || got.ResultMode != "full" {
		t.Fatalf("pre-rename policy was not read: %+v", got)
	}

	// The first write folds it into the current key and drops the old one,
	// so no reader can later enforce whichever of the two it happened to see.
	if _, err := SetGovernance(ctx, st, "calls.results", "errors", Precondition{}); err != nil {
		t.Fatal(err)
	}
	g := st.Snapshot().Governance.V
	if g.Audit != nil {
		t.Errorf("the pre-rename key survived a write: %+v", g.Audit)
	}
	if g.Calls == nil || g.Calls.V.KeyID != "key-old" || g.Calls.V.ResultMode != "errors" {
		t.Fatalf("folded policy = %+v", g.Calls)
	}
}
