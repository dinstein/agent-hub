package discovery

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/dinstein/agent-hub/internal/mcp"
)

// Grouped mode sits between full and lazy. Full ships N tool definitions:
// no search needed, but the whole catalog rides in every prompt. Lazy ships
// four: cheap, but every call costs a search round-trip first, and a search
// can miss.
//
// Grouped ships servers+1. The tool COUNT collapses — the expensive part of
// full is the schemas and grouped ships none of them — while the agent
// still needs NO search: each <server>_tools description NAMES that
// server's tools, so the agent reads the list it already has, calls the
// aggregate entry for the schemas it needs, then calls call_tool.
// Discovery therefore stays EXACT (a name is either printed or not
// visible); only the schemas are deferred by one round-trip.
//
// The description is where those semantics live, so its shape is frozen by
// a golden test.
const (
	// groupSuffix turns a sanitised server id into its aggregate tool name.
	groupSuffix = "_tools"

	// groupNameListLimit bounds how many tool names one description prints.
	// Beyond it the description says how many are hidden and how to get
	// them — a 500-tool server must not reintroduce the cost grouped mode
	// exists to remove.
	groupNameListLimit = 40

	// groupSchema: the aggregate entries take no arguments. Listing is a
	// projection of state, not a query.
	groupSchema = `{"type":"object","properties":{},"additionalProperties":false}`
)

// listGrouped builds the grouped tools/list: one aggregate entry per
// visible server (sorted by server id, hence by name assignment order),
// then the single call_tool entry. call_tool comes LAST so the aggregate
// entries — the ones the agent should read first — lead the list.
func (s *Surface) listGrouped() []mcp.ToolDef {
	out := make([]mcp.ToolDef, 0, len(s.serverIDs)+1)
	for _, id := range s.serverIDs {
		out = append(out, mcp.ToolDef{
			Name:        s.groupOf[id],
			Description: groupDescription(id, s.byServer[id]),
			InputSchema: json.RawMessage(groupSchema),
		})
	}
	out = append(out, callToolDef())
	return out
}

// callToolDef returns the shared call_tool definition — byte-identical to
// the lazy one. One entry point, one schema, one wording: an agent that
// learned call_tool under lazy mode uses it unchanged under grouped mode.
func callToolDef() mcp.ToolDef {
	for _, d := range metaDefs {
		if d.Name == MetaCallTool {
			return d
		}
	}
	panic("discovery: call_tool missing from metaDefs") // unreachable by construction
}

// groupDescription is the frozen aggregate-entry description: what the
// server is, how many tools it has, their names (bounded), and the two-step
// instruction. Names are the RAW tool names as printed for orientation; the
// callable identity is the exposed name, which the aggregate entry returns.
func groupDescription(serverID string, tools []Tool) string {
	names := make([]string, 0, len(tools))
	for i, t := range tools {
		if i == groupNameListLimit {
			names = append(names, fmt.Sprintf("(+%d more)", len(tools)-groupNameListLimit))
			break
		}
		names = append(names, t.RawTool)
	}
	list := strings.Join(names, ", ")
	if list == "" {
		list = "(none)"
	}
	return fmt.Sprintf(
		"Tools of server %q (%d): %s. Call this entry for their full input schemas, "+
			"then run one with %s{tool, arguments}.",
		serverID, len(tools), list, MetaCallTool)
}

// groupEntry is one tool in an aggregate listing: the callable exposed
// name, the raw name for orientation, the full description and the full
// input schema. Grouped mode defers schemas by one call — it does not
// summarise them, because the agent that called this entry has already
// committed to this server.
type groupEntry struct {
	Tool        string          `json:"tool"`
	RawName     string          `json:"raw_name"`
	Description string          `json:"description"`
	Schema      json.RawMessage `json:"schema"`
	CallWith    string          `json:"call_with"`
}

type groupPayload struct {
	Server string       `json:"server"`
	Tools  []groupEntry `json:"tools"`
	Hint   string       `json:"hint"`
}

// groupHint is the frozen closing instruction of an aggregate listing.
const groupHint = "call one of these with call_tool{tool, arguments}"

// HandleGroup is the handler for a <server>_tools aggregate entry. name is
// the tool name the client called; an unknown one yields the same
// anti-probing error as an unknown tool.
//
// Only VISIBLE tools are listed — the aggregate entry is a projection of
// the same scope-filtered set as tools/list and search_tools, never a
// second path into the catalog (docs/architecture.md §7).
func (s *Surface) HandleGroup(name string) *mcp.CallResult {
	id, ok := s.groupOwner[name]
	if !ok {
		return ErrorResult(newError(CodeUnknownTool,
			fmt.Sprintf("no tool named %q is available", name)))
	}
	tools := s.byServer[id]
	entries := make([]groupEntry, 0, len(tools))
	for _, t := range tools {
		entries = append(entries, groupEntry{
			Tool:        t.Exposed,
			RawName:     t.RawTool,
			Description: describe(t),
			Schema:      schemaOf(t),
			// Grouped mode keeps the SINGLE call door even when intent
			// variants are on: docs/architecture.md §9 splits lazy mode's call_tool,
			// and grouped mode's whole point is that the agent already sees
			// every callable name — there is no allowlist pressure to
			// relieve, and a second door shape would be a second contract.
			CallWith: callWithFor(t, false),
		})
	}
	body, err := json.Marshal(groupPayload{Server: id, Tools: entries, Hint: groupHint})
	if err != nil {
		return ErrorResult(newError(CodeInvalidArgs, "tool listing could not be encoded"))
	}
	return &mcp.CallResult{Content: textContent(string(body)), StructuredContent: body}
}
