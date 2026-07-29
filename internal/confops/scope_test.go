package confops

import (
	"context"
	"strings"
	"testing"

	"github.com/dinstein/agent-hub/internal/registry"
)

func TestSetClientBindingCreatesAndRebinds(t *testing.T) {
	ctx := context.Background()
	st := newStore(t)
	seedServers(t, st, "github")
	for _, name := range []string{"work", "personal"} {
		if _, err := CreateProfile(ctx, st, name, nil, Precondition{}); err != nil {
			t.Fatal(err)
		}
	}

	res, err := SetClientBinding(ctx, st, "cursor", ClientBinding{
		Profile: &ProfileBindingSpec{Kind: registry.BindingNamed, Name: "work"},
	}, Precondition{})
	if err != nil {
		t.Fatalf("set: %v", err)
	}
	if b := res.Entry.Binding(); b.Kind != registry.BindingNamed || b.Name != "work" {
		t.Fatalf("binding = %+v", b)
	}
	if res.Dangling {
		t.Error("a live profile reference was reported as dangling")
	}

	// Rebinding replaces the reference outright; there is no other state on
	// the entry that a rebind could leave inconsistent.
	again, err := SetClientBinding(ctx, st, "cursor", ClientBinding{
		Profile: &ProfileBindingSpec{Kind: registry.BindingNamed, Name: "personal"},
	}, Precondition{})
	if err != nil {
		t.Fatal(err)
	}
	if b := again.Entry.Binding(); b.Kind != registry.BindingNamed || b.Name != "personal" {
		t.Errorf("rebind = %+v, want named:personal", b)
	}
}

// The explicit form must win and the shorthand must be cleared: leaving both
// spellings behind is how two sources of truth start to disagree.
func TestSetClientBindingClearsTheShorthand(t *testing.T) {
	ctx := context.Background()
	st := newStore(t)
	if _, err := CreateProfile(ctx, st, "work", nil, Precondition{}); err != nil {
		t.Fatal(err)
	}
	res, err := SetClientBinding(ctx, st, "cursor", ClientBinding{
		Profile: &ProfileBindingSpec{Kind: registry.BindingNamed, Name: "work"},
	}, Precondition{})
	if err != nil {
		t.Fatal(err)
	}
	if res.Entry.Profile != "" {
		t.Errorf("shorthand = %q, want it cleared in favour of profileRef", res.Entry.Profile)
	}
	if res.Entry.ProfileRef == nil {
		t.Fatal("explicit profileRef not written")
	}
}

// TestSetClientBindingReportsADanglingProfile: fail-closing a client to an
// empty scope must never happen quietly.
func TestSetClientBindingReportsADanglingProfile(t *testing.T) {
	ctx := context.Background()
	st := newStore(t)

	res, err := SetClientBinding(ctx, st, "cursor", ClientBinding{
		Profile: &ProfileBindingSpec{Kind: registry.BindingNamed, Name: "ghost"},
	}, Precondition{})
	if err != nil {
		t.Fatal(err)
	}
	if !res.Dangling || res.DanglingProfile != "ghost" {
		t.Fatalf("result = %+v, want a dangling report", res)
	}
	if !strings.Contains(strings.Join(res.Warnings, " "), "EMPTY scope") {
		t.Errorf("warnings = %v, want the fail-closed warning", res.Warnings)
	}
}

func TestSetClientBindingValidation(t *testing.T) {
	ctx := context.Background()
	st := newStore(t)
	seedServers(t, st, "github")

	followActive := &ProfileBindingSpec{Kind: registry.BindingFollowActive}

	_, err := SetClientBinding(ctx, st, "", ClientBinding{Profile: followActive}, Precondition{})
	wantErrorKind(t, err, KindUsage, CodeUsage)

	_, err = SetClientBinding(ctx, st, "cursor", ClientBinding{}, Precondition{})
	wantErrorKind(t, err, KindUsage, CodeUsage)

	// A named binding with no name is a typo, not "no profile" — that is
	// spelled followActive, and resolving the typo would be a silent widening.
	_, err = SetClientBinding(ctx, st, "cursor", ClientBinding{
		Profile: &ProfileBindingSpec{Kind: registry.BindingNamed},
	}, Precondition{})
	wantErrorKind(t, err, KindUsage, CodeUsage)

	_, err = SetClientBinding(ctx, st, "cursor", ClientBinding{
		Profile: &ProfileBindingSpec{Kind: "sometimes"},
	}, Precondition{})
	wantErrorKind(t, err, KindUsage, CodeUsage)
}

func TestSetClientBindingPreconditionConflict(t *testing.T) {
	ctx := context.Background()
	st := newStore(t)
	seedServers(t, st, "a", "b")
	gen := st.Snapshot().Generation

	_, err := SetClientBinding(ctx, st, "cursor",
		ClientBinding{Profile: &ProfileBindingSpec{Kind: registry.BindingFollowActive}},
		Precondition{Generation: gen - 1})
	wantStale(t, err, gen)
	if _, ok := st.Snapshot().Clients.V.Clients["cursor"]; ok {
		t.Error("a stale binding was written anyway")
	}
}

func TestClearClientBinding(t *testing.T) {
	ctx := context.Background()
	st := newStore(t)
	seedServers(t, st, "a")
	followActive := ClientBinding{Profile: &ProfileBindingSpec{Kind: registry.BindingFollowActive}}
	if _, err := SetClientBinding(ctx, st, "cursor", followActive, Precondition{}); err != nil {
		t.Fatal(err)
	}

	if _, err := ClearClientBinding(ctx, st, "cursor", Precondition{}); err != nil {
		t.Fatalf("clear: %v", err)
	}
	if _, ok := st.Snapshot().Clients.V.Clients["cursor"]; ok {
		t.Error("the binding survived the clear")
	}

	_, err := ClearClientBinding(ctx, st, "cursor", Precondition{})
	wantErrorKind(t, err, KindNotFound, CodeNotFound)
	_, err = ClearClientBinding(ctx, st, "", Precondition{})
	wantErrorKind(t, err, KindUsage, CodeUsage)

	if _, err := SetClientBinding(ctx, st, "again", followActive, Precondition{}); err != nil {
		t.Fatal(err)
	}
	gen := st.Snapshot().Generation
	_, err = ClearClientBinding(ctx, st, "again", Precondition{Generation: gen - 1})
	wantStale(t, err, gen)
	if _, ok := st.Snapshot().Clients.V.Clients["again"]; !ok {
		t.Error("a stale clear deleted the binding anyway")
	}
}
