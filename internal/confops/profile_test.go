package confops

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dinstein/agent-hub/internal/registry"
)

// bindClient points a client entry at a profile through the explicit ref, so
// the rename/remove reference walk has something to find.
func bindClient(t *testing.T, st *registry.Store, client, profile string) {
	t.Helper()
	_, err := SetClientBinding(context.Background(), st, client, ClientBinding{
		Profile: &ProfileBindingSpec{Kind: registry.BindingNamed, Name: profile},
	}, Precondition{})
	if err != nil {
		t.Fatalf("bind %s: %v", client, err)
	}
}

func TestCreateProfile(t *testing.T) {
	ctx := context.Background()
	st := newStore(t)

	res, err := CreateProfile(ctx, st, "work", nil, Precondition{})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	// A fresh profile has NO narrowing: nil, not the empty (block-all) list.
	if res.Profile.Servers != nil {
		t.Errorf("fresh profile servers = %v, want nil (no narrowing)", res.Profile.Servers)
	}

	_, err = CreateProfile(ctx, st, "work", nil, Precondition{})
	wantErrorKind(t, err, KindConflict, CodeProfileExists)

	_, err = CreateProfile(ctx, st, "   ", nil, Precondition{})
	wantErrorKind(t, err, KindUsage, CodeUsage)

	gen := st.Snapshot().Generation
	_, err = CreateProfile(ctx, st, "other", nil, Precondition{Generation: gen + 1})
	wantStale(t, err, gen)
	if _, ok := st.Snapshot().Profiles.V.Profiles["other"]; ok {
		t.Error("a stale create wrote the profile anyway")
	}
}

// TestCreateProfileKeepsTheEmptyServerSetClosed: an empty list is block-all
// and must survive as such; collapsing it to nil would fail OPEN.
func TestCreateProfileKeepsTheEmptyServerSetClosed(t *testing.T) {
	res, err := CreateProfile(context.Background(), newStore(t), "locked", []string{}, Precondition{})
	if err != nil {
		t.Fatal(err)
	}
	if res.Profile.Servers == nil || len(res.Profile.Servers) != 0 {
		t.Errorf("servers = %v, want the EMPTY block-all list", res.Profile.Servers)
	}
}

// TestRenameProfileRepointsEveryReference: leaving a reference behind would
// fail-close that client to an empty scope, which is a total, silent loss of
// tool access.
func TestRenameProfileRepointsEveryReference(t *testing.T) {
	ctx := context.Background()
	st := newStore(t)
	if _, err := CreateProfile(ctx, st, "work", nil, Precondition{}); err != nil {
		t.Fatal(err)
	}
	bindClient(t, st, "claude-code", "work")

	if _, err := SetActiveProfile(ctx, st, "work", Precondition{}); err != nil {
		t.Fatal(err)
	}

	res, err := RenameProfile(ctx, st, "work", "work2", Precondition{})
	if err != nil {
		t.Fatalf("rename: %v", err)
	}
	if len(res.Repointed) != 1 || res.Repointed[0] != "claude-code" {
		t.Fatalf("repointed = %v, want [claude-code]", res.Repointed)
	}
	entry := st.Snapshot().Clients.V.Clients["claude-code"].V
	if got := entry.Binding(); got.Kind != registry.BindingNamed || got.Name != "work2" {
		t.Errorf("client binding = %+v, want named:work2", got)
	}
	if active, _ := ActiveProfile(st); active != "work2" {
		t.Errorf("active profile = %q, want work2 (the marker must follow the rename)", active)
	}
}

func TestRenameProfileValidation(t *testing.T) {
	ctx := context.Background()
	st := newStore(t)
	if _, err := CreateProfile(ctx, st, "work", nil, Precondition{}); err != nil {
		t.Fatal(err)
	}
	if _, err := CreateProfile(ctx, st, "taken", nil, Precondition{}); err != nil {
		t.Fatal(err)
	}

	_, err := RenameProfile(ctx, st, "work", "  ", Precondition{})
	wantErrorKind(t, err, KindUsage, CodeUsage)
	_, err = RenameProfile(ctx, st, "work", "work", Precondition{})
	wantErrorKind(t, err, KindUsage, CodeUsage)
	_, err = RenameProfile(ctx, st, "ghost", "x", Precondition{})
	wantErrorKind(t, err, KindNotFound, CodeProfileNotFound)
	_, err = RenameProfile(ctx, st, "work", "taken", Precondition{})
	wantErrorKind(t, err, KindConflict, CodeProfileExists)

	gen := st.Snapshot().Generation
	_, err = RenameProfile(ctx, st, "work", "work2", Precondition{Generation: gen - 1})
	wantStale(t, err, gen)
	if _, ok := st.Snapshot().Profiles.V.Profiles["work"]; !ok {
		t.Error("a stale rename moved the profile anyway")
	}
}

// TestRemoveProfileReportsDanglingClientsWithoutRewritingThem pins the
// fail-closed direction: the references stay (an empty scope), and the
// operator is told about every one of them.
func TestRemoveProfileReportsDanglingClientsWithoutRewritingThem(t *testing.T) {
	ctx := context.Background()
	st := newStore(t)
	if _, err := CreateProfile(ctx, st, "work", nil, Precondition{}); err != nil {
		t.Fatal(err)
	}
	bindClient(t, st, "cursor", "work")
	if _, err := SetActiveProfile(ctx, st, "work", Precondition{}); err != nil {
		t.Fatal(err)
	}

	res, err := RemoveProfile(ctx, st, "work", Precondition{})
	if err != nil {
		t.Fatalf("remove: %v", err)
	}
	if len(res.Dangling) != 1 || res.Dangling[0] != "cursor" {
		t.Fatalf("dangling = %v, want [cursor]", res.Dangling)
	}
	joined := strings.Join(res.Warnings, " ")
	if !strings.Contains(joined, "cursor") || !strings.Contains(joined, "EMPTY scope") {
		t.Errorf("warnings = %v, want a loud dangling-reference warning", res.Warnings)
	}
	if got := st.Snapshot().Clients.V.Clients["cursor"].V.Binding().Name; got != "work" {
		t.Errorf("the reference was rewritten to %q; it must stay dangling (fail-closed)", got)
	}
	if !res.ActiveCleared {
		t.Error("removing the active profile must clear the marker")
	}
	if active, _ := ActiveProfile(st); active != "" {
		t.Errorf("active profile = %q, want cleared", active)
	}

	_, err = RemoveProfile(ctx, st, "work", Precondition{})
	wantErrorKind(t, err, KindNotFound, CodeProfileNotFound)
}

func TestRemoveProfilePreconditionConflict(t *testing.T) {
	ctx := context.Background()
	st := newStore(t)
	if _, err := CreateProfile(ctx, st, "a", nil, Precondition{}); err != nil {
		t.Fatal(err)
	}
	if _, err := CreateProfile(ctx, st, "b", nil, Precondition{}); err != nil {
		t.Fatal(err)
	}
	gen := st.Snapshot().Generation

	_, err := RemoveProfile(ctx, st, "a", Precondition{Generation: gen - 1})
	wantStale(t, err, gen)
	if _, ok := st.Snapshot().Profiles.V.Profiles["a"]; !ok {
		t.Error("a stale removal deleted the profile anyway")
	}
}

func TestSetProfileServers(t *testing.T) {
	ctx := context.Background()
	st := newStore(t)
	seedServers(t, st, "github", "linear")
	if _, err := CreateProfile(ctx, st, "work", nil, Precondition{}); err != nil {
		t.Fatal(err)
	}

	// Naming one server turns "no narrowing" into an explicit set.
	res, err := SetProfileServers(ctx, st, "work",
		ServerSelection{Mode: ServerSetAdd, Servers: []string{"linear"}}, Precondition{})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Profile.Servers) != 1 || res.Profile.Servers[0] != "linear" {
		t.Fatalf("servers = %v", res.Profile.Servers)
	}
	res, err = SetProfileServers(ctx, st, "work",
		ServerSelection{Mode: ServerSetAdd, Servers: []string{"github"}}, Precondition{})
	if err != nil {
		t.Fatal(err)
	}
	if got := res.Profile.Servers; len(got) != 2 || got[0] != "github" || got[1] != "linear" {
		t.Errorf("servers = %v, want sorted [github linear]", got)
	}

	res, err = SetProfileServers(ctx, st, "work",
		ServerSelection{Mode: ServerSetRemove, Servers: []string{"linear"}}, Precondition{})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Profile.Servers) != 1 || res.Profile.Servers[0] != "github" {
		t.Errorf("servers = %v", res.Profile.Servers)
	}

	res, err = SetProfileServers(ctx, st, "work",
		ServerSelection{Mode: ServerSetReplace, Servers: nil}, Precondition{})
	if err != nil {
		t.Fatal(err)
	}
	if res.Profile.Servers != nil {
		t.Errorf("replace with nil must clear the narrowing, got %v", res.Profile.Servers)
	}

	// Validation.
	_, err = SetProfileServers(ctx, st, "ghost",
		ServerSelection{Mode: ServerSetAdd, Servers: []string{"github"}}, Precondition{})
	wantErrorKind(t, err, KindNotFound, CodeProfileNotFound)
	_, err = SetProfileServers(ctx, st, "work",
		ServerSelection{Mode: ServerSetAdd, Servers: []string{"ghost"}}, Precondition{})
	wantErrorKind(t, err, KindNotFound, CodeServerNotFound)
	_, err = SetProfileServers(ctx, st, "work",
		ServerSelection{Mode: ServerSetRemove, Servers: []string{"github"}}, Precondition{})
	wantErrorKind(t, err, KindNotFound, CodeNotFound) // no explicit set at all
	_, err = SetProfileServers(ctx, st, "work",
		ServerSelection{Servers: []string{"github"}}, Precondition{})
	wantErrorKind(t, err, KindUsage, CodeUsage) // unset mode is refused

	gen := st.Snapshot().Generation
	_, err = SetProfileServers(ctx, st, "work",
		ServerSelection{Mode: ServerSetAdd, Servers: []string{"github"}}, Precondition{Generation: gen - 1})
	wantStale(t, err, gen)
}

// TestSetProfileToolsThreeStates pins the state that fails open if it is got
// wrong: --none must persist the EMPTY allow list, --all must drop the rule.
func TestSetProfileToolsThreeStates(t *testing.T) {
	ctx := context.Background()
	st := newStore(t)
	seedServers(t, st, "github")
	if _, err := CreateProfile(ctx, st, "work", nil, Precondition{}); err != nil {
		t.Fatal(err)
	}

	res, err := SetProfileTools(ctx, st, "work", "github",
		ToolSelection{Mode: ToolSelectOnly, Tools: []string{"list_prs", "create_pr"}}, Precondition{})
	if err != nil {
		t.Fatal(err)
	}
	if got := res.Profile.Tools["github"].V.Allow; len(got) != 2 || got[0] != "create_pr" {
		t.Errorf("--only selector = %v, want the sorted subset", got)
	}

	res, err = SetProfileTools(ctx, st, "work", "github",
		ToolSelection{Mode: ToolSelectNone}, Precondition{})
	if err != nil {
		t.Fatal(err)
	}
	if got := res.Profile.Tools["github"].V.Allow; got == nil || len(got) != 0 {
		t.Errorf("block-all must store the EMPTY allow list, got %v", got)
	}

	res, err = SetProfileTools(ctx, st, "work", "github",
		ToolSelection{Mode: ToolSelectAll}, Precondition{})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := res.Profile.Tools["github"]; ok {
		t.Errorf("all-tools must drop the now inert rule, got %+v", res.Profile.Tools)
	}

	// Validation.
	_, err = SetProfileTools(ctx, st, "work", "github", ToolSelection{}, Precondition{})
	wantErrorKind(t, err, KindUsage, CodeUsage)
	_, err = SetProfileTools(ctx, st, "work", "github",
		ToolSelection{Mode: ToolSelectOnly}, Precondition{})
	wantErrorKind(t, err, KindUsage, CodeUsage)
	_, err = SetProfileTools(ctx, st, "ghost", "github",
		ToolSelection{Mode: ToolSelectAll}, Precondition{})
	wantErrorKind(t, err, KindNotFound, CodeProfileNotFound)
	_, err = SetProfileTools(ctx, st, "work", "ghost",
		ToolSelection{Mode: ToolSelectAll}, Precondition{})
	wantErrorKind(t, err, KindNotFound, CodeServerNotFound)

	gen := st.Snapshot().Generation
	_, err = SetProfileTools(ctx, st, "work", "github",
		ToolSelection{Mode: ToolSelectNone}, Precondition{Generation: gen - 1})
	wantStale(t, err, gen)
}

func TestSetActiveProfile(t *testing.T) {
	ctx := context.Background()
	st := newStore(t)
	if _, err := CreateProfile(ctx, st, "work", nil, Precondition{}); err != nil {
		t.Fatal(err)
	}
	// A second write so "one generation behind" is not generation 0, which
	// means "do not check".
	if _, err := CreateProfile(ctx, st, "spare", nil, Precondition{}); err != nil {
		t.Fatal(err)
	}

	if active, err := ActiveProfile(st); err != nil || active != "" {
		t.Fatalf("fresh active profile = %q, %v; want empty", active, err)
	}
	res, err := SetActiveProfile(ctx, st, "work", Precondition{})
	if err != nil {
		t.Fatal(err)
	}
	if !res.Changed || !res.Exists {
		t.Errorf("result = %+v", res)
	}
	if active, _ := ActiveProfile(st); active != "work" {
		t.Errorf("active = %q", active)
	}

	// Clearing writes the registry like any other edit — the marker lives
	// there now — so it still needs the store.
	if _, err := SetActiveProfile(ctx, st, "", Precondition{}); err != nil {
		t.Fatal(err)
	}
	if active, _ := ActiveProfile(st); active != "" {
		t.Errorf("active = %q, want cleared", active)
	}

	// Validation: a typo must not fail-close every following client.
	_, err = SetActiveProfile(ctx, st, "ghost", Precondition{})
	wantErrorKind(t, err, KindNotFound, CodeProfileNotFound)
	_, err = SetActiveProfile(ctx, nil, "work", Precondition{})
	wantErrorKind(t, err, KindUsage, CodeUsage)

	gen := st.Snapshot().Generation
	_, err = SetActiveProfile(ctx, st, "work", Precondition{Generation: gen - 1})
	wantStale(t, err, gen)
	if active, _ := ActiveProfile(st); active != "" {
		t.Errorf("a stale set wrote the marker anyway: %q", active)
	}
}

// TestActiveProfileWithoutARegistryIsNone: the failure mode of an
// unreadable marker must be "no narrowing source", never an arbitrary
// profile.
func TestActiveProfileWithoutARegistryIsNone(t *testing.T) {
	active, err := ActiveProfile(nil)
	if err != nil || active != "" {
		t.Errorf("marker without a registry = %q, %v; want empty and no error", active, err)
	}
}

// The marker used to live in <state>/active-profile.json, where the CLI
// wrote it and scope resolution never read it. Migrating it is not
// cosmetic: dropping a marker silently WIDENS what every following client
// sees, which is the one direction this codebase does not take quietly.
func TestActiveProfileMigratesOffTheOldStateFile(t *testing.T) {
	ctx := context.Background()
	st := newStore(t)
	stateDir := t.TempDir()
	if _, err := CreateProfile(ctx, st, "work", nil, Precondition{}); err != nil {
		t.Fatal(err)
	}
	if err := writeActiveProfile(stateDir, "work"); err != nil {
		t.Fatal(err)
	}

	moved, err := MigrateActiveProfile(ctx, st, stateDir)
	if err != nil || !moved {
		t.Fatalf("migrate = %v, %v; want moved", moved, err)
	}
	if active, _ := ActiveProfile(st); active != "work" {
		t.Fatalf("active = %q; the operator's narrowing was lost on upgrade", active)
	}
	// The file is retired, so the migration does not run forever.
	if _, err := os.Stat(filepath.Join(stateDir, ActiveProfileFileName)); !os.IsNotExist(err) {
		t.Errorf("the old marker file survived migration: %v", err)
	}
	// Idempotent: a second run finds nothing to move.
	if moved, err := MigrateActiveProfile(ctx, st, stateDir); err != nil || moved {
		t.Errorf("second migrate = %v, %v; want no-op", moved, err)
	}
}

// A registry value is the newer home, so a stale file must never overwrite
// it — that would resurrect a narrowing the operator already changed.
func TestMigrationNeverOverwritesTheRegistryMarker(t *testing.T) {
	ctx := context.Background()
	st := newStore(t)
	stateDir := t.TempDir()
	for _, n := range []string{"old", "current"} {
		if _, err := CreateProfile(ctx, st, n, nil, Precondition{}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := SetActiveProfile(ctx, st, "current", Precondition{}); err != nil {
		t.Fatal(err)
	}
	if err := writeActiveProfile(stateDir, "old"); err != nil {
		t.Fatal(err)
	}
	if moved, err := MigrateActiveProfile(ctx, st, stateDir); err != nil || moved {
		t.Fatalf("migrate = %v, %v; want no-op", moved, err)
	}
	if active, _ := ActiveProfile(st); active != "current" {
		t.Errorf("active = %q; a stale file overwrote the registry marker", active)
	}
}

// writeActiveProfile seeds the PRE-MIGRATION marker file. Production no
// longer writes it — MigrateActiveProfile only reads and retires it — so it
// lives here as the fixture that produces an upgrading installation.
func writeActiveProfile(stateDir, name string) error {
	return atomicWriteJSON(filepath.Join(stateDir, ActiveProfileFileName),
		activeProfileFile{Version: 1, Profile: name})
}
