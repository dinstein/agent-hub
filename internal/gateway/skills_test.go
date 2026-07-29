package gateway

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/dinstein/agent-hub/internal/mcp"
	"github.com/dinstein/agent-hub/internal/platform"
	"github.com/dinstein/agent-hub/internal/registry"
	"github.com/dinstein/agent-hub/internal/skills"
	"github.com/dinstein/agent-hub/internal/testutil/fakemcp"
)

// seedSkill imports one enabled skill into the library the gateway reads
// (<data>/skills) and returns its exposed tool name.
func seedSkill(t *testing.T, resolver *platform.Resolver, id, body string) string {
	t.Helper()
	data, err := resolver.DataDir()
	if err != nil {
		t.Fatalf("DataDir: %v", err)
	}
	mgr, err := skills.Open(filepath.Join(data, skillsDirName), skills.Options{HomeDir: t.TempDir()})
	if err != nil {
		t.Fatalf("skills.Open: %v", err)
	}
	src := t.TempDir()
	md := "---\nname: " + id + "\ndescription: fixture skill\nversion: 1.0.0\n---\n\n" + body + "\n"
	if err := os.WriteFile(filepath.Join(src, skills.SkillFileName), []byte(md), 0o600); err != nil {
		t.Fatalf("write SKILL.md: %v", err)
	}
	sk, err := mgr.Add(context.Background(), skills.AddRequest{Path: src, ID: id})
	if err != nil {
		t.Fatalf("skill add: %v", err)
	}
	if _, err := mgr.Enable(context.Background(), sk.ID); err != nil {
		t.Fatalf("skill enable: %v", err)
	}
	return "skills__" + skills.RawToolName(sk.ID)
}

// TestSkillsOverMCPOffByDefault: the governance switch defaults OFF, so a
// populated library adds nothing to the surface (docs/modules/config.md — a new
// supply channel of untrusted text is opted into, never inherited).
func TestSkillsOverMCPOffByDefault(t *testing.T) {
	t.Parallel()
	resolver := testResolver(t.TempDir())
	seedRegistry(t, resolver, "fake")
	exposed := seedSkill(t, resolver, "pdf", "Use pdftotext.")

	_, c, _ := startGateway(t, Config{
		ClientID: "test-client",
		Resolver: resolver,
		Dial:     scriptedDial(map[string]*fakemcp.Script{"fake": fakemcp.Minimal("echo")}),
	})
	c.initialize(mcp.ProtocolVersion, mcp.ClientCapabilities{})
	waitForTools(t, c, "fake__echo")

	if slices.Contains(toolNames(c.listTools()), exposed) {
		t.Fatal("skills appeared without the governance switch")
	}
	resp := c.call(mcp.MethodToolsCall, mcp.CallToolParams{Name: exposed})
	if resp.Error == nil {
		t.Fatal("a skill tool answered while the face is off")
	}
}

// TestSkillsOverMCPExposesAndServes is the enabled path: the skill is a
// namespaced tool, RouteOf attributes it to the "skills" pseudo-server, and
// calling it returns SKILL.md through the ordinary pipeline (the gate
// counters prove the chain was not forked).
func TestSkillsOverMCPExposesAndServes(t *testing.T) {
	t.Parallel()
	resolver := testResolver(t.TempDir())
	seedRegistry(t, resolver, "fake")
	exposed := seedSkill(t, resolver, "pdf", "Use pdftotext.")
	ext := externalRegistry(t, resolver)
	setGovernance(t, ext, func(g *registry.GovernanceDoc) { g.SkillsOverMCP = true })

	g, c, _ := startGateway(t, Config{
		ClientID: "test-client",
		Resolver: resolver,
		Dial:     scriptedDial(map[string]*fakemcp.Script{"fake": fakemcp.Minimal("echo")}),
	})
	c.initialize(mcp.ProtocolVersion, mcp.ClientCapabilities{})
	waitForTools(t, c, "fake__echo", exposed)

	// Provenance: the skill tool routes to the skills pseudo-server, never
	// to a downstream, and the route is a map lookup (no name splitting).
	rt, _, _ := g.catalog()
	route, ok := rt.RouteOf(exposed)
	if !ok || route.ServerID != skills.ProviderID {
		t.Fatalf("RouteOf(%q) = %+v ok %v", exposed, route, ok)
	}

	before := g.pipe.Counters()
	res := callToolResult(t, c, exposed, map[string]any{})
	if res.IsError {
		t.Fatalf("skill call reported an error: %s", res.Content)
	}
	var items []struct {
		Text string `json:"text"`
	}
	if err := json.Unmarshal(res.Content, &items); err != nil || len(items) == 0 {
		t.Fatalf("unexpected content %s (err %v)", res.Content, err)
	}
	if !strings.Contains(items[0].Text, "Use pdftotext.") {
		t.Fatalf("skill body missing from the reply:\n%s", items[0].Text)
	}
	after := g.pipe.Counters()
	for _, gate := range g.pipe.GateNames() {
		if after[gate] <= before[gate] {
			t.Fatalf("gate %q did not run for a skills call: %d → %d", gate, before[gate], after[gate])
		}
	}
}

// TestSkillsFaceIsAScopeSubject: the pseudo-server obeys the ordinary
// five-layer scope chain. A profile that lists its servers explicitly and
// omits "skills" hides the whole face — that IS the docs/modules/config.md
// skillScope chain, expressed in the chain that already exists.
func TestSkillsFaceIsAScopeSubject(t *testing.T) {
	t.Parallel()
	resolver := testResolver(t.TempDir())
	seedRegistry(t, resolver, "fake")
	exposed := seedSkill(t, resolver, "pdf", "Use pdftotext.")
	ext := externalRegistry(t, resolver)
	setGovernance(t, ext, func(g *registry.GovernanceDoc) { g.SkillsOverMCP = true })

	_, c, _ := startGateway(t, Config{
		ClientID: "narrowed",
		Resolver: resolver,
		Dial:     scriptedDial(map[string]*fakemcp.Script{"fake": fakemcp.Minimal("echo")}),
	})
	c.initialize(mcp.ProtocolVersion, mcp.ClientCapabilities{})
	waitForTools(t, c, "fake__echo", exposed)

	// A profile narrowing visibility to "fake" removes the face.
	updateRegistry(t, ext, func(tx *registry.Tx) {
		tx.Profiles.V.Profiles = map[string]registry.Doc[registry.Profile]{
			"only-fake": {V: registry.Profile{Servers: []string{"fake"}}},
		}
		tx.Clients.V.Clients = map[string]registry.Doc[registry.ClientEntry]{
			"narrowed": {V: registry.ClientEntry{Profile: "only-fake"}},
		}
	})
	waitForTools(t, c, "fake__echo")

	// Hidden means BLOCKED, not merely unlisted: the scope gate owns the
	// rejection on the execute path.
	resp := c.call(mcp.MethodToolsCall, mcp.CallToolParams{Name: exposed, Arguments: json.RawMessage(`{}`)})
	if resp.Error == nil {
		t.Fatal("a scope-hidden skill tool was still callable")
	}
}
