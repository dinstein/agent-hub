package pipeline

import (
	"encoding/json"

	"github.com/dinstein/agent-hub/internal/tier"
)

// The operation-tier vocabulary lives in internal/tier, not here. It is
// shared by the pipeline, the http bridge, the control plane, discovery and
// the CLI; keeping it in the pipeline forced the control plane to import
// the data plane's execution package just to name a tier, and it made the
// depguard proof for "pipeline must not import ctlapi" inexpressible (the
// probe hit an import cycle instead of the lint rule).
//
// These aliases keep the pipeline's own call sites reading in its own
// vocabulary. They are aliases, not conversions: there is exactly one type.

// CallerTier is the caller-credential operation tier (see internal/tier).
type CallerTier = tier.Tier

// Caller tiers, in escalation order.
const (
	TierRead        = tier.Read
	TierWrite       = tier.Write
	TierDestructive = tier.Destructive
)

// ValidTier reports whether t is one of the three frozen tier values.
func ValidTier(t CallerTier) bool { return tier.Valid(t) }

// TierCovers reports whether a caller holding `caller` may invoke a tool
// classified as `tool`.
func TierCovers(caller, tool CallerTier) bool { return tier.Covers(caller, tool) }

// ToolTier classifies a downstream tool from its verbatim annotations JSON.
func ToolTier(annotations json.RawMessage) CallerTier { return tier.ToolTier(annotations) }
