package downstream

import (
	"encoding/json"
	"fmt"

	"github.com/dinstein/agent-hub/internal/mcp"
)

// maxToolPages bounds one tools/list walk. A downstream that keeps handing
// out cursors would otherwise decide how long a connect takes and how much
// memory the catalog costs, and the ceiling has to be this side's.
//
// 64 pages is far past any real server: page sizes in the wild are tens of
// tools, so this is thousands. Reaching it means the downstream is broken or
// hostile, which is why the walk stops and says so rather than continuing.
const maxToolPages = 64

// listAllTools walks every page of a tools/list result and returns the tools
// concatenated.
//
// MCP has always allowed a server to page its tool list, and 2026-07-28
// names tools/list among the paginated operations explicitly. Reading only
// the first page is the worst available failure: the tools past it are
// simply absent from the catalog, with no error and nothing in the log, so a
// user sees a server that "does not offer" a tool it does offer.
//
// call issues one request; it is the seam that lets dialAndInit run this
// against a transport that is not yet installed on the Server.
//
// Failure directions, all of them stopping the walk rather than looping or
// silently truncating:
//   - a page that fails or does not decode aborts with that error; a partial
//     catalog presented as complete is the failure this function exists to
//     prevent,
//   - a cursor equal to the one just used cannot advance, so it is refused
//     rather than followed forever,
//   - maxToolPages is a bound, and hitting it is an error, not a truncation.
//
// Only a nil NextCursor ends the walk. An empty string is a valid cursor the
// specification says MUST NOT be read as the end of results, which is why
// mcp.ListToolsResult carries a pointer.
func listAllTools(call func(params json.RawMessage) (json.RawMessage, error)) ([]mcp.ToolDef, error) {
	var (
		tools  []mcp.ToolDef
		cursor *mcp.Cursor
	)
	for page := 1; ; page++ {
		var raw json.RawMessage
		if cursor != nil {
			enc, err := json.Marshal(mcp.ListToolsParams{Cursor: cursor})
			if err != nil {
				return nil, fmt.Errorf("encode tools/list cursor: %w", err)
			}
			raw = enc
		}
		out, err := call(raw)
		if err != nil {
			return nil, err
		}
		var lr mcp.ListToolsResult
		if err := json.Unmarshal(out, &lr); err != nil {
			return nil, fmt.Errorf("decode tools/list page %d: %w", page, err)
		}
		tools = append(tools, lr.Tools...)
		if lr.NextCursor == nil {
			return tools, nil
		}
		if cursor != nil && *lr.NextCursor == *cursor {
			return nil, fmt.Errorf(
				"tools/list page %d returned the cursor it was given: the server cannot advance", page)
		}
		if page == maxToolPages {
			return nil, fmt.Errorf(
				"tools/list still paginating after %d pages (%d tools so far): refusing to keep walking",
				maxToolPages, len(tools))
		}
		cursor = lr.NextCursor
	}
}
