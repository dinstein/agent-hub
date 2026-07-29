package buildrules

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// The gate targets decide pass/fail from text they piped through `tee`, and a
// pipeline's exit status is its LAST command — so without `pipefail` the thing
// being graded can fail and `tee` reports success on its behalf.
//
// The Makefile says so, and set `.SHELLFLAGS := -eu -o pipefail -c` to fix it.
// That directive arrived in GNU Make 3.82. macOS ships 3.81 as /usr/bin/make,
// and 3.81 does not warn about a variable it has never heard of — so on the
// platform this project is developed on, pipefail was configured, documented
// as load-bearing, and silently absent. `make ci-landing` would have reported
// success over a red `make ci-full`.
//
// The lesson is not "add pipefail again", it is that nothing ever checked. So
// these tests check, using the make and shell that are actually installed
// rather than the ones the Makefile hopes for.

// gateRecipes are the targets whose verdict comes out of a pipe. Adding
// another such target means adding it here.
var gateRecipes = []string{"ci-landing", "ci-depguard-proof"}

// TestPipedGateRecipesArmPipefailThemselves is the static half: every recipe
// line that pipes into tee must turn pipefail on for itself, rather than
// inheriting it from a directive an old make ignores.
func TestPipedGateRecipesArmPipefailThemselves(t *testing.T) {
	body := readMakefile(t)
	for _, line := range recipeLines(body) {
		if !strings.Contains(line, "| tee ") {
			continue
		}
		if !strings.Contains(line, "set -o pipefail;") {
			t.Errorf("this recipe line pipes into tee without arming pipefail itself:\n  %s\n"+
				"On GNU Make 3.81 (macOS's /usr/bin/make) .SHELLFLAGS is ignored, so the "+
				"command's failure would be replaced by tee's success.", strings.TrimSpace(line))
		}
	}
}

// TestPipefailIsActuallyInEffect is the half that matters. The static check
// above proves the words are present; this one proves they do something, with
// the make and the shell on this machine — which is exactly the gap the
// original bug lived in.
//
// It builds a throwaway Makefile carrying the real one's SHELL and
// .SHELLFLAGS lines verbatim, plus a recipe shaped like the gate recipes, and
// asserts that a failing command on the left of the pipe fails the target.
func TestPipefailIsActuallyInEffect(t *testing.T) {
	body := readMakefile(t)

	var header []string
	for _, line := range strings.Split(body, "\n") {
		if strings.HasPrefix(line, "SHELL ") || strings.HasPrefix(line, "SHELL:") ||
			strings.HasPrefix(line, ".SHELLFLAGS") {
			header = append(header, line)
		}
	}
	if len(header) == 0 {
		t.Fatal("the Makefile declares neither SHELL nor .SHELLFLAGS; this check " +
			"would be testing a shell the real recipes do not use")
	}

	dir := t.TempDir()
	mk := strings.Join(header, "\n") + "\n\n" +
		"probe:\n\t@set -o pipefail; false | tee /dev/null\n\t@echo REACHED\n"
	if err := os.WriteFile(filepath.Join(dir, "Makefile"), []byte(mk), 0o600); err != nil {
		t.Fatal(err)
	}

	cmd := exec.Command("make", "probe")
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Errorf("a failing command piped into tee did NOT fail the target.\n"+
			"make output:\n%s\nThe gate targets grade themselves through such a pipe, so "+
			"in this configuration a red run reports green.", out)
	}
	if strings.Contains(string(out), "REACHED") {
		t.Error("the recipe continued past a failed pipeline to its next line")
	}
}

// TestTheRealGateTargetsPropagateFailure runs the actual targets against a
// toolchain that fails, and requires each to exit non-zero. It is the
// end-to-end version of the two checks above — real recipes, real make, real
// shell — and the assertion that would have caught the original bug alone.
//
// The stub is a `go` that SUCCEEDS for `clean` and fails for everything else,
// which matters more than it looks. `ci-landing` opens with `go clean
// -testcache`; a stub that failed there too would make the target go red at
// its first line, never reach the piped one, and pass this test while proving
// nothing about the pipe. The stub has to get the recipe as far as the pipe
// for the pipe to be what is tested.
func TestTheRealGateTargetsPropagateFailure(t *testing.T) {
	root := repoRoot(t)
	stub := filepath.Join(t.TempDir(), "go-stub")
	script := "#!/bin/sh\n" +
		"# succeed for the cache drop, fail for the work\n" +
		"[ \"$1\" = clean ] && exit 0\n" +
		"echo 'stub toolchain: deliberate failure' >&2\n" +
		"exit 1\n"
	if err := os.WriteFile(stub, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}

	for _, target := range gateRecipes {
		t.Run(target, func(t *testing.T) {
			cmd := exec.Command("make", target,
				"GO="+stub, "GOLANGCI_LINT=false", "NPM=false",
				// Keep the run's scratch out of /tmp's shared path.
				"E2E_XDG="+filepath.Join(t.TempDir(), "xdg"))
			cmd.Dir = root
			out, err := cmd.CombinedOutput()
			if err == nil {
				t.Errorf("`make %s` reported SUCCESS while every real command it ran failed.\n"+
					"output:\n%s", target, out)
			}
		})
	}
}

func readMakefile(t *testing.T) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(repoRoot(t), "Makefile"))
	if err != nil {
		t.Fatalf("reading the Makefile: %v", err)
	}
	return string(data)
}

var continuation = regexp.MustCompile(`\\\s*$`)

// recipeLines returns the LOGICAL recipe lines (tab-indented, backslash
// continuations joined), so a pipe split across two physical lines is still
// examined together with the `set -o pipefail;` that arms it.
func recipeLines(body string) []string {
	var out []string
	var cur strings.Builder
	joining := false
	for _, line := range strings.Split(body, "\n") {
		if !joining && !strings.HasPrefix(line, "\t") {
			continue
		}
		cur.WriteString(line)
		if continuation.MatchString(line) {
			joining = true
			continue
		}
		joining = false
		out = append(out, cur.String())
		cur.Reset()
	}
	if cur.Len() > 0 {
		out = append(out, cur.String())
	}
	return out
}
