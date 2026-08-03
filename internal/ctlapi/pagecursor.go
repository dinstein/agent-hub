package ctlapi

import (
	"encoding/base64"
	"fmt"
	"strings"
	"time"
)

// One pagination model for every observability list.
//
// The three lists — calls, events, process logs — answer three questions
// about the same installation, and a UI that shows them side by side has to
// page them the same way. Three cursor formats would have been three sets of
// edge cases at the page boundary, and the boundary is where a pager is
// either right or quietly wrong.
//
// The model is: rows are NEWEST FIRST, and a cursor names the last row of the
// page just served. The next page starts at the first row strictly older than
// it. That makes paging stable while new records arrive — a fresh record is
// newer than every cursor, so it can only ever appear on page one, and never
// shifts a row from page two onto page three.
//
// An offset would do the opposite: with N new records between two requests,
// an offset-based page two repeats N rows it already showed.

// pageCursor is a position in a newest-first list.
type pageCursor struct {
	time time.Time
	// tie breaks records sharing a timestamp. It is whatever the list has
	// that is stable across reads — a call id, a scope/kind/pid triple. Two
	// records with the same timestamp AND the same tie can cost one row at a
	// page boundary; nothing here can prevent that without an index, and it
	// is a far smaller error than repeating a page.
	tie string
}

// encodePageCursor names one row. The encoding is opaque on purpose: a client
// that parsed it would be depending on a shape that is free to change.
func encodePageCursor(t time.Time, tie string) string {
	return base64.RawURLEncoding.EncodeToString([]byte(t.UTC().Format(time.RFC3339Nano) + "\n" + tie))
}

// decodePageCursor parses one. An unreadable cursor is a client error, not an
// empty page: answering with no rows would be indistinguishable from "you
// have reached the end", and the client would stop paging silently.
func decodePageCursor(raw string) (pageCursor, error) {
	decoded, err := base64.RawURLEncoding.DecodeString(raw)
	if err != nil {
		return pageCursor{}, fmt.Errorf("decode cursor: %w", err)
	}
	stamp, tie, ok := strings.Cut(string(decoded), "\n")
	if !ok {
		return pageCursor{}, fmt.Errorf("cursor has invalid shape")
	}
	t, err := time.Parse(time.RFC3339Nano, stamp)
	if err != nil {
		return pageCursor{}, fmt.Errorf("cursor has invalid timestamp: %w", err)
	}
	return pageCursor{time: t, tie: tie}, nil
}

// isZero reports whether this is the "start at the beginning" cursor.
func (c pageCursor) isZero() bool { return c.time.IsZero() }

// after reports whether a row at (t, tie) sits strictly after this cursor in
// a newest-first list — that is, whether it belongs on the NEXT page.
func (c pageCursor) after(t time.Time, tie string) bool {
	if t.Before(c.time) {
		return true
	}
	return t.Equal(c.time) && tie < c.tie
}
