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

var (
	// `repository: ${{ ... }}` — the Release upload's target repository.
	uploadTarget = regexp.MustCompile(`(?m)^\s*repository:\s*(\S.*?)\s*$`)
	// `HOMEBREW_SOURCE_REPO: ${{ ... }}` — the repository whose URLs the
	// formula is rendered with.
	formulaTarget = regexp.MustCompile(`(?m)^\s*HOMEBREW_SOURCE_REPO:\s*(\S.*?)\s*$`)
)

// TestReleaseWorkflowUploadsWhereTheFormulaPoints is the workflow's half of the
// agreement TestReleaseScriptsAgreeOnTheArtifactRepo holds for the scripts.
//
// The workflow names the same repository twice: `repository:` on the Release
// upload, and HOMEBREW_SOURCE_REPO on the formula render. Both were once left
// to a default instead, and the two defaults disagreed — the upload fell back
// to this repository, homebrew-formula.sh's own default named the tap — so a
// fully configured release published assets to one repository and a formula
// pointing at the other.
//
// Nothing about that run looks wrong from inside: every job is green, the
// formula is valid Ruby, and the sha256s are the real ones. It surfaces as a
// 404 during `brew install` on someone else's machine, which is the one place
// this project cannot see.
func TestReleaseWorkflowUploadsWhereTheFormulaPoints(t *testing.T) {
	data := releaseWorkflow(t)

	upload := uploadTarget.FindSubmatch(data)
	if upload == nil {
		t.Fatal("the release workflow no longer sets `repository:` on the upload, so the " +
			"Release goes to whichever repository the workflow lives in, while the formula " +
			"is rendered for HOMEBREW_SOURCE_REPO — the pair this check exists to keep together")
	}
	render := formulaTarget.FindSubmatch(data)
	if render == nil {
		t.Fatal("the release workflow no longer sets HOMEBREW_SOURCE_REPO when rendering the " +
			"formula; it falls back to the script's own default, which is then free to " +
			"disagree with where the workflow actually uploaded")
	}
	if a, b := string(upload[1]), string(render[1]); a != b {
		t.Errorf("the Release is uploaded to %s but the formula's URLs are rendered for %s.\n"+
			"Both jobs would pass and `brew install` would 404 on the first machine that is "+
			"not this one.", a, b)
	}
}

// releaseWorkflow returns .github/workflows/release.yml with YAML comments
// stripped, so prose about a setting is not mistaken for the setting.
func releaseWorkflow(t *testing.T) []byte {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(repoRoot(t), ".github", "workflows", "release.yml"))
	if err != nil {
		t.Fatalf("reading the release workflow: %v", err)
	}
	return []byte(workflowCommands(string(data)))
}
