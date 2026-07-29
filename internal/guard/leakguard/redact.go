package leakguard

import (
	"strconv"
	"strings"
)

// Label is the text an inline-redacted span is replaced with. Its shape is
// contract (docs/modules/security.md): the agent sees WHAT was removed, never a byte of
// it, and can reason about the gap instead of retrying the call.
func Label(ruleID string) string { return "[REDACTED:" + ruleID + "]" }

// preview builds a Finding's evidence string from the rule ID and the length
// ALONE.
//
// This is the package's central non-negotiable: an evidence field is rendered
// into terminals, GUIs, logs and audit records, so if it could carry matched
// bytes then every one of those surfaces would become a place secrets land.
// Because preview is a pure function of (ruleID, n), no code path — not a new
// rule, not a validator, not a caller — can leak content through a Finding.
// A golden test pins the format; a property test pins the invariant.
func preview(ruleID string, n int) string {
	return Label(ruleID) + "(" + strconv.Itoa(n) + "B)"
}

// redactable reports whether f may be rewritten inline at the given severity
// floor. RedactNone findings are excluded unconditionally: a low-confidence
// signal must not mangle a result no matter how the policy is set.
func (f Finding) redactable(min Severity) bool {
	return f.Redaction != RedactNone && f.Severity >= min
}

// Redact rewrites content, replacing the span of every eligible finding with
// Label(rule), and reports how many spans it replaced.
//
// Precondition: findings come from Scan(content) — the spans are byte offsets
// into THAT string, ascending and non-overlapping (Scan guarantees both).
// Spans that violate the precondition are skipped rather than trusted: a
// mismatched offset would otherwise slice a rune in half or panic, and a
// guard that panics on a hostile payload is a denial-of-service vector.
func Redact(content string, findings []Finding, min Severity) (string, int) {
	var b strings.Builder
	last, n := 0, 0
	for _, f := range findings {
		if !f.redactable(min) {
			continue
		}
		if f.Start < last || f.End > len(content) || f.Start >= f.End {
			continue // out of order, out of range, or empty: not ours to trust
		}
		if n == 0 {
			b.Grow(len(content))
		}
		b.WriteString(content[last:f.Start])
		b.WriteString(Label(f.Rule))
		last = f.End
		n++
	}
	if n == 0 {
		return content, 0
	}
	b.WriteString(content[last:])
	return b.String(), n
}
