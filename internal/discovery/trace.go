package discovery

// Trace is one searchtrace record (docs/flows.md: "persist searchtrace (tool
// names only)"). It is the structure the gateway hands to internal/savings.
//
// PRIVACY INVARIANT — the reason this type exists at all: a search query is
// agent-authored free text and may carry secrets, file paths or an injected
// payload. The trace therefore records the query's MEASUREMENTS (byte
// length, word count) and never a single byte of its content. Tool names
// and scores are safe: they come from the catalog, not from the caller.
//
// Adding a field to this struct is a privacy decision. The golden test
// asserts that a marshalled trace of a distinctive query contains no
// fragment of that query — it will fail the moment someone adds one.
type Trace struct {
	// QueryBytes is len(query) in bytes — a size, never the content.
	QueryBytes int `json:"query_bytes"`
	// QueryWords is the token count before deduplication.
	QueryWords int `json:"query_words"`
	// Results are the exposed tool names returned, in returned order.
	Results []string `json:"results,omitempty"`
	// Matched is the number of candidates that scored above zero, before
	// the limit was applied (Results may be shorter).
	Matched int `json:"matched"`
	// TopScore is the rank-1 score (0 when nothing matched).
	TopScore int `json:"top_score"`
	// Truncated reports that Matched exceeded the requested limit.
	Truncated bool `json:"truncated,omitempty"`
	// Escalated reports that SearchGuard replaced the results with its
	// single imperative line.
	Escalated bool `json:"escalated,omitempty"`
	// Rejected carries the validation error code when the query never
	// reached the ranker (see CodeQuery* constants); empty otherwise.
	Rejected string `json:"rejected,omitempty"`
}

// traceOfRejection builds the trace for a query that failed validation.
// The measurements are still recorded — an agent hammering the 512-byte
// limit is exactly the pattern audit wants to see — but nothing else.
func traceOfRejection(raw string, code string) Trace {
	return Trace{
		QueryBytes: len(raw),
		QueryWords: len(tokenize(raw, MaxQueryWords+1)),
		Rejected:   code,
	}
}
