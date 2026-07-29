package toonenc

import "strings"

// Reason is the stable, machine-readable outcome of Consider. The values are
// audit fields and golden-tested strings, not prose.
type Reason string

const (
	// ReasonApplied: the TOON form was accepted.
	ReasonApplied Reason = "applied"
	// ReasonNotJSON: the input is not a single JSON document, so there is
	// nothing structured to re-encode.
	ReasonNotJSON Reason = "not_json"
	// ReasonNoGain: the TOON form did not beat the baseline by
	// MinSavingsPct. This is the never-larger guarantee firing.
	ReasonNoGain Reason = "no_gain"
	// ReasonEmpty: the input is blank.
	ReasonEmpty Reason = "empty"
)

// Decision records what Consider did and what it cost, in BYTES. Token
// estimation lives in internal/shaping (which imports this package, so the
// estimator cannot travel down here without a cycle) and both layers use the
// same divisor — the numbers are directly comparable with
// shaping.EstimateSavings.
type Decision struct {
	// Applied reports whether the returned text is the TOON form.
	Applied bool
	// Reason is why. Always set, including when Applied is true.
	Reason Reason
	// BaselineBytes is the input length; ActualBytes the returned length.
	// When Applied is false the two are equal by construction.
	BaselineBytes int
	ActualBytes   int
	// Truncated reports that the budget cut the document short. A truncated
	// document is still Applied — it carries TruncationMarker and is honest
	// about what it dropped.
	Truncated bool
}

// SavedBytes is BaselineBytes-ActualBytes, floored at zero.
func (d Decision) SavedBytes() int {
	if d.BaselineBytes > d.ActualBytes {
		return d.BaselineBytes - d.ActualBytes
	}
	return 0
}

// Consider is the adaptive entry point of docs/modules/dataplane.md: re-encode a JSON
// text as TOON, but only hand it back if it actually wins.
//
// The never-larger guarantee is constructive, not aspirational: the returned
// string is either the TOON form (strictly smaller than the baseline by at
// least Options.MinSavingsPct percent) or the input verbatim. A caller may
// therefore always use the returned string with no size check of its own.
//
// Every failure direction returns the input unchanged. Re-encoding is an
// economy mechanism; garbling a tool result to save tokens would be a far
// worse outcome than spending them (the same fail-open rule the parent
// package states in doc.go).
func Consider(jsonText string, opts Options) (string, Decision) {
	base := len(jsonText)
	d := Decision{BaselineBytes: base, ActualBytes: base}

	if strings.TrimSpace(jsonText) == "" {
		d.Reason = ReasonEmpty
		return jsonText, d
	}
	out, truncated, err := encodeJSON([]byte(jsonText), opts)
	if err != nil {
		d.Reason = ReasonNotJSON
		return jsonText, d
	}
	if !beats(len(out), base, opts.minSavingsPct()) {
		d.Reason = ReasonNoGain
		return jsonText, d
	}
	d.Applied = true
	d.Reason = ReasonApplied
	d.ActualBytes = len(out)
	d.Truncated = truncated
	return out, d
}

// beats reports whether actual is smaller than baseline by at least pct
// percent. Integer arithmetic only: a float comparison here would make the
// accept/reject boundary depend on rounding, and that boundary is golden-
// tested.
func beats(actual, baseline, pct int) bool {
	if baseline <= 0 || actual >= baseline {
		return false
	}
	return (baseline-actual)*100 >= baseline*pct
}
