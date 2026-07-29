package confops

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"testing"

	"github.com/dinstein/agent-hub/internal/integrity"
)

// cachedTool is the lookup an offline kill switch works from: the gateway's
// persisted catalog, faked here so the test does not need a live server.
func cachedTool(name, desc string) ToolSnapshotFunc {
	return func(_, tool string) (integrity.ToolSnapshot, bool, error) {
		if tool != name {
			return integrity.ToolSnapshot{}, false, nil
		}
		return integrity.ToolSnapshot{
			Name: name, Description: desc,
			InputSchema: json.RawMessage(`{"type":"object","properties":{}}`),
		}, true, nil
	}
}

// TestSetToolEnabledWorksOffline: the kill switch must not require starting
// the suspicious server first, which is exactly backwards.
func TestSetToolEnabledWorksOffline(t *testing.T) {
	ctx := context.Background()
	opt := StateOptions{Dir: t.TempDir()}
	lookup := cachedTool("list_prs", "List pull requests")

	res, err := SetToolEnabled(ctx, nil, opt, "github", "list_prs", false, lookup, Precondition{})
	if err != nil {
		t.Fatalf("disable: %v", err)
	}
	if !res.Record.Disabled || res.Record.CallAllowed() {
		t.Fatalf("record = %+v, want disabled and not callable", res.Record)
	}

	back, err := SetToolEnabled(ctx, nil, opt, "github", "list_prs", true, lookup, Precondition{})
	if err != nil {
		t.Fatalf("enable: %v", err)
	}
	if back.Record.Disabled {
		t.Errorf("record = %+v, want enabled", back.Record)
	}
	// Re-enabling must not grant trust: the record was created Pending by an
	// operator command and stays that way until an explicit approval.
	if back.Record.CallAllowed() {
		t.Errorf("re-enabling auto-approved a pending tool: %+v", back.Record)
	}
}

// TestSetToolEnabledUnknownTool: neither the store nor the cache knows it.
func TestSetToolEnabledUnknownTool(t *testing.T) {
	_, err := SetToolEnabled(context.Background(), nil, StateOptions{Dir: t.TempDir()},
		"github", "ghost", false, cachedTool("list_prs", ""), Precondition{})
	if !errors.Is(err, integrity.ErrNotFound) {
		t.Fatalf("error = %v, want integrity.ErrNotFound", err)
	}
}

func TestSetToolEnabledValidationAndPrecondition(t *testing.T) {
	ctx := context.Background()
	st := newStore(t)
	seedServers(t, st, "a", "b")
	opt := StateOptions{Dir: t.TempDir()}

	_, err := SetToolEnabled(ctx, nil, StateOptions{}, "github", "list_prs", false, nil, Precondition{})
	wantErrorKind(t, err, KindUsage, CodeUsage)

	gen := st.Snapshot().Generation
	_, err = SetToolEnabled(ctx, st, opt, "github", "list_prs", false,
		cachedTool("list_prs", ""), Precondition{Generation: gen - 1})
	wantStale(t, err, gen)
	// Nothing was written: the refusal happens before the store is opened.
	if _, serr := os.Stat(opt.Dir + "/tool-approvals.json"); serr == nil {
		t.Error("a stale precondition still touched the approval store")
	}
}

func TestSetToolOverride(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()

	name, desc := "prs", "List pull requests"
	res, err := SetToolOverride(ctx, nil, dir, "github", "list_prs",
		ToolOverrideEdit{Name: &name, Description: &desc}, Precondition{})
	if err != nil {
		t.Fatalf("override: %v", err)
	}
	if res.Override.Name != "prs" || res.Override.Description != desc {
		t.Fatalf("override = %+v", res.Override)
	}

	// Persisted keyed by the RAW tool name: an override keyed on the exposed
	// name would move out from under itself the moment it renamed a tool.
	raw, err := os.ReadFile(ToolOverridesPath(dir))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), `"list_prs"`) {
		t.Errorf("override store must key on the raw name:\n%s", raw)
	}

	// Blanking the description (the neutralization case) must not drop the
	// rename that was set separately.
	blank := ""
	blanked, err := SetToolOverride(ctx, nil, dir, "github", "list_prs",
		ToolOverrideEdit{Description: &blank}, Precondition{})
	if err != nil {
		t.Fatal(err)
	}
	if blanked.Override.Description != "" || blanked.Override.Name != "prs" {
		t.Errorf("blanking touched the name: %+v", blanked.Override)
	}

	cleared, err := SetToolOverride(ctx, nil, dir, "github", "list_prs",
		ToolOverrideEdit{Clear: true}, Precondition{})
	if err != nil || !cleared.Cleared {
		t.Fatalf("clear = %+v, %v", cleared, err)
	}
	_, err = SetToolOverride(ctx, nil, dir, "github", "list_prs",
		ToolOverrideEdit{Clear: true}, Precondition{})
	wantErrorKind(t, err, KindNotFound, CodeToolNotFound)
}

func TestSetToolOverrideValidation(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	name := "x"

	_, err := SetToolOverride(ctx, nil, dir, "github", "list_prs", ToolOverrideEdit{}, Precondition{})
	wantErrorKind(t, err, KindUsage, CodeUsage)
	_, err = SetToolOverride(ctx, nil, dir, "github", "list_prs",
		ToolOverrideEdit{Clear: true, Name: &name}, Precondition{})
	wantErrorKind(t, err, KindUsage, CodeUsage)
	_, err = SetToolOverride(ctx, nil, dir, "", "list_prs", ToolOverrideEdit{Name: &name}, Precondition{})
	wantErrorKind(t, err, KindUsage, CodeUsage)
	_, err = SetToolOverride(ctx, nil, "", "github", "list_prs", ToolOverrideEdit{Name: &name}, Precondition{})
	wantErrorKind(t, err, KindUsage, CodeUsage)
}

func TestSetToolOverridePreconditionConflict(t *testing.T) {
	ctx := context.Background()
	st := newStore(t)
	seedServers(t, st, "a", "b")
	dir := t.TempDir()
	gen := st.Snapshot().Generation
	name := "x"

	_, err := SetToolOverride(ctx, st, dir, "github", "list_prs",
		ToolOverrideEdit{Name: &name}, Precondition{Generation: gen - 1})
	wantStale(t, err, gen)
	if _, serr := os.Stat(ToolOverridesPath(dir)); serr == nil {
		t.Error("a stale precondition still wrote the override store")
	}
}

// TestLoadToolOverridesRefusesACorruptStore: reading "no overrides" out of a
// corrupt file would silently restore a poisoned description.
func TestLoadToolOverridesRefusesACorruptStore(t *testing.T) {
	dir := t.TempDir()
	if err := corrupt(t, dir, ToolOverridesFileName); err != nil {
		t.Fatal(err)
	}
	_, err := LoadToolOverrides(dir)
	wantErrorKind(t, err, KindState, CodeStateCorrupt)

	name := "x"
	_, err = SetToolOverride(context.Background(), nil, dir, "github", "list_prs",
		ToolOverrideEdit{Name: &name}, Precondition{})
	wantErrorKind(t, err, KindState, CodeStateCorrupt)
}

// TestSaveToolOverridesPrunesEmptied keeps the file a faithful listing of
// what is actually overridden.
func TestSaveToolOverridesPrunesEmptied(t *testing.T) {
	dir := t.TempDir()
	doc := ToolOverrides{Version: 1, Overrides: map[string]map[string]ToolOverride{
		"github": {"list_prs": {}},
	}}
	if err := SaveToolOverrides(dir, doc); err != nil {
		t.Fatal(err)
	}
	back, err := LoadToolOverrides(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(back.Overrides) != 0 {
		t.Errorf("overrides = %+v, want the emptied entry pruned", back.Overrides)
	}
}
