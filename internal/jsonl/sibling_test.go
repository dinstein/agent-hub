package jsonl

import (
	"os"
	"path/filepath"
	"slices"
	"testing"
)

// A stream's glob matches every stream whose name extends it — process logs are
// named gateway-<client>.log from ids an operator chooses, so `claude-code` and
// `claude-code-dev` are the whole precondition. IsSegment is what tells the two
// apart, and Segments has to apply it: everything downstream of Segments reads
// or DELETES what it returns.

// writeFiles creates each path with one line in it.
func writeFiles(t *testing.T, paths ...string) {
	t.Helper()
	for _, p := range paths {
		if err := os.WriteFile(p, []byte("{}\n"), 0o600); err != nil {
			t.Fatalf("write %s: %v", p, err)
		}
	}
}

func TestSegmentsExcludesASiblingStream(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	own := filepath.Join(dir, "gateway-claude-code.log")
	sibling := filepath.Join(dir, "gateway-claude-code-dev.log")
	siblingSeg := filepath.Join(dir, "gateway-claude-code-dev-20260810T030000.000000000Z.p9.log")
	ownSeg := filepath.Join(dir, "gateway-claude-code-20260810T020000.000000000Z.p1.log")
	writeFiles(t, own, sibling, siblingSeg, ownSeg)

	got := Segments(own)
	want := []string{ownSeg, own}
	if !slices.Equal(got, want) {
		t.Errorf("Segments(%s) = %v,\n want %v\n(a sibling stream's active log — or its segments — must never join this stream)",
			filepath.Base(own), got, want)
	}
	// The sibling reads only its own, including the segment whose name also
	// matches the shorter stream's glob.
	if got, want := Segments(sibling), []string{siblingSeg, sibling}; !slices.Equal(got, want) {
		t.Errorf("Segments(%s) = %v, want %v", filepath.Base(sibling), got, want)
	}
}

// The consequence that is not merely a wrong read: NewWriter calls Prune on
// every open, Prune removes what Segments returned, so an unfiltered list makes
// starting one client's gateway delete another client's live log.
func TestPruneNeverRemovesASiblingStream(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	own := filepath.Join(dir, "gateway-claude-code.log")
	// Sorts BEFORE any timestamp, so an unfiltered Prune reaches it first.
	sibling := filepath.Join(dir, "gateway-claude-code-0dev.log")
	paths := []string{own, sibling}
	for _, stamp := range []string{"010000", "020000", "030000", "040000"} {
		paths = append(paths, filepath.Join(dir, "gateway-claude-code-20260810T"+stamp+".000000000Z.p1.log"))
	}
	writeFiles(t, paths...)

	Prune(own, 3)

	if _, err := os.Stat(sibling); err != nil {
		t.Fatalf("Prune(%s) removed another client's ACTIVE log %s: %v",
			filepath.Base(own), filepath.Base(sibling), err)
	}
	if _, err := os.Stat(own); err != nil {
		t.Fatalf("Prune removed the active file it was pruning: %v", err)
	}
	// Its own oldest segment is gone: retention still works.
	oldest := filepath.Join(dir, "gateway-claude-code-20260810T010000.000000000Z.p1.log")
	if _, err := os.Stat(oldest); !os.IsNotExist(err) {
		t.Fatalf("Prune kept the oldest segment %s; retention stopped working", filepath.Base(oldest))
	}
}
