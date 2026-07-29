package gateway

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/dinstein/agent-hub/internal/integrity"
	"github.com/dinstein/agent-hub/internal/mcp"
	"github.com/dinstein/agent-hub/internal/platform"
	"github.com/dinstein/agent-hub/internal/testutil/fakemcp"
)

// These tests pin the property the whole tool-governance plane exists for:
// a tool the operator switched off, or that integrity isolated, must be
// invisible AND unroutable on the gateway path. Both used to fail — the
// gateway passed a zero router.Policy on every build, so `tool disable`
// reported "callable=false" while the gateway kept listing and routing the
// tool.

// stateStores opens the integrity stores under the resolver's state dir.
func stateStores(t *testing.T, resolver *platform.Resolver) (*integrity.ApprovalStore, *integrity.QuarantineStore) {
	t.Helper()
	dir, err := resolver.StateDir()
	if err != nil {
		t.Fatalf("StateDir: %v", err)
	}
	ap, err := integrity.OpenApprovalStore(dir, integrity.Options{})
	if err != nil {
		t.Fatalf("OpenApprovalStore: %v", err)
	}
	q, err := integrity.OpenQuarantineStore(dir, integrity.Options{})
	if err != nil {
		t.Fatalf("OpenQuarantineStore: %v", err)
	}
	return ap, q
}

// disableTool switches one tool off exactly the way `agenthub tool disable`
// does: observe it into the approval store, then set the Disabled flag.
func disableTool(t *testing.T, resolver *platform.Resolver, server, tool string) {
	t.Helper()
	ap, _ := stateStores(t, resolver)
	ctx := context.Background()
	snap := integrity.ToolSnapshot{
		Name:        tool,
		Description: "echoes its arguments back as text",
		InputSchema: json.RawMessage(`{"type":"object"}`),
	}
	if _, err := ap.Observe(ctx, server, snap, integrity.ModeManual); err != nil {
		t.Fatalf("Observe %s/%s: %v", server, tool, err)
	}
	if _, err := ap.SetDisabled(ctx, server, tool, true); err != nil {
		t.Fatalf("SetDisabled %s/%s: %v", server, tool, err)
	}
}

// quarantineTool isolates one EXPOSED name, the key the quarantine store
// uses (integrity doc.go, #423).
func quarantineTool(t *testing.T, resolver *platform.Resolver, exposed, server, tool string) {
	t.Helper()
	_, q := stateStores(t, resolver)
	err := q.Add(context.Background(), exposed, integrity.QuarantineEntry{
		Server: server, Tool: tool, Reason: "test drift",
	})
	if err != nil {
		t.Fatalf("quarantine.Add %s: %v", exposed, err)
	}
}

// assertNotRoutable asserts that calling exposed is refused as an unknown
// name — the same anti-probing answer a scope-hidden name gets.
func assertNotRoutable(t *testing.T, c *testClient, exposed string) {
	t.Helper()
	resp := c.call(mcp.MethodToolsCall, mcp.CallToolParams{
		Name: exposed, Arguments: json.RawMessage(`{"x":1}`),
	})
	if resp.Error == nil {
		t.Fatalf("tools/call %s succeeded; a governance-denied tool must not be routable", exposed)
	}
	if resp.Error.Code != mcp.CodeInvalidParams {
		t.Fatalf("tools/call %s error = %+v, want InvalidParams (unknown tool)", exposed, resp.Error)
	}
}

// TestDisabledToolIsNeitherListedNorRoutable is the kill switch on the data
// plane: `tool disable` must remove the tool from the catalog a live gateway
// serves, not merely from a CLI report.
func TestDisabledToolIsNeitherListedNorRoutable(t *testing.T) {
	t.Parallel()
	resolver := testResolver(t.TempDir())
	seedRegistry(t, resolver, "fake")
	disableTool(t, resolver, "fake", "danger")

	_, c, _ := startGateway(t, Config{
		ClientID: "test-client",
		Resolver: resolver,
		Dial:     scriptedDial(map[string]*fakemcp.Script{"fake": fakemcp.Minimal("danger", "safe")}),
	})
	c.initialize(mcp.ProtocolVersion, mcp.ClientCapabilities{})
	c.waitNotification(mcp.NotificationToolsListChanged)

	// "safe" proves the catalog is live; "fake__danger" must be gone.
	waitForTools(t, c, "fake__safe")
	assertNotRoutable(t, c, "fake__danger")
}

// TestQuarantinedToolIsNeitherListedNorRoutable is the same property for the
// detection side: an explicitly isolated tool must be uncallable.
func TestQuarantinedToolIsNeitherListedNorRoutable(t *testing.T) {
	t.Parallel()
	resolver := testResolver(t.TempDir())
	seedRegistry(t, resolver, "fake")
	quarantineTool(t, resolver, "fake__drifted", "fake", "drifted")

	_, c, _ := startGateway(t, Config{
		ClientID: "test-client",
		Resolver: resolver,
		Dial:     scriptedDial(map[string]*fakemcp.Script{"fake": fakemcp.Minimal("drifted", "safe")}),
	})
	c.initialize(mcp.ProtocolVersion, mcp.ClientCapabilities{})
	c.waitNotification(mcp.NotificationToolsListChanged)

	waitForTools(t, c, "fake__safe")
	assertNotRoutable(t, c, "fake__drifted")
}

// TestToolPolicyHotReload proves the kill switch does not need a restart:
// disabling a tool while the gateway is serving removes it from the live
// catalog. Without this, "disabled" would merely be deferred, not enforced.
func TestToolPolicyHotReload(t *testing.T) {
	t.Parallel()
	resolver := testResolver(t.TempDir())
	seedRegistry(t, resolver, "fake")

	_, c, _ := startGateway(t, Config{
		ClientID: "test-client",
		Resolver: resolver,
		Dial:     scriptedDial(map[string]*fakemcp.Script{"fake": fakemcp.Minimal("danger", "safe")}),
	})
	c.initialize(mcp.ProtocolVersion, mcp.ClientCapabilities{})
	c.waitNotification(mcp.NotificationToolsListChanged)
	waitForTools(t, c, "fake__danger", "fake__safe")

	disableTool(t, resolver, "fake", "danger")
	waitForTools(t, c, "fake__safe")
	assertNotRoutable(t, c, "fake__danger")

	// And back: re-enabling restores the tool without a restart.
	ap, _ := stateStores(t, resolver)
	if _, err := ap.SetDisabled(context.Background(), "fake", "danger", false); err != nil {
		t.Fatalf("SetDisabled(false): %v", err)
	}
	waitForTools(t, c, "fake__danger", "fake__safe")
}

// TestQuarantineDoesNotRenumberSiblings pins the reason quarantined entries
// are dropped AFTER exposed-name assignment: isolating one tool must not
// change the name an agent already knows for the tool it collided with.
func TestQuarantineDoesNotRenumberSiblings(t *testing.T) {
	t.Parallel()
	resolver := testResolver(t.TempDir())
	seedRegistry(t, resolver, "fake")
	// "my.tool" and "my_tool" both sanitize to "fake__my_tool", so they
	// collide: raw-name order gives "my.tool" the base name and "my_tool"
	// the "_2" suffix. Quarantining the base must leave the sibling on _2.
	quarantineTool(t, resolver, "fake__my_tool", "fake", "my.tool")

	_, c, _ := startGateway(t, Config{
		ClientID: "test-client",
		Resolver: resolver,
		Dial:     scriptedDial(map[string]*fakemcp.Script{"fake": fakemcp.Minimal("my.tool", "my_tool")}),
	})
	c.initialize(mcp.ProtocolVersion, mcp.ClientCapabilities{})
	c.waitNotification(mcp.NotificationToolsListChanged)

	// The survivor keeps the suffixed name it had before the quarantine.
	waitForTools(t, c, "fake__my_tool_2")
	assertNotRoutable(t, c, "fake__my_tool")
}

// TestUnreadableGovernanceStateFailsClosed: a corrupt approval store is what
// erasing a disable looks like, so the gateway must expose NOTHING rather
// than fall back to the full catalog.
func TestUnreadableGovernanceStateFailsClosed(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	resolver := testResolver(dir)
	seedRegistry(t, resolver, "fake")
	// Materialize the store, then corrupt it.
	disableTool(t, resolver, "fake", "danger")
	stateDir, err := resolver.StateDir()
	if err != nil {
		t.Fatalf("StateDir: %v", err)
	}
	approvals := filepath.Join(stateDir, "tool-approvals.json")
	if _, err := os.Stat(approvals); err != nil {
		t.Fatalf("approval store was not written: %v", err)
	}
	if err := os.WriteFile(approvals, []byte("{ not json"), 0o600); err != nil {
		t.Fatalf("corrupt approval store: %v", err)
	}

	_, c, _ := startGateway(t, Config{
		ClientID: "test-client",
		Resolver: resolver,
		Dial:     scriptedDial(map[string]*fakemcp.Script{"fake": fakemcp.Minimal("danger", "safe")}),
	})
	c.initialize(mcp.ProtocolVersion, mcp.ClientCapabilities{})
	c.waitNotification(mcp.NotificationToolsListChanged)

	waitForTools(t, c)
	assertNotRoutable(t, c, "fake__safe")
}
