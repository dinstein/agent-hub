package buildrules

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// TestSelectorListsDoNotUseOmitempty enforces AGENTS.md's `omitzero`, not
// `omitempty` rule on the fields it is about.
//
// `nil` and `[]` are different everywhere a selector appears: nil means "no
// rule", `[]` means "nothing". `omitempty` drops both, so an explicit
// block-all is encoded exactly like an absent one and arrives as allow-all —
// api/profiles.go says it in one sentence: "A plain slice with omitempty
// would encode those two identically and collapse block-all into allow-all."
//
// It is not hypothetical. ctlapi.TokenCreateRequest.Servers carried
// `omitempty` on an exported request type callers marshal themselves, so
// `Servers: []string{}` — a token scoped to no servers — went out as
// `{"name":"..."}` and was read as scoped to every server. Fail-open, on a
// credential's own allowlist, and nothing refused it.
//
// Scope is deliberately narrow, because the rule is about a LIST that carries
// three states rather than about the word omitempty:
//
//   - `*[]string` is correct and stays legal. The pointer holds the
//     distinction — nil omits the key, &[]string{} sends [] — and omitempty on
//     a pointer only drops the nil. api/profiles.go and api/scope.go use it
//     that way on purpose.
//   - A slice of anything else (`[]InspectedServer` and friends) is an
//     inventory, not a selector; empty and absent really do mean the same.
//   - A `map` container may be omitempty too: the three states live one level
//     down, on ToolSelector.Allow, which is already omitzero.
//
// So: a plain `[]string` named `servers` or `allow` must be omitzero,
// untagged, or a pointer — never omitempty.
func TestSelectorListsDoNotUseOmitempty(t *testing.T) {
	root := repoRoot(t)
	var offences []string
	scanned := 0

	skip := map[string]bool{".git": true, "node_modules": true, "testdata": true, "dist": true, "bin": true}
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if skip[d.Name()] {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		data, rerr := os.ReadFile(path)
		if rerr != nil {
			return rerr
		}
		scanned++
		rel, _ := filepath.Rel(root, path)
		for i, line := range strings.Split(string(data), "\n") {
			if selectorOmitempty.MatchString(line) {
				offences = append(offences, rel+":"+itoa(i+1)+"  "+strings.TrimSpace(line))
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if scanned < 100 {
		t.Fatalf("scanned only %d non-test Go files; the walk is not reaching the tree", scanned)
	}

	for _, o := range offences {
		t.Errorf("this selector list would lose the difference between [] and absent:\n  %s\n"+
			"omitempty drops both, so an explicit block-all encodes exactly like no rule at "+
			"all and is read as allow-all. Use omitzero, drop the option, or make it a "+
			"*[]string so the pointer carries the distinction.", o)
	}
}

// selectorOmitempty matches a PLAIN []string selector tagged omitempty. The
// leading boundary is what keeps `*[]string` out: on a pointer the tag is
// correct, and matching it would push the tree toward the one encoding that
// does collapse the states.
var selectorOmitempty = regexp.MustCompile(
	`(^|[^*\w])\[\]string\s+` + "`" + `json:"(servers|allow)[^"]*,omitempty"`)
