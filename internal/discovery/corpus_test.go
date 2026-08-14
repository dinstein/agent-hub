package discovery

import (
	"encoding/json"
	"flag"
	"os"
	"path/filepath"
	"testing"

	"github.com/dinstein/agent-hub/internal/mcp"
)

// updateGolden regenerates the frozen files under testdata. Run with
//
//	go test ./internal/discovery -update
//
// and REVIEW the diff: every file under testdata is a contract (exposure
// sets, ranking order, wording), not an artefact.
var updateGolden = flag.Bool("update", false, "rewrite testdata golden files")

// corpus is the fixed tool set every golden test ranks and exposes. It is
// deliberately small, multi-server, and contains overlapping vocabulary
// ("file" in three tools, "search" in two, "status" as both a raw tool name
// and a meta-tool name) so that ordering, tie-breaking and name-shadowing
// are all exercised.
func corpus() []Tool {
	raw := []struct {
		server, tool, desc string
	}{
		{"fs", "read_file", "Read the contents of a file from disk."},
		{"fs", "write_file", "Write text to a file, creating the file if needed."},
		{"fs", "list_directory", "List the entries of a directory on disk."},
		{"git", "commit", "Record the staged changes in the repository history."},
		{"git", "log", "Show the commit log of the repository."},
		{"git", "status", "Show the working tree status of the repository."},
		{"web", "fetch_url", "Fetch a URL over HTTP and return the response body as text."},
		{"web", "search_web", "Search the web for a query and return matching links."},
	}
	out := make([]Tool, 0, len(raw))
	for _, r := range raw {
		exposed := r.server + "__" + r.tool
		out = append(out, Tool{
			Exposed:  exposed,
			ServerID: r.server,
			RawTool:  r.tool,
			Def: mcp.ToolDef{
				Name:        exposed,
				Description: r.desc,
				InputSchema: json.RawMessage(`{"type":"object","properties":{"path":{"type":"string"}}}`),
			},
		})
	}
	return out
}

// goldenPath resolves a testdata file.
func goldenPath(name string) string { return filepath.Join("testdata", name) }

// assertGolden compares got against the frozen file, or rewrites it under
// -update.
func assertGolden(t *testing.T, name string, got []byte) {
	t.Helper()
	path := goldenPath(name)
	if *updateGolden {
		if err := os.MkdirAll("testdata", 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, got, 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read golden %s: %v (run: go test ./internal/discovery -update)", path, err)
	}
	if string(got) != string(want) {
		t.Fatalf("%s drifted — determinism is contract (docs/conventions.md#engineering-conventions)\n--- got ---\n%s\n--- want ---\n%s",
			path, got, want)
	}
}

// marshalDefs renders a tool-definition list as reviewable JSON.
func marshalDefs(t *testing.T, defs []mcp.ToolDef) []byte {
	t.Helper()
	b, err := json.MarshalIndent(defs, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	return append(b, '\n')
}
