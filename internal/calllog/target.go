package calllog

import "github.com/dinstein/agent-hub/internal/mcp"

// HubServer names agenthub itself where a server id is expected: the hub
// answered the call rather than routing it to a downstream.
//
// The parentheses are the repository's mark for a name that is not an entry —
// the same convention profile names reserve for "(default)". A bare
// "agenthub" would be a server id somebody can add, and then one filter
// option would mean two things; this one reads as the hub even to someone who
// has never seen it before.
const HubServer = "(agenthub)"

// TargetServer says WHERE a call went, as a groupable, filterable value.
//
// It is the one rule for that question: the reading faces derive nothing of
// their own, because a label computed twice is a filter option that selects
// rows a list renders under a different name. Routing wins when it happened;
// otherwise the hub owns every call it answered itself — a meta-tool, a
// grouped listing, and the methods that are not tools/call at all.
//
// Empty means unrouted, and it is reserved for the case that describes: a
// tools/call whose name resolved to no server, or one the scope gate refused
// before a route was chosen. A tools/list is NOT unrouted.
func TargetServer(server, surface, method string) string {
	if server != "" {
		return server
	}
	if surface == "meta" || surface == "group" {
		return HubServer
	}
	if method != "" && method != mcp.MethodToolsCall {
		return HubServer
	}
	return ""
}

// TargetTool says WHAT was invoked, in the same spirit: the routed tool name
// when the call reached a server, the exposed name when the hub answered it
// (`search_tools` is a tool, and the only name that call ever had), and the
// method for a request that is not a tools/call — initialize and tools/list
// are recorded too, and a row keyed on the routed name alone leaves them
// indistinguishable from each other.
//
// Empty means the call carried no name to report: a tools/call dropped before
// its params could be read.
func TargetTool(tool, exposed, method string) string {
	if tool != "" {
		return tool
	}
	if exposed != "" {
		return exposed
	}
	if method != "" && method != mcp.MethodToolsCall {
		return method
	}
	return ""
}
