package registry_test

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/dinstein/agent-hub/internal/registry"
)

// IntentVariants is the one governance switch whose default is ON (ruling
// #18), so its tri-state must survive both directions of a round trip:
// absent means the default, an explicit false is an opt-out and must NOT be
// read back as absent.
func TestIntentVariantsTriState(t *testing.T) {
	t.Parallel()
	no, yes := false, true
	cases := []struct {
		name string
		doc  registry.GovernanceDoc
		want bool
	}{
		{"absent means the default (on)", registry.GovernanceDoc{}, true},
		{"explicit false opts out", registry.GovernanceDoc{IntentVariants: &no}, false},
		{"explicit true", registry.GovernanceDoc{IntentVariants: &yes}, true},
	}
	for _, tc := range cases {
		if got := tc.doc.IntentVariantsEnabled(); got != tc.want {
			t.Errorf("%s: IntentVariantsEnabled = %v, want %v", tc.name, got, tc.want)
		}
	}
}

func TestIntentVariantsSurvivesTheStore(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	store, err := registry.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	off := false
	if err := store.Update(context.Background(), func(tx *registry.Tx) error {
		tx.Governance.V.IntentVariants = &off
		return nil
	}); err != nil {
		t.Fatalf("Update: %v", err)
	}

	reopened, err := registry.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	got := reopened.Snapshot().Governance.V
	if got.IntentVariants == nil {
		t.Fatal("an explicit false was read back as absent — the opt-out would silently revert")
	}
	if got.IntentVariantsEnabled() {
		t.Fatal("the opt-out did not survive the round trip")
	}

	// An absent switch must marshal as absent (no "intentVariants": null
	// noise in a hand-edited file).
	raw, err := json.Marshal(registry.GovernanceDoc{})
	if err != nil {
		t.Fatal(err)
	}
	if string(raw) != "{}" {
		t.Errorf("empty governance doc marshals as %s, want {}", raw)
	}
}
