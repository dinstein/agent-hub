package discovery

import "sort"

// PinSet reports which tools are PINNED: exposed directly in lazy mode, so
// the agent can call them without a search round-trip (docs/modules/dataplane.md, the
// "pinned" leg of the token-saving stack).
//
// Pinning is an EXPERIENCE decision, never a security one. It cannot widen
// visibility: a Surface only ever pins tools that are already in its
// scope-filtered set, so pinning a tool the scope hides is a no-op. The
// interface exists so the registry-backed implementation (governance /
// profile documents, M1 wiring) can drop in without touching this package.
type PinSet interface {
	// IsPinned reports whether the ORIGINAL (server, raw tool) pair is
	// pinned. Keys are original names, never exposed names — the same rule
	// as every other scope-facing selector (docs/architecture.md §7 invariant 1).
	IsPinned(serverID, rawTool string) bool
}

// StaticPins is the config-backed PinSet: a fixed serverID -> raw tool set.
// The zero value pins nothing. It is immutable after construction and safe
// for concurrent use.
type StaticPins struct {
	byServer map[string]map[string]bool
	all      map[string]bool // serverIDs pinned wholesale via "*"
}

// PinAll is the raw-tool wildcard: listing it for a server pins every
// visible tool of that server.
const PinAll = "*"

// NewStaticPins copies the given serverID -> raw tool names mapping. A nil
// or empty map yields a PinSet that pins nothing (the neutral direction:
// misconfiguration must not silently dump a whole catalog into lazy mode).
func NewStaticPins(pins map[string][]string) *StaticPins {
	p := &StaticPins{
		byServer: make(map[string]map[string]bool, len(pins)),
		all:      make(map[string]bool),
	}
	for id, tools := range pins {
		if id == "" {
			continue
		}
		set := make(map[string]bool, len(tools))
		for _, t := range tools {
			if t == PinAll {
				p.all[id] = true
				continue
			}
			if t != "" {
				set[t] = true
			}
		}
		p.byServer[id] = set
	}
	return p
}

// IsPinned implements PinSet.
func (p *StaticPins) IsPinned(serverID, rawTool string) bool {
	if p == nil {
		return false
	}
	if p.all[serverID] {
		return true
	}
	return p.byServer[serverID][rawTool]
}

// Servers lists the serverIDs carrying at least one pin, sorted —
// diagnostics only.
func (p *StaticPins) Servers() []string {
	if p == nil {
		return nil
	}
	seen := make(map[string]bool, len(p.byServer))
	for id, set := range p.byServer {
		if len(set) > 0 {
			seen[id] = true
		}
	}
	for id := range p.all {
		seen[id] = true
	}
	out := make([]string, 0, len(seen))
	for id := range seen {
		out = append(out, id)
	}
	sort.Strings(out)
	return out
}
