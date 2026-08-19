// Package tier is the operation-tier vocabulary of docs/architecture.md#what-a-call-passes-through
// (read | write | destructive) — the one ladder the whole repository
// counts on.
//
// It lives in its own leaf package because six packages import it and none
// of them owns it: internal/pipeline gates calls by it, internal/httpbridge
// stores it on agent tokens, internal/ctlapi mints those tokens,
// internal/gateway carries it through an assembly, internal/cli parses it
// from user input, and internal/discovery names its intent variants after
// these three values.
//
// internal/daemon carries a tier too and is deliberately NOT on that list:
// the value reaches gateway.Config.CallerTier as httpbridge.Caller.Tier, so
// the daemon never names the type and its production code does not import
// this package. Only its tests do. The list is the import pressure that put
// this vocabulary in a leaf, and a package that routes the value without
// depending on it is not part of that pressure.
//
// When the vocabulary lived in the pipeline, the
// control plane had to import the data plane's execution package to say the
// word "read" — an edge that contradicts the layering AND made the
// depguard proof for "pipeline must not import ctlapi" impossible to
// express: the probe produced an import cycle instead of a lint violation,
// so the rule stopped being provable.
//
// The package depends on the standard library only, by construction: a
// vocabulary that pulls in dependencies is not a vocabulary.
package tier

import (
	"bytes"
	"encoding/json"
)

// Operation tiers (docs/architecture.md#what-a-call-passes-through). One ladder, repo-wide: agent-token
// credentials carry a Tier, downstream tools are classified into the
// same three values by ToolTier, and internal/discovery names its intent
// variants after them. A second enumeration would be a second answer.

// Tier is a caller credential's operation tier. The empty string means "no
// tier authority": stdio callers are the human's own session and carry no
// agent token, so the tier gate has nothing to enforce for them. The same
// three values classify TOOLS (see ToolTier) and name the intent variants
// of internal/discovery — one ladder, no parallel enumeration.
type Tier string

// The tiers, in escalation order.
const (
	Read        Tier = "read"
	Write       Tier = "write"
	Destructive Tier = "destructive"
)

// tierRank orders the ladder. An unrecognised tier ranks 0, below every
// real tier, so it covers nothing (fail-closed: a typo in a stored token
// denies rather than escalates).
func tierRank(t Tier) int {
	switch t {
	case Read:
		return 1
	case Write:
		return 2
	case Destructive:
		return 3
	default:
		return 0
	}
}

// Valid reports whether t is one of the three frozen tier values. It is
// the validation predicate for stored credentials and CLI input.
func Valid(t Tier) bool { return tierRank(t) > 0 }

// Covers reports whether a caller holding tier `caller` may invoke a
// tool classified as `tool`. Coverage is by RANK, not equality: a write
// credential may call read tools, a destructive credential may call
// anything. (The intent VARIANTS of internal/discovery use exact equality
// instead — see the doc there for why the two rules differ.)
//
// Failure direction: an unrecognised caller tier covers nothing.
func Covers(caller, tool Tier) bool {
	return tierRank(caller) >= tierRank(tool) && tierRank(caller) > 0
}

// ToolTier classifies a downstream tool into the operation ladder from its
// verbatim annotations JSON (docs/architecture.md#what-a-call-passes-through "tier is derived from downstream annotations"):
//
//	annotations field absent / null / unparsable → destructive
//	readOnlyHint == true                         → read
//	destructiveHint == true                      → destructive
//	destructiveHint == false                     → write
//	annotations object present, neither hint set  → write
//
// The first and the last line are the delicate ones, and they are NOT the
// same case:
//
//   - NO annotations object at all means the server told us nothing. That is
//     the fail-closed case frozen by docs/conventions.md#engineering-conventions (determinism is the contract) ("unknown annotations mean
//     destructive"): an unannotated tool must never be reachable with a
//     read-only credential.
//   - An annotations object that simply stays silent about destructiveHint
//     is a server that DID describe itself; docs/architecture.md#what-a-call-passes-through reads that silence
//     as write, and that is what the tier ladder does.
//
// `{}` therefore answers write, not destructive, even though destructive is
// the MCP spec default for a missing hint. This function feeds coarse
// credential separation and the intent variants, where treating every
// silent-but-annotated tool as destructive would collapse the ladder to a
// single tier. It used to have a blunter counterpart — DefaultDestructive,
// which read `{}` as destructive and fed the global denyDestructive veto —
// and the asymmetry between the two was the point; the veto went with the
// rest of the runtime governance.
//
// TWO readers remain, and this sentence used to name only the first.
// internal/pipeline's tier gate compares the result against the caller's
// credential; internal/discovery re-exports it (variants.go) to decide which
// call_tool_read / _write / _destructive door a tool sits behind. The second
// reader is why `{}` answering write rather than destructive is not a
// private choice: change it and every silent-but-annotated tool moves door
// in lazy mode — which is exactly what an agent's tool allow list is written
// against, and a consequence a reader who checked only the gate would not
// see coming.
func ToolTier(annotations json.RawMessage) Tier {
	trimmed := bytes.TrimSpace(annotations)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) {
		return Destructive
	}
	var a struct {
		ReadOnlyHint    *bool `json:"readOnlyHint"`
		DestructiveHint *bool `json:"destructiveHint"`
	}
	if err := json.Unmarshal(trimmed, &a); err != nil {
		return Destructive // unparsable: fail-closed
	}
	if a.ReadOnlyHint != nil && *a.ReadOnlyHint {
		return Read
	}
	if a.DestructiveHint != nil {
		if *a.DestructiveHint {
			return Destructive
		}
		return Write
	}
	return Write
}
