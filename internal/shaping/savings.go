package shaping

// BytesPerToken is the token approximation used across the savings stack:
// four bytes per token, rounded up. It is deliberately crude — the number
// feeds an operator-facing "tokens saved" aggregate, not billing — and it
// is a constant rather than a tokenizer so the estimate stays deterministic
// and model-independent (an estimate that changed with a vendor's tokenizer
// would make historical savings records incomparable).
const BytesPerToken = 4

// SavingsMode is the audit.SavingsRecord.Mode value for result shaping.
// The caller writes the record; this package only supplies the numbers.
const SavingsMode = "shaping"

// Savings is the token-savings estimate for one shaped result. It maps
// field-for-field onto audit.SavingsRecord — this package deliberately does
// NOT import internal/audit (shaping is on the data path and must not drag
// the audit writer along); the caller copies the fields across.
type Savings struct {
	// BaselineBytes is what the unshaped result would have delivered.
	BaselineBytes int
	// ActualBytes is what the shaped page actually delivers, trailer
	// included — the trailer is a real cost and hiding it would inflate
	// the reported saving.
	ActualBytes int
	// BaselineTokens / ActualTokens are the byte counts through
	// EstimateTokens.
	BaselineTokens int64
	ActualTokens   int64
	// SavedTokens is BaselineTokens-ActualTokens, floored at zero: a
	// "saving" that shows the shaped page cost more is a bug in the caller,
	// not a negative saving to be aggregated.
	SavedTokens int64
}

// EstimateTokens converts bytes to an estimated token count (ceiling
// division by BytesPerToken). Negative input yields 0.
func EstimateTokens(bytes int) int64 {
	if bytes <= 0 {
		return 0
	}
	return int64((bytes + BytesPerToken - 1) / BytesPerToken)
}

// EstimateSavings builds the savings record for a baseline/actual byte pair.
func EstimateSavings(baselineBytes, actualBytes int) Savings {
	s := Savings{
		BaselineBytes:  baselineBytes,
		ActualBytes:    actualBytes,
		BaselineTokens: EstimateTokens(baselineBytes),
		ActualTokens:   EstimateTokens(actualBytes),
	}
	if s.BaselineTokens > s.ActualTokens {
		s.SavedTokens = s.BaselineTokens - s.ActualTokens
	}
	return s
}
