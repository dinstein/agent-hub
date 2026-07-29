package clients

import (
	"slices"
	"testing"
)

// TestScanTOMLServersHidesNothingInAMultilineString is the property the
// scanner exists to hold, from the direction that actually bites.
//
// agenthub reads codex's config to answer "is agenthub already in here, and
// does it point at this binary". Getting the END of a multi-line string wrong
// is how that answer goes wrong in the dangerous direction: terminate the
// string early and the scanner starts reading the string's own contents as
// configuration, so a `[mcp_servers.…]` written inside a comment field
// becomes a server agenthub believes is installed.
//
// The line-continuation form is the case with no other coverage. A backslash
// at end of line escapes the newline AND the indentation after it, so a
// scanner that treats the backslash as an ordinary escape mis-reads the next
// character and can fall out of the string at the wrong quote.
func TestScanTOMLServersHidesNothingInAMultilineString(t *testing.T) {
	t.Parallel()
	src := []byte(`notes = """
a line that wraps with a backslash \
    and continues here, still inside the string, where this:
[mcp_servers.ghost]
command = "should-never-be-seen"
is just text.
"""

[mcp_servers.real]
command = "/bin/agenthub"
args = ["connect", "--client", "codex"]
`)

	got, ok := scanTOMLServers(src, "mcp_servers")
	if !ok {
		t.Fatal("the document was refused; it is valid TOML the scanner has to understand")
	}
	if _, found := got["ghost"]; found {
		t.Error("a table header inside a multi-line string was read as a server")
	}
	entry, found := got["real"]
	if !found {
		t.Fatalf("the real server was not found; got %+v", got)
	}
	if entry.command != "/bin/agenthub" {
		t.Errorf("command = %q, want /bin/agenthub", entry.command)
	}
	if want := []string{"connect", "--client", "codex"}; !slices.Equal(entry.args, want) {
		t.Errorf("args = %v, want %v", entry.args, want)
	}
}

// TestScanTOMLServersReadsTheShapesCodexWrites covers the entry fields the
// caller acts on, and the two states that are NOT the same as absence: a
// server explicitly disabled, and a remote entry with a url instead of a
// command.
func TestScanTOMLServersReadsTheShapesCodexWrites(t *testing.T) {
	t.Parallel()
	src := []byte(`[mcp_servers.stdio]
command = "fs-server"
args = ["--root", "/tmp"]

[mcp_servers.off]
command = "x"
enabled = false

[mcp_servers.remote]
url = "https://example.test/mcp"

[mcp_servers."quoted name"]
command = "q"

[other_table]
command = "not ours"
`)

	got, ok := scanTOMLServers(src, "mcp_servers")
	if !ok {
		t.Fatal("the document was refused")
	}
	if _, found := got["not ours"]; found {
		t.Error("a table outside mcp_servers was read as a server")
	}
	for _, tc := range []struct {
		name string
		want tomlEntry
	}{
		{"stdio", tomlEntry{command: "fs-server", args: []string{"--root", "/tmp"}}},
		{"off", tomlEntry{command: "x", disabled: true}},
		{"remote", tomlEntry{url: "https://example.test/mcp"}},
		{"quoted name", tomlEntry{command: "q"}},
	} {
		entry, found := got[tc.name]
		if !found {
			t.Errorf("%s: not found; got %+v", tc.name, got)
			continue
		}
		if entry.command != tc.want.command || entry.url != tc.want.url ||
			entry.disabled != tc.want.disabled || !slices.Equal(entry.args, tc.want.args) {
			t.Errorf("%s = %+v, want %+v", tc.name, entry, tc.want)
		}
	}
}

// TestScanTOMLServersRefusesRatherThanGuesses. A partial answer is worse than
// none: half a server map is indistinguishable from a complete one, and the
// caller uses it to decide whether agenthub is already installed. Every shape
// here is one the scanner does not fully model, so each must come back
// (nil, false) — never a map with whatever it managed to read first.
func TestScanTOMLServersRefusesRatherThanGuesses(t *testing.T) {
	t.Parallel()
	for name, src := range map[string]string{
		"unterminated string":    "[mcp_servers.x]\ncommand = \"no closing quote\n",
		"unterminated multiline": "[mcp_servers.x]\ncommand = \"\"\"\nno closing delimiter\n",
		"unterminated array":     "[mcp_servers.x]\nargs = [\"a\", \"b\"\n",
		"array of tables":        "[[mcp_servers.x]]\ncommand = \"y\"\n",
		"unknown escape":         "[mcp_servers.x]\ncommand = \"a\\0b\"\n",
	} {
		got, ok := scanTOMLServers([]byte(src), "mcp_servers")
		if ok {
			t.Errorf("%s: accepted, want a refusal (got %+v)", name, got)
		}
		if got != nil {
			t.Errorf("%s: a refusal carried %+v, want nothing", name, got)
		}
	}
}
