package jsonl

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
)

// The reading and retention side of the rotation scheme. It lives here
// because segmentPath (writer.go) is what names a rotated file, and a caller
// that composed the glob itself would be the second place that scheme lives —
// which is how a reader ends up opening only the active file and reporting
// "nothing happened" for everything rotation moved aside.

// Segments lists a stream's files oldest first, with the active file last.
//
// The rotated name carries a fixed-width, zero-padded UTC stamp, so lexical
// order is chronological order.
func Segments(path string) []string {
	ext := filepath.Ext(path)
	base := strings.TrimSuffix(path, ext)
	matches, err := filepath.Glob(base + "-*" + ext)
	if err != nil {
		matches = nil
	}
	slices.Sort(matches)
	return append(matches, path)
}

// Prune deletes all but the newest keep rotated segments. The active file is
// never touched, and a keep below zero is treated as zero.
//
// Failure to remove one is ignored on purpose: another process may have
// pruned it already, and a retention sweep able to fail its caller's Open
// would turn "the disk is briefly busy" into "this process does not start".
func Prune(path string, keep int) {
	if keep < 0 {
		keep = 0
	}
	all := Segments(path)
	segments := all[:len(all)-1] // drop the active file
	if len(segments) <= keep {
		return
	}
	for _, old := range segments[:len(segments)-keep] {
		_ = os.Remove(old)
	}
}
