package ctlapi

import (
	"net/http"
	"testing"

	"github.com/dinstein/agent-hub/internal/confops"
)

// TestToolsListOrderIsStableAcrossCalls pins the ordering of GET /v1/tools.
//
// handleToolsList assembles the body by ranging over maps — the registry's
// servers, then the override document's servers and their tools — and Go
// randomizes map iteration deliberately. The SortFunc at the end is not
// tidiness; it is the only reason two identical requests return the same
// bytes.
//
// The fixture is built around which key actually decides what. Approval
// records arrive from integrity.ListServer already sorted by tool, so rows
// that come from a record would stay ordered even without the second key.
// The rows that would NOT are the override-only ones — an override on a tool
// with no approval record, which handleToolsList lists on purpose because a
// neutralized description is a governance fact even for a tool nobody has
// observed. Those are appended straight out of a nested map range, so this
// test seeds three of them under one server: Server is equal for all three
// and Tool is the only thing left deciding.
//
// Verified by mutation, all three against this test: dropping the Tool
// comparison fails it, swapping the two keys fails it, and returning a
// constant fails it. An earlier version of this fixture used only observed
// tools and was blind to the first of those — the pre-sorted input hid it.
func TestToolsListOrderIsStableAcrossCalls(t *testing.T) {
	env, stateDir, _ := adminServer(t, nil)
	seedServer(t, env.reg, "github", true)
	seedServer(t, env.reg, "azure", true)
	observeTool(t, stateDir, "github", "read_file")
	observeTool(t, stateDir, "azure", "read_blob")

	// Override-only rows: no approval record, so they take the map-ranging
	// path and nothing pre-sorts them.
	if err := confops.SaveToolOverrides(stateDir, confops.ToolOverrides{
		Overrides: map[string]map[string]confops.ToolOverride{
			"github": {
				"zeta":  {Description: "neutralized"},
				"alpha": {Description: "neutralized"},
				"mid":   {Description: "neutralized"},
			},
		},
	}); err != nil {
		t.Fatalf("seeding overrides: %v", err)
	}

	type row struct {
		Server string `json:"server"`
		Tool   string `json:"tool"`
	}
	get := func() []row {
		var list struct {
			Tools []row `json:"tools"`
		}
		doAdmin(t, env.sock, http.MethodGet, "/v1/tools", nil).decode(t, &list)
		return list.Tools
	}

	want := []row{
		{"azure", "read_blob"},
		{"github", "alpha"},
		{"github", "mid"},
		{"github", "read_file"},
		{"github", "zeta"},
	}
	first := get()
	if len(first) != len(want) {
		t.Fatalf("tools = %+v, want %d rows", first, len(want))
	}
	for i, w := range want {
		if first[i] != w {
			t.Fatalf("row %d = %+v, want %+v (full list %+v)", i, first[i], w, first)
		}
	}

	// Map iteration order varies per range statement rather than per process,
	// so one agreeing call proves little. Ten do.
	for i := range 10 {
		again := get()
		if len(again) != len(first) {
			t.Fatalf("call %d returned %d rows, first returned %d", i+2, len(again), len(first))
		}
		for j := range again {
			if again[j] != first[j] {
				t.Fatalf("call %d disagrees with the first at row %d: %+v vs %+v",
					i+2, j, again[j], first[j])
			}
		}
	}
}
