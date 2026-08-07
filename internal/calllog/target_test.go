package calllog

import "testing"

// TestTargetServer pins the one rule for "where did this call go", including
// the two cases the reading faces used to get wrong: a meta-tool is the hub
// answering, not an unrouted attempt, and a grouped listing is the same.
func TestTargetServer(t *testing.T) {
	for _, tc := range []struct {
		name                    string
		server, surface, method string
		want                    string
	}{
		{"routed tool", "github", "tool", "tools/call", "github"},
		{"routed through call_tool", "github", "meta", "tools/call", "github"},
		{"meta tool", "", "meta", "tools/call", HubServer},
		{"grouped listing", "", "group", "tools/call", HubServer},
		{"tools/list", "", "", "tools/list", HubServer},
		{"initialize", "", "", "initialize", HubServer},
		{"unroutable name", "", "unknown", "tools/call", ""},
		{"denied before routing", "", "tool", "tools/call", ""},
		{"nothing known yet", "", "", "", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := TargetServer(tc.server, tc.surface, tc.method); got != tc.want {
				t.Fatalf("TargetServer(%q, %q, %q) = %q, want %q",
					tc.server, tc.surface, tc.method, got, tc.want)
			}
		})
	}
}

func TestTargetTool(t *testing.T) {
	for _, tc := range []struct {
		name                  string
		tool, exposed, method string
		want                  string
	}{
		{"routed tool reports its raw name", "create_issue", "github__create_issue", "tools/call", "create_issue"},
		{"meta tool reports the exposed name", "", "search_tools", "tools/call", "search_tools"},
		{"tools/list reports the method", "", "", "tools/list", "tools/list"},
		{"a tools/call is never a tool name", "", "", "tools/call", ""},
		{"nothing known yet", "", "", "", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := TargetTool(tc.tool, tc.exposed, tc.method); got != tc.want {
				t.Fatalf("TargetTool(%q, %q, %q) = %q, want %q",
					tc.tool, tc.exposed, tc.method, got, tc.want)
			}
		})
	}
}

// TestHubServerIsNotABareName is the whole reason for the parentheses: the
// sentinel must not be spellable as a server id somebody would plausibly
// choose, or one filter option means two things.
func TestHubServerIsNotABareName(t *testing.T) {
	if TargetServer("agenthub", "", "tools/call") == HubServer {
		t.Fatal("a downstream server named agenthub must not collapse into the hub sentinel")
	}
}
