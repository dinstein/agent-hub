package jsonl_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/dinstein/agent-hub/internal/jsonl"
)

// touchSegments creates the active file plus n rotated segments whose names
// sort chronologically, and returns the segment paths oldest first.
func touchSegments(t *testing.T, dir string, n int) (active string, segments []string) {
	t.Helper()
	active = filepath.Join(dir, "stream.jsonl")
	if err := os.WriteFile(active, []byte("{}\n"), 0o600); err != nil {
		t.Fatalf("write active: %v", err)
	}
	for i := range n {
		// Same stamp shape jsonl.segmentPath produces: fixed width, so
		// lexical order is chronological order.
		p := filepath.Join(dir, "stream-2026080"+string(rune('1'+i))+"T000000.000000000Z.p1.jsonl")
		if err := os.WriteFile(p, []byte("{}\n"), 0o600); err != nil {
			t.Fatalf("write segment: %v", err)
		}
		segments = append(segments, p)
	}
	return active, segments
}

func TestSegmentsListsRotatedFilesOldestFirstWithActiveLast(t *testing.T) {
	dir := t.TempDir()
	active, segments := touchSegments(t, dir, 3)

	got := jsonl.Segments(active)

	want := append(append([]string{}, segments...), active)
	if len(got) != len(want) {
		t.Fatalf("Segments returned %d paths, want %d: %v", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("Segments[%d] = %s, want %s", i, filepath.Base(got[i]), filepath.Base(want[i]))
		}
	}
}

// A stream nothing has rotated yet still occupies its active file, and a
// caller must be able to tell that from "this stream does not exist" — so the
// active path is returned whether or not it is on disk.
func TestSegmentsReturnsTheActiveFileEvenWhenAbsent(t *testing.T) {
	active := filepath.Join(t.TempDir(), "stream.jsonl")

	got := jsonl.Segments(active)

	if len(got) != 1 || got[0] != active {
		t.Fatalf("Segments = %v, want just the active path", got)
	}
}

func TestPruneKeepsTheNewestSegmentsAndNeverTheActiveFile(t *testing.T) {
	dir := t.TempDir()
	active, segments := touchSegments(t, dir, 5)

	jsonl.Prune(active, 2)

	for i, p := range segments {
		_, err := os.Stat(p)
		gone := os.IsNotExist(err)
		wantGone := i < len(segments)-2
		if gone != wantGone {
			t.Errorf("segment %s gone=%v, want %v", filepath.Base(p), gone, wantGone)
		}
	}
	if _, err := os.Stat(active); err != nil {
		t.Fatalf("the active file must never be pruned: %v", err)
	}
}

// keep <= 0 means "no rotated history", not "delete everything": the active
// file is outside the policy by construction.
func TestPruneWithZeroKeepRemovesEverySegmentButTheActiveFile(t *testing.T) {
	dir := t.TempDir()
	active, segments := touchSegments(t, dir, 3)

	jsonl.Prune(active, -1)

	for _, p := range segments {
		if _, err := os.Stat(p); !os.IsNotExist(err) {
			t.Errorf("%s survived a zero-keep prune", filepath.Base(p))
		}
	}
	if _, err := os.Stat(active); err != nil {
		t.Fatalf("the active file must never be pruned: %v", err)
	}
}

func TestPruneWithFewerSegmentsThanKeepRemovesNothing(t *testing.T) {
	dir := t.TempDir()
	active, segments := touchSegments(t, dir, 2)

	jsonl.Prune(active, 3)

	for _, p := range segments {
		if _, err := os.Stat(p); err != nil {
			t.Errorf("%s was pruned below the keep count: %v", filepath.Base(p), err)
		}
	}
}

func TestIsSegmentSeparatesRotatedFilesFromStreamsOfTheirOwn(t *testing.T) {
	cases := map[string]bool{
		"/logs/gateway-claude-code.log":                               false,
		"/logs/gateway-claude-code-20260803T120000.000000000Z.p7.log": true,
		"/logs/events.jsonl":                                          false,
		"/logs/events-20260803T120000.000000000Z.p12345.jsonl":        true,
		"/logs/gateway-2026.log":                                      false,
		"/logs/gateway-client-with-p7.log":                            false,
		"/logs/server-github-20260803T120000.000000000Z.p1.log":       true,
	}
	for path, want := range cases {
		if got := jsonl.IsSegment(path); got != want {
			t.Errorf("IsSegment(%q) = %v, want %v", path, got, want)
		}
	}
}
