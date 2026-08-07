package e2e_test

import (
	"encoding/json"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

// Grouped was the one discovery mode with no end-to-end case: full is what
// most of this suite runs under, lazy has lazy_test.go's full chain, and
// grouped was covered only in-process (internal/gateway/discovery_test.go),
// against a Surface built from a hand-made scope rather than from a registry
// the real CLI wrote.
//
// The mode is worth its own e2e for a reason the other two are not: it is
// the only one that ENUMERATES a server's tools somewhere other than
// tools/list. Each <server>_tools entry names them in its description, and
// calling it returns their full schemas. That is a second projection of the
// scope-filtered set (internal/discovery/grouped.go, HandleGroup: "never a
// second path into the catalog"), and a second projection is a second place
// a narrowing can fail to apply — the only place in the product where an
// operator's allow list has to be honoured by code that is not tools/list.

// groupListing is the structured payload of a <server>_tools aggregate entry.
type groupListing struct {
	Server string `json:"server"`
	Tools  []struct {
		Tool        string          `json:"tool"`
		RawName     string          `json:"raw_name"`
		Description string          `json:"description"`
		Schema      json.RawMessage `json:"schema"`
		CallWith    string          `json:"call_with"`
	} `json:"tools"`
	Hint string `json:"hint"`
}

// openGroup calls one aggregate entry and decodes its listing.
func (c *gatewayClient) openGroup(name string, timeout time.Duration) groupListing {
	c.t.Helper()
	res := c.callTool(name, map[string]any{}, timeout)
	var out struct {
		StructuredContent json.RawMessage `json:"structuredContent"`
		IsError           bool            `json:"isError"`
	}
	if err := json.Unmarshal(res, &out); err != nil {
		c.fatalf("%s result: %v\n%s", name, err, res)
	}
	if out.IsError {
		c.fatalf("%s reported an error: %s", name, res)
	}
	var listing groupListing
	if err := json.Unmarshal(out.StructuredContent, &listing); err != nil {
		c.fatalf("%s payload: %v\n%s", name, err, out.StructuredContent)
	}
	return listing
}

// rawNames returns the raw downstream names a group listing carries.
func (l groupListing) rawNames() []string {
	out := make([]string, 0, len(l.Tools))
	for _, t := range l.Tools {
		out = append(out, t.RawName)
	}
	slices.Sort(out)
	return out
}

// TestGroupedDiscoveryFullChain is the grouped-mode acceptance path, driven
// end to end through the real spawned gateway:
//
//	two enabled servers, a profile holding one → grouped →
//	tools/list answers ONE aggregate entry plus call_tool (no bare tool
//	names, and no entry for the server the profile hides) → the aggregate
//	entry returns that server's tools with schemas → call_tool runs one →
//	call_tool refuses the hidden server's tool → widening the profile
//	brings its aggregate entry into the list.
func TestGroupedDiscoveryFullChain(t *testing.T) {
	dataDir := t.TempDir()
	twoServerProfile(t, dataDir, "narrow", "groupedclient")
	// enableServer pinned the global default to full, so every disagreement
	// below is the profile override's doing — the same shape
	// TestProfileDiscoveryOverridesTheGlobalDefault relies on.
	runProfile(t, dataDir, "discovery", "narrow", "grouped")

	c := startGateway(t, dataDir, "groupedclient")
	c.initialize()
	// Exactly this list, in this order: the aggregate entries lead and
	// call_tool comes last (internal/discovery/grouped.go, listGrouped).
	// Exactness is what makes the assertion mean something — presence alone
	// would also hold for a mode that had collapsed nothing.
	waitTools(t, c, 30*time.Second, "one aggregate entry plus call_tool", func(names []string) bool {
		return equalStrings(names, []string{"alpha_tools", "call_tool"})
	})

	listing := c.openGroup("alpha_tools", 30*time.Second)
	if listing.Server != "alpha" {
		c.fatalf("alpha_tools listing names server %q", listing.Server)
	}
	if got := listing.rawNames(); !slices.Equal(got, []string{"echo"}) {
		c.fatalf("alpha_tools listed %v, want [echo]", got)
	}
	// The deferred half of the mode's bargain: grouped ships no schemas in
	// tools/list and hands them over here instead. An entry without one
	// would leave the agent unable to call the tool it just found.
	if len(listing.Tools[0].Schema) == 0 {
		c.fatalf("alpha_tools returned no schema for echo: %+v", listing.Tools[0])
	}
	if listing.Tools[0].Tool != "alpha__echo" || listing.Tools[0].CallWith != "call_tool" {
		c.fatalf("alpha_tools entry = %+v, want alpha__echo callable through call_tool", listing.Tools[0])
	}

	// The callable identity the listing handed back actually works.
	if got := c.textContent(c.callTool("call_tool", map[string]any{
		"tool":      "alpha__echo",
		"arguments": map[string]any{"marker": "grouped-call-tool"},
	}, 30*time.Second)); !strings.Contains(got, "grouped-call-tool") {
		c.fatalf("call_tool did not reach alpha__echo: %q", got)
	}

	// Grouped mode shares lazy mode's call door byte for byte
	// (grouped.go, callToolDef), so it inherits the same way past a
	// narrowing — a name the session was never shown, typed anyway.
	hidden := c.errorText(c.callTool("call_tool", map[string]any{
		"tool":      "beta__echo",
		"arguments": map[string]any{"marker": "should-not-run"},
	}, 30*time.Second))
	if !strings.Contains(hidden, "beta__echo") {
		c.fatalf("the refusal does not name what was refused: %q", hidden)
	}

	// Widening brings the whole aggregate entry with it, which is what
	// proves beta was serving all along.
	runProfile(t, dataDir, "server", "add", "narrow", "beta")
	waitTools(t, c, 30*time.Second, "beta's aggregate entry joins the list", func(names []string) bool {
		return equalStrings(names, []string{"alpha_tools", "beta_tools", "call_tool"})
	})
	if got := c.openGroup("beta_tools", 30*time.Second).rawNames(); !slices.Equal(got, []string{"echo"}) {
		c.fatalf("beta_tools listed %v, want [echo]", got)
	}
	c.close()
}

// TestGroupedAggregateEntryHonoursTheToolAllowList drives the leak channel
// the mode adds.
//
// In full and lazy modes a withheld tool has exactly one way to become
// visible, and both are the same projection. Grouped adds two more, and
// neither is tools/list: the aggregate entry's DESCRIPTION prints the raw
// names — read by every agent in every prompt, without calling anything —
// and the entry itself returns names, descriptions and full schemas. Either
// built from the router's catalog rather than the visible set would leak
// exactly what `server tool allow` was written to withhold, while tools/list
// went on looking correct, because in this mode tools/list does not name
// tools at all.
func TestGroupedAggregateEntryHonoursTheToolAllowList(t *testing.T) {
	dataDir := t.TempDir()

	script := filepath.Join(t.TempDir(), "three.json")
	writeScript(t, script, "read_thing", "write_thing", "purge_thing")
	runAgenthub(t, dataDir, "", "server", "add", "alpha", "--cmd", fakemcpBin, "--args", script, "--json")
	enableServer(t, dataDir, "alpha")
	runAgenthub(t, dataDir, "", "server", "tool", "allow", "alpha", "--only", "read_thing")
	runAgenthub(t, dataDir, "", "profile", "create", "narrow", "--servers", "alpha")
	runAgenthub(t, dataDir, "", "profile", "tool", "allow", "narrow", "alpha", "--only", "read_thing,purge_thing")
	runAgenthub(t, dataDir, "", "client", "bind", "groupedallow", "narrow")
	runProfile(t, dataDir, "discovery", "narrow", "grouped")

	c := startGateway(t, dataDir, "groupedallow")
	c.initialize()
	waitTools(t, c, 30*time.Second, "alpha's aggregate entry", func(names []string) bool {
		return equalStrings(names, []string{"alpha_tools", "call_tool"})
	})

	// The intersection: the machine offers {read,write}, the profile names
	// {read,purge}, and purge_thing must not come back through this door
	// any more than through tools/list.
	var listing groupListing
	deadline := time.Now().Add(30 * time.Second)
	for {
		listing = c.openGroup("alpha_tools", 30*time.Second)
		if slices.Equal(listing.rawNames(), []string{"read_thing"}) {
			break
		}
		if !time.Now().Before(deadline) {
			c.fatalf("alpha_tools listed %v, want [read_thing]", listing.rawNames())
		}
		time.Sleep(300 * time.Millisecond)
	}

	// And the description, which is the copy an agent reads without calling
	// anything at all.
	desc := groupDescriptionOf(t, c, "alpha_tools")
	for _, withheld := range []string{"write_thing", "purge_thing"} {
		if strings.Contains(desc, withheld) {
			c.fatalf("the aggregate entry's description names the withheld %q: %q", withheld, desc)
		}
	}
	if !strings.Contains(desc, "read_thing") {
		c.fatalf("the aggregate entry's description names no tool at all: %q", desc)
	}
	c.close()
}

// groupDescriptionOf reads one exposed tool's description out of tools/list.
// The suite's listTools keeps only names, and here the description is the
// payload under test rather than a label.
func groupDescriptionOf(t *testing.T, c *gatewayClient, name string) string {
	t.Helper()
	res, rpcErr := c.call("tools/list", map[string]any{}, 30*time.Second)
	if rpcErr != nil {
		c.fatalf("tools/list failed: %v", rpcErr)
	}
	var list struct {
		Tools []struct {
			Name        string `json:"name"`
			Description string `json:"description"`
		} `json:"tools"`
	}
	if err := json.Unmarshal(res, &list); err != nil {
		c.fatalf("tools/list result: %v\n%s", err, res)
	}
	for _, tool := range list.Tools {
		if tool.Name == name {
			return tool.Description
		}
	}
	c.fatalf("%q is not in tools/list", name)
	return ""
}
