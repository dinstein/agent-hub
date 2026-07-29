package clients

import (
	"strings"
	"testing"
)

// FuzzScanTOMLServers guards the one path in this package where agenthub
// reads untrusted bytes by hand. Everything else goes through encoding/json.
//
// Two properties:
//
//  1. it returns rather than panicking or running away — index arithmetic
//     over attacker-shaped input is exactly where a scanner walks off the
//     end of its buffer;
//  2. every server it reports was really written there. The check is that a
//     name it claims appears verbatim in the input — a scanner that invents
//     a server, or reads one out of a comment or a multi-line string, is
//     how a config nobody wrote gets reported as connected.
//
// (That ok=true means the whole document was consumed is structural: the
// scanner returns true only at EOF.)
//
// Run: go test ./internal/clients/ -run xxx -fuzz FuzzScanTOMLServers
func FuzzScanTOMLServers(f *testing.F) {
	f.Add([]byte("[mcp_servers.agenthub]\ncommand = \"/bin/agenthub\"\nargs = [\"connect\", \"--client\", \"codex\"]\n"))
	f.Add([]byte("[mcp_servers.x]\ncommand = 'y'\nenabled = false\n[mcp_servers.x.env]\nK = \"V\"\n"))
	f.Add([]byte("mcp_servers = { x = { command = \"y\" } }\n"))
	f.Add([]byte("[[mcp_servers.x]]\ncommand = \"y\"\n"))
	f.Add([]byte("desc = \"\"\"\n[mcp_servers.ghost]\ncommand = \"no\"\n\"\"\"\n"))
	// The same hiding place, reached through the line-continuation form: a
	// backslash at end of line escapes the newline and the indentation after
	// it, which is the one way into the multi-line scanner the other seeds
	// never take.
	f.Add([]byte("desc = \"\"\"\nwrapped \\\n   [mcp_servers.ghost]\n\"\"\"\n[mcp_servers.real]\ncommand = \"y\"\n"))
	f.Add([]byte("[mcp_servers.\"quoted name\"]\nurl = \"https://x\"\n"))
	f.Add([]byte("# comment [mcp_servers.nope]\n[other]\nk = 1\n"))
	f.Add([]byte("[mcp_servers.x]\ncommand = \"unterminated\n"))
	f.Add([]byte("[mcp_servers.x]\nargs = [\n  \"a\",\n  \"b\",\n]\n"))
	f.Add([]byte(""))

	f.Fuzz(func(t *testing.T, data []byte) {
		got, ok := scanTOMLServers(data, "mcp_servers")
		if !ok {
			if got != nil {
				t.Fatalf("a refused document must yield no servers, got %+v", got)
			}
			return
		}
		for name, entry := range got {
			// The name must come from the document. Quoted keys are
			// unescaped, so only the plain case is checkable — but the
			// plain case is every real config, and an invented name would
			// have to survive it.
			if isPlainTOMLName(name) && !strings.Contains(string(data), name) {
				t.Fatalf("reported server %q that is not in the input: %q", name, data)
			}
			if entry.command != "" && isPlainTOMLName(entry.command) &&
				!strings.Contains(string(data), entry.command) {
				t.Fatalf("server %q reports command %q that is not in the input", name, entry.command)
			}
		}
	})
}

// isPlainTOMLName reports whether a scanned string is free of escapes and
// quoting, and so must appear verbatim in the source.
func isPlainTOMLName(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		if !isBareKeyByte(s[i]) && s[i] != '/' && s[i] != '.' {
			return false
		}
	}
	return true
}
