package integrity

import (
	"context"
	"testing"
)

// TestPinStoreForgetServer covers the cleanup half of `agenthub server rm`
// and, in the second subtest, the reason it exists at all.
func TestPinStoreForgetServer(t *testing.T) {
	ctx := context.Background()

	t.Run("drops only the named server", func(t *testing.T) {
		s, _ := newPinStore(t)
		if _, err := s.CheckServer(ctx, "gone", []ToolSnapshot{snap("t", "d", "")}); err != nil {
			t.Fatalf("seed gone: %v", err)
		}
		if _, err := s.CheckServer(ctx, "stays", []ToolSnapshot{snap("t", "d", "")}); err != nil {
			t.Fatalf("seed stays: %v", err)
		}
		if err := s.ForgetServer(ctx, "gone"); err != nil {
			t.Fatalf("forget: %v", err)
		}
		pins, err := s.Pins(ctx)
		if err != nil {
			t.Fatalf("pins: %v", err)
		}
		if _, ok := pins["gone"]; ok {
			t.Error("the removed server kept its pins")
		}
		if _, ok := pins["stays"]; !ok {
			t.Error("the cleanup crossed into another server")
		}
	})

	// The security property. A pin left behind is a baseline a DIFFERENT
	// server can inherit merely by reusing the id: its tools would compare
	// equal to the old ones and be classified Unchanged, so drift detection
	// would be disarmed for tools that were never reviewed. After a removal
	// the same catalog must read as New.
	t.Run("a re-added id starts from no baseline", func(t *testing.T) {
		s, _ := newPinStore(t)
		tools := []ToolSnapshot{snap("deploy", "ship it", `{"a":1}`)}
		if _, err := s.CheckServer(ctx, "srv", tools); err != nil {
			t.Fatalf("seed: %v", err)
		}
		if err := s.ForgetServer(ctx, "srv"); err != nil {
			t.Fatalf("forget: %v", err)
		}
		drifts, err := s.CheckServer(ctx, "srv", tools)
		if err != nil {
			t.Fatalf("recheck: %v", err)
		}
		if len(drifts) != 1 || drifts[0].Kind != DriftNew {
			t.Fatalf("drifts = %+v, want one DriftNew — the old baseline survived", drifts)
		}
	})

	t.Run("unknown server is a no-op", func(t *testing.T) {
		s, _ := newPinStore(t)
		if err := s.ForgetServer(ctx, "never-existed"); err != nil {
			t.Errorf("forgetting an unknown server errored: %v", err)
		}
	})
}

// TestApprovalStoreForgetServer pins that approval records die with their
// server. Leaving them lets a server re-added under the same id inherit
// Approved records for tools no human ever looked at.
func TestApprovalStoreForgetServer(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	s, err := OpenApprovalStore(dir, Options{})
	if err != nil {
		t.Fatalf("OpenApprovalStore: %v", err)
	}
	tool := snap("t", "d", "")
	if _, err := s.Observe(ctx, "gone", tool, ModeAuto); err != nil {
		t.Fatalf("seed gone: %v", err)
	}
	if _, err := s.Observe(ctx, "stays", tool, ModeAuto); err != nil {
		t.Fatalf("seed stays: %v", err)
	}
	if err := s.ForgetServer(ctx, "gone"); err != nil {
		t.Fatalf("forget: %v", err)
	}

	// A re-observation must land in the store's first-sight state, not
	// inherit the approval the removed server earned.
	rec, err := s.Observe(ctx, "gone", tool, ModeManual)
	if err != nil {
		t.Fatalf("re-observe: %v", err)
	}
	if rec.Status != StatePending {
		t.Errorf("a re-added server was seen as %v, want Pending", rec.Status)
	}
	kept, err := s.Observe(ctx, "stays", tool, ModeManual)
	if err != nil {
		t.Fatalf("observe stays: %v", err)
	}
	if kept.Status != StateApproved {
		t.Errorf("the cleanup crossed into another server: %v", kept.Status)
	}
}

// TestQuarantineStoreForgetServer pins the scan-by-field behaviour: entries
// are keyed by EXPOSED name, so a cleanup keyed on the server id has to match
// Entry.Server instead of indexing.
func TestQuarantineStoreForgetServer(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	s, err := OpenQuarantineStore(dir, Options{})
	if err != nil {
		t.Fatalf("OpenQuarantineStore: %v", err)
	}
	// A renamed tool: the exposed key bears no resemblance to the server id,
	// which is exactly the case a filename-derived cleanup would miss.
	if err := s.Add(ctx, "renamed_alias", QuarantineEntry{Server: "gone", Tool: "raw"}); err != nil {
		t.Fatalf("seed renamed: %v", err)
	}
	if err := s.Add(ctx, "gone__other", QuarantineEntry{Server: "gone", Tool: "other"}); err != nil {
		t.Fatalf("seed other: %v", err)
	}
	if err := s.Add(ctx, "stays__t", QuarantineEntry{Server: "stays", Tool: "t"}); err != nil {
		t.Fatalf("seed stays: %v", err)
	}

	if err := s.ForgetServer(ctx, "gone"); err != nil {
		t.Fatalf("forget: %v", err)
	}
	got, err := s.Snapshot(ctx)
	if err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("entries = %+v, want only the surviving server's", got)
	}
	if _, ok := got["stays__t"]; !ok {
		t.Errorf("the cleanup crossed into another server: %+v", got)
	}
}
