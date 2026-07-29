package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// seedToolCache writes one gateway tool-cache entry by hand, in the exact
// on-disk shape internal/gateway writes.
func seedToolCache(t *testing.T, dataDir, serverID string, tools []map[string]any) {
	t.Helper()
	dir := filepath.Join(dataDir, "cache", "tools")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	data, err := json.Marshal(map[string]any{
		"server":  serverID,
		"savedAt": time.Unix(0, 0).UTC(),
		"tools":   tools,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, serverID+".json"), data, 0o600); err != nil {
		t.Fatal(err)
	}
}

// seedCatalog sets up two enabled servers with cached tools plus one
// disabled server whose cached tools must never be listed.
func seedCatalog(t *testing.T) string {
	t.Helper()
	dir := setDataDir(t)
	runCLI(t, "", "server", "add", "fs", "--cmd", "fs-server")
	runCLI(t, "", "server", "add", "git", "--cmd", "git-server")

	seedToolCache(t, dir, "fs", []map[string]any{
		{"name": "read_file", "description": "Read a file from disk and return its contents.",
			"inputSchema": map[string]any{"type": "object"}},
		{"name": "write_file", "description": "Write   a file\nto disk.",
			"inputSchema": map[string]any{"type": "object"}},
	})
	seedToolCache(t, dir, "git", []map[string]any{
		{"name": "log", "description": "Show the commit log.",
			"inputSchema": map[string]any{"type": "object"}},
	})
	return dir
}

func decodeToolRows(t *testing.T, out string) []ToolRow {
	t.Helper()
	env := decodeEnvelope(t, out)
	if !env.OK {
		t.Fatalf("envelope not ok: %s", out)
	}
	var rows []ToolRow
	if err := json.Unmarshal(env.Data, &rows); err != nil {
		t.Fatalf("data is not a tool list: %v\n%s", err, env.Data)
	}
	return rows
}

// The plain listing is the aggregated catalog in exposed-name order. The
// human table is frozen: determinism is contract, and an operator's table is
// as much a contract as a wire format (canonical.md §6).
func TestToolLsGolden(t *testing.T) {
	seedCatalog(t)

	code, out, stderr := runCLI(t, "", "tool", "ls")
	if code != ExitOK {
		t.Fatalf("exit = %d, stderr: %s", code, stderr)
	}
	const want = "NAME            SERVER  TOOL        DESCRIPTION\n" +
		"fs__read_file   fs      read_file   Read a file from disk and return its contents.\n" +
		"fs__write_file  fs      write_file  Write a file to disk.\n" +
		"git__log        git     log         Show the commit log.\n"
	if out != want {
		t.Errorf("tool ls =\n%q\nwant\n%q", out, want)
	}

	rows := decodeToolRowsFromCLI(t, "tool", "ls", "--json")
	if len(rows) != 3 {
		t.Fatalf("rows = %+v, want 3", rows)
	}
	if rows[0].Name != "fs__read_file" || rows[0].Server != "fs" || rows[0].RawName != "read_file" {
		t.Errorf("row 0 = %+v", rows[0])
	}
	// Ranking fields stay absent outside --search.
	if rows[0].Rank != 0 || rows[0].Score != 0 {
		t.Errorf("plain listing carries ranking fields: %+v", rows[0])
	}
	// The JSON description is the FULL text; only the table column truncates.
	if rows[1].Description != "Write   a file\nto disk." {
		t.Errorf("row 1 description = %q, want the verbatim description", rows[1].Description)
	}
}

func decodeToolRowsFromCLI(t *testing.T, args ...string) []ToolRow {
	t.Helper()
	code, out, stderr := runCLI(t, "", args...)
	if code != ExitOK {
		t.Fatalf("%v exit = %d, stderr: %s", args, code, stderr)
	}
	return decodeToolRows(t, out)
}

// --search reuses the discovery ranker, so the CLI order IS the order
// search_tools reports: score descending, exposed name ascending.
func TestToolLsSearchGolden(t *testing.T) {
	seedCatalog(t)

	code, out, stderr := runCLI(t, "", "tool", "ls", "--search", "read file")
	if code != ExitOK {
		t.Fatalf("exit = %d, stderr: %s", code, stderr)
	}
	const want = "RANK  SCORE  NAME            SERVER  TOOL        DESCRIPTION\n" +
		"1     82     fs__read_file   fs      read_file   Read a file from disk and return its contents.\n" +
		"2     41     fs__write_file  fs      write_file  Write a file to disk.\n"
	if out != want {
		t.Errorf("tool ls --search =\n%q\nwant\n%q", out, want)
	}

	rows := decodeToolRowsFromCLI(t, "tool", "ls", "--search", "read file", "--json")
	if len(rows) != 2 || rows[0].Rank != 1 || rows[0].Name != "fs__read_file" || rows[0].Score <= rows[1].Score {
		t.Fatalf("search rows = %+v", rows)
	}

	// A query nothing matches is an empty list, not an error.
	code, out, _ = runCLI(t, "", "tool", "ls", "--search", "kubernetes")
	if code != ExitOK {
		t.Fatalf("exit = %d, want %d", code, ExitOK)
	}
	if out != "no tool matches this query\n" {
		t.Errorf("empty search output = %q", out)
	}
}

// A server argument narrows to that server; an unknown one is exit 3 with
// the server-not-found code (the frozen table of docs/modules/controlplane.md).
func TestToolLsServerArgument(t *testing.T) {
	seedCatalog(t)

	rows := decodeToolRowsFromCLI(t, "tool", "ls", "git", "--json")
	if len(rows) != 1 || rows[0].Name != "git__log" {
		t.Fatalf("rows = %+v, want only git__log", rows)
	}

	code, out, _ := runCLI(t, "", "tool", "ls", "ghost", "--json")
	if code != ExitNotFound {
		t.Fatalf("exit = %d, want %d", code, ExitNotFound)
	}
	env := decodeEnvelope(t, out)
	if env.OK || env.Error == nil || env.Error.Code != CodeServerNotFound {
		t.Errorf("envelope = %s, want a %s failure", out, CodeServerNotFound)
	}
}

// A disabled server's cached tools are not listed: the CLI mirrors what a
// gateway would aggregate, and a disabled server is not aggregated.
func TestToolLsSkipsDisabledServers(t *testing.T) {
	dir := seedCatalog(t)
	runCLI(t, "", "server", "rm", "git")
	seedToolCache(t, dir, "git", []map[string]any{
		{"name": "log", "description": "Show the commit log."},
	})

	rows := decodeToolRowsFromCLI(t, "tool", "ls", "--json")
	for _, r := range rows {
		if r.Server == "git" {
			t.Fatalf("unregistered server surfaced in the listing: %+v", rows)
		}
	}
}

// An empty cache is a helpful empty listing, never an error: nothing has
// connected yet is a state, not a failure.
func TestToolLsEmptyCache(t *testing.T) {
	setDataDir(t)
	code, out, stderr := runCLI(t, "", "tool", "ls")
	if code != ExitOK {
		t.Fatalf("exit = %d, stderr: %s", code, stderr)
	}
	const want = "no tools cached yet (connect a client once so the gateway can populate the cache)\n"
	if out != want {
		t.Errorf("output = %q, want %q", out, want)
	}
	code, out, _ = runCLI(t, "", "tool", "ls", "--json")
	if code != ExitOK {
		t.Fatalf("exit = %d", code)
	}
	if env := decodeEnvelope(t, out); string(env.Data) != "[]" {
		t.Errorf("empty listing must serialize as [], got %s", env.Data)
	}
}

// An invalid query is a usage error (exit 2) carrying the ranker's own
// frozen message — the CLI and the meta-tool must say the same thing.
func TestToolLsRejectsInvalidQuery(t *testing.T) {
	seedCatalog(t)
	code, out, _ := runCLI(t, "", "tool", "ls", "--search", "!!!", "--json")
	if code != ExitUsage {
		t.Fatalf("exit = %d, want %d", code, ExitUsage)
	}
	env := decodeEnvelope(t, out)
	if env.Error == nil || env.Error.Message != "--search: query must not be empty" {
		t.Errorf("error = %s, want the ranker's frozen message", out)
	}
}

// oneLine is what keeps a verbose or multi-line description from wrecking
// the table; the bound is bytes, the cut lands on a rune boundary.
func TestOneLine(t *testing.T) {
	cases := []struct {
		in   string
		max  int
		want string
	}{
		{"", 10, ""},
		{"  spaced   out\ttext\n", 40, "spaced out text"},
		{"exactly ten", 11, "exactly ten"},
		{"truncate me please", 10, "truncat…"},
		{"日本語のテキストです", 10, "日本…"},
		// Degenerate bound: below the ellipsis size the marker alone is
		// returned — a cut that cannot be marked is worse than 3 bytes over.
		{"anything", 2, "…"},
	}
	for _, tc := range cases {
		got := oneLine(tc.in, tc.max)
		if got != tc.want {
			t.Errorf("oneLine(%q, %d) = %q, want %q", tc.in, tc.max, got, tc.want)
		}
		if tc.max > len("…") && len(got) > tc.max {
			t.Errorf("oneLine(%q, %d) = %q exceeds the byte bound", tc.in, tc.max, got)
		}
	}
}
