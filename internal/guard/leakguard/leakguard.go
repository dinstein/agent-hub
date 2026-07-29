// Package leakguard detects sensitive data leaving through downstream tool
// results (docs/modules/security.md). Where guard/injection defends the way IN (poisoned
// instructions entering the agent context), leakguard defends the way OUT:
// credentials, private keys and personal data flowing from a tool result into
// the agent — and from there into whatever the agent does next.
//
// Two dispositions, per ruling #17:
//
//   - AUDIT (default on): the scan runs off the call path and only a
//     redacted record — rule ID, severity, position, length — reaches the
//     audit stream. Never the matched content. Zero call latency.
//   - INLINE (default off, explicit configuration): the scan runs on the
//     call path and every eligible span is replaced by [REDACTED:<ruleID>]
//     before the result reaches the agent. Rewriting a result has semantic
//     risk, which is why it must be chosen, never inherited.
//
// Confidence is the organising principle of the rule table. High-confidence
// rules key on a credential's own structure (a PEM header, a `ghp_` prefix, a
// decodable JWT header) and may be redacted inline; the entropy heuristic is
// a LOW-confidence signal carrying its own severity, and its Redaction is
// RedactNone so it can never rewrite a result no matter how the policy is
// configured.
//
// Failure direction: detection FAILS OPEN. Content outside the scan windows,
// secrets that evade the table, and secrets hidden inside encodings we do not
// decode all pass through unreported; scanning never errors and never blocks
// a call. The closed directions are narrow and deliberate: a preview never
// contains the matched value, an audit record never contains content, and
// (in the pipeline) a payload that cannot be parsed for rewriting is withheld
// rather than delivered unredacted.
//
// Determinism is contract: findings are sorted by (segment, start, end, rule),
// overlapping findings collapse to the highest severity by a fixed rule, and
// previews are pure functions of (rule ID, length) — all golden-tested.
//
// Dependency constraint (canonical.md §2 rule 4, depguard-enforced): only the
// standard library plus internal/guard.
package leakguard

import (
	"slices"
	"strings"
	"unicode/utf8"
)

// Window labels findings carry (head/tail dual-window scanning, mirroring
// guard/injection so audit records of the two scanners read the same).
const (
	WindowFull = "full"
	WindowHead = "head"
	WindowTail = "tail"
)

// Finding is one detection. It carries NO matched content: Preview is a pure
// function of (Rule, Length) and Length is the only content-derived number.
type Finding struct {
	// Rule is the ID of the matching rule (stable, golden-tested; treat as
	// ABI once emitted into audit records).
	Rule     string   `json:"rule"`
	Severity Severity `json:"severity"`
	// Redaction is the rule's disposition strategy. RedactNone findings are
	// audit-only signals and are never rewritten inline.
	Redaction Redaction `json:"redaction"`
	// Start/End are byte offsets into the scanned content bounding the span
	// that inline redaction would replace: the secret sub-match for
	// RedactSecret rules, the whole match otherwise.
	Start int `json:"start"`
	End   int `json:"end"`
	// Length is End-Start, carried explicitly because audit records keep the
	// length after the offsets have lost their meaning.
	Length int `json:"length"`
	// Window records which scan window produced the finding.
	Window string `json:"window"`
	// Segment is the index of the segment passed to ScanResult (0 for Scan).
	Segment int `json:"segment,omitempty"`
	// Preview is the redacted evidence string, "[REDACTED:<rule>](<n>B)".
	// It is derived from the rule ID and the length ALONE — no byte of the
	// match is ever copied into it (see redact.go).
	Preview string `json:"preview"`
	// Entropy is the Shannon entropy in bits/char of an entropy-rule match,
	// rounded to two decimals. Zero for every other rule.
	Entropy float64 `json:"entropy,omitempty"`

	// fullStart/fullEnd bound the WHOLE match, which for a RedactSecret rule
	// is wider than [Start,End). Overlap resolution runs on the full span
	// (see resolveOverlaps): two rules that describe the same text must
	// collide even when their redaction spans are disjoint sub-parts of it.
	// Unexported: this is scanner bookkeeping, not part of the record.
	fullStart, fullEnd int
}

// Config configures a Scanner. The zero value is the built-in table with all
// defaults.
type Config struct {
	// Rules replaces the detection table; nil means DefaultRules().
	Rules []Rule
	// WindowBytes is the head/tail window size for large content: content
	// longer than 2*WindowBytes is scanned only in its first and last
	// WindowBytes (docs/modules/security.md bounds the scan; the same dual-window shape
	// as guard/injection). 0 means the 32 KiB default; negative disables
	// windowing (always full scan).
	WindowBytes int
	// MaxFindings caps the findings ONE Scan reports (docs/modules/security.md: 50 per
	// call). 0 means the default; negative means unlimited.
	MaxFindings int
	// EntropyMinLen is the shortest token the entropy heuristic considers.
	// 0 means the default (32); negative disables entropy scanning. Values
	// below 16 have no effect — the candidate pattern floors token length
	// there, and below ~23 characters the default threshold is unreachable
	// anyway (log2 of the length caps the entropy).
	EntropyMinLen int
	// EntropyThreshold is the Shannon entropy (bits/char) a token must reach.
	// 0 means the default (4.5, docs/modules/security.md).
	EntropyThreshold float64
}

const (
	defaultWindowBytes      = 32 * 1024
	defaultMaxFindings      = 50
	defaultEntropyMinLen    = 32
	defaultEntropyThreshold = 4.5
	// maxEntropyTokenLen bounds one entropy candidate so a megabyte-long
	// base64 blob costs one bounded window, not a quadratic scan.
	maxEntropyTokenLen = 512
	// perRuleMatchFactor bounds how many raw matches one rule may contribute
	// before capping, relative to MaxFindings. Overlap resolution and the
	// report cap shrink this further; the factor only keeps a pathological
	// input from allocating without bound.
	perRuleMatchFactor = 4
)

// Scanner holds the compiled rule table. It is immutable after construction
// and safe for concurrent use.
type Scanner struct {
	rules       []compiledRule
	window      int
	maxFindings int
	perRuleCap  int
	entropyMin  int
	entropyBits float64
}

// New builds a Scanner from cfg. It fails only on invalid configuration (an
// unparsable or malformed rule), never at scan time.
func New(cfg Config) (*Scanner, error) {
	rules := cfg.Rules
	if rules == nil {
		rules = DefaultRules()
	}
	compiled, err := compileRules(rules)
	if err != nil {
		return nil, err
	}
	s := &Scanner{
		rules:       compiled,
		window:      cfg.WindowBytes,
		maxFindings: cfg.MaxFindings,
		entropyMin:  cfg.EntropyMinLen,
		entropyBits: cfg.EntropyThreshold,
	}
	if s.window == 0 {
		s.window = defaultWindowBytes
	}
	switch {
	case s.maxFindings == 0:
		s.maxFindings = defaultMaxFindings
	case s.maxFindings < 0:
		s.maxFindings = 0 // unlimited; capping is skipped below
	}
	if s.entropyMin == 0 {
		s.entropyMin = defaultEntropyMinLen
	}
	if s.entropyBits == 0 {
		s.entropyBits = defaultEntropyThreshold
	}
	s.perRuleCap = -1
	if s.maxFindings > 0 {
		s.perRuleCap = perRuleMatchFactor * s.maxFindings
	}
	return s, nil
}

// NewDefault returns a Scanner over the built-in table. It panics only if
// that table fails to compile, which a test pins.
func NewDefault() *Scanner {
	s, err := New(Config{})
	if err != nil {
		panic(err)
	}
	return s
}

// Scan scans one piece of content and returns the findings, sorted by span
// then rule, overlaps resolved, capped at MaxFindings.
//
// Content larger than twice the configured window is scanned head and tail
// only; the untouched middle is a documented fail-open trade-off — bounded
// work over completeness — and is why inline redaction is a mitigation, not
// a guarantee.
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
	fs = resolveOverlaps(fs)
	if s.maxFindings > 0 && len(fs) > s.maxFindings {
		fs = fs[:s.maxFindings]
	}
	return fs
}

// scanChunk runs every rule plus the entropy pass over one window. Matching
// happens on the RAW text: unlike injection rules, secrets are case-sensitive
// and alphabet-sensitive, and normalising would destroy exactly the structure
// the high-confidence rules key on.
func (s *Scanner) scanChunk(chunk string, base int, window string) []Finding {
	var out []Finding
	for i := range s.rules {
		r := &s.rules[i]
		for _, m := range r.re.FindAllStringSubmatchIndex(chunk, s.perRuleCap) {
			if f, ok := evalMatch(r, chunk, m, base, window); ok {
				out = append(out, f)
			}
		}
	}
	return append(out, s.scanEntropy(chunk, base, window)...)
}

// evalMatch turns one regexp match into a Finding, or drops it when the
// rule's validator rejects it (the placeholder / checksum / decode gates that
// keep documentation samples out of the report).
func evalMatch(r *compiledRule, chunk string, m []int, base int, window string) (Finding, bool) {
	secStart, secEnd := m[0], m[1]
	if r.secretIdx > 0 && 2*r.secretIdx+1 < len(m) && m[2*r.secretIdx] >= 0 {
		secStart, secEnd = m[2*r.secretIdx], m[2*r.secretIdx+1]
	}
	mt := Match{Full: chunk[m[0]:m[1]], Secret: chunk[secStart:secEnd]}
	if len(r.groupNames) > 0 {
		mt.groups = make(map[string]string, len(r.groupNames))
		for name, idx := range r.groupNames {
			if 2*idx+1 < len(m) && m[2*idx] >= 0 {
				mt.groups[name] = chunk[m[2*idx]:m[2*idx+1]]
			}
		}
	}
	if r.Validate != nil && !r.Validate(mt) {
		return Finding{}, false
	}
	start, end := m[0], m[1]
	if r.Redaction == RedactSecret {
		start, end = secStart, secEnd
	}
	f := newFinding(r.ID, r.Severity, r.Redaction, base+start, base+end, window)
	f.fullStart, f.fullEnd = base+m[0], base+m[1]
	return f, true
}

// newFinding builds a Finding with its derived Length and Preview. It is the
// only constructor: no other path can produce a Finding whose Preview was not
// computed from (rule, length). The full span defaults to the redaction span;
// evalMatch widens it for RedactSecret rules.
func newFinding(rule string, sev Severity, red Redaction, start, end int, window string) Finding {
	n := end - start
	return Finding{
		Rule:      rule,
		Severity:  sev,
		Redaction: red,
		Start:     start,
		End:       end,
		Length:    n,
		Window:    window,
		Preview:   preview(rule, n),
		fullStart: start,
		fullEnd:   end,
	}
}

// resolveOverlaps keeps, among findings whose FULL spans overlap, the one
// with the highest severity (ties: the longer span, then the earlier start,
// then the rule ID). Two DISTINCT secrets never overlap in text, so an
// overlap always means two rules described the same bytes — the
// authorization-header rule and the bare-Bearer rule seeing one header, the
// entropy heuristic re-reporting a token a vendor rule already identified,
// the email/password rule reading the tail of a connection URL. Reporting all
// of them would double-count the leak in audit and make inline redaction's
// replacement order matter.
//
// The kept set is returned sorted by (segment, start, end, rule): the output
// order is contract (golden-tested), and because full spans are disjoint the
// redaction spans are too — which is exactly Redact's precondition.
func resolveOverlaps(fs []Finding) []Finding {
	if len(fs) < 2 {
		return fs
	}
	slices.SortFunc(fs, comparePriority)
	kept := make([]Finding, 0, len(fs))
	for _, f := range fs {
		overlapped := false
		for _, k := range kept {
			if f.fullStart < k.fullEnd && k.fullStart < f.fullEnd {
				overlapped = true
				break
			}
		}
		if !overlapped {
			kept = append(kept, f)
		}
	}
	slices.SortFunc(kept, compareFindings)
	return kept
}

// comparePriority orders findings by which one wins an overlap.
func comparePriority(a, b Finding) int {
	if c := int(b.Severity) - int(a.Severity); c != 0 {
		return c // higher severity first
	}
	if c := (b.fullEnd - b.fullStart) - (a.fullEnd - a.fullStart); c != 0 {
		return c // wider match first
	}
	if c := a.fullStart - b.fullStart; c != 0 {
		return c
	}
	return strings.Compare(a.Rule, b.Rule)
}

// compareFindings is the output order: (segment, start, end, rule).
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
	return strings.Compare(a.Rule, b.Rule)
}

// runeBoundary returns the smallest index >= i that starts a rune in s, so
// window slicing never splits a multi-byte rune.
func runeBoundary(s string, i int) int {
	for i < len(s) && !utf8.RuneStart(s[i]) {
		i++
	}
	return i
}
