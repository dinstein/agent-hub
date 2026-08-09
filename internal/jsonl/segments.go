package jsonl

import (
	"os"
	"path/filepath"
	"regexp"
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
//
// The glob is NECESSARY BUT NOT SUFFICIENT, which is what IsSegment is here
// for. <base>-*<ext> matches every rotated file of this stream and also every
// stream whose own name extends this one: the pattern for
// gateway-claude-code.log matches gateway-claude-code-dev.log — a second
// client's ACTIVE log — and everything rotated off it. Two clients named
// claude-code and claude-code-dev are the whole precondition, and process logs
// are named gateway-<client>.log from ids an operator chooses.
//
// Unfiltered, that reached both halves of this file. A reader merged the
// sibling's records into this stream's, while the sibling's own read stayed
// clean, so the two disagreed about what one client did. Worse, Prune below
// walks the same list and calls os.Remove on it — and NewWriter calls Prune on
// every open — so starting one client's gateway could DELETE another client's
// live log.
func Segments(path string) []string {
	ext := filepath.Ext(path)
	base := strings.TrimSuffix(path, ext)
	matches, err := filepath.Glob(base + "-*" + ext)
	if err != nil {
		matches = nil
	}
	segments := matches[:0]
	for _, m := range matches {
		if stemOf(m) == base {
			segments = append(segments, m)
		}
	}
	slices.Sort(segments)
	return append(segments, path)
}

// stemOf returns the extension-less path of the stream a rotated file belongs
// to, or "" when the file carries no segment suffix at all.
//
// Comparing the stem is the whole filter, and IsSegment alone is not enough:
// gateway-claude-code-dev-<stamp>.p9.log IS a segment, of a DIFFERENT stream,
// and it matches the shorter stream's glob. Only the name the suffix was
// appended to says which stream a file is part of.
func stemOf(path string) string {
	body := strings.TrimSuffix(path, filepath.Ext(path))
	loc := segmentSuffix.FindStringIndex(body)
	if loc == nil {
		return ""
	}
	return body[:loc[0]]
}

// segmentSuffix matches the "-<stamp>.p<pid>" tail segmentPath appends. The
// stamp is fixed-width, so this cannot mistake a stream whose own name ends
// in a number for a rotated file.
var segmentSuffix = regexp.MustCompile(`-\d{8}T\d{6}\.\d{9}Z\.p\d+$`)

// IsSegment reports whether path names a rotated segment rather than a
// stream's active file.
//
// A caller listing streams by glob needs it: gateway-*.log matches
// gateway-claude-code.log and every segment rotated off it, and taking a
// segment for a stream of its own reads the same records twice — once
// directly and once as part of the stream it belongs to.
func IsSegment(path string) bool {
	ext := filepath.Ext(path)
	return segmentSuffix.MatchString(strings.TrimSuffix(path, ext))
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
