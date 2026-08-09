package shaping

import (
	"context"
	"encoding/json"
	"fmt"
	"unicode/utf8"

	"github.com/dinstein/agent-hub/internal/mcp"
)

// notFoundText is the ONE message every fetch_result failure returns:
// unknown cursor, expired cursor, another session's cursor, malformed
// cursor, unreadable store. Frozen text (golden-tested).
//
// Cursor ids are a guessable sequence, so telling "expired" apart from
// "not yours" would turn fetch_result into an oracle for enumerating other
// sessions' cursor space (docs/flows.md). One message, no variants, no
// error codes that differ.
const notFoundText = "fetch_result: unknown or expired cursor. " +
	"Re-run the original tool call to obtain a fresh result and cursor."

// Fetch returns the page of cursor starting at the given rune offset, and
// whether the cursor was served. On any failure it returns the frozen
// not-found result (isError, notFoundText) and false — callers deliver that
// result as-is; there is no second error channel to leak through.
//
// Offset is a rune ("character") offset into the retained payload, the same
// unit Shape reports in the trailer. A negative offset is clamped to 0; an
// offset at or past the end serves an empty page (end of stream), which is
// a success, not a miss.
func Fetch(ctx context.Context, store Store, owner Owner, cursor string, offset int) (*mcp.CallResult, bool) {
	if store == nil {
		return notFoundResult(), false
	}
	e, err := store.Get(ctx, owner, cursor)
	if err != nil {
		return notFoundResult(), false
	}
	// Defence in depth: Store.Get is contractually required to verify the
	// owner, but this is the isolation boundary, so it is checked on both
	// sides of the interface.
	if !e.ownedBy(owner) {
		return notFoundResult(), false
	}
	return page(e, offset), true
}

// page renders one page of a retained entry.
func page(e Entry, offset int) *mcp.CallResult {
	if offset < 0 {
		offset = 0
	}
	total := utf8.RuneCountInString(e.Full)
	if offset >= total {
		// End of stream: an empty content array, no trailer. Nothing is
		// left to continue, so a recovery hint would be a lie.
		return &mcp.CallResult{Content: json.RawMessage(`[]`)}
	}
	start := runeIndexToByte(e.Full, offset)
	rest := e.Full[start:]

	// Unbounded budget: the whole remainder in one page.
	nRunes, nBytes := total-offset, len(rest)
	if e.Budget.Bytes > 0 {
		avail := e.Budget.Bytes - arrayOverhead - textBlockOverhead
		if avail < minPartialBytes {
			avail = minPartialBytes
		}
		nRunes, nBytes = fitRunes(rest, avail)
		if nRunes == 0 {
			// Unreachable while avail >= minPartialBytes (no rune escapes
			// to more than 6 bytes), kept as a backstop: a page that
			// delivers nothing can never advance, which is a livelock the
			// agent cannot escape.
			_, size := utf8.DecodeRuneInString(rest)
			nRunes, nBytes = 1, size
		}
	}

	blocks := []json.RawMessage{textBlock(rest[:nBytes])}
	next := offset + nRunes
	if next < total {
		blocks = append(blocks, textBlock(fmt.Sprintf(trailerFormat, next, total, e.ID, next)))
	}
	content, err := json.Marshal(blocks)
	if err != nil {
		return notFoundResult()
	}
	return &mcp.CallResult{Content: content}
}

// notFoundResult is the frozen miss response.
func notFoundResult() *mcp.CallResult {
	content, err := json.Marshal([]json.RawMessage{textBlock(notFoundText)})
	if err != nil {
		content = json.RawMessage(`[]`)
	}
	return &mcp.CallResult{Content: content, IsError: true}
}
