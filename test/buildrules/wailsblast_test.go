package buildrules

import (
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
)

// wailsGated is the blast radius decision 0003 fixes: the files allowed to
// carry a `wails` build constraint, and with it the only files that break the
// day the wails/v3 alpha stops building.
//
// The decision states it as the fallback plan itself — "the fallback plan is
// not switch frameworks, it is compressing the alpha dependency down to three
// files" — so the number is the commitment, not a description of where the
// code happens to sit today.
var wailsGated = []string{
	"cmd/agenthub-gui/gui_main.go",
	"cmd/agenthub-gui/services/service_wails.go",
	"cmd/agenthub-gui/tray_wails.go",
}

// TestWailsBlastRadiusIsThreeFiles keeps the alpha dependency where decision
// 0003 put it.
//
// Nothing else checks this. The GUI is deliberately outside `make ci` — "the
// GUI is optional" is a compile-time property — so the packages here are built
// only by the separate gui job, and the ONE property that makes that safe is
// that almost none of the GUI needs the alpha to compile. A fourth tagged file
// would not fail any build; it would quietly enlarge what has to be rewritten
// on the day the alpha breaks, which is the day nobody wants to discover it.
//
// Both traps this walked into are encoded in constrainsOnWails below: the tag
// appears in PROSE (services/hub.go explains that the wiring is behind it and
// this file is not), and it appears NEGATED (main.go is `//go:build !wails`,
// the placeholder the default build compiles). A grep for the text reports
// five files and is wrong about two of them.
func TestWailsBlastRadiusIsThreeFiles(t *testing.T) {
	root := repoRoot(t)
	sources := allGoSources(t, root)
	if len(sources) == 0 {
		t.Fatal("found no Go sources; the walk is wrong, not the tree")
	}

	var gated []string
	for rel, src := range sources {
		if constrainsOnWails(src) {
			gated = append(gated, filepath.ToSlash(rel))
		}
	}
	slices.Sort(gated)

	if !slices.Equal(gated, wailsGated) {
		t.Errorf("files carrying a wails build constraint:\n  got  %v\n  want %v\n"+
			"docs/decisions/0003-wails3-and-the-frontend-stack.md commits to exactly these three: "+
			"the fallback plan for the alpha IS the size of this list. Move the new code into an "+
			"untagged body the tagged file assembles, the way services/hub.go and "+
			"services/service_wails.go are split — or amend the decision, which is append-only and "+
			"says why the number was chosen.", gated, wailsGated)
	}
}

// requiresWails matches the tag used POSITIVELY — `wails`, `linux && wails` —
// and not its negation. `!wails` is the opposite of a blast radius: it marks
// the placeholder the DEFAULT build compiles, which exists because of the
// split rather than being part of it.
var requiresWails = regexp.MustCompile(`(^|[^!\w])wails\b`)

// constrainsOnWails reports whether src is compiled only when the wails tag is
// set. Two things it deliberately is not:
//
//   - a text search. `//go:build wails` also appears in PROSE — services/hub.go
//     explains that the wiring sits behind the tag and this file does not — so
//     only the lines above the package clause are read.
//   - a check for the word. cmd/agenthub-gui/main.go carries `//go:build
//     !wails`, and counting it would report the placeholder as part of the
//     radius it exists to keep small.
func constrainsOnWails(src string) bool {
	for line := range strings.SplitSeq(src, "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "package ") {
			return false
		}
		if expr, ok := strings.CutPrefix(trimmed, "//go:build "); ok && requiresWails.MatchString(expr) {
			return true
		}
	}
	return false
}
