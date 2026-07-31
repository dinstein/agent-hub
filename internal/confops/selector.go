package confops

import "github.com/dinstein/agent-hub/internal/registry"

// ToolSelectMode is the three-state tool selector of docs/architecture.md §7, keyed
// by ORIGINAL tool names (never exposed/renamed names — otherwise a rename
// would walk out from under its own narrowing rule).
type ToolSelectMode int

const (
	// ToolSelectUnset is the zero value and is REFUSED. Making the zero
	// value mean "all tools" would let a caller that forgot to fill the
	// field silently widen a selector; unset must never be the loose case.
	ToolSelectUnset ToolSelectMode = iota
	// ToolSelectAll exposes the server's full tool set. At a single layer
	// that is indistinguishable from "no intervention", so the rule is
	// dropped rather than stored as an inert object.
	ToolSelectAll
	// ToolSelectOnly narrows to the named subset.
	ToolSelectOnly
	// ToolSelectNone blocks every tool of the server (the EMPTY allow list;
	// omitzero keeps it on disk, because dropping it would flip block-all
	// into allow-all).
	ToolSelectNone
)

// ToolSelection is one three-state edit.
type ToolSelection struct {
	Mode  ToolSelectMode
	Tools []string
}

// validate refuses an unset mode and an empty --only list. "Narrow to
// nothing" must be spelled ToolSelectNone: an empty Only is far more often a
// caller bug than an intent, and guessing would pick the fail-open reading.
func (s ToolSelection) validate() error {
	switch s.Mode {
	case ToolSelectAll, ToolSelectNone:
		return nil
	case ToolSelectOnly:
		if len(dedupSorted(s.Tools)) == 0 {
			return usagef("a tool selection needs at least one tool name (use the block-all mode to block every tool)")
		}
		return nil
	default:
		return usagef("a tool selection needs a mode: all, only or none")
	}
}

// allowList turns a three-state edit into the allow list it stores, and it is
// the ONLY place either narrowing layer answers that question. The global
// layer (ServerEntry.Tools) and the profile layer (ToolSelector.Allow) hold
// the same three states in the same encoding, so a second translation would
// be a second chance to get the pair below the wrong way round.
//
// The pair to be careful with is ToolSelectAll-drops-the-rule against
// ToolSelectNone-keeps-the-empty-list: nil means "no narrowing", an empty
// list means "block everything", and collapsing the two fails OPEN.
//
// ok=false is the unset mode, which writes nothing at all. It cannot be
// folded into the nil return: nil is a decision to drop an existing rule,
// unset is the absence of a decision.
func allowList(sel ToolSelection) (list []string, ok bool) {
	switch sel.Mode {
	case ToolSelectAll:
		return nil, true
	case ToolSelectOnly:
		out := dedupSorted(sel.Tools)
		if out == nil {
			// dedupSorted answers nil for an empty input, which is the one
			// value that must not round-trip here: validate() rejects an
			// empty --only, and this is the second door.
			out = []string{}
		}
		return out, true
	case ToolSelectNone:
		return []string{}, true
	default:
		return nil, false
	}
}

// applySelector writes the requested three-state edit into a profile's
// selector map, deleting entries that became fully inert so the document does
// not accumulate `{}` noise.
func applySelector(m map[string]registry.Doc[registry.ToolSelector], server string, sel ToolSelection) {
	allow, ok := allowList(sel)
	if !ok {
		return
	}
	cur := m[server].V
	cur.Allow = allow
	if cur.Allow == nil {
		delete(m, server)
		return
	}
	m[server] = registry.Doc[registry.ToolSelector]{V: cur}
}

// ServerSetMode selects how a server set is edited. The three-state server
// list mirrors ToolSelector.Allow: nil = no narrowing (every registered
// server), [] = none, [...] = that set.
type ServerSetMode int

const (
	// ServerSetUnset is the zero value and is REFUSED, for the same reason
	// ToolSelectUnset is.
	ServerSetUnset ServerSetMode = iota
	// ServerSetReplace sets the list outright (nil clears the narrowing).
	ServerSetReplace
	// ServerSetAdd adds ids to the list. A nil (no narrowing) list becomes
	// an explicit set the moment one server is named: "these and only these".
	ServerSetAdd
	// ServerSetRemove drops ids from the list.
	ServerSetRemove
)

// ServerSelection is one edit of a profile's server set.
type ServerSelection struct {
	Mode    ServerSetMode
	Servers []string
}
