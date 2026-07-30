package buildrules

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestFormulaCaveatsPutTheServerIntoService keeps the first thing a new user
// reads from being a path that cannot work.
//
// `agenthub server add` writes the definition and leaves the entry DISABLED —
// the whole point of the two-step design, and something every other document
// in this repository says out loud. The Homebrew caveats did not: they went
// `server add` → `server test` → `client connect`, so following them exactly
// left the user with a client correctly wired to a gateway serving nothing.
//
// That failure is invisible in the worst way. Nothing errors, no command
// exits non-zero, and the symptom appears in a different program (the client
// shows no tools) minutes later, with no reason to suspect the install notes.
//
// The check is textual on purpose: the caveats are a shell transcript in a
// Ruby heredoc inside a generator script, so nothing compiles them and no
// test would otherwise read them. Any of the three things that enable a
// server satisfies it — `server enable`, `auth login` (which enables the
// server it authorizes) or `catalog add` (which composes add + enable).
func TestFormulaCaveatsPutTheServerIntoService(t *testing.T) {
	caveats := formulaCaveats(t)

	// Matched with the `agenthub` prefix: "server add" alone also appears in
	// `profile server add`, further down the same block, which would let a
	// walkthrough that dropped the real step still satisfy this.
	if !strings.Contains(caveats, "agenthub server add") {
		t.Skip("caveats no longer walk through 'server add'; nothing to hold")
	}
	enablers := []string{"agenthub server enable", "agenthub auth login", "agenthub catalog add"}
	for _, e := range enablers {
		if strings.Contains(caveats, e) {
			return
		}
	}
	t.Errorf("the formula caveats run 'server add' but never put the server into service.\n"+
		"'add' leaves the entry disabled, so a user following these exactly gets a client\n"+
		"wired to a gateway that serves nothing, with no error anywhere to explain it.\n"+
		"Name one of %v in the same block.\n\n%s", enablers, caveats)
}

// formulaCaveats returns the body of the generator's `def caveats` heredoc.
//
// It reads scripts/homebrew-formula.sh rather than a generated Formula file:
// the formula is written into the tap repository, which is not present here,
// and the script is what the next release will use anyway.
func formulaCaveats(t *testing.T) string {
	t.Helper()
	path := filepath.Join(repoRoot(t), "scripts", "homebrew-formula.sh")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading %s: %v", path, err)
	}
	const open, close = "def caveats", "EOS\n  end"
	i := strings.Index(string(data), open)
	if i < 0 {
		t.Fatalf("%s no longer defines caveats; this check has lost its subject", path)
	}
	rest := string(data)[i:]
	j := strings.Index(rest, close)
	if j < 0 {
		t.Fatalf("%s: the caveats heredoc is not closed as expected", path)
	}
	return rest[:j]
}
