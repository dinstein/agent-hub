package gateway

import (
	"testing"

	"github.com/dinstein/agent-hub/internal/mcp"
	"github.com/dinstein/agent-hub/internal/registry"
	"github.com/dinstein/agent-hub/internal/testutil/fakemcp"
)

// The per-project scope layer is the narrowest persisted layer and the only
// one keyed on something the CLIENT reports rather than something the
// operator writes. These tests pin that chain end to end, because the failure
// direction is fail-OPEN: a project binding that does not match is not an
// error, it silently leaves the WIDER client-level binding in force, and
// `agenthub scope ls` still lists the binding as though it applied.

// seedProjectRegistry gives client-id "proj-client" a client-level binding to
// "wide" (both servers) plus two project bindings, so a match and a non-match
// differ in the tools/list surface rather than only inside the resolver.
func seedProjectRegistry(t *testing.T, store *registry.Store) {
	t.Helper()
	updateRegistry(t, store, func(tx *registry.Tx) {
		tx.Profiles.V.Profiles["wide"] = registry.Doc[registry.Profile]{V: registry.Profile{
			Servers: []string{"s1", "s2"},
		}}
		tx.Profiles.V.Profiles["narrow"] = registry.Doc[registry.Profile]{V: registry.Profile{
			Servers: []string{"s1"},
		}}
		tx.Clients.V.Clients["proj-client"] = registry.Doc[registry.ClientEntry]{V: registry.ClientEntry{
			Profile: "wide",
			Projects: map[string]registry.Doc[registry.ProjectBinding]{
				// Both prefixes of the reported root. The longest must win;
				// if prefix matching were reversed, "/w" (block-all) would
				// apply and the surface would be empty instead of s1-only.
				"/w":          {V: registry.ProjectBinding{Servers: []string{}}},
				"/w/payments": {V: registry.ProjectBinding{Profile: "narrow"}},
			},
		}}
	})
}

// TestProjectBindingAppliesOnReportedRoot is the validation named in
// docs/backlog.md item 3: a client reporting /w/payments, with bindings on
// both /w and /w/payments, gets the longest-prefix one — and it outranks the
// client-level binding to the wider profile.
func TestProjectBindingAppliesOnReportedRoot(t *testing.T) {
	t.Parallel()
	resolver := testResolver(t.TempDir())
	seedRegistry(t, resolver, "s1", "s2")
	seedProjectRegistry(t, externalRegistry(t, resolver))

	dial := scriptedDial(map[string]*fakemcp.Script{
		"s1": fakemcp.Minimal("echo"),
		"s2": fakemcp.Minimal("echo"),
	})
	_, c, _ := startGateway(t, Config{ClientID: "proj-client", Resolver: resolver, Dial: dial})
	c.answerRoots = true
	c.roots = []mcp.Root{{URI: "file:///w/payments", Name: "payments"}}
	c.initialize(mcp.ProtocolVersion, mcp.ClientCapabilities{
		Roots: &mcp.RootsCapability{ListChanged: true},
	})

	// The root arrives via the post-initialized prefetch, so the surface
	// converges to the project layer rather than starting there. waitForTools
	// polls, which is exactly the client-visible behavior: an early list may
	// still show the wide surface.
	waitForTools(t, c, "s1__echo")
}

// TestProjectBindingIgnoredWithoutRoots pins the fail-open direction as
// DELIBERATE: a client that declares no roots capability reports no root,
// matches no project binding, and keeps the client-level (wider) binding.
// This is the pre-wiring behavior for every client, which is what makes an
// unpopulated roots cache safe rather than merely tolerable.
func TestProjectBindingIgnoredWithoutRoots(t *testing.T) {
	t.Parallel()
	resolver := testResolver(t.TempDir())
	seedRegistry(t, resolver, "s1", "s2")
	seedProjectRegistry(t, externalRegistry(t, resolver))

	dial := scriptedDial(map[string]*fakemcp.Script{
		"s1": fakemcp.Minimal("echo"),
		"s2": fakemcp.Minimal("echo"),
	})
	_, c, _ := startGateway(t, Config{ClientID: "proj-client", Resolver: resolver, Dial: dial})
	// No roots capability: the gateway must not send roots/list at all.
	c.initialize(mcp.ProtocolVersion, mcp.ClientCapabilities{})

	waitForTools(t, c, "s1__echo", "s2__echo")
	if got := c.rootsCalls.Load(); got != 0 {
		t.Errorf("roots/list sent %d times to a client without the capability, want 0", got)
	}
}

// TestProjectBindingUnmatchedRootKeepsClientLayer separates "reported a root"
// from "matched a binding": a root outside every configured prefix must fall
// back to the client layer, not to the nearest or the first binding.
func TestProjectBindingUnmatchedRootKeepsClientLayer(t *testing.T) {
	t.Parallel()
	resolver := testResolver(t.TempDir())
	seedRegistry(t, resolver, "s1", "s2")
	seedProjectRegistry(t, externalRegistry(t, resolver))

	dial := scriptedDial(map[string]*fakemcp.Script{
		"s1": fakemcp.Minimal("echo"),
		"s2": fakemcp.Minimal("echo"),
	})
	_, c, _ := startGateway(t, Config{ClientID: "proj-client", Resolver: resolver, Dial: dial})
	c.answerRoots = true
	c.roots = []mcp.Root{{URI: "file:///elsewhere", Name: "elsewhere"}}
	c.initialize(mcp.ProtocolVersion, mcp.ClientCapabilities{
		Roots: &mcp.RootsCapability{ListChanged: true},
	})

	waitForTools(t, c, "s1__echo", "s2__echo")
}
