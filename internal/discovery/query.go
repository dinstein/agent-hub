package discovery

import (
	"strings"
	"unicode"
)

// Query limits (docs/flows.md: "validate query (512B / 64 words)"). They bound the work
// one search can cost and, more importantly, keep an injected agent from
// smuggling a payload through the search path: the query is never echoed
// back and never logged, but it is still parsed, so its size is capped.
const (
	// MaxQueryBytes is the hard byte length limit of a raw query.
	MaxQueryBytes = 512
	// MaxQueryWords is the hard token count limit after tokenisation.
	MaxQueryWords = 64
	// MaxDescriptionTokens bounds how much of a tool description feeds the
	// index. A malicious server cannot make ranking arbitrarily expensive
	// by shipping a megabyte description; the tail is simply not indexed.
	MaxDescriptionTokens = 256
)

// Stable machine-readable error codes. Both codes and messages are frozen
// by golden tests: agents key retry logic off them (docs/conventions.md#engineering-conventions).
const (
	CodeQueryEmpty        = "query_empty"
	CodeQueryTooLong      = "query_too_long"
	CodeQueryTooManyWords = "query_too_many_words"
	CodeInvalidArgs       = "invalid_args"
	CodeUnknownTool       = "unknown_tool"
)

// Error is the typed meta-tool failure. Code is the stable machine-readable
// discriminator; Message is a fixed, deterministic sentence. Message NEVER
// contains the query text — only its measurements (see Validate).
type Error struct {
	Code    string
	Message string
}

func (e *Error) Error() string { return e.Code + ": " + e.Message }

// newError builds a typed error; the constructor keeps all message text in
// one place so the golden test covers every phrasing.
func newError(code, msg string) *Error { return &Error{Code: code, Message: msg} }

// Query is a validated search query: the normalised token list plus the
// measurements the trace records. The raw text is deliberately NOT kept —
// nothing downstream of validation may reach it.
type Query struct {
	Tokens []string // lowercased, in query order, duplicates removed
	Bytes  int      // raw byte length
	Words  int      // token count BEFORE deduplication
}

// Validate parses and bounds-checks a raw query.
//
// Check order is fixed (empty → bytes → words) so a query that violates two
// limits always reports the same code. Byte length is checked on the raw
// string before tokenisation: an oversized query must be rejected without
// being fully processed.
func Validate(raw string) (Query, error) {
	q := Query{Bytes: len(raw)}
	if strings.TrimSpace(raw) == "" {
		return Query{}, newError(CodeQueryEmpty, "query must not be empty")
	}
	if q.Bytes > MaxQueryBytes {
		return Query{}, newError(CodeQueryTooLong,
			"query exceeds "+itoa(MaxQueryBytes)+" bytes (got "+itoa(q.Bytes)+")")
	}
	words := tokenize(raw, MaxQueryWords+1)
	q.Words = len(words)
	if q.Words > MaxQueryWords {
		return Query{}, newError(CodeQueryTooManyWords,
			"query exceeds "+itoa(MaxQueryWords)+" words")
	}
	q.Tokens = dedupTokens(words)
	if len(q.Tokens) == 0 {
		// Punctuation-only input: syntactically non-empty, semantically not
		// a query. Same code as empty — one recovery instruction, not two.
		return Query{}, newError(CodeQueryEmpty, "query must not be empty")
	}
	return q, nil
}

// tokenize lowercases and splits on every non-alphanumeric rune. This is
// the whole of the "minimal stemming" ruling (keeps the
// existing lexical ranker): lowercase + split, no suffix stripping, no
// synonym table. Prefix weighting in the scorer covers the plural/verb-form
// cases a stemmer would, without a language-specific table to get wrong.
//
// limit caps the number of tokens produced (0 = unlimited); callers pass
// limit+1 when they only need to know that a bound was exceeded.
func tokenize(s string, limit int) []string {
	var out []string
	var b strings.Builder
	flush := func() bool {
		if b.Len() == 0 {
			return true
		}
		out = append(out, b.String())
		b.Reset()
		return limit <= 0 || len(out) < limit
	}
	for _, r := range s {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			b.WriteRune(unicode.ToLower(r))
			continue
		}
		if !flush() {
			return out
		}
	}
	flush()
	return out
}

// dedupTokens removes repeats while preserving first-occurrence order:
// repeating a word must not inflate a score (that would be a trivially
// gameable ranking signal).
func dedupTokens(in []string) []string {
	seen := make(map[string]bool, len(in))
	out := make([]string, 0, len(in))
	for _, t := range in {
		if seen[t] {
			continue
		}
		seen[t] = true
		out = append(out, t)
	}
	return out
}
