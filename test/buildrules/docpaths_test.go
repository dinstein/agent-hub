package buildrules

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// retiredNames are the old package names docs/conventions.md §"Retired old names"
// deliberately writes down BECAUSE they no longer exist: the table's whole
// job is to tell a reader who met them in early material what replaced them.
// They are the one legitimate reason for a docs path not to resolve, so they
// are listed here rather than exempted by matching on the table's shape —
// a heading or a table row is easy to reformat, and the exemption would
// silently widen to whatever else moved next to it.
//
// The value is the package that answers to the name now, or "" for one that
// was REMOVED rather than renamed. The two are worth keeping apart: a rename
// leaves a forwarding address a reader should follow, and a removal leaves
// none — writing one in anyway would send them to a package that never did
// the job they came looking for.
var retiredNames = map[string]string{
	"internal/control":              "internal/ctlapi",
	"internal/controlapi":           "internal/ctlapi",
	"internal/vault":                "internal/secrets",
	"internal/gatewaymode":          "internal/gateway",
	"internal/downstream/transport": "internal/mcp/transport",
	"internal/accesslog":            "internal/calllog",
	// Removed with the runtime governance surface. internal/audit's one
	// surviving primitive was extracted to internal/jsonl first, which is a
	// narrower claim than a rename and is why it is not spelled as one.
	"internal/audit":     "",
	"internal/integrity": "",
	"internal/approval":  "",
	// The token-savings ledger, removed rather than replaced: nothing else
	// records what a call cost against what it would have cost, so there is
	// no forwarding address to write here (docs/decisions/0009-savings-ledger-removed.md).
	"internal/savings": "",
}

// docPathRef matches a backticked path rooted at one of the repository's
// real top-level directories. Anchoring on those prefixes is what keeps the
// pattern from claiming every backticked identifier in the prose: a bare
// `slices.Sorted` or `tools/list` has no such prefix and is left alone.
var docPathRef = regexp.MustCompile("`((?:\\.agents|internal|cmd|api|test|docs|scripts)/[A-Za-z0-9_./-]+)`")

// TestDocsCitePathsThatExist keeps the prose honest about the tree.
//
// This is the same discipline as the Makefile registries one door over,
// pointed at documentation instead: the docs name specific packages and
// files constantly, and nothing at all notices when one of them moves. The
// failure is quiet and it is worse than a broken link, because a path that
// does not resolve does not read as stale — it reads as a package the
// reader has failed to find, and the honest response to that is to go
// looking rather than to distrust the sentence.
//
// Two real cases were fixed the round this test was written, both of them
// sitting in load-bearing sentences: `cmd/fakemcp` (the binary is at
// internal/testutil/fakemcp/cmd/fakemcp) and `internal/healthgen` (at
// cmd/agenthub-gui/internal/healthgen), the latter cited while arguing
// about which half of the GUI code `make test` reaches.
//
// Scope: existence only, and only for paths under a real top-level
// directory. A file:line anchor's LINE is not checked — line numbers drift
// on every edit above them, and a test that failed for that would be
// deleted within a week rather than obeyed.
func TestDocsCitePathsThatExist(t *testing.T) {
	root := repoRoot(t)
	docs := markdownFiles(t, root)
	if len(docs) == 0 {
		t.Fatal("found no markdown files to check; the walk or the root is wrong")
	}

	for _, doc := range docs {
		data, err := os.ReadFile(filepath.Join(root, doc))
		if err != nil {
			t.Fatalf("reading %s: %v", doc, err)
		}
		for i, line := range strings.Split(string(data), "\n") {
			for _, m := range docPathRef.FindAllStringSubmatch(line, -1) {
				ref := strings.TrimSuffix(m[1], "/")
				// A "path" whose last element contains a dot that is not a
				// known file extension is a symbol, not a file:
				// `internal/ctlapi.Listen` names a function.
				if isSymbolRef(ref) {
					continue
				}
				if canonical, ok := retiredNames[ref]; ok {
					// Named on purpose. Assert the replacement still exists,
					// so the table cannot quietly start pointing at a second
					// name that has also since moved. An empty replacement is
					// a removal, and there is nothing to assert about it.
					if canonical != "" && !exists(root, canonical) {
						t.Errorf("%s:%d cites the retired name %q, but its canonical replacement %q "+
							"does not exist either — docs/conventions.md's retirement table is now wrong in both columns",
							doc, i+1, ref, canonical)
					}
					continue
				}
				if !exists(root, ref) {
					t.Errorf("%s:%d cites %q, which does not exist.\n"+
						"Point it at the real path, or — if it is an old name worth recording — add it to "+
						"docs/conventions.md's retirement table and to retiredNames in this test.",
						doc, i+1, ref)
				}
			}
		}
	}
}

// isSymbolRef reports whether ref's last element looks like pkg.Symbol
// rather than a filename.
func isSymbolRef(ref string) bool {
	last := ref[strings.LastIndex(ref, "/")+1:]
	dot := strings.LastIndex(last, ".")
	if dot < 0 {
		return false
	}
	switch last[dot:] {
	case ".go", ".md", ".ts", ".tsx", ".json", ".yml", ".yaml", ".toml", ".sh", ".mod", ".sum":
		return false
	}
	// An exported Go identifier after the dot: internal/ctlapi.Listen.
	rest := last[dot+1:]
	return rest != "" && rest[0] >= 'A' && rest[0] <= 'Z'
}

func exists(root, rel string) bool {
	_, err := os.Stat(filepath.Join(root, rel))
	return err == nil
}

// markdownFiles walks the tree for .md files (walkRepoFiles owns the skip
// set, which is what keeps this to the documents that are ours to keep
// accurate).
func markdownFiles(t *testing.T, root string) []string {
	t.Helper()
	return walkRepoFiles(t, root, "markdown", func(name string) bool {
		return strings.HasSuffix(name, ".md")
	})
}
