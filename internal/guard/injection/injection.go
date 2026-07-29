// Package injection detects prompt-injection payloads in downstream tool
// results (docs/flows.md / A.2).
//
// Pipeline position: the defend_and_shape stage calls the single entry point
// ScanResult for BOTH the success branch and the error branch of every tool
// call (#421 — a hostile server must not dodge scanning by answering with a
// JSON-RPC error). Scan is the lower-level per-content primitive.
//
// Failure direction: detection is a heuristic and FAILS OPEN — content the
// scanner cannot decode, or that evades the rule tables, passes through
// unlabeled. The scanner never errors at scan time; enforcement strength is
// chosen by Policy (label by default, block opt-in), and per-server
// exemptions are explicit configuration, never inferred.
//
// Dependency constraint (canonical.md §2 rule 4, depguard-enforced): this
// package imports only the standard library.
package injection

import (
	"encoding/base64"
	"regexp"
	"slices"
	"strings"
	"unicode"
	"unicode/utf8"
)

// Window labels findings carry (SOU-333 head/tail dual-window scanning).
const (
	WindowFull = "full"
	WindowHead = "head"
	WindowTail = "tail"
)

// Finding is one rule hit.
type Finding struct {
	// Rule is the ID of the matching rule.
	Rule     string   `json:"rule"`
	Severity Severity `json:"severity"`
	// Start/End are byte offsets into the original (segment) content. For
	// findings inside base64 payloads (Depth > 0) the span is that of the
	// outermost enclosing base64 blob in the original content.
	Start int `json:"start"`
	End   int `json:"end"`
	// Depth is the base64 nesting depth the match was found at (0 = surface).
	Depth int `json:"depth,omitempty"`
	// Window records which scan window produced the finding.
	Window string `json:"window"`
	// Segment is the index of the segment passed to ScanResult (0 for Scan).
	Segment int `json:"segment,omitempty"`
	// Excerpt is a short normalized-text excerpt of the match, for labels
	// and audit. Capped; never contains the full content.
	Excerpt string `json:"excerpt,omitempty"`
}

// Config configures a Scanner.
type Config struct {
	// Rules replaces the detection table; nil means DefaultRules().
	Rules []Rule
	// WindowBytes is the head/tail window size for large content (SOU-333):
	// content longer than 2*WindowBytes is scanned only in its first and
	// last WindowBytes. 0 means the 32 KiB default; negative disables
	// windowing (always full scan).
	WindowBytes int
	// MaxBase64Depth bounds nested base64 decoding. 0 means the default (3);
	// negative disables base64 scanning entirely.
	MaxBase64Depth int
}

const (
	defaultWindowBytes    = 32 * 1024
	defaultMaxBase64Depth = 3
	excerptMaxLen         = 80
	// minBase64Len is the shortest candidate blob considered (18 decoded
	// bytes) — shorter runs are overwhelmingly ordinary text.
	minBase64Len = 24
)

// Scanner holds the compiled rule tables. It is immutable after construction
// and safe for concurrent use.
type Scanner struct {
	rules    []compiledRule
	window   int
	maxDepth int
}

// New builds a Scanner from cfg. It fails only on invalid configuration
// (unparsable rule), never at scan time.
func New(cfg Config) (*Scanner, error) {
	rules := cfg.Rules
	if rules == nil {
		rules = DefaultRules()
	}
	compiled, err := compileRules(rules)
	if err != nil {
		return nil, err
	}
	window := cfg.WindowBytes
	if window == 0 {
		window = defaultWindowBytes
	}
	depth := cfg.MaxBase64Depth
	switch {
	case depth == 0:
		depth = defaultMaxBase64Depth
	case depth < 0:
		depth = 0 // disables: scanBase64 requires depth <= maxDepth starting at 1
	}
	return &Scanner{rules: compiled, window: window, maxDepth: depth}, nil
}

// NewDefault returns a Scanner over the built-in table. It panics only if the
// built-in table fails to compile, which is covered by tests.
func NewDefault() *Scanner {
	s, err := New(Config{})
	if err != nil {
		panic(err)
	}
	return s
}

// Scan scans one piece of content and returns the findings, sorted by span
// then rule, deduplicated. Content larger than twice the configured window is
// scanned head and tail only (SOU-333); the untouched middle is a documented
// fail-open trade-off, bounded-work over completeness.
func (s *Scanner) Scan(content string) []Finding {
	var fs []Finding
	if s.window < 0 || len(content) <= 2*s.window {
		fs = s.scanChunk(content, 0, WindowFull)
	} else {
		headEnd := runeBoundary(content, s.window)
		tailStart := runeBoundary(content, len(content)-s.window)
		fs = s.scanChunk(content[:headEnd], 0, WindowHead)
		fs = append(fs, s.scanChunk(content[tailStart:], tailStart, WindowTail)...)
	}
	return dedupSort(fs)
}

// scanChunk scans one window: rule matching on the normalized form, then
// base64 blob discovery on the raw form (base64 is case-sensitive, so it runs
// before normalization).
func (s *Scanner) scanChunk(chunk string, base int, window string) []Finding {
	n := normalizeContent(chunk)
	out := s.matchRules(n, func(ns, ne int) (int, int) {
		return base + n.offs[ns], base + n.offs[ne]
	}, 0, window)
	out = append(out, s.scanBase64(chunk, base, 1, window, nil)...)
	return out
}

// matchRules runs every rule against n.text; span maps a normalized match
// [ns,ne) to the original span recorded on the finding.
func (s *Scanner) matchRules(n normText, span func(ns, ne int) (int, int), depth int, window string) []Finding {
	var out []Finding
	emit := func(r *compiledRule, ns, ne int) {
		start, end := span(ns, ne)
		out = append(out, Finding{
			Rule:     r.ID,
			Severity: r.Severity,
			Start:    start,
			End:      end,
			Depth:    depth,
			Window:   window,
			Excerpt:  excerpt(n.text[ns:ne]),
		})
	}
	for i := range s.rules {
		r := &s.rules[i]
		if r.re != nil {
			for _, m := range r.re.FindAllStringIndex(n.text, -1) {
				emit(r, m[0], m[1])
			}
			continue
		}
		for from := 0; ; {
			idx := strings.Index(n.text[from:], r.phrase)
			if idx < 0 {
				break
			}
			start := from + idx
			emit(r, start, start+len(r.phrase))
			from = start + len(r.phrase)
		}
	}
	return out
}

// base64Pattern matches candidate blobs in the std or URL-safe alphabets.
var base64Pattern = regexp.MustCompile(`[A-Za-z0-9+/_-]{24,}={0,2}`)

// scanBase64 finds base64-looking blobs in text, decodes those that yield
// mostly-printable UTF-8 and rescans the plaintext, recursing up to the
// configured depth. anchor, when non-nil, is the original span of the
// outermost blob — every nested finding is anchored there. Decode failures
// are treated as "not base64" and skipped (fail-open by design: this is a
// detector, not a validator).
func (s *Scanner) scanBase64(text string, base, depth int, window string, anchor *[2]int) []Finding {
	if depth > s.maxDepth {
		return nil
	}
	var out []Finding
	for _, loc := range base64Pattern.FindAllStringIndex(text, -1) {
		if loc[1]-loc[0] < minBase64Len {
			continue
		}
		decoded, ok := tryDecodeBase64(text[loc[0]:loc[1]])
		if !ok || !mostlyText(decoded) {
			continue
		}
		span := anchor
		if span == nil {
			span = &[2]int{base + loc[0], base + loc[1]}
		}
		plain := string(decoded)
		n := normalizeContent(plain)
		out = append(out, s.matchRules(n, func(_, _ int) (int, int) {
			return span[0], span[1]
		}, depth, window)...)
		out = append(out, s.scanBase64(plain, 0, depth+1, window, span)...)
	}
	return out
}

// tryDecodeBase64 attempts std (padded then raw) or URL-safe decoding.
// Mixed-alphabet candidates are rejected.
func tryDecodeBase64(blob string) ([]byte, bool) {
	if strings.ContainsAny(blob, "-_") {
		if strings.ContainsAny(blob, "+/") {
			return nil, false
		}
		b, err := base64.RawURLEncoding.DecodeString(strings.TrimRight(blob, "="))
		return b, err == nil
	}
	if b, err := base64.StdEncoding.DecodeString(blob); err == nil {
		return b, true
	}
	b, err := base64.RawStdEncoding.DecodeString(strings.TrimRight(blob, "="))
	return b, err == nil
}

// mostlyText reports whether decoded bytes look like human-readable text
// (valid UTF-8, >= 90% printable-or-space runes). Random bytes — hashes, hex
// tokens that happen to decode — fail this and are skipped.
func mostlyText(b []byte) bool {
	if len(b) == 0 || !utf8.Valid(b) {
		return false
	}
	total, bad := 0, 0
	for _, r := range string(b) {
		total++
		if !unicode.IsPrint(r) && !unicode.IsSpace(r) {
			bad++
		}
	}
	return bad*10 <= total
}

func excerpt(s string) string {
	if len(s) <= excerptMaxLen {
		return s
	}
	return s[:runeBoundary(s, excerptMaxLen-1)] + "…"
}

// dedupSort orders findings by (segment, start, end, depth, rule) and drops
// exact duplicates so output is deterministic (golden-testable).
func dedupSort(fs []Finding) []Finding {
	slices.SortFunc(fs, compareFindings)
	return slices.CompactFunc(fs, func(a, b Finding) bool { return compareFindings(a, b) == 0 })
}

func compareFindings(a, b Finding) int {
	if c := a.Segment - b.Segment; c != 0 {
		return c
	}
	if c := a.Start - b.Start; c != 0 {
		return c
	}
	if c := a.End - b.End; c != 0 {
		return c
	}
	if c := a.Depth - b.Depth; c != 0 {
		return c
	}
	return strings.Compare(a.Rule, b.Rule)
}
