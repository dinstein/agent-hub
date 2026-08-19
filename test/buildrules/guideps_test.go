package buildrules

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"testing"
)

// frontendRuntimeDeps is the frontend's entire runtime dependency list.
//
// The number is an argument, not an observation. docs/subsystems/gui.md's
// "Explicitly not doing" rests the case against React + shadcn on it —
// "runtime dependencies would go from 1 to roughly 13 direct plus hundreds of
// transitive … for a security gateway that supply-chain surface is ironic" —
// and decision 0003 states the same commitment as a property of the stack:
// @wailsio/runtime is THE only frontend runtime dependency.
var frontendRuntimeDeps = []string{"@wailsio/runtime"}

const frontendPackageJSON = "cmd/agenthub-gui/frontend/package.json"

// TestFrontendRuntimeDependenciesStayAtOne keeps that argument honest.
//
// Nothing else can. Adding a dependency edits no document, breaks no
// invariant a compiler can see, and `npm ci` installs it without comment —
// while the GUI is the one corner of the tree deliberately outside `make ci`,
// so the change arrives with less scrutiny than anywhere else. The doc would
// go on making an argument from a number that had moved.
//
// Scope: runtime dependencies only. devDependencies are typescript and vite,
// which build the bundle and ship in nothing, and the claim this guards is
// about what reaches a user's machine.
//
// This is not a ban. It is a requirement that adding one be a decision
// somebody made on purpose — decisions/ is append-only, and 0003 says why the
// number was chosen — rather than a line in a file nobody re-reads.
func TestFrontendRuntimeDependenciesStayAtOne(t *testing.T) {
	root := repoRoot(t)
	raw, err := os.ReadFile(filepath.Join(root, frontendPackageJSON))
	if err != nil {
		t.Fatalf("reading %s: %v", frontendPackageJSON, err)
	}
	var pkg struct {
		Dependencies map[string]string `json:"dependencies"`
	}
	if err := json.Unmarshal(raw, &pkg); err != nil {
		t.Fatalf("parsing %s: %v", frontendPackageJSON, err)
	}

	got := make([]string, 0, len(pkg.Dependencies))
	for name := range pkg.Dependencies {
		got = append(got, name)
	}
	slices.Sort(got)

	if !slices.Equal(got, frontendRuntimeDeps) {
		t.Errorf("%s runtime dependencies:\n  got  %v\n  want %v\n"+
			"gui.md's \"Explicitly not doing\" argues against a framework FROM this number, and "+
			"decision 0003 commits to it. Adding one means amending both — or, if the addition is "+
			"right, saying so where the next person weighing the same trade-off will read it.",
			frontendPackageJSON, got, frontendRuntimeDeps)
	}
}
