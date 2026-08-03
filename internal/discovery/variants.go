package discovery

import (
	"encoding/json"
	"fmt"

	"github.com/dinstein/agent-hub/internal/mcp"
	"github.com/dinstein/agent-hub/internal/tier"
)

// Intent variants (docs/architecture.md §9, ruling #18).
//
// In lazy mode the single call_tool meta-tool splits into three independent
// meta-tools, one per operation tier. The split buys exactly one thing, and
// it is worth the extra names: a client whose permission UI can only allow
// or deny WHOLE TOOLS (Claude Code's allowlist et al.) can allow
// call_tool_read and leave writes needing a prompt. With a single call_tool
// that UI is all-or-nothing.
//
// The variant names are ABI: they appear in agent prompts, in client
// allowlists and in audit records.
const (
	// MetaCallToolRead invokes tools classified tier.Read.
	MetaCallToolRead = "call_tool_read"
	// MetaCallToolWrite invokes tools classified tier.Write.
	MetaCallToolWrite = "call_tool_write"
	// MetaCallToolDestructive invokes tools classified
	// tier.Destructive — including every tool whose server shipped
	// no annotations at all (fail-closed, see tier.ToolTier).
	MetaCallToolDestructive = "call_tool_destructive"
)

// CodeTierMismatch is returned when a call arrives through the wrong intent
// variant. It is a stable code (query.go documents the freeze).
const CodeTierMismatch = "tier_mismatch"

// variantSchemas reuse the frozen call_tool schema verbatim: the three
// variants take the same arguments, and an agent that learned one has
// learned all three.
const schemaCallVariant = schemaCallTool

// variantDefs are the three call variants in frozen order (read → write →
// destructive: least to most dangerous, which is also the order a reviewer
// reads an allowlist in).
var variantDefs = []mcp.ToolDef{
	{
		Name: MetaCallToolRead,
		Description: "Invoke a READ-ONLY tool by the exposed name reported by search_tools. " +
			"Only tools whose call_with field says call_tool_read are callable here.",
		InputSchema: json.RawMessage(schemaCallVariant),
	},
	{
		Name: MetaCallToolWrite,
		Description: "Invoke a WRITING tool by the exposed name reported by search_tools. " +
			"Only tools whose call_with field says call_tool_write are callable here.",
		InputSchema: json.RawMessage(schemaCallVariant),
	},
	{
		Name: MetaCallToolDestructive,
		Description: "Invoke a DESTRUCTIVE tool by the exposed name reported by search_tools. " +
			"Only tools whose call_with field says call_tool_destructive are callable here; " +
			"a tool whose server declared no annotations counts as destructive.",
		InputSchema: json.RawMessage(schemaCallVariant),
	},
}

// VariantNames returns the three variant names in frozen order.
func VariantNames() []string {
	return []string{MetaCallToolRead, MetaCallToolWrite, MetaCallToolDestructive}
}

// VariantFor names the meta-tool that invokes a tool of tier t. An
// unrecognised tier maps to the destructive variant (fail-closed: an agent
// following the pointer lands on the most restricted door, never the least).
func VariantFor(t tier.Tier) string {
	switch t {
	case tier.Read:
		return MetaCallToolRead
	case tier.Write:
		return MetaCallToolWrite
	default:
		return MetaCallToolDestructive
	}
}

// TierOfVariant maps a variant name back to its tier. ok=false for anything
// that is not one of the three variants.
func TierOfVariant(name string) (tier.Tier, bool) {
	switch name {
	case MetaCallToolRead:
		return tier.Read, true
	case MetaCallToolWrite:
		return tier.Write, true
	case MetaCallToolDestructive:
		return tier.Destructive, true
	default:
		return "", false
	}
}

// IsCallVariant reports whether name is one of the three call variants.
func IsCallVariant(name string) bool {
	_, ok := TierOfVariant(name)
	return ok
}

// ToolTier classifies a visible tool into the operation ladder. It is a thin
// re-export of tier.ToolTier so this package never grows a second
// derivation of the same fact.
func ToolTier(t Tool) tier.Tier { return tier.ToolTier(t.Def.Annotations) }

// ResolveCallVariant resolves a call arriving through meta-tool `metaName`.
//
// It is ResolveCall plus the variant check: the tool's tier must EQUAL the
// variant's tier. Equality, not coverage (tier.Covers): the variants
// exist so that allowing call_tool_read in a client's tool allowlist means
// "read tools only". If the destructive variant also accepted read tools,
// each variant would be a superset of the ones below it and allowing the top
// one would silently grant everything — the very property the split is
// supposed to make visible. Every tool therefore has exactly ONE correct
// door, which is also what makes the rejection message actionable.
//
// The rejection names the correct variant so the agent's next attempt
// succeeds without a second search round trip. metaName that is not a
// variant (plain call_tool, compatibility mode) skips the check entirely.
func (s *Surface) ResolveCallVariant(metaName string, raw json.RawMessage) (Tool, json.RawMessage, error) {
	t, args, err := s.ResolveCall(raw)
	if err != nil {
		return Tool{}, nil, err
	}
	want, isVariant := TierOfVariant(metaName)
	if !isVariant {
		return t, args, nil
	}
	if got := ToolTier(t); got != want {
		return Tool{}, nil, newError(CodeTierMismatch, tierMismatchMessage(t.Exposed, got, metaName))
	}
	return t, args, nil
}

// tierMismatchMessage is the frozen rejection wording (golden-tested). It
// names the tool, the tier it actually has, and the variant to use instead —
// nothing else, so the sentence is a stable contract.
func tierMismatchMessage(exposed string, got tier.Tier, used string) string {
	return fmt.Sprintf("tool %q is a %s tool and cannot be invoked through %s; use %s instead",
		exposed, string(got), used, VariantFor(got))
}
