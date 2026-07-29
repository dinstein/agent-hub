package downstream_test

import (
	"context"
	"errors"
	"maps"
	"reflect"
	"slices"
	"testing"

	"github.com/dinstein/agent-hub/internal/downstream"
	"github.com/dinstein/agent-hub/internal/mcp/transport"
	"github.com/dinstein/agent-hub/internal/registry"
	"github.com/dinstein/agent-hub/internal/secrets"
)

// TestParseDeriveMode: the on-disk spelling, including the two directions
// that matter — an absent value is the backward-compatible "none", and an
// unknown value is an ERROR (a typo must not silently collapse isolation).
func TestParseDeriveMode(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		in      string
		want    downstream.DeriveMode
		wantErr bool
	}{
		{"", downstream.DeriveNone, false},
		{"none", downstream.DeriveNone, false},
		{"root", downstream.DeriveRoot, false},
		{"session", downstream.DeriveSession, false},
		{" root ", downstream.DeriveRoot, false},
		{"roots", downstream.DeriveNone, true},
		{"per-session", downstream.DeriveNone, true},
	} {
		got, err := downstream.ParseDeriveMode(tc.in)
		if (err != nil) != tc.wantErr {
			t.Fatalf("ParseDeriveMode(%q) err = %v, wantErr %v", tc.in, err, tc.wantErr)
		}
		if got != tc.want {
			t.Fatalf("ParseDeriveMode(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// TestSpecFromEntryDerive proves the registry field reaches the spec and
// that an invalid value fails the whole entry rather than defaulting.
func TestSpecFromEntryDerive(t *testing.T) {
	t.Parallel()
	spec, err := downstream.SpecFromEntry("fs", registry.ServerEntry{
		Transport: registry.TransportStdio, Command: "srv", Derive: "root",
	})
	if err != nil {
		t.Fatalf("SpecFromEntry: %v", err)
	}
	if spec.Derive != downstream.DeriveRoot {
		t.Fatalf("Derive = %q, want %q", spec.Derive, downstream.DeriveRoot)
	}
	if spec.DeriveKey != "" || spec.ScopeName != "" {
		t.Fatalf("a registry entry must describe the POLICY only, got key=%q scope=%q",
			spec.DeriveKey, spec.ScopeName)
	}
	if _, err := downstream.SpecFromEntry("fs", registry.ServerEntry{
		Transport: registry.TransportStdio, Command: "srv", Derive: "sesion",
	}); err == nil {
		t.Fatal("a misspelled derive mode must fail the entry, not default to none")
	}
}

// TestDeriveKeysNamespaced: a root and a session id that spell the same
// string must not collide, and an empty input means "base instance".
func TestDeriveKeys(t *testing.T) {
	t.Parallel()
	if got := downstream.RootDeriveKey("/Users/x/proj/"); got != "root:/Users/x/proj" {
		t.Fatalf("RootDeriveKey = %q", got)
	}
	if downstream.RootDeriveKey("/a") == downstream.SessionDeriveKey("/a") {
		t.Fatal("root and session key namespaces collided")
	}
	if downstream.RootDeriveKey("  ") != "" || downstream.SessionDeriveKey("") != "" {
		t.Fatal("an empty input must yield the empty (base) key")
	}
	// Idempotent over an already-normalized path, and slash-run collapsing
	// keeps two spellings of one directory on ONE instance.
	if downstream.RootDeriveKey("/a//b/") != downstream.RootDeriveKey("/a/b") {
		t.Fatal("two spellings of one root produced two keys")
	}
}

// TestSpecDerivedExpandsRoot: ${ROOT} lands in args, env values and cwd;
// explicit env overrides win; the base spec is never mutated or aliased.
func TestSpecDerivedExpandsRoot(t *testing.T) {
	t.Parallel()
	base := downstream.Spec{
		ID:      "fs",
		Kind:    transport.Stdio,
		Command: "server",
		Args:    []string{"--project", "${ROOT}", "--flag"},
		Env:     map[string]string{"WORKSPACE": "${ROOT}/src", "TOKEN": "${SECRET_T}"},
		Cwd:     "${ROOT}",
	}
	baseArgs := slices.Clone(base.Args)
	baseEnv := maps.Clone(base.Env)

	key := downstream.RootDeriveKey("/w/app")
	d := base.Derived(key, downstream.DeriveContext{
		Root: "/w/app",
		Env:  map[string]string{"ORG": "acme", "WORKSPACE": "${ROOT}/override"},
	})

	if d.ID != base.ID {
		t.Fatalf("derived spec changed the server id: %q", d.ID)
	}
	if d.DeriveKey != key || d.ScopeName != string(key) {
		t.Fatalf("key/scope = %q/%q, want %q", d.DeriveKey, d.ScopeName, key)
	}
	if want := []string{"--project", "/w/app", "--flag"}; !reflect.DeepEqual(d.Args, want) {
		t.Fatalf("Args = %v, want %v", d.Args, want)
	}
	if d.Cwd != "/w/app" {
		t.Fatalf("Cwd = %q", d.Cwd)
	}
	if d.Env["WORKSPACE"] != "/w/app/override" {
		t.Fatalf("explicit env override lost: %q", d.Env["WORKSPACE"])
	}
	if d.Env["ORG"] != "acme" {
		t.Fatalf("ORG = %q", d.Env["ORG"])
	}
	// Secret placeholders survive verbatim: resolution happens at dial time.
	if d.Env["TOKEN"] != "${SECRET_T}" {
		t.Fatalf("secret placeholder was expanded too early: %q", d.Env["TOKEN"])
	}
	if !reflect.DeepEqual(base.Args, baseArgs) || !maps.Equal(base.Env, baseEnv) {
		t.Fatal("Derived mutated the base spec")
	}
	if d.InstanceID() != "fs#"+string(key) {
		t.Fatalf("InstanceID = %q", d.InstanceID())
	}
}

// TestSpecDerivedWithoutRootKeepsPlaceholder: an empty root must NOT expand
// ${ROOT} to "" — a cwd of "" or a bare `--project ` runs against the wrong
// directory, while an unexpanded placeholder fails loudly at spawn.
func TestSpecDerivedWithoutRootKeepsPlaceholder(t *testing.T) {
	t.Parallel()
	d := downstream.Spec{ID: "fs", Cwd: "${ROOT}", Args: []string{"${ROOT}"}}.
		Derived(downstream.SessionDeriveKey("cursor:3"), downstream.DeriveContext{})
	if d.Cwd != "${ROOT}" || d.Args[0] != "${ROOT}" {
		t.Fatalf("placeholder expanded without a root: cwd=%q args=%v", d.Cwd, d.Args)
	}
}

// TestDerivedSecretsUseCompositeVaultKey is the M1 pull-forward paying off
// (docs/modules/dataplane.md early warning): a derived instance resolves (serverID,
// deriveKey, KEY) first and falls back to (serverID, "_global", KEY) when
// its own scope stores nothing.
//
// Observed through the fail-closed direction: a placeholder that resolves
// nowhere is ErrUnresolvedSecret BEFORE the spawn, so "which vault entry
// was found" is visible as "did the connect fail for the secret or for the
// missing binary".
func TestDerivedSecretsUseCompositeVaultKey(t *testing.T) {
	t.Parallel()
	appKey := downstream.RootDeriveKey("/w/app")
	base := downstream.Spec{
		ID:      "gh",
		Command: "/nonexistent/agenthub-test-binary",
		Env:     map[string]string{"T": "${SECRET_T}"},
	}

	for _, tc := range []struct {
		name string
		// stored are the vault entries that exist.
		stored   map[secrets.Ref]string
		spec     downstream.Spec
		resolved bool // true = the placeholder found a value
		wantRefs []secrets.Ref
	}{
		{
			name:     "base instance reads the global scope only",
			stored:   map[secrets.Ref]string{secrets.UserRef("gh", "T"): "global"},
			spec:     base,
			resolved: true,
			wantRefs: []secrets.Ref{secrets.UserRef("gh", "T")},
		},
		{
			name: "derived instance prefers its own scope",
			stored: map[secrets.Ref]string{
				secrets.UserRef("gh", "T"):                        "global",
				{ServerID: "gh", Scope: string(appKey), Key: "T"}: "per-root",
			},
			spec:     base.Derived(appKey, downstream.DeriveContext{Root: "/w/app"}),
			resolved: true,
			// The scoped ref is consulted FIRST and answers, so the global
			// entry is never read.
			wantRefs: []secrets.Ref{{ServerID: "gh", Scope: string(appKey), Key: "T"}},
		},
		{
			name:     "derived instance inherits the global entry",
			stored:   map[secrets.Ref]string{secrets.UserRef("gh", "T"): "global"},
			spec:     base.Derived(appKey, downstream.DeriveContext{Root: "/w/app"}),
			resolved: true,
			wantRefs: []secrets.Ref{
				{ServerID: "gh", Scope: string(appKey), Key: "T"},
				secrets.UserRef("gh", "T"),
			},
		},
		{
			name:     "neither scope stores it: fail closed, never a passthrough",
			stored:   map[secrets.Ref]string{},
			spec:     base.Derived(appKey, downstream.DeriveContext{Root: "/w/app"}),
			resolved: false,
			wantRefs: []secrets.Ref{
				{ServerID: "gh", Scope: string(appKey), Key: "T"},
				secrets.UserRef("gh", "T"),
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var asked []secrets.Ref
			resolve := func(_ context.Context, ref secrets.Ref) (string, bool, error) {
				asked = append(asked, ref)
				v, ok := tc.stored[ref]
				return v, ok, nil
			}
			_, err := downstream.Connect(context.Background(), tc.spec, downstream.Deps{Secrets: resolve})
			if err == nil {
				t.Fatal("spawning a nonexistent binary succeeded")
			}
			unresolved := errors.Is(err, downstream.ErrUnresolvedSecret)
			if unresolved == tc.resolved {
				t.Fatalf("resolved = %v, want %v (err %v)", !unresolved, tc.resolved, err)
			}
			if !reflect.DeepEqual(asked, tc.wantRefs) {
				t.Fatalf("vault lookups = %+v, want %+v", asked, tc.wantRefs)
			}
		})
	}
}
