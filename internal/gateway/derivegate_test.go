package gateway

import (
	"testing"

	"github.com/dinstein/agent-hub/internal/mcp"
	"github.com/dinstein/agent-hub/internal/registry"
	"github.com/dinstein/agent-hub/internal/testutil/fakemcp"
)

// TestScopeDeniedCallAcquiresNoDerivedInstance pins the ordering that makes
// "a rejected call never reaches the downstream side" true for the connection
// plane as well as the data plane.
//
// Acquiring a derived instance is not a lookup: it can spawn a child process
// or open an authenticated remote connection for a (server, root) pair that
// has never been used before. That used to happen BEFORE pipeline.Execute —
// the comment at the call site said so explicitly, "after routing and before
// the gates" — so an out-of-scope exposed name still cost a process. The
// router deliberately does not filter by scope (visibility is a query-time
// projection; see TestHotReloadProfileNarrowRestore), so any name a client
// has ever seen, or guesses, is routable and reached that acquisition.
func TestScopeDeniedCallAcquiresNoDerivedInstance(t *testing.T) {
	t.Parallel()
	resolver := testResolver(t.TempDir())
	seedRegistry(t, resolver, "s1")
	seedDerivingServer(t, resolver, "fs", "root")

	ext := externalRegistry(t, resolver)
	updateRegistry(t, ext, func(tx *registry.Tx) {
		// The profile omits "fs" entirely: routable, invisible, out of scope.
		tx.Profiles.V.Profiles["team"] = registry.Doc[registry.Profile]{V: registry.Profile{
			Servers: []string{"s1"},
		}}
		tx.Clients.V.Clients["prof-client"] = registry.Doc[registry.ClientEntry]{V: registry.ClientEntry{
			Profile: "team",
		}}
	})

	rec := newSpecRecorder(map[string]*fakemcp.Script{
		"s1": fakemcp.Minimal("echo"),
		"fs": fakemcp.Minimal("echo"),
	})
	_, c, _ := startGateway(t, Config{ClientID: "prof-client", Resolver: resolver, Dial: rec.dial})
	c.answerRoots = true
	c.roots = []mcp.Root{{URI: "file:///w/app", Name: "app"}}
	c.initialize(mcp.ProtocolVersion, mcp.ClientCapabilities{Roots: &mcp.RootsCapability{}})
	waitForTools(t, c, "s1__echo")

	// The BASE connection to fs is expected: the connection plane is
	// independent of the visibility plane, and narrowing a profile must not
	// reconnect anything. It is the DERIVED instance that must not appear.
	waitFor(t, "the base connection to the out-of-scope server", func() bool {
		for _, s := range rec.recorded() {
			if s.ID == "fs" && s.DeriveKey == "" {
				return true
			}
		}
		return false
	})

	callBlockedWithCode(t, c, "fs__echo", "E_SCOPE_DENIED")

	if derived := rec.derivedSpecs(); len(derived) != 0 {
		t.Fatalf("a scope-denied call acquired %d derived instance(s): %+v — acquisition must happen "+
			"inside the call closure, after both gates", len(derived), derived)
	}

	// The same name in scope still derives, so the assertion above is about
	// ordering rather than about deriving having quietly stopped working.
	updateRegistry(t, ext, func(tx *registry.Tx) {
		tx.Profiles.V.Profiles["team"] = registry.Doc[registry.Profile]{V: registry.Profile{
			Servers: []string{"s1", "fs"},
		}}
	})
	waitForTools(t, c, "fs__echo", "s1__echo")
	callToolOK(t, c, "fs__echo")
	if derived := rec.derivedSpecs(); len(derived) != 1 {
		t.Fatalf("derived dials after the call came into scope = %d, want 1", len(derived))
	}
}
