package skills

import (
	"strings"
)

// Sentinel markers. Frozen: changing either string orphans every block
// agenthub has ever written, and an orphaned block is indistinguishable
// from user content — it would be left behind forever.
const (
	sentinelPrefix = "<!-- agenthub:skill:"
	sentinelStart  = ":start -->"
	sentinelEnd    = ":end -->"
)

// startMarker and endMarker render the sentinel pair for one skill.
func startMarker(id string) string { return sentinelPrefix + id + sentinelStart }
func endMarker(id string) string   { return sentinelPrefix + id + sentinelEnd }

// containsSentinelMarker reports whether text carries any agenthub sentinel.
// Content that does is refused at import: embedding an END marker inside a
// block truncates it, and everything after the fake END silently becomes
// "user content" that agenthub will never manage or remove again.
func containsSentinelMarker(text string) bool {
	return strings.Contains(text, sentinelPrefix)
}

// scanCarriesSentinelMarker reports whether any field of a scanned skill that
// renderSkillBody embeds into a managed block carries an agenthub marker: the
// name, description, body, VERSION, and every bundled file PATH. Version and
// paths were missing from the original import guard, so a marker smuggled in
// either reached the shared client file. It matches any id (the prefix), so a
// marker naming a DIFFERENT skill — inert to findBlock for this id — is also
// kept out of the library.
func scanCarriesSentinelMarker(name string, sc *scanned) bool {
	if containsSentinelMarker(name) ||
		containsSentinelMarker(sc.meta.Description) ||
		containsSentinelMarker(sc.meta.Body) ||
		containsSentinelMarker(sc.meta.Version) {
		return true
	}
	for _, f := range sc.files {
		if containsSentinelMarker(f.Path) {
			return true
		}
	}
	return false
}

// blockSpan locates one skill's block inside a file.
type blockSpan struct {
	// start is the byte offset of the start marker.
	start int
	// end is the byte offset one past the end marker's trailing newline
	// (or one past the marker at EOF).
	end int
	// body is the text strictly between the two marker lines.
	body string
}

// findBlock locates the block for id.
//
// Fail direction, and the entire point of this function: anything other
// than "exactly zero" or "exactly one well-formed pair" is a *SentinelError
// and the caller MUST refuse to write. Unpaired, inverted or duplicated
// markers mean we can no longer tell our bytes from the user's, and the
// only safe move is to stop and say so. Overwriting on a guess is how a
// managed-block tool eats somebody's file.
func findBlock(content, id, path string) (blockSpan, bool, error) {
	sm, em := startMarker(id), endMarker(id)

	starts := allIndexes(content, sm)
	ends := allIndexes(content, em)
	switch {
	case len(starts) == 0 && len(ends) == 0:
		return blockSpan{}, false, nil
	case len(starts) > 1:
		return blockSpan{}, false, &SentinelError{Path: path, SkillID: id, Reason: "duplicate start markers"}
	case len(ends) > 1:
		return blockSpan{}, false, &SentinelError{Path: path, SkillID: id, Reason: "duplicate end markers"}
	case len(starts) == 0:
		return blockSpan{}, false, &SentinelError{Path: path, SkillID: id, Reason: "end marker without a start marker"}
	case len(ends) == 0:
		return blockSpan{}, false, &SentinelError{Path: path, SkillID: id, Reason: "start marker without an end marker"}
	}

	s, e := starts[0], ends[0]
	if e < s {
		return blockSpan{}, false, &SentinelError{Path: path, SkillID: id, Reason: "end marker precedes start marker"}
	}

	bodyStart := s + len(sm)
	if strings.HasPrefix(content[bodyStart:], "\n") {
		bodyStart++
	}
	end := e + len(em)
	if strings.HasPrefix(content[end:], "\n") {
		end++
	}
	return blockSpan{start: s, end: end, body: content[bodyStart:e]}, true, nil
}

// allIndexes returns every occurrence offset of sub in s.
func allIndexes(s, sub string) []int {
	var out []int
	for off := 0; ; {
		i := strings.Index(s[off:], sub)
		if i < 0 {
			return out
		}
		out = append(out, off+i)
		off += i + len(sub)
	}
}

// upsertBlock returns content with id's block set to body.
//
// Bytes outside the block are preserved verbatim, with ONE documented
// exception: appending to a file that does not end with a newline adds one,
// so the start marker begins on its own line. That is a POSIX text-file
// normalization and the only byte outside our sentinels that this package
// ever writes.
//
// Fail direction: a damaged sentinel pair returns *SentinelError and
// content is returned unchanged (errors.Is(err, ErrConflict)).
func upsertBlock(content, id, body, path string) (string, error) {
	span, found, err := findBlock(content, id, path)
	if err != nil {
		return content, err
	}
	block := renderSentinel(id, body)
	if found {
		return content[:span.start] + block + content[span.end:], nil
	}
	if content == "" {
		return block, nil
	}
	if !strings.HasSuffix(content, "\n") {
		content += "\n"
	}
	return content + block, nil
}

// removeBlockFrom returns content with id's block removed, reporting
// whether anything was removed. Damaged sentinels refuse the edit for the
// same reason upsertBlock does.
func removeBlockFrom(content, id, path string) (string, bool, error) {
	span, found, err := findBlock(content, id, path)
	if err != nil {
		return content, false, err
	}
	if !found {
		return content, false, nil
	}
	return content[:span.start] + content[span.end:], true, nil
}

// renderSentinel wraps body in the marker pair. The result always ends with
// a newline so successive blocks stack cleanly.
func renderSentinel(id, body string) string {
	body = strings.TrimRight(body, "\n")
	var b strings.Builder
	b.WriteString(startMarker(id))
	b.WriteString("\n")
	if body != "" {
		b.WriteString(body)
		b.WriteString("\n")
	}
	b.WriteString(endMarker(id))
	b.WriteString("\n")
	return b.String()
}
