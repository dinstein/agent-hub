package discovery

import (
	"encoding/json"
	"strings"
	"unicode"

	"github.com/dinstein/agent-hub/internal/discovery/toolsig"
)

// Budget projection (docs/flows.md "full schema for rank 1, a 140-character
// summary for the rest", superseded by 7.2 "compact signature + two-stage
// describe").
//
// The projection is the whole point of lazy discovery: one search answer
// must cost roughly one tool definition, not N.
//
// M1.5 CHANGED THE SHAPE. No hit carries a schema any more — every hit
// carries a one-line compact signature (internal/discovery/toolsig) instead,
// and an agent that needs the schema asks describe_tool for it. Rank 1 used
// to carry the full inputSchema, which made the first hit cost as much as
// the other four combined for a benefit only the tools with awkward schemas
// actually needed. The two-step split moves that cost onto the calls that
// need it and caps the loss at exactly one round trip; the "lossy" flag on a
// hit says when that round trip would tell the agent something new.
const (
	// SummaryMaxBytes bounds a non-top hit's summary. It is a BYTE bound,
	// not a rune count: token budget is what is actually being defended,
	// and a byte bound is the only one that holds for CJK descriptions.
	// Truncation always lands on a rune boundary, so the result is valid
	// UTF-8 and never exceeds the bound.
	SummaryMaxBytes = 140

	// ellipsis marks a truncated summary and is counted INSIDE the budget.
	ellipsis = "…" // 3 bytes

	// emptySchema is what a hit reports when the downstream shipped no
	// inputSchema: a permissive object, never a missing field (the MCP
	// inputSchema field is not optional).
	emptySchema = `{"type":"object"}`

	// noDescription is the frozen placeholder for a tool that documents
	// nothing. Honest and stable beats an empty string the agent has to
	// special-case.
	noDescription = "(no description)"
)

// Hit is one ranked search result under the budget projection. Every hit
// carries Sig; exactly one of Description (rank 1) or Summary (rank > 1) is
// populated.
type Hit struct {
	// Tool is the exposed name — the value to pass to call_tool.
	Tool string `json:"tool"`
	// Server is the owning server id, for the agent's orientation only;
	// it is NOT part of the call.
	Server string `json:"server"`
	Rank   int    `json:"rank"`
	Score  int    `json:"score"`
	// CallWith names the meta-tool that invokes this hit: call_tool in
	// compatibility mode, one of the three intent variants otherwise. It is
	// the agent's only instruction about which door to use — the variant
	// check rejects any other one (Surface.ResolveCallVariant).
	CallWith string `json:"call_with"`
	// Sig is the compact one-line signature (docs/modules/dataplane.md). It is present
	// on EVERY hit and is what replaced the rank-1 schema.
	Sig string `json:"sig"`
	// Lossy reports that Sig could not state the schema in full, so
	// describe_tool would say more. It is absent (false) on the common case
	// of a flat scalar schema, which is the honest and cheap answer.
	Lossy bool `json:"lossy,omitempty"`
	// Description is the FULL description, rank 1 only.
	Description string `json:"description,omitempty"`
	// Summary is the <=SummaryMaxBytes projection, ranks 2..N only.
	Summary string `json:"summary,omitempty"`
}

// callWithFor names the meta-tool an agent must use to invoke t
// (docs/architecture.md §9, ruling #18).
//
// Compatibility mode (variants=false): always call_tool.
//
// Variant mode: the tier derived from t.Def.Annotations by tier.ToolTier
// picks the door — readOnlyHint→read, destructiveHint→destructive/write,
// no annotations at all→destructive (fail-closed). This is the ONLY place
// the choice is made; ResolveCallVariant then enforces the same derivation
// on the way in, so the pointer the agent was given and the check it must
// pass can never disagree.
func callWithFor(t Tool, variants bool) string {
	if !variants {
		return MetaCallTool
	}
	return VariantFor(ToolTier(t))
}

// project turns ranked candidates into budgeted hits. Every rank carries a
// compact signature; rank 1 additionally carries the full description, every
// other rank a summary bounded by SummaryMaxBytes.
//
// Signatures come from the process-wide toolsig cache, which the index build
// has already warmed, so this is a map lookup per hit.
func project(cands []scored, variants bool) []Hit {
	out := make([]Hit, 0, len(cands))
	for i, c := range cands {
		sig := toolsig.Shared().OfNamed(c.tool.Exposed, c.tool.Def)
		h := Hit{
			Tool:     c.tool.Exposed,
			Server:   c.tool.ServerID,
			Rank:     i + 1,
			Score:    c.score,
			CallWith: callWithFor(c.tool, variants),
			Sig:      sig.Text,
			Lossy:    sig.Lossy,
		}
		if i == 0 {
			h.Description = describe(c.tool)
		} else {
			h.Summary = summarize(describe(c.tool))
		}
		out = append(out, h)
	}
	return out
}

// describe returns the tool's description or the frozen placeholder.
func describe(t Tool) string {
	d := strings.TrimSpace(t.Def.Description)
	if d == "" {
		return noDescription
	}
	return d
}

// schemaOf returns the tool's inputSchema, or the permissive default when
// the downstream shipped none or shipped something unparsable. An invalid
// schema is replaced rather than forwarded: the agent would otherwise get
// a hit it cannot possibly call correctly.
func schemaOf(t Tool) json.RawMessage {
	raw := t.Def.InputSchema
	if len(raw) == 0 || !json.Valid(raw) {
		return json.RawMessage(emptySchema)
	}
	return raw
}

// summarize collapses whitespace and truncates to SummaryMaxBytes,
// appending the ellipsis INSIDE the budget. Post-condition (golden-tested):
// len(result) <= SummaryMaxBytes and the result is valid UTF-8.
func summarize(s string) string {
	return truncateBytes(collapseSpace(s), SummaryMaxBytes)
}

// collapseSpace folds every run of Unicode whitespace into a single space
// and trims the ends, so a multi-line description costs the same as its
// single-line equivalent.
func collapseSpace(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	space := false
	for _, r := range s {
		if unicode.IsSpace(r) {
			space = true
			continue
		}
		if space && b.Len() > 0 {
			b.WriteByte(' ')
		}
		space = false
		b.WriteRune(r)
	}
	return b.String()
}

// truncateBytes cuts s to at most max bytes on a rune boundary, marking a
// cut with the ellipsis. max is assumed > len(ellipsis).
func truncateBytes(s string, max int) string {
	if len(s) <= max {
		return s
	}
	limit := max - len(ellipsis)
	if limit <= 0 {
		return ellipsis
	}
	cut := 0
	for i := range s {
		if i > limit {
			break
		}
		cut = i
	}
	return s[:cut] + ellipsis
}
