package integrity

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func newPinStore(t *testing.T) (*PinStore, string) {
	t.Helper()
	dir := t.TempDir()
	s, err := OpenPinStore(dir, Options{})
	if err != nil {
		t.Fatalf("OpenPinStore: %v", err)
	}
	return s, dir
}

func snap(name, desc, schema string) ToolSnapshot {
	s := ToolSnapshot{Name: name, Description: desc}
	if schema != "" {
		s.InputSchema = json.RawMessage(schema)
	}
	return s
}

func driftByTool(ds []Drift) map[string]Drift {
	m := make(map[string]Drift, len(ds))
	for _, d := range ds {
		m[d.Tool] = d
	}
	return m
}

// The full drift matrix in one pass: New / Unchanged / Changed(desc) /
// Changed(schema) / Removed, plus the persistence rules behind each kind.
func TestCheckServerDriftMatrix(t *testing.T) {
	ctx := context.Background()
	s, _ := newPinStore(t)

	initial := []ToolSnapshot{
		snap("alpha", "a", `{"type":"object"}`),
		snap("beta", "b", `{"type":"object","properties":{"x":{"type":"string"}}}`),
		snap("gamma", "g", ""),
		snap("delta", "d", `{"type":"object"}`),
	}
	first, err := s.CheckServer(ctx, "srv", initial)
	if err != nil {
		t.Fatalf("first CheckServer: %v", err)
	}
	if len(first) != 4 {
		t.Fatalf("first check: %d drifts, want 4", len(first))
	}
	for _, d := range first {
		if d.Kind != DriftNew {
			t.Errorf("first sight of %s: kind %s, want new", d.Tool, d.Kind)
		}
		if d.PinnedHash != "" {
			t.Errorf("new %s carries PinnedHash %q", d.Tool, d.PinnedHash)
		}
	}

	second := []ToolSnapshot{
		snap("alpha", "a", `{ "type" : "object" }`),                                         // formatting only: unchanged
		snap("beta", "b CHANGED", `{"type":"object","properties":{"x":{"type":"string"}}}`), // desc drift
		snap("gamma", "g", `{"type":"object","properties":{"evil":{}}}`),                    // schema drift (was nil)
		// delta absent: removed
		snap("epsilon", "e", ""), // brand new
	}
	got, err := s.CheckServer(ctx, "srv", second)
	if err != nil {
		t.Fatalf("second CheckServer: %v", err)
	}
	byTool := driftByTool(got)

	if d := byTool["alpha"]; d.Kind != DriftUnchanged {
		t.Errorf("alpha: %s, want unchanged", d.Kind)
	}
	if d := byTool["beta"]; d.Kind != DriftChanged || !d.DescChanged || d.SchemaChanged {
		t.Errorf("beta: kind=%s desc=%v schema=%v, want changed/desc-only", d.Kind, d.DescChanged, d.SchemaChanged)
	}
	if d := byTool["gamma"]; d.Kind != DriftChanged || d.DescChanged || !d.SchemaChanged {
		t.Errorf("gamma: kind=%s desc=%v schema=%v, want changed/schema-only", d.Kind, d.DescChanged, d.SchemaChanged)
	}
	if d := byTool["delta"]; d.Kind != DriftRemoved || d.CurrentHash != "" {
		t.Errorf("delta: kind=%s current=%q, want removed with empty current hash", d.Kind, d.CurrentHash)
	}
	if d := byTool["epsilon"]; d.Kind != DriftNew {
		t.Errorf("epsilon: %s, want new", d.Kind)
	}

	// Determinism: sorted by tool name.
	for i := 1; i < len(got); i++ {
		if got[i-1].Tool >= got[i].Tool {
			t.Errorf("drifts not sorted: %s before %s", got[i-1].Tool, got[i].Tool)
		}
	}

	pins, err := s.Pins(ctx)
	if err != nil {
		t.Fatalf("Pins: %v", err)
	}
	srv := pins["srv"]
	// Merge never deletes: delta's pin survives its removal.
	if _, ok := srv["delta"]; !ok {
		t.Error("removed tool delta lost its pin (merge must never delete)")
	}
	// Changed tools keep the OLD pin until explicit rebaseline.
	if srv["beta"].Snapshot.Description != "b" {
		t.Errorf("beta pin was re-baselined by CheckServer: desc %q", srv["beta"].Snapshot.Description)
	}
	// New tools were pinned immediately.
	if _, ok := srv["epsilon"]; !ok {
		t.Error("new tool epsilon was not pinned")
	}
}

// A removed tool reappearing with identical content is Unchanged (checked
// against its original baseline), and reappearing altered is Changed —
// exactly why merge never deletes.
func TestCheckServerReappearance(t *testing.T) {
	ctx := context.Background()
	s, _ := newPinStore(t)
	orig := snap("tool", "d", `{"a":1}`)

	if _, err := s.CheckServer(ctx, "srv", []ToolSnapshot{orig}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CheckServer(ctx, "srv", nil); err != nil { // tool removed
		t.Fatal(err)
	}

	back, err := s.CheckServer(ctx, "srv", []ToolSnapshot{orig})
	if err != nil {
		t.Fatal(err)
	}
	if d := driftByTool(back)["tool"]; d.Kind != DriftUnchanged {
		t.Errorf("identical reappearance: %s, want unchanged (not new)", d.Kind)
	}

	altered, err := s.CheckServer(ctx, "srv", []ToolSnapshot{snap("tool", "EVIL", `{"a":1}`)})
	if err != nil {
		t.Fatal(err)
	}
	if d := driftByTool(altered)["tool"]; d.Kind != DriftChanged {
		t.Errorf("altered reappearance: %s, want changed", d.Kind)
	}
}

func TestCheckServerDuplicateToolRejected(t *testing.T) {
	s, _ := newPinStore(t)
	_, err := s.CheckServer(context.Background(), "srv", []ToolSnapshot{snap("x", "a", ""), snap("x", "b", "")})
	if err == nil {
		t.Fatal("duplicate tool names in one catalog must error")
	}
}

// Rebaseline is the only path that moves a Changed pin forward; FirstSeen is
// preserved and LastChanged advances.
func TestRebaseline(t *testing.T) {
	ctx := context.Background()
	s, _ := newPinStore(t)
	if _, err := s.CheckServer(ctx, "srv", []ToolSnapshot{snap("tool", "old", "")}); err != nil {
		t.Fatal(err)
	}
	before, _ := s.Pins(ctx)

	updated := snap("tool", "new", "")
	pin, err := s.Rebaseline(ctx, "srv", "tool", updated)
	if err != nil {
		t.Fatalf("Rebaseline: %v", err)
	}
	if pin.Snapshot.Description != "new" {
		t.Errorf("rebaselined snapshot desc = %q", pin.Snapshot.Description)
	}
	if !pin.FirstSeen.Equal(before["srv"]["tool"].FirstSeen) {
		t.Error("Rebaseline must preserve FirstSeen")
	}
	if !pin.LastChanged.After(pin.FirstSeen) && pin.LastChanged.Equal(before["srv"]["tool"].LastChanged) {
		t.Error("Rebaseline must advance LastChanged")
	}

	// Post-rebaseline, the updated content is the baseline.
	ds, err := s.CheckServer(ctx, "srv", []ToolSnapshot{updated})
	if err != nil {
		t.Fatal(err)
	}
	if d := driftByTool(ds)["tool"]; d.Kind != DriftUnchanged {
		t.Errorf("after rebaseline: %s, want unchanged", d.Kind)
	}

	if _, err := s.Rebaseline(ctx, "srv", "tool", snap("other", "x", "")); err == nil {
		t.Error("Rebaseline with mismatched snapshot name must error")
	}
}

// A pin recorded under an older hash formula whose content is unchanged is
// migrated in place and reported Unchanged — a formula bump must never look
// like a fleet-wide rug-pull.
func TestCheckServerFormulaMigration(t *testing.T) {
	ctx := context.Background()
	s, dir := newPinStore(t)
	content := snap("tool", "d", `{"a":1}`)
	if _, err := s.CheckServer(ctx, "srv", []ToolSnapshot{content}); err != nil {
		t.Fatal(err)
	}

	// Rewrite the pin as if written by an older binary: legacy version tag
	// and a hash the current formula can't reproduce, same snapshot content.
	path := filepath.Join(dir, pinsFileName)
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var file pinsFile
	if err := json.Unmarshal(raw, &file); err != nil {
		t.Fatal(err)
	}
	pin := file.Pins["srv"]["tool"]
	pin.HashSchemaVersion = "v0"
	pin.Hash = "v0:legacyhash"
	file.Pins["srv"]["tool"] = pin
	b, _ := json.Marshal(file)
	if err := os.WriteFile(path, b, 0o600); err != nil {
		t.Fatal(err)
	}

	ds, err := s.CheckServer(ctx, "srv", []ToolSnapshot{content})
	if err != nil {
		t.Fatal(err)
	}
	if d := driftByTool(ds)["tool"]; d.Kind != DriftUnchanged {
		t.Fatalf("formula migration with identical content: %s, want unchanged", d.Kind)
	}
	pins, _ := s.Pins(ctx)
	migrated := pins["srv"]["tool"]
	if migrated.HashSchemaVersion != HashSchemaVersion {
		t.Errorf("pin not migrated: version %q", migrated.HashSchemaVersion)
	}

	// Content that ALSO changed across the formula bump is still drift.
	ds, err = s.CheckServer(ctx, "srv", []ToolSnapshot{snap("tool", "EVIL", `{"a":1}`)})
	if err != nil {
		t.Fatal(err)
	}
	if d := driftByTool(ds)["tool"]; d.Kind != DriftChanged {
		t.Errorf("changed content across formula bump: %s, want changed", d.Kind)
	}
}
