package discovery

import (
	"sort"

	"github.com/dinstein/agent-hub/internal/discovery/toolsig"
)

// Lexical ranker weights (KEEP the lexical ranker;
// the compact signature and describe-in-two-steps layers of 7.2 sit on top
// of its OUTPUT, they do not replace the engine).
//
// Scores are integers on purpose. Float scores would make "equal score"
// depend on accumulation order, and the tie-break rule (score desc, exposed
// name asc) is a golden-tested contract — it must be exactly decidable.
const (
	weightName   = 10 // the raw tool name: what the agent is actually after
	weightServer = 4  // the server id: useful disambiguation, weak signal
	weightDesc   = 2  // the description: broad recall, lots of noise

	exactFactor  = 3 // token equality
	prefixFactor = 1 // query token is a prefix of an indexed token

	// coverageBonus rewards matching MORE DISTINCT query terms, so a tool
	// hit by both words of "read file" outranks one hit twice by "read".
	coverageBonus = 5

	// minPrefixLen keeps one-letter query tokens from matching everything.
	minPrefixLen = 2

	// ConfidenceThreshold is the score at or above which a top result is
	// considered CONFIDENT. Calibration: one exact tool-name token match
	// scores weightName*exactFactor + coverageBonus = 35; a description-only
	// exact match scores 11 and a bare name-prefix match 15. So "confident"
	// means "the query literally named the tool". SearchGuard refuses to
	// escalate below this line (docs/flows.md: low confidence does not escalate).
	ConfidenceThreshold = 30
)

// toolIndex is the per-tool token index, built once per Surface.
type toolIndex struct {
	name   tokenSet
	server tokenSet
	desc   tokenSet
}

// tokenSet holds a field's tokens twice: a set for exact lookup and a
// sorted, deduplicated slice for prefix scanning. Sorting makes the prefix
// scan short-circuit and, more importantly, makes it order-independent.
type tokenSet struct {
	set    map[string]bool
	sorted []string
}

func newTokenSet(tokens []string) tokenSet {
	ts := tokenSet{set: make(map[string]bool, len(tokens))}
	for _, t := range tokens {
		if ts.set[t] {
			continue
		}
		ts.set[t] = true
		ts.sorted = append(ts.sorted, t)
	}
	sort.Strings(ts.sorted)
	return ts
}

// hasExact reports token equality.
func (ts tokenSet) hasExact(tok string) bool { return ts.set[tok] }

// hasPrefix reports whether any indexed token starts with tok. Binary
// search finds the first candidate; only the run of tokens sharing the
// prefix is examined.
func (ts tokenSet) hasPrefix(tok string) bool {
	if len(tok) < minPrefixLen {
		return false
	}
	i := sort.SearchStrings(ts.sorted, tok)
	return i < len(ts.sorted) && len(ts.sorted[i]) >= len(tok) && ts.sorted[i][:len(tok)] == tok
}

// buildIndex tokenises every visible tool once. Descriptions are truncated
// at MaxDescriptionTokens tokens: a hostile server must not be able to make
// every search expensive.
//
// It also WARMS the compact-signature cache (docs/modules/dataplane.md, "warm during catalog indexing"):
// rendering happens here, at surface-build time, so the first search of a
// session pays a map lookup instead of N schema walks. The cache is
// fingerprint-keyed, so a rebuild after a scope change re-warms nothing that
// did not actually change.
func (s *Surface) buildIndex() {
	s.index = make([]toolIndex, len(s.tools))
	sigs := toolsig.Shared()
	for i, t := range s.tools {
		s.index[i] = toolIndex{
			name:   newTokenSet(tokenize(t.RawTool, 0)),
			server: newTokenSet(tokenize(t.ServerID, 0)),
			desc:   newTokenSet(tokenize(t.Def.Description, MaxDescriptionTokens)),
		}
		sigs.OfNamed(t.Exposed, t.Def)
	}
}

// scoreTool scores one indexed tool against the validated query tokens.
// Per query token, each field contributes at most once (exact beats prefix
// within a field, fields sum across each other) — occurrence counts are
// ignored, so padding a description with repetitions buys nothing.
func scoreTool(idx toolIndex, tokens []string) int {
	score, matched := 0, 0
	for _, tok := range tokens {
		part := 0
		part += fieldScore(idx.name, tok, weightName)
		part += fieldScore(idx.server, tok, weightServer)
		part += fieldScore(idx.desc, tok, weightDesc)
		if part > 0 {
			matched++
			score += part
		}
	}
	if matched == 0 {
		return 0
	}
	return score + matched*coverageBonus
}

func fieldScore(ts tokenSet, tok string, weight int) int {
	switch {
	case ts.hasExact(tok):
		return weight * exactFactor
	case ts.hasPrefix(tok):
		return weight * prefixFactor
	default:
		return 0
	}
}

// scored is one ranked candidate before budget projection.
type scored struct {
	tool  Tool
	score int
}

// rank scores every visible tool and returns the matches in the frozen
// order: score DESCENDING, then exposed name ASCENDING. Zero-score tools
// are dropped — a search must not recommend something it has no reason to
// believe in. Exposed names are unique, so the order is a total order: the
// same inputs always produce byte-identical output.
func (s *Surface) rank(q Query) []scored {
	out := make([]scored, 0, 8)
	for i, t := range s.tools {
		if sc := scoreTool(s.index[i], q.Tokens); sc > 0 {
			out = append(out, scored{tool: t, score: sc})
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].score != out[j].score {
			return out[i].score > out[j].score
		}
		return out[i].tool.Exposed < out[j].tool.Exposed
	})
	return out
}
