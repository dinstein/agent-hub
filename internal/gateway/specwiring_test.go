package gateway

import (
	"context"
	"testing"

	"github.com/dinstein/agent-hub/internal/downstream"
	"github.com/dinstein/agent-hub/internal/mcp/transport"
	"github.com/dinstein/agent-hub/internal/platform"
	"github.com/dinstein/agent-hub/internal/registry"
	"github.com/dinstein/agent-hub/internal/secrets"
)

// The gateway is the path that serves real clients, so a registry field it
// does not carry is a field that does not exist in practice. It used to
// hand-build its specs instead of calling downstream.SpecFromEntry, and the
// container runtime, http endpoints, headers and provenance were all
// silently dropped there — a server configured as contained ran on the host
// with nothing saying so. These tests pin the translation, not the fields:
// they fail if anyone reintroduces a second, divergent conversion.

func seedEntry(t *testing.T, resolver *platform.Resolver, id string, entry registry.ServerEntry) {
	t.Helper()
	dir, err := resolver.RegistryDir()
	if err != nil {
		t.Fatalf("RegistryDir: %v", err)
	}
	store, err := registry.Open(dir)
	if err != nil {
		t.Fatalf("registry.Open: %v", err)
	}
	err = store.Update(context.Background(), func(tx *registry.Tx) error {
		if tx.Servers.V.Servers == nil {
			tx.Servers.V.Servers = map[string]registry.Doc[registry.ServerEntry]{}
		}
		tx.Servers.V.Servers[id] = registry.Doc[registry.ServerEntry]{V: entry}
		return nil
	})
	if err != nil {
		t.Fatalf("seed %q: %v", id, err)
	}
}

// specsOf assembles a gateway far enough to read what it made of the
// registry, without running it.
func specsOf(t *testing.T, resolver *platform.Resolver) []downstream.Spec {
	t.Helper()
	g, _, _ := startGateway(t, Config{ClientID: "spec-test", Resolver: resolver})
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.specs
}

// The OAuth bearer is the same class of omission as the dropped container
// runtime above: Deps.Auth nil means "attach no credential and attempt no
// refresh", so every HTTP downstream got a bare request and answered 401
// while `auth ls` reported the very same tokens as authorized. The gateway
// held the vault and never opened it. These tests pin that the dial path
// carries a credential source, not what the credential is.

func TestGatewayAttachesACredentialSourceToHTTPDownstreams(t *testing.T) {
	resolver := testResolver(t.TempDir())
	g, _, _ := startGateway(t, Config{ClientID: "auth-test", Resolver: resolver,
		Auth: func(serverID, scopeName string) downstream.TokenSource {
			return downstream.NewScopedVaultTokenSource(serverID, scopeName, nil, nil)
		}})
	deps := g.downstreamDeps()
	if deps.AuthFor == nil {
		t.Fatal("the gateway dials with no credential source: every OAuth downstream would 401")
	}
	if ts := deps.AuthFor(downstream.Spec{ID: "remote", Kind: transport.StreamableHTTP}); ts == nil {
		t.Fatal("no TokenSource for an HTTP downstream: the bearer would never be attached")
	}
}

// A stdio child receives its credentials through the environment, which
// Secrets already covers; handing it a bearer would be a second, divergent
// path to the same vault entry.
func TestStdioDownstreamsGetNoBearer(t *testing.T) {
	resolver := testResolver(t.TempDir())
	g, _, _ := startGateway(t, Config{ClientID: "auth-test", Resolver: resolver,
		Auth: func(serverID, scopeName string) downstream.TokenSource {
			return downstream.NewScopedVaultTokenSource(serverID, scopeName, nil, nil)
		}})
	authFor := g.downstreamDeps().AuthFor
	if authFor == nil {
		t.Fatal("the gateway dials with no credential source")
	}
	if ts := authFor(downstream.Spec{ID: "local", Kind: transport.Stdio}); ts != nil {
		t.Fatal("a stdio child was handed an HTTP bearer")
	}
}

// The credential is keyed on (server, scope): Deps is shared by every
// instance of the derived pool, so a source that ignored the spec would hand
// one derivation's identity to another. Empty ScopeName is the base
// instance and must resolve to the default scope, matching how the same
// spec's ${SECRET_X} placeholders are looked up.
func TestDerivedInstanceCarriesItsOwnVaultScope(t *testing.T) {
	resolver := testResolver(t.TempDir())
	var got []string
	g, _, _ := startGateway(t, Config{ClientID: "auth-test", Resolver: resolver,
		Auth: func(serverID, scopeName string) downstream.TokenSource {
			got = append(got, serverID+"/"+scopeName)
			return nil
		}})
	authFor := g.downstreamDeps().AuthFor
	if authFor == nil {
		t.Fatal("the gateway dials with no credential source")
	}
	authFor(downstream.Spec{ID: "remote", Kind: transport.StreamableHTTP})
	authFor(downstream.Spec{ID: "remote", Kind: transport.StreamableHTTP, ScopeName: "root-a"})
	want := []string{"remote/" + secrets.DefaultScope, "remote/root-a"}
	if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("vault scopes = %v, want %v", got, want)
	}
}

func TestContainerRuntimeSurvivesIntoTheGatewaySpec(t *testing.T) {
	resolver := testResolver(t.TempDir())
	seedEntry(t, resolver, "contained", registry.ServerEntry{
		Transport: "stdio",
		Command:   "server",
		Enabled:   true,
		Runtime:   registry.RuntimeDocker,
		Docker: &registry.DockerRuntime{
			Image:  "example/mcp:1",
			Mounts: []registry.DockerMount{{Source: "/host/data"}},
		},
	})
	specs := specsOf(t, resolver)
	if len(specs) != 1 {
		t.Fatalf("specs = %d, want 1", len(specs))
	}
	if specs[0].Docker == nil {
		t.Fatal("the gateway dropped the container runtime: this server would run on the host")
	}
	if specs[0].Docker.Image != "example/mcp:1" {
		t.Errorf("image = %q", specs[0].Docker.Image)
	}
	if len(specs[0].Docker.Mounts) != 1 || specs[0].Docker.Mounts[0].Write {
		t.Errorf("mounts = %+v, want one read-only mount", specs[0].Docker.Mounts)
	}
}

func TestHTTPDownstreamReachesTheGatewaySpec(t *testing.T) {
	resolver := testResolver(t.TempDir())
	seedEntry(t, resolver, "remote", registry.ServerEntry{
		Transport: registry.TransportHTTP,
		URL:       "https://example.test/mcp",
		Enabled:   true,
		Headers:   map[string]string{"X-Team": "core"},
	})
	specs := specsOf(t, resolver)
	if len(specs) != 1 {
		t.Fatalf("specs = %d, want 1 (an http downstream must not be skipped)", len(specs))
	}
	if specs[0].Kind != transport.StreamableHTTP {
		t.Errorf("kind = %q, want %q", specs[0].Kind, transport.StreamableHTTP)
	}
	if specs[0].URL != "https://example.test/mcp" || specs[0].Headers["X-Team"] != "core" {
		t.Errorf("url/headers = %q / %v", specs[0].URL, specs[0].Headers)
	}
}

func TestUnusableEntryDisablesOnlyItself(t *testing.T) {
	resolver := testResolver(t.TempDir())
	seedEntry(t, resolver, "broken", registry.ServerEntry{
		Transport: "stdio", Command: "server", Enabled: true, Runtime: "dcoker",
	})
	seedEntry(t, resolver, "fine", registry.ServerEntry{
		Transport: "stdio", Command: "server", Enabled: true,
	})
	specs := specsOf(t, resolver)
	if len(specs) != 1 || specs[0].ID != "fine" {
		t.Fatalf("specs = %+v, want only the healthy entry", specs)
	}
}

func TestSpecEqualSeesContainerChanges(t *testing.T) {
	base := downstream.Spec{ID: "s", Kind: transport.Stdio, Command: "server"}
	contained := base
	contained.Docker = &transport.DockerConfig{Image: "example/mcp:1"}
	rebuilt := base
	rebuilt.Docker = &transport.DockerConfig{Image: "example/mcp:2"}

	if specEqual(base, contained) {
		t.Error("host and contained specs must differ, or enabling isolation would not reconnect")
	}
	if specEqual(contained, rebuilt) {
		t.Error("an image change must reconnect; otherwise the old container keeps serving")
	}
	same := contained
	same.Docker = &transport.DockerConfig{Image: "example/mcp:1"}
	if !specEqual(contained, same) {
		t.Error("identical container configs must compare equal, or every reload respawns")
	}
}
