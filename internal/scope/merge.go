package scope

import (
	"fmt"
	"slices"

	"github.com/dinstein/agent-hub/internal/router"
)

// pickDiscovery applies the discovery rule — most specific non-nil layer
// wins, later layer wins ties — and reports which layer supplied the answer.
// ok=false means no layer set one at all, which is NOT the same as a layer
// setting "": the caller's default applies only in the first case.
//
// It is a function rather than eight lines inside Merge because `profile ls`
// asks the same question without a session (DiscoveryFor), and two copies of
// a precedence rule is how a listing starts describing a resolution that no
// longer happens.
func pickDiscovery(layers []ScopeLayer) (DiscoveryMode, LayerKind, bool) {
	var disc DiscoveryMode
	from := LayerGlobal
	best := -1
	for _, l := range layers {
		if l.Discovery != nil && int(l.Kind) >= best {
			best = int(l.Kind)
			disc, from = *l.Discovery, l.Kind
		}
	}
	return disc, from, best >= 0
}

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
//   - discovery: most specific layer wins (among equal kinds the later layer
//     wins).
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

	// Per-server tool sets: an allow INTERSECTION keyed by original tool
	// names, and nothing else. There is no deny half and there must not be
	// one — a selector is an allow list, so a tool the downstream adds
	// tomorrow arrives blocked, and a deny list would admit it. Layers can
	// therefore only ever narrow.
	servers := make(map[string]ToolView, len(visible))
	for id := range visible {
		allowed := make(map[string]bool, len(cat.Servers[id]))
		for _, t := range cat.Servers[id] {
			allowed[t] = true
		}
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
		}
		tools := make([]string, 0, len(allowed))
		for t := range allowed {
			tools = append(tools, t)
		}
		slices.Sort(tools)
		servers[id] = ToolView{Tools: tools}
	}

	disc, _, _ := pickDiscovery(layers)

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

	es := &EffectiveScope{
		Servers:   servers,
		Discovery: disc,
		Budgets:   budgets,
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
func cloneDiags(in []Diagnostic) []Diagnostic {
	if len(in) == 0 {
		return nil
	}
	out := make([]Diagnostic, len(in))
	copy(out, in)
	return out
}
