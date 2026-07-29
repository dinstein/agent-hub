package scope

// Overlay is the session-layer input: an in-memory, never-persisted
// narrowing applied on top of the persisted three layers (docs/architecture.md §7
// "session layer (in-memory overlay, never persisted)"). Fields mirror ScopeLayer minus what
// a session must never touch.
//
// Approval can only TIGHTEN: the fields merge by boolean OR, so a session
// can add an approval requirement but a false is inert and can never remove
// one set by a lower layer (fail-closed). DenyDestructive is absent BY
// CONSTRUCTION — it is global-governance-only and never agent-writable;
// keeping the field off this type makes that unrepresentable rather than
// merely validated.
type Overlay struct {
	// Version is the session-local monotonic overlay version (bumped on
	// every mutation); it is part of the Resolver cache key.
	Version uint64

	// Servers: nil = no intervention; [] = block-all; [...] = narrow.
	Servers []string

	// Tools maps serverID -> selector, ORIGINAL tool names.
	Tools map[string]*ToolSelector

	// Discovery: nil = no intervention.
	Discovery *DiscoveryMode

	// ResultBudget maps serverID or "*".
	ResultBudget map[string]*Budget

	Approval OverlayApproval
}

// OverlayApproval is the session-writable subset of ApprovalPolicy:
// tighten-only switches, no DenyDestructive (see Overlay doc).
type OverlayApproval struct {
	HumanApproval *bool
}

// Layer converts the overlay into its ScopeLayer form (Kind LayerSession).
// The result deep-copies every field so later overlay mutations cannot
// reach into an already-merged scope.
func (o *Overlay) Layer(origin string) ScopeLayer {
	l := ScopeLayer{
		Kind:    LayerSession,
		Origin:  origin,
		Servers: cloneStrings(o.Servers),
	}
	if len(o.Tools) > 0 {
		l.Tools = make(map[string]*ToolSelector, len(o.Tools))
		for k, sel := range o.Tools {
			if sel == nil {
				continue
			}
			cp := ToolSelector{Allow: cloneStrings(sel.Allow), Deny: cloneStrings(sel.Deny)}
			l.Tools[k] = &cp
		}
	}
	if o.Discovery != nil {
		d := *o.Discovery
		l.Discovery = &d
	}
	if len(o.ResultBudget) > 0 {
		l.ResultBudget = make(map[string]*Budget, len(o.ResultBudget))
		for k, b := range o.ResultBudget {
			if b == nil {
				continue
			}
			cp := *b
			l.ResultBudget[k] = &cp
		}
	}
	l.Approval.HumanApproval = cloneBool(o.Approval.HumanApproval)
	// l.Approval.DenyDestructive stays nil: unrepresentable in an Overlay.
	return l
}
