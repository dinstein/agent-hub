package shaping

import (
	"encoding/json"
	"strings"

	"github.com/dinstein/agent-hub/internal/mcp"
	"github.com/dinstein/agent-hub/internal/shaping/toonenc"
)

// Format selects the wire encoding of a delivered result (docs/modules/dataplane.md,
// "TOON encoding"). It is the typed form of the governance switch
// `result_format`; ParseFormat maps the config string onto it.
//
// The default is and stays JSON. TOON is a display projection with no
// decoder (internal/shaping/toonenc doc.go), so turning it on is a decision
// about what an agent READS — never about what the gateway stores, caches or
// accepts back.
type Format string

const (
	// FormatJSON delivers results exactly as the downstream sent them.
	FormatJSON Format = "json"
	// FormatTOON re-encodes JSON-shaped TEXT blocks as TOON when that is
	// strictly cheaper.
	FormatTOON Format = "toon"
)

// DefaultFormat is what an absent, empty or unrecognised governance value
// resolves to. Defaulting to JSON is the conservative direction: a typo in
// governance.json must not change what an agent is reading.
const DefaultFormat = FormatJSON

// ParseFormat maps a governance string onto a Format. Comparison is
// case-insensitive and whitespace-trimmed; anything else is DefaultFormat.
func ParseFormat(s string) Format {
	switch Format(strings.ToLower(strings.TrimSpace(s))) {
	case FormatTOON:
		return FormatTOON
	default:
		return DefaultFormat
	}
}

// toonOptions is the frozen encoder configuration of the result path.
//
// Budget is deliberately ZERO here: truncation belongs to the pagination
// layer below, which is the only one that can hand the remainder back
// through a fetch_result cursor. A TOON-level truncation would drop data
// with no way to recover it.
var toonOptions = toonenc.Options{
	Indent:        toonenc.MinIndent,
	MinSavingsPct: toonenc.DefaultMinSavingsPct,
}

// Reformat re-encodes the TEXT content blocks of res according to f and
// reports the byte savings and whether anything changed.
//
// Scope of the rewrite, and the reasons for each boundary:
//
//   - Only "text" blocks are touched, and only those whose payload is a
//     single JSON document. Image and resource blocks are opaque.
//   - structuredContent is NEVER re-encoded. It is the machine-readable
//     channel: a client may parse it, and TOON does not round-trip.
//   - The contract marker (toonenc.HeaderLine) is emitted at most once per
//     result, on the first block that is actually re-encoded. It tells the
//     agent that arguments still travel as JSON — the "byte-level marker contract"
//     of docs/modules/dataplane.md.
//   - Per block, toonenc.Consider gives the never-larger guarantee: a block
//     that does not win keeps its original bytes. A result can therefore
//     only shrink.
//
// ORDERING INVARIANT: Reformat runs on the delivery path, i.e. AFTER the
// pipeline's defences have scanned the downstream text (docs/modules/security.md
// sequencing: leakguard and the injection scanner read the PRE-encoding
// text). Moving it earlier would hand the scanners a notation they were not
// written against.
//
// Failure direction is open: an undecodable content array, a block that is
// not JSON, an encoder error — all deliver the original.
func Reformat(res *mcp.CallResult, f Format) (*mcp.CallResult, Savings, bool) {
	baseline := resultBytes(res)
	unchanged := func() (*mcp.CallResult, Savings, bool) {
		return res, EstimateSavings(baseline, baseline), false
	}
	if res == nil || f != FormatTOON || len(res.Content) == 0 {
		return unchanged()
	}
	var blocks []json.RawMessage
	if err := json.Unmarshal(res.Content, &blocks); err != nil {
		return unchanged()
	}

	out := make([]json.RawMessage, len(blocks))
	changed := false
	headerPending := true
	for i, b := range blocks {
		out[i] = b
		text, ok := textOf(b)
		if !ok {
			continue
		}
		opts := toonOptions
		opts.Header = headerPending
		encoded, d := toonenc.Consider(text, opts)
		if !d.Applied {
			continue
		}
		out[i] = textBlock(encoded)
		changed = true
		headerPending = false
	}
	if !changed {
		return unchanged()
	}
	content, err := json.Marshal(out)
	if err != nil {
		// Re-encoding blocks that were just decoded cannot realistically
		// fail; deliver the original rather than an empty result.
		return unchanged()
	}
	next := &mcp.CallResult{
		Content:           content,
		StructuredContent: res.StructuredContent,
		IsError:           res.IsError,
	}
	actual := resultBytes(next)
	if actual >= baseline {
		// Never-larger, enforced a second time at result granularity: the
		// per-block guarantee does not by itself bound the re-marshalled
		// array (escaping differences, block reordering costs).
		return unchanged()
	}
	return next, EstimateSavings(baseline, actual), true
}

// textOf extracts the payload of an MCP text content block. A non-text block
// (or an unparsable one) reports false and is left untouched.
func textOf(raw json.RawMessage) (string, bool) {
	var probe struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if err := json.Unmarshal(raw, &probe); err != nil || probe.Type != "text" {
		return "", false
	}
	return probe.Text, true
}

// resultBytes is the delivered size of a result: content plus
// structuredContent, the same measure Shape budgets against.
func resultBytes(res *mcp.CallResult) int {
	if res == nil {
		return 0
	}
	return len(res.Content) + len(res.StructuredContent)
}

// Result is the full outcome of the M1.5 delivery path: the page an agent
// receives, the cursor for whatever was deferred, and ONE end-to-end savings
// record covering both re-encoding and truncation.
//
// It exists because Shape's three return values cannot express "nothing was
// truncated but the payload still got smaller" — the exact case TOON creates.
type Result struct {
	// Page is what the caller delivers. Never nil unless res was nil.
	Page *mcp.CallResult
	// Cursor describes the deferred remainder; zero when nothing was
	// deferred. Retain it BEFORE delivering Page.
	Cursor Cursor
	// Truncated reports that Cursor is live.
	Truncated bool
	// Reformatted reports that the encoding changed.
	Reformatted bool
	// Savings compares the ORIGINAL result against Page: one number for the
	// whole stack, in the same units as EstimateSavings everywhere else.
	Savings Savings
}

// ShapeResult is the format-aware delivery path: re-encode, then bound.
//
// Order is load-bearing. Re-encoding first means the budget is spent on the
// CHEAPER representation, so a result that fits after TOON is delivered whole
// instead of being paginated; and the retained remainder is the text the
// agent actually saw, so a fetch_result page continues in the same notation
// rather than switching mid-stream.
//
// The recovery trailer stays LAST regardless: it is appended by the
// truncation step, which runs after re-encoding by construction.
func ShapeResult(res *mcp.CallResult, budget Budget, opts Options) Result {
	baseline := resultBytes(res)
	src, _, reformatted := Reformat(res, opts.Format)
	page, cursor, truncated := shape(src, budget, opts)
	return Result{
		Page:        page,
		Cursor:      cursor,
		Truncated:   truncated,
		Reformatted: reformatted,
		Savings:     EstimateSavings(baseline, resultBytes(page)),
	}
}
