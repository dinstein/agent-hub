package session

import (
	"fmt"
	"slices"

	"github.com/dinstein/agent-hub/internal/scope"
)

// This file implements the A.1 #8 "tighten-only" validation for overlay
// mutation. Security fields (Servers, Tools, Approval, and Forced budgets)
// may only move in the narrowing direction relative to the PREVIOUS overlay;
// experience fields (Discovery, non-forced ResultBudget) move freely.
//
// Failure direction: every ambiguous transition is classified as LOOSENING
// and rejected — an over-strict check costs the caller a human grant, an
// over-lenient one hands a widening to the threat-model subject (the agent).
//
// Note the merge already guarantees an overlay can never widen beyond the
// static three-layer waterline (session layers intersect). This check guards
// the OTHER direction: an agent undoing its own (or an operator's) runtime
// narrowing without a grant.

// cloneOverlay deep-copies ov so Mutate can hand the mutation fn a private
// copy (copy-on-write; published overlays are immutable). nil yields a
// fresh zero overlay.
func cloneOverlay(ov *scope.Overlay) *scope.Overlay {
	if ov == nil {
		return &scope.Overlay{}
	}
	cp := &scope.Overlay{
		Version: ov.Version,
		Servers: cloneStrings(ov.Servers),
	}
	if len(ov.Tools) > 0 {
		cp.Tools = make(map[string]*scope.ToolSelector, len(ov.Tools))
		for k, sel := range ov.Tools {
			if sel == nil {
				continue
			}
			c := scope.ToolSelector{Allow: cloneStrings(sel.Allow), Deny: cloneStrings(sel.Deny)}
			cp.Tools[k] = &c
		}
	}
	if ov.Discovery != nil {
		d := *ov.Discovery
		cp.Discovery = &d
	}
	if len(ov.ResultBudget) > 0 {
		cp.ResultBudget = make(map[string]*scope.Budget, len(ov.ResultBudget))
		for k, b := range ov.ResultBudget {
			if b == nil {
				continue
			}
			c := *b
			cp.ResultBudget[k] = &c
		}
	}
	cp.Approval.HumanApproval = cloneBool(ov.Approval.HumanApproval)
	cp.Approval.ConfirmDestructive = cloneBool(ov.Approval.ConfirmDestructive)
	return cp
}

func cloneBool(b *bool) *bool {
	if b == nil {
		return nil
	}
	v := *b
	return &v
}

// loosenings returns every way next is LOOSER than prev on a security
// field; empty means the transition is tighten-only and needs no grant.
// prev == nil is the no-overlay baseline (anything a fresh overlay does is
// narrowing, because overlays can only intersect the static waterline).
func loosenings(prev, next *scope.Overlay) []string {
	var out []string
	if prev == nil {
		return nil
	}
	if next == nil {
		// Dropping the overlay entirely restores the static waterline —
		// a loosening unless everything in prev was already inert.
		next = &scope.Overlay{}
	}

	// Servers: nil = no intervention (widest); a present list can only
	// shrink. non-nil -> nil and any added server are loosenings.
	if prev.Servers != nil {
		if next.Servers == nil {
			out = append(out, "servers: narrowing removed")
		} else {
			for _, s := range next.Servers {
				if !slices.Contains(prev.Servers, s) {
					out = append(out, fmt.Sprintf("servers: %q added", s))
				}
			}
		}
	}

	// Tools: for every server prev constrained, the constraint must remain
	// and only shrink: Allow (nil = full set) must stay a subset, Deny must
	// stay a superset (deny merges by union — removing one loosens).
	for id, psel := range prev.Tools {
		if psel == nil {
			continue
		}
		nsel := next.Tools[id]
		if nsel == nil {
			if psel.Allow != nil || len(psel.Deny) > 0 {
				out = append(out, fmt.Sprintf("tools[%s]: selector removed", id))
			}
			continue
		}
		if psel.Allow != nil {
			if nsel.Allow == nil {
				out = append(out, fmt.Sprintf("tools[%s]: allow narrowing removed", id))
			} else {
				for _, t := range nsel.Allow {
					if !slices.Contains(psel.Allow, t) {
						out = append(out, fmt.Sprintf("tools[%s]: allow %q added", id, t))
					}
				}
			}
		}
		for _, d := range psel.Deny {
			if !slices.Contains(nsel.Deny, d) {
				out = append(out, fmt.Sprintf("tools[%s]: deny %q removed", id, d))
			}
		}
	}

	// Approval: a set true can never be unset (merge is boolean OR, so a
	// false or nil in prev is inert and may change freely).
	if isTrue(prev.Approval.HumanApproval) && !isTrue(next.Approval.HumanApproval) {
		out = append(out, "approval.humanApproval: true removed")
	}
	if isTrue(prev.Approval.ConfirmDestructive) && !isTrue(next.Approval.ConfirmDestructive) {
		out = append(out, "approval.confirmDestructive: true removed")
	}

	// Budgets are experience fields EXCEPT Forced entries, which are
	// tighten-only caps (scope.Budget doc): a Forced cap must survive as a
	// Forced cap that is not higher.
	for k, pb := range prev.ResultBudget {
		if pb == nil || !pb.Forced {
			continue
		}
		nb := next.ResultBudget[k]
		switch {
		case nb == nil || !nb.Forced:
			out = append(out, fmt.Sprintf("budget[%s]: forced cap removed", k))
		case nb.Bytes > pb.Bytes:
			out = append(out, fmt.Sprintf("budget[%s]: forced cap raised %d -> %d", k, pb.Bytes, nb.Bytes))
		}
	}

	return out
}

// isTrue reads a three-state bool pointer; nil and false are equivalent
// (inert) for merge purposes.
func isTrue(b *bool) bool { return b != nil && *b }
