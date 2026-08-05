package downstream

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/dinstein/agent-hub/internal/mcp"
)

// pager returns a call func serving names in pages of size, handing out the
// cursors it is told to. cursors[i] is the cursor emitted after page i.
func pager(t *testing.T, size int, names []string, cursorFor func(end int) string) (func(json.RawMessage) (json.RawMessage, error), *[]string) {
	t.Helper()
	var seen []string
	return func(params json.RawMessage) (json.RawMessage, error) {
		start := 0
		if len(params) > 0 {
			var p mcp.ListToolsParams
			if err := json.Unmarshal(params, &p); err != nil {
				return nil, err
			}
			if p.Cursor == nil {
				return nil, fmt.Errorf("params present but no cursor: %s", params)
			}
			seen = append(seen, *p.Cursor)
			n, err := cursorIndex(*p.Cursor, size)
			if err != nil {
				return nil, err
			}
			start = n
		}
		end := min(start+size, len(names))
		res := mcp.ListToolsResult{}
		for _, n := range names[start:end] {
			res.Tools = append(res.Tools, mcp.ToolDef{Name: n})
		}
		if end < len(names) {
			c := cursorFor(end)
			res.NextCursor = &c
		}
		raw, err := json.Marshal(res)
		return raw, err
	}, &seen
}

func cursorIndex(c string, size int) (int, error) {
	if c == "" {
		return size, nil
	}
	var n int
	if _, err := fmt.Sscanf(c, "%d", &n); err != nil {
		return 0, fmt.Errorf("bad cursor %q", c)
	}
	return n, nil
}

// TestListAllToolsWalksEveryPage is the whole point: a downstream that pages
// its tool list must contribute all of its tools, not the first page. Before
// this walk existed the rest were absent from the catalog with no error and
// nothing in the log.
func TestListAllToolsWalksEveryPage(t *testing.T) {
	names := []string{"a", "b", "c", "d", "e"}
	call, _ := pager(t, 2, names, func(end int) string { return fmt.Sprint(end) })
	got, err := listAllTools(call)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != len(names) {
		t.Fatalf("got %d tools, want %d: %+v", len(got), len(names), got)
	}
	for i, n := range names {
		if got[i].Name != n {
			t.Fatalf("tool %d = %q, want %q", i, got[i].Name, n)
		}
	}
}

// TestListAllToolsFollowsAnEmptyCursor pins the rule that is easiest to get
// wrong and impossible to see: an empty string is a valid cursor, and a
// client that reads it as the end of results loses every page after it.
func TestListAllToolsFollowsAnEmptyCursor(t *testing.T) {
	names := []string{"a", "b", "c", "d"}
	// The first boundary hands out "", the second a normal cursor.
	call, seen := pager(t, 2, names, func(end int) string {
		if end == 2 {
			return ""
		}
		return fmt.Sprint(end)
	})
	got, err := listAllTools(call)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 4 {
		t.Fatalf("got %d tools, want 4 — an empty cursor was read as the end", len(got))
	}
	if len(*seen) == 0 || (*seen)[0] != "" {
		t.Fatalf("cursors sent = %q, want the empty one first", *seen)
	}
}

// TestListAllToolsRefusesAStuckCursor: a server handing back the cursor it
// was just given cannot advance, and following it is an infinite loop that
// grows the catalog forever. It fails instead.
func TestListAllToolsRefusesAStuckCursor(t *testing.T) {
	stuck := "same"
	call := func(params json.RawMessage) (json.RawMessage, error) {
		res := mcp.ListToolsResult{Tools: []mcp.ToolDef{{Name: "a"}}, NextCursor: &stuck}
		return json.Marshal(res)
	}
	_, err := listAllTools(call)
	if err == nil || !strings.Contains(err.Error(), "cannot advance") {
		t.Fatalf("err = %v, want a refusal to follow a non-advancing cursor", err)
	}
}

// TestListAllToolsBoundsThePages: the ceiling on how long a connect takes
// belongs to this side, and reaching it is an error rather than a silent
// truncation that would present a partial catalog as a complete one.
func TestListAllToolsBoundsThePages(t *testing.T) {
	n := 0
	call := func(params json.RawMessage) (json.RawMessage, error) {
		n++
		c := fmt.Sprint(n)
		return json.Marshal(mcp.ListToolsResult{
			Tools: []mcp.ToolDef{{Name: c}}, NextCursor: &c,
		})
	}
	_, err := listAllTools(call)
	if err == nil || !strings.Contains(err.Error(), "refusing to keep walking") {
		t.Fatalf("err = %v, want the page bound to stop it", err)
	}
	if n != maxToolPages {
		t.Fatalf("made %d calls, want the bound of %d", n, maxToolPages)
	}
}

// TestListAllToolsStopsOnNilCursor: only an absent cursor ends the walk, and
// it ends it without a second request.
func TestListAllToolsStopsOnNilCursor(t *testing.T) {
	n := 0
	call := func(params json.RawMessage) (json.RawMessage, error) {
		n++
		return json.Marshal(mcp.ListToolsResult{Tools: []mcp.ToolDef{{Name: "only"}}})
	}
	got, err := listAllTools(call)
	if err != nil || len(got) != 1 || n != 1 {
		t.Fatalf("got %d tools in %d calls, err %v; want 1 in 1", len(got), n, err)
	}
}
