package integrity

import (
	"context"
	"testing"
)

func TestQuarantineAddReleaseCycle(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	s, err := OpenQuarantineStore(dir, Options{})
	if err != nil {
		t.Fatal(err)
	}

	// Keyed by the client-visible EXPOSED name (post-override, #423) — the
	// raw route travels inside the entry.
	const exposed = "renamed__tool"
	entry := QuarantineEntry{
		Server: "srv", Tool: "raw-tool",
		Reason:      "high drift: destructive hint removed",
		PinnedHash:  "v1:aaa",
		CurrentHash: "v1:bbb",
	}
	if err := s.Add(ctx, exposed, entry); err != nil {
		t.Fatalf("Add: %v", err)
	}

	q, err := s.IsQuarantined(ctx, exposed)
	if err != nil || !q {
		t.Fatalf("IsQuarantined(%s) = %v, %v; want true", exposed, q, err)
	}
	if q, err := s.IsQuarantined(ctx, "raw-tool"); err != nil || q {
		t.Errorf("raw name must not be quarantined (keys are exposed names): %v, %v", q, err)
	}

	snap, err := s.Snapshot(ctx)
	if err != nil {
		t.Fatal(err)
	}
	got := snap[exposed]
	if got.Server != "srv" || got.Tool != "raw-tool" || got.At.IsZero() {
		t.Errorf("stored entry mangled: %+v", got)
	}

	released, found, err := s.Release(ctx, exposed)
	if err != nil || !found {
		t.Fatalf("Release: found=%v err=%v", found, err)
	}
	if released.Tool != "raw-tool" {
		t.Errorf("released entry tool = %q", released.Tool)
	}
	if q, _ := s.IsQuarantined(ctx, exposed); q {
		t.Error("still quarantined after release")
	}

	// Idempotent release on a healthy store: found=false, no error.
	if _, found, err := s.Release(ctx, exposed); err != nil || found {
		t.Errorf("second release: found=%v err=%v, want false,nil", found, err)
	}

	if err := s.Add(ctx, "", entry); err == nil {
		t.Error("Add with empty exposed name must error")
	}
}
