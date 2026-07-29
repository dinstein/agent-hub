package buildrules

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// fsyncFreeByDesign are the atomic writers that deliberately stop at rename,
// each with the reason its own doc comment also gives.
//
// A writer belongs here only if losing the file to a crash costs nothing. "It
// is only small state" is not a reason: the confops documents that prompted
// this test were small too, and one of them decides whether a poisoned tool
// description stays neutralized. All three current entries pass a harder test
// than size — the file is either about to be rewritten by the process that
// owns it, or reproducible on demand.
var fsyncFreeByDesign = map[string]string{
	"internal/daemon/daemon.go":                        "daemon.json describes a RUNNING daemon; outliving the crash that killed it is pointless",
	"internal/gateway/cache.go":                        "the tool cache is an accelerator; an entry lost to a crash costs one re-fetch",
	"cmd/agenthub-gui/internal/healthgen/healthgen.go": "a generated file; `make generate` reproduces it from the api package",
}

var (
	createTempCall = regexp.MustCompile(`\bos\.CreateTemp\(`)
	renameCall     = regexp.MustCompile(`\bos\.Rename\(`)
	// afterRenameSync matches either shape the tree uses for the last rung: a
	// named helper, or the inline open-and-sync that most of the writers spell
	// out. Both must appear AFTER the rename, which is why this is applied to
	// the tail of the body rather than the whole of it.
	afterRenameSync = regexp.MustCompile(`\bsyncDir\(|\bfsyncDir\(|\bsyncParent\(|\.Sync\(\)`)
	funcHeader      = regexp.MustCompile(`^func (?:\([^)]*\) )?(\w+)`)
)

// TestAtomicWritersFsyncTheParentDirectory fails when a temp-file-plus-rename
// writer does not end on a parent-directory fsync, unless it is listed above.
//
// The ladder is written down in three places — security.md, foundation.md and
// config.md all spell out "temp file in the same directory → chmod 0600 →
// write → fsync → rename → fsync the parent directory" — and implemented
// twelve times, on purpose: registry, integrity and their peers are forbidden
// from importing one another's document model to share a syscall wrapper, so
// each carries its own copy. Deliberate duplication is a reasonable answer to
// that constraint. Deliberate duplication with nothing checking the copies
// agree is how one of them ends up a rung short.
//
// One had. confops/atomicWriteJSON stopped at rename, which on ext4 and xfs
// makes the write atomic but not durable, and it writes tool-overrides.json —
// the file whose entries neutralize a poisoned tool description.
//
// WHAT THIS CHECKS, AND WHAT IT DOES NOT. For every function that both creates
// a temp file and renames it, a sync call must appear in the text AFTER the
// rename. Matching only the tail is what makes the check see a rung rather than
// merely a mention: a sync that happens before the rename does not make the
// rename durable, and would be reported here.
//
// Two forms count, because the tree uses both — a named syncDir-style helper
// (registry, integrity, confops) and the inline open-and-sync spelled out in
// place (ratelimit, shaping, clients). An earlier draft of this test matched
// only the helper and reported those three inline writers as defects; they were
// correct all along.
//
// It is deliberately not more than that:
//
//   - It does not verify the earlier rungs (chmod, file fsync). Those were read
//     by hand when this test was written: every writer has them EXCEPT
//     daemon.go, which skips the file fsync too and says so. Adding partial
//     checks for each rung would imply a completeness this file does not have.
//   - It cannot test the fsync ITSELF. Proving durability needs a crash
//     harness, not a unit test. What is enforceable is that the call is there,
//     and that removing it means arguing with a test rather than slipping past
//     review.
//   - It says nothing about whether an exemption is CORRECT, only that one was
//     declared in two places. Whether losing a given file really costs nothing
//     stays a review question.
func TestAtomicWritersFsyncTheParentDirectory(t *testing.T) {
	root := repoRoot(t)
	files := productionGoFiles(t, root)
	if len(files) == 0 {
		t.Fatal("found no non-test .go files; the walk or the root is wrong")
	}

	found := 0
	for _, rel := range files {
		data, err := os.ReadFile(filepath.Join(root, rel))
		if err != nil {
			if os.IsNotExist(err) {
				// The file was listed and then deleted underneath the walk. See
				// isTransientProbe: another package's test owns it, and a file
				// that no longer exists cannot be a defect in this tree.
				continue
			}
			t.Fatalf("reading %s: %v", rel, err)
		}
		for _, fn := range topLevelFuncs(string(data)) {
			if !createTempCall.MatchString(fn.body) || !renameCall.MatchString(fn.body) {
				continue
			}
			found++
			tail := fn.body[strings.Index(fn.body, "os.Rename("):]
			if afterRenameSync.MatchString(tail) {
				continue
			}
			if why, ok := fsyncFreeByDesign[rel]; ok {
				if !strings.Contains(fn.doc, "fsync") && !strings.Contains(fn.doc, "urab") {
					t.Errorf("%s: %s is exempt in this test (%s) but its own comment does not say so.\n"+
						"The exemption has to be readable where the code is, not only here.", rel, fn.name, why)
				}
				continue
			}
			t.Errorf("%s: %s creates a temp file and renames it but never fsyncs the parent directory.\n"+
				"On ext4/xfs the rename is atomic and NOT durable, so a crash can restore the old contents "+
				"after the write reported success. End on syncDir(dir) — or, if losing this file after a crash "+
				"genuinely costs nothing, say why in the function's comment and add it to fsyncFreeByDesign.",
				rel, fn.name)
		}
	}
	// A refactor that renames os.CreateTemp out of every writer would make this
	// test vacuously green; the count is what notices.
	if found < 10 {
		t.Errorf("only %d temp-file-and-rename writers found, expected at least 10 — "+
			"the detection stopped matching and this test is no longer checking anything", found)
	}
}

type goFunc struct {
	name string
	body string
	doc  string // the contiguous // block above the declaration
}

// topLevelFuncs splits a file into its top-level functions. Bodies end at the
// first column-zero closing brace, which is what gofmt guarantees and what
// makes this good enough without parsing Go.
//
// The doc comment is captured separately because an exemption has to be
// justified where a reader will meet it, and that is above the func rather
// than inside it.
func topLevelFuncs(src string) []goFunc {
	lines := strings.Split(src, "\n")
	var out []goFunc
	for i, line := range lines {
		m := funcHeader.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		end := len(lines)
		for j := i + 1; j < len(lines); j++ {
			if strings.HasPrefix(lines[j], "}") {
				end = j
				break
			}
		}
		top := i
		for top > 0 && strings.HasPrefix(lines[top-1], "//") {
			top--
		}
		out = append(out, goFunc{
			name: m[1],
			body: strings.Join(lines[i:end], "\n"),
			doc:  strings.Join(lines[top:i], "\n"),
		})
	}
	return out
}

// isTransientProbe reports whether a filename belongs to another test rather
// than to the tree.
//
// internal/depguardtest proves the dependency rules fire by writing a violating
// .go file into a constrained package, linting it, and removing it in
// t.Cleanup. Those files are named zz_depguard_probe_*.go by convention (see
// that package's doc.go) and are git-ignored.
//
// `go test ./...` runs the two packages concurrently, so a walk here can list a
// probe and then find it gone a moment later — which is exactly how this test
// first went red on a full run after passing every time it was run alone. The
// files are not production code and must not be examined even when the timing
// does hand one over intact: a probe is a deliberate violation, and reading it
// would be reading someone else's fixture.
func isTransientProbe(name string) bool {
	return strings.HasPrefix(name, "zz_depguard_probe_")
}

// productionGoFiles lists non-test .go files, skipping vendored and generated
// directories, and anything another package's test owns.
func productionGoFiles(t *testing.T, root string) []string {
	t.Helper()
	var out []string
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			switch d.Name() {
			case "node_modules", ".git", "dist", "bin", "testdata":
				return filepath.SkipDir
			}
			return nil
		}
		name := d.Name()
		if !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			return nil
		}
		if isTransientProbe(name) {
			return nil
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		out = append(out, rel)
		return nil
	})
	if err != nil {
		t.Fatalf("walking %s for go files: %v", root, err)
	}
	return out
}
