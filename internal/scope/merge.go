package scope

import (
	"fmt"
	"slices"

	"github.com/dinstein/agent-hub/internal/router"
)

// Merge folds the given layers over the tool catalog into an EffectiveScope
// (docs/architecture.md §7). It is a PURE function: deterministic, no side
// effects, inputs are never mutated and never aliased by the output.
//
// Merge rules (the whole table of 4.1):
//
//   - server visibility: per-layer INTERSECTION, seeded from the catalog's
//     server set (nil = no intervention, [] = block-all) — can only narrow.
//   - tool allow: per-layer INTERSECTION keyed by ORIGINAL tool names,
//     seeded from the catalog's raw tool set of each server.
//   - tool deny: UNION across layers.
//   - approval switches: boolean OR — any layer requiring approval wins
//     (tighten-only; a false never loosens; fail-closed).
//   - discovery: most specific layer wins (session > project > client >
//     profile > global; among equal kinds the later layer wins).
//   - result budget: most specific layer wins per key; Forced entries
//     impose a tighten-only min cap over whatever the specific value is.
//
// Layer order in the slice does not need to be sorted: specificity comes
// from LayerKind. Generation on the result is zero — the caller (Resolver)
// stamps it; Hash intentionally excludes it.
func Merge(layers []ScopeLayer, cat router.Catalog) (*EffectiveScope, error) {
	return MergeWithDiagnostics(layers, cat, nil)
}

// MergeWithDiagnostics is Merge with pre-collected diagnostics (typically
// from FromRegistry, e.g. dangling profile references) folded into the value
// BEFORE hashing, so content addressing covers them.
func MergeWithDiagnostics(layers []ScopeLayer, cat router.Catalog, diags []Diagnostic) (*EffectiveScope, error) {
	for i := range layers {
		if layers[i].Kind > LayerSession {
			return nil, fmt.Errorf("scope: layer %d has invalid kind %d", i, layers[i].Kind)
		}
	}

	// Server visibility: intersection seeded from the catalog.
	visible := make(map[string]bool, len(cat.Servers))
	for id := range cat.Servers {
		visible[id] = true
	}
	for _, l := range layers {
		if l.Servers == nil {
			continue // no intervention
		}
		want := make(map[string]bool, len(l.Servers))
		for _, s := range l.Servers {
			want[s] = true
		}
		for id := range visible {
			if !want[id] {
				delete(visible, id)
			}
		}
	}

	// Per-server tool sets: allow intersection + deny union, both keyed by
	// original tool names.
	servers := make(map[string]ToolView, len(visible))
	for id := range visible {
		allowed := make(map[string]bool, len(cat.Servers[id]))
		for _, t := range cat.Servers[id] {
			allowed[t] = true
		}
		denied := make(map[string]bool)
		for _, l := range layers {
			sel := l.Tools[id]
			if sel == nil {
				continue // selector absent = no intervention
			}
			if sel.Allow != nil { // nil = full server; [] = block-all
				want := make(map[string]bool, len(sel.Allow))
				for _, t := range sel.Allow {
					want[t] = true
				}
				for t := range allowed {
					if !want[t] {
						delete(allowed, t)
					}
				}
			}
			for _, d := range sel.Deny {
				denied[d] = true
			}
		}
		tools := make([]string, 0, len(allowed))
		for t := range allowed {
			if !denied[t] {
				tools = append(tools, t)
			}
		}
		slices.Sort(tools)
		servers[id] = ToolView{Tools: tools}
	}

	// Discovery: most specific non-nil wins; later layer wins ties.
	var disc DiscoveryMode
	discKind := -1
	for _, l := range layers {
		if l.Discovery != nil && int(l.Kind) >= discKind {
			discKind = int(l.Kind)
			disc = *l.Discovery
		}
	}

	// Budgets: most specific wins per key; Forced entries cap via min.
	type budgetAcc struct {
		bestKind  int
		bytes     int
		forcedMin int
		hasForced bool
	}
	acc := make(map[string]*budgetAcc)
	for _, l := range layers {
		for k, b := range l.ResultBudget {
			if b == nil {
				continue
			}
			a := acc[k]
			if a == nil {
				a = &budgetAcc{bestKind: -1}
				acc[k] = a
			}
			if int(l.Kind) >= a.bestKind {
				a.bestKind = int(l.Kind)
				a.bytes = b.Bytes
			}
			if b.Forced && (!a.hasForced || b.Bytes < a.forcedMin) {
				a.hasForced = true
				a.forcedMin = b.Bytes
			}
		}
	}
	budgets := make(map[string]int, len(acc))
	for k, a := range acc {
		v := a.bytes
		if a.hasForced && a.forcedMin < v {
			v = a.forcedMin // tighten-only: a forced rule can only lower the budget
		}
		budgets[k] = v
	}

	// Approval: boolean OR — tighten-only, fail-closed.
	var ap EffectiveApproval
	for _, l := range layers {
		orInto(&ap.HumanApproval, l.Approval.HumanApproval)
		orInto(&ap.DenyDestructive, l.Approval.DenyDestructive)
	}

	es := &EffectiveScope{
		Servers:   servers,
		Discovery: disc,
		Budgets:   budgets,
		Approval:  ap,
		Diags:     cloneDiags(diags),
	}
	es.Hash = hashScope(es)
	return es, nil
}

// Changed reports whether two scopes differ in CONTENT (Hash), ignoring
// Generation. Only a content change warrants pushing tools/list_changed to
// the session (docs/architecture.md §7 — avoid rebuild amplification). A nil-to-
// non-nil transition counts as changed.
func Changed(prev, next *EffectiveScope) bool {
	if prev == nil || next == nil {
		return prev != next
	}
	return prev.Hash != next.Hash
}

// orInto ORs a three-state pointer into dst: nil = no intervention; only a
// true can flip dst (tighten-only — false never loosens; fail-closed).
func orInto(dst *bool, src *bool) {
	if src != nil && *src {
		*dst = true
	}
}

func cloneDiags(in []Diagnostic) []Diagnostic {
	if len(in) == 0 {
		return nil
	}
	out := make([]Diagnostic, len(in))
	copy(out, in)
	return out
}
