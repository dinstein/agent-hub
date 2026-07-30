package buildrules

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// encoderImport matches an import of a standard-library serializer — the tools
// that turn an in-memory value into bytes somebody could write down.
var encoderImport = regexp.MustCompile(`"encoding/(json|gob|xml)"|"gopkg\.in/yaml`)

// overlayOwningPackages are the two packages that hold a *scope.Overlay: scope
// defines it, session owns the live one.
var overlayOwningPackages = []string{"internal/scope", "internal/session"}

// TestOverlayPackagesCarryNoSerializer keeps the structural half of "overlays
// are never persisted to disk".
//
// docs/modules/config.md states the invariant absolutely, under internal/session's
// invariants: "Nowhere in this package serializes an overlay. Losing them on
// daemon restart is the design intent — a 'resurrected runtime loosening' is a
// security incident, not an availability improvement." AGENTS.md says the same
// in one line. An overlay is a RUNTIME RELAXATION of a security scope, so one
// that survives a restart is a narrowing the operator revoked, quietly back in
// force.
//
// The invariant has two halves and only one was guarded. The behavioural half —
// Close leaves the overlay nil — is covered by
// session.TestCloseCascadesAndIsIdempotent. The structural half, that nothing
// in these packages can serialize one in the first place, rested on nobody
// having written the code yet.
//
// Both packages import no serializer at all today, which is what makes this
// enforceable as an absence rather than as a review habit.
//
// WHAT THIS PROVES, AND WHAT IT DOES NOT. It proves these two packages cannot
// reach encoding/json, gob, xml or yaml. That is narrower than the invariant:
//
//   - Absent JSON tags are NOT what protects the type. encoding/json marshals
//     exported fields with no tags, using their Go names, so scope.Overlay
//     would serialize perfectly well if anything asked it to. The protection is
//     the absence of a caller, which is what this checks.
//   - A serializer is not the only way bytes escape. fmt.Sprintf("%+v") into a
//     log line would leak an overlay's contents just as effectively, and this
//     check cannot see that. It narrows the surface; it does not close it.
//   - A package that legitimately needs JSON later will fail this test. That is
//     the intended cost: the doc calls a regression here a security incident,
//     so the import deserves an argument rather than a quiet addition. Adding
//     the encoder AND keeping the invariant means moving the serializable type
//     out of these two packages.
func TestOverlayPackagesCarryNoSerializer(t *testing.T) {
	root := repoRoot(t)

	for _, pkg := range overlayOwningPackages {
		files, err := filepath.Glob(filepath.Join(root, pkg, "*.go"))
		if err != nil {
			t.Fatalf("globbing %s: %v", pkg, err)
		}
		checked := 0
		for _, path := range files {
			if strings.HasSuffix(path, "_test.go") {
				continue
			}
			data, err := os.ReadFile(path)
			if err != nil {
				if os.IsNotExist(err) {
					continue // see isTransientProbe
				}
				t.Fatalf("reading %s: %v", path, err)
			}
			checked++
			if m := encoderImport.FindString(string(data)); m != "" {
				rel, _ := filepath.Rel(root, path)
				t.Errorf("%s imports %s, but this package holds a *scope.Overlay.\n"+
					"An overlay is a runtime relaxation of a security scope: one that survives a "+
					"restart is a narrowing the operator revoked, silently back in force "+
					"(docs/modules/config.md, internal/session invariants).\n"+
					"If the encoder is genuinely needed, move the type that needs serializing out "+
					"of this package rather than making the overlay reachable from a marshaller.",
					rel, m)
			}
		}
		if checked == 0 {
			t.Errorf("found no production .go files under %s — the glob stopped matching "+
				"and this test is no longer checking anything", pkg)
		}
	}
}
