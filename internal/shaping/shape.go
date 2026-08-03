package shaping

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/dinstein/agent-hub/internal/mcp"
)

// Budget bounds the payload delivered to the agent for one tool result.
// Bytes <= 0 means "unbounded": Shape and Fetch then deliver everything.
//
// The bound covers the encoded content array plus structuredContent. The
// recovery trailer is deliberately NOT counted (see doc.go): a page may
// exceed Bytes by exactly the trailer block.
type Budget struct {
	// Bytes is the maximum encoded payload size in bytes.
	Bytes int
}

// Options carries the per-call inputs Shape cannot derive from the result.
//
// Owner and ID are both required for truncation. Without them there is no
// way to hand the remainder back, so Shape delivers the whole result
// (fail-open, doc.go).
type Options struct {
	// Owner binds the cursor to a session. It is the only isolation
	// fetch_result has.
	Owner Owner
	// ID is the cursor id, minted by Store.NextID.
	ID string
	// Now stamps the cursor; zero means time.Now().
	Now time.Time
	// TTL bounds the cursor's life; zero means DefaultTTL.
	TTL time.Duration
	// Format selects the delivered encoding (docs/modules/dataplane.md). The zero value
	// is DefaultFormat (json), so a caller that predates M1.5 keeps its
	// exact previous behaviour.
	Format Format
}

// Cursor is Shape's record of a truncated result: the wire id embedded in
// the trailer, the session it is bound to, where the remainder starts, and
// the payload the Store must retain. The zero Cursor means "nothing was
// deferred".
type Cursor struct {
	// ID is the wire form the agent passes back to fetch_result.
	ID string
	// Owner is the session the cursor is bound to.
	Owner Owner
	// NextOffset is the rune offset of the first undelivered character.
	NextOffset int
	// Total is the rune length of the retained payload.
	Total int
	// Budget governs pages 2+ as well, so it travels with the entry.
	Budget Budget
	// CreatedAt / TTL define expiry.
	CreatedAt time.Time
	TTL       time.Duration

	// full is the retained payload, handed to the Store by Entry/Retain.
	full string
}

// IsZero reports whether nothing was deferred.
func (c Cursor) IsZero() bool { return c.ID == "" }

// String returns the wire form (the id), so a Cursor can be formatted
// straight into a message.
func (c Cursor) String() string { return c.ID }

// Entry renders the cursor as the Store record that backs it.
func (c Cursor) Entry() Entry {
	return Entry{
		ID:        c.ID,
		Owner:     c.Owner,
		CreatedAt: c.CreatedAt,
		TTL:       c.TTL,
		Budget:    c.Budget,
		Full:      c.full,
	}
}

// trailerFormat is the recovery hint appended as the LAST content block of
// a truncated page. Frozen text (golden-tested): agent-side prompting keys
// off it, so wording changes are ABI changes.
//
// Arguments: delivered runes, total runes, cursor id, next offset.
const trailerFormat = "Truncated by agenthub to fit the result budget: %d of %d characters delivered. " +
	"Use fetch_result with cursor=%s offset=%d to continue."

// minPartialBytes is the smallest text slice worth splitting a block for.
// Below it the whole block is deferred instead — a two-character page plus
// a trailer costs more than it delivers.
const minPartialBytes = 16

// DefaultTTL is the cursor lifetime when Options.TTL is zero.
const DefaultTTL = 30 * time.Minute

// Shape bounds res to budget. It returns the page delivered to the agent,
// the cursor describing the deferred remainder, and whether truncation
// happened. When the result fits (or cannot be shaped safely) it returns
// res unchanged, a zero Cursor and false.
//
// The caller retains the remainder with Retain before delivering the page;
// a page whose cursor was never stored still reads correctly, the fetch
// just misses.
//
// Shape honours Options.Format, but a re-encoding that saves bytes WITHOUT
// truncating is invisible in its return values. Callers that record savings
// should use ShapeResult, which reports the whole outcome.
func Shape(res *mcp.CallResult, budget Budget, opts Options) (*mcp.CallResult, Cursor, bool) {
	r := ShapeResult(res, budget, opts)
	return r.Page, r.Cursor, r.Truncated
}

// shape is the truncation core: everything Shape did before M1.5 added the
// re-encoding step in front of it. It is unexported so there is exactly one
// public entry (ShapeResult) that decides the ORDER of the two steps.
func shape(res *mcp.CallResult, budget Budget, opts Options) (*mcp.CallResult, Cursor, bool) {
	if res == nil || budget.Bytes <= 0 {
		return res, Cursor{}, false
	}
	baselineBytes := len(res.Content) + len(res.StructuredContent)
	if baselineBytes <= budget.Bytes {
		return res, Cursor{}, false
	}
	// No cursor authority: deliver everything rather than destroy data.
	if opts.ID == "" || opts.Owner == "" {
		return res, Cursor{}, false
	}
	segs, ok := segmentize(res)
	if !ok || len(segs) == 0 {
		// Content is not a JSON array we can rebuild: fail open.
		return res, Cursor{}, false
	}

	page, cutSeg, cutRunes, keepStructured := paginate(segs, budget)
	if cutSeg < 0 {
		// Everything fit once compacted (the raw payload was whitespace).
		return res, Cursor{}, false
	}

	full, starts, total := linearize(segs)
	nextOffset := starts[cutSeg] + cutRunes

	trailer := textBlock(fmt.Sprintf(trailerFormat, nextOffset, total, opts.ID, nextOffset))
	content, err := json.Marshal(append(page, trailer))
	if err != nil {
		// Re-encoding blocks we just decoded cannot realistically fail;
		// deliver the original rather than an empty page.
		return res, Cursor{}, false
	}

	out := &mcp.CallResult{Content: content, IsError: res.IsError}
	if keepStructured {
		out.StructuredContent = res.StructuredContent
	}

	// Never-larger guarantee (same constructive property 7.2 requires of
	// toonenc): the trailer is not free, so on a result that only just
	// exceeds the budget the shaped page can cost MORE than the original.
	// Delivering the original is then better on every axis — fewer bytes
	// AND no data withheld — so shaping stands down.
	actualBytes := len(out.Content) + len(out.StructuredContent)
	if actualBytes >= baselineBytes {
		return res, Cursor{}, false
	}

	now := opts.Now
	if now.IsZero() {
		now = time.Now()
	}
	ttl := opts.TTL
	if ttl <= 0 {
		ttl = DefaultTTL
	}
	c := Cursor{
		ID:         opts.ID,
		Owner:      opts.Owner,
		NextOffset: nextOffset,
		Total:      total,
		Budget:     budget,
		CreatedAt:  now.UTC(),
		TTL:        ttl,
		full:       full,
	}
	return out, c, true
}

// segment is one linearizable unit of a result: a content block, or the
// structuredContent payload as a final trailing unit.
type segment struct {
	// raw is the compacted encoding delivered on page 1.
	raw json.RawMessage
	// text is what the segment contributes to the retained payload.
	text string
	// splittable marks a text block: only these may be cut mid-segment.
	splittable bool
	// structured marks the structuredContent unit (never a content block).
	structured bool
}

// segmentize decodes a result into ordered segments. It reports false when
// the content array cannot be decoded — the caller then fails open.
func segmentize(res *mcp.CallResult) ([]segment, bool) {
	var segs []segment
	if len(res.Content) > 0 {
		var blocks []json.RawMessage
		if err := json.Unmarshal(res.Content, &blocks); err != nil {
			return nil, false
		}
		for _, b := range blocks {
			raw, err := compact(b)
			if err != nil {
				return nil, false
			}
			var probe struct {
				Type string `json:"type"`
				Text string `json:"text"`
			}
			if err := json.Unmarshal(b, &probe); err == nil && probe.Type == "text" {
				segs = append(segs, segment{raw: raw, text: probe.Text, splittable: true})
				continue
			}
			segs = append(segs, segment{raw: raw, text: string(raw)})
		}
	}
	if len(res.StructuredContent) > 0 {
		raw, err := compact(res.StructuredContent)
		if err != nil {
			return nil, false
		}
		segs = append(segs, segment{raw: raw, text: string(raw), structured: true})
	}
	return segs, true
}

// linearize renders the segments into the retained payload: segment texts
// joined by "\n". starts[i] is the rune offset at which segment i begins,
// so a cut inside segment i maps to starts[i]+k. Total is the payload's
// rune length.
func linearize(segs []segment) (full string, starts []int, total int) {
	var b strings.Builder
	starts = make([]int, len(segs))
	for i, s := range segs {
		if i > 0 {
			b.WriteByte('\n')
			total++
		}
		starts[i] = total
		b.WriteString(s.text)
		total += utf8.RuneCountInString(s.text)
	}
	return b.String(), starts, total
}

// paginate walks the segments in order filling the budget. It returns the
// page blocks, the index of the first segment not fully delivered (-1 when
// everything fit), how many runes of THAT segment were delivered, and
// whether structuredContent survived.
//
// A structured segment is all-or-nothing; a text segment may be cut. The
// walk stops at the first segment that does not fit, so everything after it
// is deferred too — order is preserved, which is what makes the rune offset
// into the linearized payload meaningful.
func paginate(segs []segment, budget Budget) (page []json.RawMessage, cutSeg, cutRunes int, keepStructured bool) {
	used := 2 // the enclosing "[" and "]"
	cutSeg = -1
	for i, s := range segs {
		if s.structured {
			if used+len(s.raw) <= budget.Bytes {
				used += len(s.raw)
				keepStructured = true
				continue
			}
			return page, i, 0, false
		}
		comma := 0
		if len(page) > 0 {
			comma = 1
		}
		if used+comma+len(s.raw) <= budget.Bytes {
			page = append(page, s.raw)
			used += comma + len(s.raw)
			continue
		}
		if s.splittable {
			avail := budget.Bytes - used - comma - textBlockOverhead
			if avail >= minPartialBytes {
				if nRunes, nBytes := fitRunes(s.text, avail); nRunes > 0 {
					page = append(page, textBlock(s.text[:nBytes]))
					return page, i, nRunes, false
				}
			}
		}
		return page, i, 0, false
	}
	return page, cutSeg, 0, keepStructured
}

// compact normalizes a JSON value to its whitespace-free encoding so the
// retained payload and the page are byte-deterministic.
func compact(raw json.RawMessage) (json.RawMessage, error) {
	var buf bytes.Buffer
	if err := json.Compact(&buf, raw); err != nil {
		return nil, err
	}
	return json.RawMessage(buf.Bytes()), nil
}

// textBlock builds one MCP text content block. Encoding is the same shape
// internal/pipeline emits, so trailer blocks look identical wherever they
// come from.
func textBlock(text string) json.RawMessage {
	raw, err := json.Marshal(struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}{Type: "text", Text: text})
	if err != nil {
		return json.RawMessage(`{"type":"text","text":"agenthub: internal encoding failure"}`)
	}
	return raw
}

// textBlockOverhead is the encoded size of an empty text block, i.e. the
// fixed cost of delivering a partial text segment.
var textBlockOverhead = len(textBlock(""))

// fitRunes returns the largest rune prefix of s whose JSON-escaped encoding
// fits in avail bytes, as (rune count, byte length). escapedRuneLen mirrors
// encoding/json exactly (invariant test), so the emitted block size is
// predictable rather than estimated.
func fitRunes(s string, avail int) (nRunes, nBytes int) {
	used := 0
	for i := 0; i < len(s); {
		r, size := utf8.DecodeRuneInString(s[i:])
		l := escapedRuneLen(r, size)
		if used+l > avail {
			return nRunes, i
		}
		used += l
		nRunes++
		i += size
	}
	return nRunes, len(s)
}

// escapedRuneLen is the number of bytes encoding/json spends on one rune
// inside a string literal, with HTML escaping on (the encoder's default).
// Kept in lockstep with the stdlib by TestEscapedRuneLenMatchesStdlib.
func escapedRuneLen(r rune, size int) int {
	if r == utf8.RuneError && size == 1 {
		return 6 // invalid UTF-8 byte is replaced by �
	}
	switch r {
	case '"', '\\', '\b', '\f', '\n', '\r', '\t':
		return 2 // \" \\ \b \f \n \r \t
	case '<', '>', '&':
		return 6 // \u003c \u003e \u0026 (HTML escaping is on by default)
	case '\u2028', '\u2029':
		return 6 // JS line terminators are escaped too
	}
	if r < 0x20 {
		return 6 // \u00XX
	}
	return size
}

// runeIndexToByte maps a rune offset into a byte index, clamping at the end
// of s. Offsets are rune ("character") offsets by design; converting here
// keeps every slice on a rune boundary, so no page can ever split a UTF-8
// sequence.
func runeIndexToByte(s string, runes int) int {
	if runes <= 0 {
		return 0
	}
	n := 0
	for i := 0; i < len(s); {
		if n == runes {
			return i
		}
		_, size := utf8.DecodeRuneInString(s[i:])
		i += size
		n++
	}
	return len(s)
}
