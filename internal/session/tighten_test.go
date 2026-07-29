package session

import (
	"testing"

	"github.com/dinstein/agent-hub/internal/scope"
)

func bp(v bool) *bool { return &v }

func sel(allow []string, deny ...string) *scope.ToolSelector {
	return &scope.ToolSelector{Allow: allow, Deny: deny}
}

// TestLooseningsMatrix is the tighten-only decision matrix (A.1 #8).
// want=true means the transition is a loosening and must require a grant.
func TestLooseningsMatrix(t *testing.T) {
	disc := scope.DiscoveryLazy
	cases := []struct {
		name string
		prev *scope.Overlay
		next *scope.Overlay
		want bool
	}{
		// --- baseline ---
		{"nil prev accepts anything", nil,
			&scope.Overlay{Servers: []string{"a"}}, false},
		{"nil next of empty prev is fine", &scope.Overlay{}, nil, false},
		{"nil next of narrowing prev restores waterline", &scope.Overlay{Servers: []string{"a"}}, nil, true},

		// --- servers: nil = no intervention; list can only shrink ---
		{"servers nil->list", &scope.Overlay{}, &scope.Overlay{Servers: []string{"a"}}, false},
		{"servers shrink", &scope.Overlay{Servers: []string{"a", "b"}}, &scope.Overlay{Servers: []string{"a"}}, false},
		{"servers grow", &scope.Overlay{Servers: []string{"a"}}, &scope.Overlay{Servers: []string{"a", "b"}}, true},
		{"servers list->nil", &scope.Overlay{Servers: []string{"a"}}, &scope.Overlay{}, true},
		{"servers block-all kept", &scope.Overlay{Servers: []string{}}, &scope.Overlay{Servers: []string{}}, false},
		{"servers block-all->one", &scope.Overlay{Servers: []string{}}, &scope.Overlay{Servers: []string{"a"}}, true},

		// --- tools: allow subset-only, deny superset-only ---
		{"allow shrink",
			&scope.Overlay{Tools: map[string]*scope.ToolSelector{"s": sel([]string{"x", "y"})}},
			&scope.Overlay{Tools: map[string]*scope.ToolSelector{"s": sel([]string{"x"})}}, false},
		{"allow add tool",
			&scope.Overlay{Tools: map[string]*scope.ToolSelector{"s": sel([]string{"x"})}},
			&scope.Overlay{Tools: map[string]*scope.ToolSelector{"s": sel([]string{"x", "z"})}}, true},
		{"allow list->nil (full set)",
			&scope.Overlay{Tools: map[string]*scope.ToolSelector{"s": sel([]string{"x"})}},
			&scope.Overlay{Tools: map[string]*scope.ToolSelector{"s": sel(nil)}}, true},
		{"selector removed",
			&scope.Overlay{Tools: map[string]*scope.ToolSelector{"s": sel([]string{"x"})}},
			&scope.Overlay{}, true},
		{"no-op selector removed",
			&scope.Overlay{Tools: map[string]*scope.ToolSelector{"s": sel(nil)}},
			&scope.Overlay{}, false},
		{"deny grow",
			&scope.Overlay{Tools: map[string]*scope.ToolSelector{"s": sel(nil, "d")}},
			&scope.Overlay{Tools: map[string]*scope.ToolSelector{"s": sel(nil, "d", "e")}}, false},
		{"deny drop",
			&scope.Overlay{Tools: map[string]*scope.ToolSelector{"s": sel(nil, "d")}},
			&scope.Overlay{Tools: map[string]*scope.ToolSelector{"s": sel(nil)}}, true},
		{"new server constraint added",
			&scope.Overlay{Tools: map[string]*scope.ToolSelector{"s": sel([]string{"x"})}},
			&scope.Overlay{Tools: map[string]*scope.ToolSelector{
				"s": sel([]string{"x"}), "t": sel([]string{}),
			}}, false},

		// --- approval: a set true can never be unset ---
		{"approval nil->true",
			&scope.Overlay{},
			&scope.Overlay{Approval: scope.OverlayApproval{HumanApproval: bp(true)}}, false},
		{"approval true->nil",
			&scope.Overlay{Approval: scope.OverlayApproval{HumanApproval: bp(true)}},
			&scope.Overlay{}, true},
		{"approval true->false",
			&scope.Overlay{Approval: scope.OverlayApproval{HumanApproval: bp(true)}},
			&scope.Overlay{Approval: scope.OverlayApproval{HumanApproval: bp(false)}}, true},
		{"approval false->nil (inert)",
			&scope.Overlay{Approval: scope.OverlayApproval{HumanApproval: bp(false)}},
			&scope.Overlay{}, false},

		// --- budgets: only Forced entries are security-relevant ---
		{"forced cap lowered",
			&scope.Overlay{ResultBudget: map[string]*scope.Budget{"*": {Bytes: 100, Forced: true}}},
			&scope.Overlay{ResultBudget: map[string]*scope.Budget{"*": {Bytes: 50, Forced: true}}}, false},
		{"forced cap raised",
			&scope.Overlay{ResultBudget: map[string]*scope.Budget{"*": {Bytes: 100, Forced: true}}},
			&scope.Overlay{ResultBudget: map[string]*scope.Budget{"*": {Bytes: 200, Forced: true}}}, true},
		{"forced cap removed",
			&scope.Overlay{ResultBudget: map[string]*scope.Budget{"*": {Bytes: 100, Forced: true}}},
			&scope.Overlay{}, true},
		{"forced cap unforced",
			&scope.Overlay{ResultBudget: map[string]*scope.Budget{"*": {Bytes: 100, Forced: true}}},
			&scope.Overlay{ResultBudget: map[string]*scope.Budget{"*": {Bytes: 100}}}, true},
		{"unforced budget raised (experience)",
			&scope.Overlay{ResultBudget: map[string]*scope.Budget{"*": {Bytes: 100}}},
			&scope.Overlay{ResultBudget: map[string]*scope.Budget{"*": {Bytes: 900}}}, false},

		// --- experience fields move freely ---
		{"discovery change",
			&scope.Overlay{Discovery: &disc},
			&scope.Overlay{}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := loosenings(tc.prev, tc.next)
			if (len(got) > 0) != tc.want {
				t.Fatalf("loosenings = %v, want loosening=%v", got, tc.want)
			}
		})
	}
}

func TestCloneOverlayIsDeep(t *testing.T) {
	disc := scope.DiscoveryFull
	src := &scope.Overlay{
		Version:      7,
		Servers:      []string{"a"},
		Tools:        map[string]*scope.ToolSelector{"s": sel([]string{"x"}, "d")},
		Discovery:    &disc,
		ResultBudget: map[string]*scope.Budget{"*": {Bytes: 10, Forced: true}},
		Approval:     scope.OverlayApproval{HumanApproval: bp(true)},
	}
	cp := cloneOverlay(src)

	cp.Servers[0] = "MUT"
	cp.Tools["s"].Allow[0] = "MUT"
	cp.Tools["s"].Deny[0] = "MUT"
	*cp.Discovery = scope.DiscoveryLazy
	cp.ResultBudget["*"].Bytes = 999
	*cp.Approval.HumanApproval = false

	if src.Servers[0] != "a" || src.Tools["s"].Allow[0] != "x" || src.Tools["s"].Deny[0] != "d" {
		t.Fatal("clone aliased servers/tools")
	}
	if *src.Discovery != scope.DiscoveryFull || src.ResultBudget["*"].Bytes != 10 {
		t.Fatal("clone aliased discovery/budget")
	}
	if !*src.Approval.HumanApproval {
		t.Fatal("clone aliased approval pointer")
	}
	if got := cloneOverlay(nil); got == nil || got.Version != 0 {
		t.Fatalf("cloneOverlay(nil) = %+v, want fresh zero overlay", got)
	}
}
