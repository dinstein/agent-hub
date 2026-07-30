package buildrules

import (
	"os"
	"path/filepath"
	"regexp"
	"testing"
)

// TestReleaseScriptsAgreeOnTheArtifactRepo keeps the two halves of a release
// pointing at the same place.
//
// scripts/release-local.sh uploads the tarballs to $HOMEBREW_SOURCE_REPO;
// scripts/homebrew-formula.sh writes that repo's download URLs into the
// formula. Each reads the variable and each supplies its own default, so a
// change to one default alone produces a formula whose URLs point at a
// repository nothing was ever uploaded to.
//
// The failure is invisible from here: both scripts succeed, the release
// publishes, the tap commit lands, and the formula is syntactically valid. It
// surfaces as a 404 during `brew install` on someone else's machine — the one
// place this project cannot see.
func TestReleaseScriptsAgreeOnTheArtifactRepo(t *testing.T) {
	root := repoRoot(t)
	defaults := map[string]string{}
	for _, name := range []string{"release-local.sh", "homebrew-formula.sh"} {
		path := filepath.Join(root, "scripts", name)
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("reading %s: %v", path, err)
		}
		m := sourceRepoDefault.FindSubmatch(data)
		if m == nil {
			t.Fatalf("%s no longer defaults HOMEBREW_SOURCE_REPO; if the variable is gone, "+
				"delete this check, but do not leave the two scripts free to disagree", name)
		}
		defaults[name] = string(m[1])
	}
	if a, b := defaults["release-local.sh"], defaults["homebrew-formula.sh"]; a != b {
		t.Errorf("HOMEBREW_SOURCE_REPO defaults disagree: release-local.sh uploads to %q "+
			"while homebrew-formula.sh writes URLs for %q.\n"+
			"A release built with neither overridden would publish a formula that 404s.", a, b)
	}
}

// sourceRepoDefault matches `repo="${HOMEBREW_SOURCE_REPO:-<default>}"`.
var sourceRepoDefault = regexp.MustCompile(`HOMEBREW_SOURCE_REPO:-([^}"]+)`)
