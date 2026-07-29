package confops

import (
	"context"
	"strings"
	"testing"

	"github.com/dinstein/agent-hub/internal/registry"
)

func strptr(s string) *string { return &s }

func TestSetClientBindingCreatesAndAmends(t *testing.T) {
	ctx := context.Background()
	st := newStore(t)
	seedServers(t, st, "github")
	if _, err := CreateProfile(ctx, st, "work", nil, Precondition{}); err != nil {
		t.Fatal(err)
	}

	servers := []string{"github"}
	res, err := SetClientBinding(ctx, st, "cursor", ClientBinding{
		Profile:   &ProfileBindingSpec{Kind: registry.BindingNamed, Name: "work"},
		Servers:   &servers,
		Tools:     map[string]ToolSelection{"github": {Mode: ToolSelectOnly, Tools: []string{"list_prs"}}},
		Discovery: strptr("grouped"),
	}, Precondition{})
	if err != nil {
		t.Fatalf("set: %v", err)
	}
	if b := res.Entry.Binding(); b.Kind != registry.BindingNamed || b.Name != "work" {
		t.Fatalf("binding = %+v", b)
	}
	if res.Entry.Discovery != "grouped" || len(res.Entry.Servers) != 1 {
		t.Fatalf("entry = %+v", res.Entry)
	}
	if got := res.Entry.Tools["github"].V.Allow; len(got) != 1 || got[0] != "list_prs" {
		t.Fatalf("selector = %v", got)
	}
	if res.Dangling {
		t.Error("a live profile reference was reported as dangling")
	}

	// An amend touches only what it names.
	amended, err := SetClientBinding(ctx, st, "cursor", ClientBinding{
		Discovery: strptr("lazy"),
	}, Precondition{})
	if err != nil {
		t.Fatal(err)
	}
	if amended.Entry.Binding().Name != "work" || amended.Entry.Discovery != "lazy" ||
		len(amended.Entry.Servers) != 1 {
		t.Errorf("amend dropped an unrelated field: %+v", amended.Entry)
	}
}

// TestSetClientBindingEmptyToolListIsBlockAll: the fail-open state. An empty
// selection must persist the EMPTY allow list, not "all tools".
func TestSetClientBindingEmptyToolListIsBlockAll(t *testing.T) {
	ctx := context.Background()
	st := newStore(t)
	seedServers(t, st, "github")

	res, err := SetClientBinding(ctx, st, "cursor", ClientBinding{
		Tools: map[string]ToolSelection{"github": {Mode: ToolSelectNone}},
	}, Precondition{})
	if err != nil {
		t.Fatal(err)
	}
	if got := res.Entry.Tools["github"].V.Allow; got == nil || len(got) != 0 {
		t.Errorf("allow = %v, want the EMPTY block-all list", got)
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

	_, err := SetClientBinding(ctx, st, "", ClientBinding{Discovery: strptr("lazy")}, Precondition{})
	wantErrorKind(t, err, KindUsage, CodeUsage)

	_, err = SetClientBinding(ctx, st, "cursor", ClientBinding{}, Precondition{})
	wantErrorKind(t, err, KindUsage, CodeUsage)

	_, err = SetClientBinding(ctx, st, "cursor", ClientBinding{Discovery: strptr("bogus")}, Precondition{})
	wantErrorKind(t, err, KindUsage, CodeUsage)

	_, err = SetClientBinding(ctx, st, "cursor", ClientBinding{
		Profile: &ProfileBindingSpec{Kind: registry.BindingNamed},
	}, Precondition{})
	wantErrorKind(t, err, KindUsage, CodeUsage)

	_, err = SetClientBinding(ctx, st, "cursor", ClientBinding{
		Profile: &ProfileBindingSpec{Kind: "sometimes"},
	}, Precondition{})
	wantErrorKind(t, err, KindUsage, CodeUsage)

	ghost := []string{"ghost"}
	_, err = SetClientBinding(ctx, st, "cursor", ClientBinding{Servers: &ghost}, Precondition{})
	wantErrorKind(t, err, KindNotFound, CodeServerNotFound)

	_, err = SetClientBinding(ctx, st, "cursor", ClientBinding{
		Tools: map[string]ToolSelection{"ghost": {Mode: ToolSelectAll}},
	}, Precondition{})
	wantErrorKind(t, err, KindNotFound, CodeServerNotFound)

	_, err = SetClientBinding(ctx, st, "cursor", ClientBinding{
		Tools: map[string]ToolSelection{"github": {}},
	}, Precondition{})
	wantErrorKind(t, err, KindUsage, CodeUsage)
}

func TestSetClientBindingPreconditionConflict(t *testing.T) {
	ctx := context.Background()
	st := newStore(t)
	seedServers(t, st, "a", "b")
	gen := st.Snapshot().Generation

	_, err := SetClientBinding(ctx, st, "cursor", ClientBinding{Discovery: strptr("lazy")},
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
	if _, err := SetClientBinding(ctx, st, "cursor",
		ClientBinding{Discovery: strptr("lazy")}, Precondition{}); err != nil {
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

	if _, err := SetClientBinding(ctx, st, "again",
		ClientBinding{Discovery: strptr("full")}, Precondition{}); err != nil {
		t.Fatal(err)
	}
	gen := st.Snapshot().Generation
	_, err = ClearClientBinding(ctx, st, "again", Precondition{Generation: gen - 1})
	wantStale(t, err, gen)
	if _, ok := st.Snapshot().Clients.V.Clients["again"]; !ok {
		t.Error("a stale clear deleted the binding anyway")
	}
}
