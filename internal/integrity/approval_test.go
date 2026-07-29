package integrity

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

// The transition table, exhaustively: every (from, to, reason) triple is
// checked against the expected verdict. The two load-bearing rows are
// Changed->Approved (user actions ONLY — the rug-pull security property) and
// the absence of any Approved->Pending edge.
func TestAssertTransitionTable(t *testing.T) {
	states := []ToolState{stateNone, StatePending, StateApproved, StateChanged}
	reasons := []TransitionReason{
		ReasonFirstSeen, ReasonAutoApprove, ReasonUserApprove, ReasonUserBlock,
		ReasonBaselineTrust, ReasonDriftDetected, ReasonFormulaMigration,
	}
	allowed := map[[3]string]bool{
		{"", "pending", "first_seen"}:                 true,
		{"", "approved", "auto_approve"}:              true,
		{"pending", "approved", "user_approve"}:       true,
		{"pending", "approved", "baseline_trust"}:     true,
		{"pending", "approved", "user_block"}:         true,
		{"pending", "pending", "drift_detected"}:      true,
		{"approved", "changed", "drift_detected"}:     true,
		{"approved", "approved", "formula_migration"}: true,
		{"changed", "changed", "drift_detected"}:      true,
		{"changed", "approved", "user_approve"}:       true,
		{"changed", "approved", "user_block"}:         true,
	}
	for _, from := range states {
		for _, to := range states {
			for _, reason := range reasons {
				err := assertTransition(from, to, reason)
				want := allowed[[3]string{string(from), string(to), string(reason)}]
				if want && err != nil {
					t.Errorf("(%q -> %q, %s): unexpectedly forbidden: %v", from, to, reason, err)
				}
				if !want && err == nil {
					t.Errorf("(%q -> %q, %s): unexpectedly allowed", from, to, reason)
				}
				if !want {
					var te *TransitionError
					if err != nil && !errors.As(err, &te) {
						t.Errorf("(%q -> %q, %s): error is not *TransitionError: %v", from, to, reason, err)
					}
				}
			}
		}
	}
}

// The hard-coded security property must hold even against a hypothetical
// table edit: Changed -> Approved with any non-user reason is forbidden.
func TestChangedToApprovedRequiresUserAction(t *testing.T) {
	for _, reason := range []TransitionReason{
		ReasonBaselineTrust, ReasonAutoApprove, ReasonDriftDetected,
		ReasonFormulaMigration, ReasonFirstSeen, TransitionReason("made_up"),
	} {
		if err := assertTransition(StateChanged, StateApproved, reason); err == nil {
			t.Errorf("Changed->Approved allowed with non-user reason %s", reason)
		}
	}
}

func newApprovalStore(t *testing.T) *ApprovalStore {
	t.Helper()
	s, err := OpenApprovalStore(t.TempDir(), Options{})
	if err != nil {
		t.Fatalf("OpenApprovalStore: %v", err)
	}
	return s
}

func TestObserveFirstSightModes(t *testing.T) {
	ctx := context.Background()
	s := newApprovalStore(t)

	manual, err := s.Observe(ctx, "srv", ToolSnapshot{Name: "m"}, ModeManual)
	if err != nil {
		t.Fatal(err)
	}
	if manual.Status != StatePending || manual.Reason != ReasonFirstSeen || manual.ApprovedHash != "" {
		t.Errorf("manual first sight: %+v", manual)
	}
	if manual.CallAllowed() {
		t.Error("Pending tool must not be callable (fail-closed)")
	}

	auto, err := s.Observe(ctx, "srv", ToolSnapshot{Name: "a"}, ModeAuto)
	if err != nil {
		t.Fatal(err)
	}
	if auto.Status != StateApproved || auto.Reason != ReasonAutoApprove || auto.ApprovedHash != auto.CurrentHash {
		t.Errorf("auto first sight: %+v", auto)
	}
	if !auto.CallAllowed() {
		t.Error("auto-approved tool should be callable")
	}

	// Unknown mode fails closed to Pending.
	weird, err := s.Observe(ctx, "srv", ToolSnapshot{Name: "w"}, ApprovalMode("garbage"))
	if err != nil {
		t.Fatal(err)
	}
	if weird.Status != StatePending {
		t.Errorf("unknown mode landed in %s, want pending (fail-closed)", weird.Status)
	}
}

// The rug-pull path: Approved -> Changed on drift, automatically, with the
// diff snapshots preserved; auto mode does not bypass it; only Approve/Block
// clears it; baseline trust does not.
func TestObserveDriftLifecycle(t *testing.T) {
	ctx := context.Background()
	s := newApprovalStore(t)

	v1 := ToolSnapshot{Name: "t", Description: "honest", InputSchema: json.RawMessage(`{"a":1}`)}
	v2 := ToolSnapshot{Name: "t", Description: "evil", InputSchema: json.RawMessage(`{"a":1}`)}
	v3 := ToolSnapshot{Name: "t", Description: "more evil", InputSchema: json.RawMessage(`{"a":2}`)}

	if _, err := s.Observe(ctx, "srv", v1, ModeAuto); err != nil {
		t.Fatal(err)
	}

	changed, err := s.Observe(ctx, "srv", v2, ModeAuto) // auto mode must NOT auto-clear drift
	if err != nil {
		t.Fatal(err)
	}
	if changed.Status != StateChanged || changed.Reason != ReasonDriftDetected {
		t.Fatalf("post-drift: %+v", changed)
	}
	if changed.CallAllowed() {
		t.Error("Changed tool must not be callable (fail-closed)")
	}
	if changed.Previous == nil || changed.Previous.Description != "honest" {
		t.Errorf("Previous snapshot for diff review missing/wrong: %+v", changed.Previous)
	}
	if changed.Current.Description != "evil" {
		t.Errorf("Current snapshot not refreshed: %+v", changed.Current)
	}
	if changed.ApprovedHash == changed.CurrentHash {
		t.Error("ApprovedHash moved with drift")
	}

	// Further drift while Changed: state kept, Current refreshed, Previous
	// (the approved-time snapshot) preserved.
	changed2, err := s.Observe(ctx, "srv", v3, ModeAuto)
	if err != nil {
		t.Fatal(err)
	}
	if changed2.Status != StateChanged || changed2.Previous.Description != "honest" || changed2.Current.Description != "more evil" {
		t.Errorf("second drift: %+v", changed2)
	}

	// Baseline trust must NOT clear the Changed mark.
	promoted, err := s.BaselineTrust(ctx, "srv")
	if err != nil {
		t.Fatal(err)
	}
	if len(promoted) != 0 {
		t.Errorf("baseline trust promoted %d records, want 0 (only Pending qualifies)", len(promoted))
	}
	rec, err := s.Get(ctx, "srv", "t")
	if err != nil {
		t.Fatal(err)
	}
	if rec.Status != StateChanged {
		t.Errorf("baseline trust cleared a rug-pull mark: %s", rec.Status)
	}

	// User approval is the way out; the approved hash becomes the current one.
	approved, err := s.Approve(ctx, "srv", "t")
	if err != nil {
		t.Fatalf("Approve: %v", err)
	}
	if approved.Status != StateApproved || approved.ApprovedHash != approved.CurrentHash || approved.Previous != nil {
		t.Errorf("post-approve: %+v", approved)
	}
	if !approved.CallAllowed() {
		t.Error("re-approved tool should be callable")
	}

	// Re-observing the approved content is a no-op.
	same, err := s.Observe(ctx, "srv", v3, ModeAuto)
	if err != nil {
		t.Fatal(err)
	}
	if same.Status != StateApproved {
		t.Errorf("stable re-observation flipped state: %s", same.Status)
	}

	// A revert to previously approved content is still drift (an attacker
	// hiding after being noticed must stay flagged), because ApprovedHash is
	// now v3.
	reverted, err := s.Observe(ctx, "srv", v1, ModeAuto)
	if err != nil {
		t.Fatal(err)
	}
	if reverted.Status != StateChanged {
		t.Errorf("revert-after-approve: %s, want changed", reverted.Status)
	}
}

func TestBaselineTrustPromotesPendingOnly(t *testing.T) {
	ctx := context.Background()
	s := newApprovalStore(t)
	if _, err := s.Observe(ctx, "srv", ToolSnapshot{Name: "p1"}, ModeManual); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Observe(ctx, "srv", ToolSnapshot{Name: "p2"}, ModeManual); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Observe(ctx, "other", ToolSnapshot{Name: "px"}, ModeManual); err != nil {
		t.Fatal(err)
	}

	promoted, err := s.BaselineTrust(ctx, "srv")
	if err != nil {
		t.Fatal(err)
	}
	if len(promoted) != 2 || promoted[0].Tool != "p1" || promoted[1].Tool != "p2" {
		t.Fatalf("promoted = %+v", promoted)
	}
	for _, rec := range promoted {
		if rec.Status != StateApproved || rec.Reason != ReasonBaselineTrust || rec.ApprovedHash != rec.CurrentHash {
			t.Errorf("promoted record: %+v", rec)
		}
	}
	// Other servers untouched.
	other, err := s.Get(ctx, "other", "px")
	if err != nil {
		t.Fatal(err)
	}
	if other.Status != StatePending {
		t.Errorf("baseline trust leaked to another server: %s", other.Status)
	}
}

// Block writes Approved+Disabled in one record atomically: after Block there
// is no observable point at which the tool is approved AND enabled.
func TestBlockIsAtomic(t *testing.T) {
	ctx := context.Background()
	s := newApprovalStore(t)
	v1 := ToolSnapshot{Name: "t", Description: "a"}
	v2 := ToolSnapshot{Name: "t", Description: "b"}
	if _, err := s.Observe(ctx, "srv", v1, ModeAuto); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Observe(ctx, "srv", v2, ModeAuto); err != nil { // -> Changed
		t.Fatal(err)
	}

	blocked, err := s.Block(ctx, "srv", "t")
	if err != nil {
		t.Fatalf("Block: %v", err)
	}
	if blocked.Status != StateApproved || !blocked.Disabled || blocked.Reason != ReasonUserBlock {
		t.Errorf("post-block: %+v", blocked)
	}
	if blocked.ApprovedHash != blocked.CurrentHash {
		t.Error("Block must approve the current content it is blocking")
	}
	if blocked.CallAllowed() {
		t.Error("blocked tool must not be callable")
	}

	// Persisted shape matches (single write, no intermediate state on disk).
	rec, err := s.Get(ctx, "srv", "t")
	if err != nil {
		t.Fatal(err)
	}
	if rec.Status != StateApproved || !rec.Disabled {
		t.Errorf("persisted post-block: %+v", rec)
	}

	// Re-enable preserves the approval (Approved <-> Disabled is a flag).
	enabled, err := s.SetDisabled(ctx, "srv", "t", false)
	if err != nil {
		t.Fatal(err)
	}
	if enabled.Status != StateApproved || enabled.Disabled || !enabled.CallAllowed() {
		t.Errorf("post-enable: %+v", enabled)
	}
}

// DisabledTools is the projection the data plane aggregates against. Two
// properties matter: it reports the Disabled FLAG only (an unreviewed but
// enabled tool must not appear, or every Pending tool would vanish from the
// catalog), and a corrupt store is an ERROR, never an empty deny set.
func TestDisabledToolsProjection(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	s, err := OpenApprovalStore(dir, Options{})
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"off", "on", "pending"} {
		if _, err := s.Observe(ctx, "srv", ToolSnapshot{Name: name}, ModeAuto); err != nil {
			t.Fatal(err)
		}
	}
	// "pending" is approved above; make it genuinely unreviewed on another
	// server so the Status-vs-flag distinction is exercised.
	if _, err := s.Observe(ctx, "other", ToolSnapshot{Name: "unreviewed"}, ModeManual); err != nil {
		t.Fatal(err)
	}
	if _, err := s.SetDisabled(ctx, "srv", "off", true); err != nil {
		t.Fatal(err)
	}

	got, err := s.DisabledTools(ctx)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]map[string]bool{"srv": {"off": true}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("DisabledTools = %v, want %v (flag only, never Status)", got, want)
	}

	// Fail direction: a corrupt store must not read as "nothing disabled".
	if err := os.WriteFile(filepath.Join(dir, "tool-approvals.json"), []byte("{ nope"), 0o600); err != nil {
		t.Fatal(err)
	}
	corrupt, err := s.DisabledTools(ctx)
	if !errors.Is(err, ErrStoreCorrupt) {
		t.Fatalf("DisabledTools on a corrupt store = %v, %v; want ErrStoreCorrupt", corrupt, err)
	}
	if corrupt != nil {
		t.Fatalf("DisabledTools returned %v alongside an error; the deny set must be nil", corrupt)
	}
}

func TestApprovalNotFoundVsCorrupt(t *testing.T) {
	ctx := context.Background()
	s := newApprovalStore(t)
	if _, err := s.Approve(ctx, "srv", "ghost"); !errors.Is(err, ErrNotFound) {
		t.Errorf("Approve(ghost): %v, want ErrNotFound", err)
	}
	if _, err := s.Get(ctx, "srv", "ghost"); !errors.Is(err, ErrNotFound) {
		t.Errorf("Get(ghost): %v, want ErrNotFound", err)
	}
	if _, err := s.SetDisabled(ctx, "srv", "ghost", true); !errors.Is(err, ErrNotFound) {
		t.Errorf("SetDisabled(ghost): %v, want ErrNotFound", err)
	}
}

// Records stored under an older hash formula with unchanged content migrate
// in place without flipping to Changed (never a fake rug-pull), while real
// drift across the bump is still caught.
func TestObserveFormulaMigration(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	s, err := OpenApprovalStore(dir, Options{})
	if err != nil {
		t.Fatal(err)
	}
	content := ToolSnapshot{Name: "t", Description: "d", InputSchema: json.RawMessage(`{"a":1}`)}
	if _, err := s.Observe(ctx, "srv", content, ModeAuto); err != nil {
		t.Fatal(err)
	}

	// Rewrite the record as if produced by an older formula.
	rewriteApprovalRecord(t, dir, "srv", "t", func(rec *ApprovalRecord) {
		rec.HashSchemaVersion = "v0"
		rec.ApprovedHash = "v0:legacy"
		rec.CurrentHash = "v0:legacy"
	})

	migrated, err := s.Observe(ctx, "srv", content, ModeAuto)
	if err != nil {
		t.Fatal(err)
	}
	if migrated.Status != StateApproved || migrated.Reason != ReasonFormulaMigration {
		t.Fatalf("migration flagged as drift: %+v", migrated)
	}
	if migrated.HashSchemaVersion != HashSchemaVersion || migrated.ApprovedHash != migrated.CurrentHash {
		t.Errorf("hashes not migrated: %+v", migrated)
	}

	// Same setup but the content ALSO changed: still a rug-pull.
	rewriteApprovalRecord(t, dir, "srv", "t", func(rec *ApprovalRecord) {
		rec.HashSchemaVersion = "v0"
		rec.ApprovedHash = "v0:legacy"
		rec.CurrentHash = "v0:legacy"
	})
	drifted, err := s.Observe(ctx, "srv", ToolSnapshot{Name: "t", Description: "EVIL"}, ModeAuto)
	if err != nil {
		t.Fatal(err)
	}
	if drifted.Status != StateChanged {
		t.Errorf("real drift across formula bump missed: %+v", drifted)
	}
}

func rewriteApprovalRecord(t *testing.T, dir, server, tool string, mutate func(*ApprovalRecord)) {
	t.Helper()
	path := filepath.Join(dir, approvalsFileName)
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var file approvalsFile
	if err := json.Unmarshal(raw, &file); err != nil {
		t.Fatal(err)
	}
	rec := file.Servers[server][tool]
	mutate(&rec)
	file.Servers[server][tool] = rec
	b, err := json.Marshal(file)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, b, 0o600); err != nil {
		t.Fatal(err)
	}
}
