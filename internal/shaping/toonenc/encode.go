package toonenc

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strconv"
	"strings"
	"unicode"
)

// HeaderLine is the in-band contract marker of docs/subsystems/shaping.md: it tells the
// agent that what follows is a display encoding and that arguments still
// travel as JSON. Frozen bytes — agent-side prompting keys off it, so a
// wording change is an ABI change.
const HeaderLine = "#toon/1 (display encoding; send tool arguments as JSON)"

// TruncationMarker is the frozen last line of a budget-truncated document.
// Arguments: delivered lines, total lines.
const TruncationMarker = "…truncated by agenthub: %d of %d lines"

// Grammar limits. They bound the shapes the encoder will produce, not the
// input it accepts: an input outside a limit falls back to a plainer form
// rather than erroring.
const (
	// MinIndent is both the minimum and the default indent width. Two is
	// not cosmetic: "- " occupies exactly one level, which is what lets a
	// nested list element reuse its block's first line.
	MinIndent = 2

	// MinTableRows is the shortest list worth a table header. At one row
	// the header costs more than the "- key: value" block it replaces.
	MinTableRows = 2

	// MaxTableCols caps table width. A wider homogeneous list degrades to
	// list-of-blocks, which stays readable; a 200-column row does not.
	MaxTableCols = 32

	// MaxDepth bounds nesting. Beyond it a value is rendered as its compact
	// JSON on one line — hostile input must not be able to drive unbounded
	// recursion, and a pathologically deep document was never going to be
	// read anyway.
	MaxDepth = 12
)

// Options configures one encode. The zero value is valid: 2-space indent,
// no header, no budget.
type Options struct {
	// Indent is spaces per nesting level; anything below MinIndent is
	// raised to MinIndent.
	Indent int
	// Budget bounds the encoded document in bytes. 0 means unbounded.
	// Truncation lands on a line boundary and appends TruncationMarker.
	Budget int
	// Header emits HeaderLine as the first line. It counts against Budget.
	Header bool
	// MinSavingsPct is the percentage by which the encoding must beat the
	// JSON baseline for Consider to apply it. 0 means DefaultMinSavingsPct.
	// A negative value means "apply whenever it is not larger".
	MinSavingsPct int
}

// DefaultMinSavingsPct is the gain Consider demands before it swaps an
// agent's JSON for TOON. A 1% win is not worth teaching a model a second
// notation mid-conversation; 10% is.
const DefaultMinSavingsPct = 10

func (o Options) indent() int {
	if o.Indent < MinIndent {
		return MinIndent
	}
	return o.Indent
}

func (o Options) minSavingsPct() int {
	if o.MinSavingsPct == 0 {
		return DefaultMinSavingsPct
	}
	if o.MinSavingsPct < 0 {
		return 0
	}
	return o.MinSavingsPct
}

// ErrUnencodable reports a value that cannot be projected — in practice only
// a value encoding/json itself refuses (a channel, a NaN, a cyclic pointer
// graph). It is never returned for a value that arrived as JSON.
var ErrUnencodable = errors.New("toonenc: value cannot be encoded")

// Encode projects v into TOON.
//
// v is normalised through encoding/json first, so any Go value with a JSON
// form encodes, and json.Number / large integers keep their exact literal
// text (the decode side uses UseNumber). The cost of that round trip is
// deliberate: it makes ONE code path — the JSON value model — responsible
// for what the grammar has to cover.
func Encode(v any, opts Options) (string, error) {
	raw, err := json.Marshal(v)
	if err != nil {
		return "", fmt.Errorf("%w: %v", ErrUnencodable, err)
	}
	return EncodeJSON(raw, opts)
}

// EncodeJSON projects an already-encoded JSON document into TOON. Invalid
// JSON is an error, never a partial document.
func EncodeJSON(raw []byte, opts Options) (string, error) {
	out, _, err := encodeJSON(raw, opts)
	return out, err
}

// encodeJSON is EncodeJSON plus the budget verdict, which Consider records
// but callers of the plain API can read off TruncationMarker themselves.
func encodeJSON(raw []byte, opts Options) (string, bool, error) {
	v, err := decodeJSON(raw)
	if err != nil {
		return "", false, err
	}
	out, truncated := encodeValue(v, opts)
	return out, truncated, nil
}

// decodeJSON decodes with UseNumber so no number ever passes through
// float64. An int64 above 2^53 would otherwise come back rounded and be
// emitted as a different number than the server sent — silent corruption is
// the one failure this package must not have.
func decodeJSON(raw []byte) (any, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return nil, fmt.Errorf("toonenc: input is not valid JSON: %w", err)
	}
	// Trailing content means the input was not one document; refuse rather
	// than silently encode the prefix.
	if dec.More() {
		return nil, errors.New("toonenc: input holds more than one JSON document")
	}
	return v, nil
}

// encodeValue renders a decoded JSON value and applies the header and the
// budget. Line assembly happens first and truncation second, so the budget
// can never split a row or a quoted string.
func encodeValue(v any, opts Options) (string, bool) {
	w := &writer{indent: opts.indent()}
	w.block(v, 0)
	lines := w.lines
	if opts.Header {
		lines = append([]string{HeaderLine}, lines...)
	}
	return joinBudgeted(lines, opts.Budget)
}

// joinBudgeted joins lines with "\n" and, when budget is exceeded, keeps the
// longest line prefix that fits together with the truncation marker.
//
// Failure direction: if not even one line plus the marker fits, the marker
// alone is returned — a document that claims to be complete while being cut
// would be worse than an empty one.
func joinBudgeted(lines []string, budget int) (string, bool) {
	full := strings.Join(lines, "\n")
	if budget <= 0 || len(full) <= budget {
		return full, false
	}
	total := len(lines)
	for keep := total - 1; keep > 0; keep-- {
		marker := fmt.Sprintf(TruncationMarker, keep, total)
		out := strings.Join(lines[:keep], "\n") + "\n" + marker
		if len(out) <= budget {
			return out, true
		}
	}
	return fmt.Sprintf(TruncationMarker, 0, total), true
}

// writer accumulates the document line by line. Building lines (rather than
// streaming bytes) is what makes the budget rule expressible as "drop whole
// lines", which is in turn what keeps a truncated table readable.
type writer struct {
	lines  []string
	indent int
}

func (w *writer) pad(depth int) string { return strings.Repeat(" ", depth*w.indent) }

func (w *writer) push(depth int, s string) { w.lines = append(w.lines, w.pad(depth)+s) }

// block writes v at depth in block position (its own line, or lines).
func (w *writer) block(v any, depth int) {
	switch t := v.(type) {
	case map[string]any:
		if len(t) == 0 {
			w.push(depth, "{}")
			return
		}
		if depth >= MaxDepth {
			w.push(depth, compactJSON(v))
			return
		}
		for _, k := range sortedKeys(t) {
			w.member(k, t[k], depth)
		}
	case []any:
		if len(t) == 0 {
			w.push(depth, "[]")
			return
		}
		if depth >= MaxDepth {
			w.push(depth, compactJSON(v))
			return
		}
		if cols, ok := tableColumns(t); ok {
			w.push(depth, tableHeader("", len(t), cols))
			w.rows(t, cols, depth+1)
			return
		}
		w.list(t, depth)
	default:
		w.push(depth, scalar(v))
	}
}

// member writes one "key: value" entry of an object.
func (w *writer) member(k string, v any, depth int) {
	key := quoteKey(k)
	switch t := v.(type) {
	case map[string]any:
		if len(t) == 0 {
			w.push(depth, key+": {}")
			return
		}
		w.push(depth, key+":")
		w.block(v, depth+1)
	case []any:
		if len(t) == 0 {
			w.push(depth, key+": []")
			return
		}
		if cols, ok := tableColumns(t); ok {
			w.push(depth, tableHeader(key, len(t), cols))
			w.rows(t, cols, depth+1)
			return
		}
		w.push(depth, key+":")
		w.list(t, depth+1)
	default:
		w.push(depth, key+": "+scalar(v))
	}
}

// list writes a non-tabular array, one "- " entry per element.
func (w *writer) list(items []any, depth int) {
	for _, it := range items {
		switch it.(type) {
		case map[string]any, []any:
			mark := len(w.lines)
			w.block(it, depth+1)
			// Reuse the element's first line: its indent is exactly one
			// level deeper, and MinIndent >= 2 guarantees room for "- ".
			if mark < len(w.lines) {
				w.lines[mark] = w.pad(depth) + "- " + strings.TrimLeft(w.lines[mark], " ")
			}
		default:
			w.push(depth, "- "+scalar(it))
		}
	}
}

// rows writes the value rows of a table in column order.
func (w *writer) rows(items []any, cols []string, depth int) {
	var b strings.Builder
	for _, it := range items {
		obj := it.(map[string]any) // guaranteed by tableColumns
		b.Reset()
		for i, c := range cols {
			if i > 0 {
				b.WriteByte(',')
			}
			b.WriteString(field(obj[c]))
		}
		w.push(depth, b.String())
	}
}

// tableHeader renders "key[N]{c1,c2}:" (or "[N]{c1,c2}:" at the root).
func tableHeader(key string, n int, cols []string) string {
	var b strings.Builder
	b.WriteString(key)
	b.WriteByte('[')
	b.WriteString(strconv.Itoa(n))
	b.WriteString("]{")
	for i, c := range cols {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteString(quoteKey(c))
	}
	b.WriteString("}:")
	return b.String()
}

// tableColumns decides whether items is a homogeneous scalar-valued object
// array and returns its sorted column list. The predicate is deliberately
// strict — a table whose rows do not all mean the same thing is worse than
// no table — and every rejection has a plainer fallback.
func tableColumns(items []any) ([]string, bool) {
	if len(items) < MinTableRows {
		return nil, false
	}
	first, ok := items[0].(map[string]any)
	if !ok || len(first) == 0 || len(first) > MaxTableCols {
		return nil, false
	}
	cols := sortedKeys(first)
	for _, it := range items {
		obj, ok := it.(map[string]any)
		if !ok || len(obj) != len(cols) {
			return nil, false
		}
		for _, c := range cols {
			v, present := obj[c]
			if !present || !isScalar(v) {
				return nil, false
			}
		}
	}
	return cols, true
}

func isScalar(v any) bool {
	switch v.(type) {
	case map[string]any, []any:
		return false
	default:
		return true
	}
}

func sortedKeys(m map[string]any) []string {
	return slices.Sorted(maps.Keys(m))
}

// scalar renders a scalar in value position.
func scalar(v any) string {
	switch t := v.(type) {
	case nil:
		return "null"
	case bool:
		if t {
			return "true"
		}
		return "false"
	case json.Number:
		return t.String()
	case string:
		return quoteValue(t)
	default:
		// Unreachable for JSON-decoded input; stay well-formed rather than
		// panic if a caller hands in a value from elsewhere.
		return compactJSON(v)
	}
}

// field renders a scalar in table-cell position. Identical to scalar today;
// it exists so a future cell-specific rule (alignment, empty-cell marker)
// has one place to land instead of being sprinkled through rows.
func field(v any) string { return scalar(v) }

// compactJSON is the last-resort rendering for a value the grammar does not
// cover (past MaxDepth, or a non-JSON Go value). It is always valid JSON, so
// the document degrades to "readable" rather than "wrong".
func compactJSON(v any) string {
	raw, err := json.Marshal(v)
	if err != nil {
		return "null"
	}
	var buf bytes.Buffer
	if err := json.Compact(&buf, raw); err != nil {
		return string(raw)
	}
	return buf.String()
}

// quoteValue quotes a string only when a bare form could be misread; see the
// package doc for the full rule. Quoting uses strconv.Quote, which keeps
// printable non-ASCII intact (unlike encoding/json, which would also escape
// < > & and cost bytes for nothing).
func quoteValue(s string) string {
	if needsQuote(s) {
		return strconv.Quote(s)
	}
	return s
}

// quoteKey is quoteValue plus "quote on any interior whitespace": a bare key
// with a space in it would be ambiguous against the "- " list marker and
// against a reader's word boundaries, while a value with spaces is just
// prose.
func quoteKey(s string) string {
	if s == "" || needsQuote(s) || strings.ContainsFunc(s, unicode.IsSpace) {
		return strconv.Quote(s)
	}
	return s
}

// needsQuote is the frozen predicate of the package doc. Order of the checks
// is irrelevant to the result (they are all disjunctions) but the SET is
// contract: adding a character here changes golden bytes.
func needsQuote(s string) bool {
	if s == "" {
		return true
	}
	if strings.TrimSpace(s) != s {
		return true // leading or trailing whitespace would be lost
	}
	if looksLikeLiteral(s) {
		return true // would read back as a number, bool or null
	}
	switch s[0] {
	case '[', '{', '#', '"':
		return true
	case '-':
		if len(s) == 1 || s[1] == ' ' {
			return true // indistinguishable from a list marker
		}
	}
	for _, r := range s {
		switch r {
		case ',', ':', '"', '\\', '\n', '\r', '\t':
			return true
		}
		if r < 0x20 || r == 0x7f {
			return true
		}
	}
	return false
}

// looksLikeLiteral reports whether a bare s would be read as a non-string.
// It parses rather than pattern-matches: "1e999", "0x10" and "007" must be
// classified the same way a reader would classify them, and json.Number's
// own grammar is the only definition that stays in step with the decoder.
func looksLikeLiteral(s string) bool {
	switch s {
	case "true", "false", "null":
		return true
	}
	if _, err := strconv.ParseFloat(s, 64); err == nil {
		return true
	}
	return false
}
