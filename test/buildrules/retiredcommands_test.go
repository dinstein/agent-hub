package buildrules

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// retiredCommands are command spellings canonical.md §2 has retired, mapped
// to what replaced them. They are exactly the spellings a hidden alias keeps
// WORKING, which is what makes them dangerous in prose: a doc or a help
// string naming one is not broken, it is quietly teaching the form that is
// about to be deleted, and nothing fails until the alias goes.
//
// Retiring `agenthub tool …` proved the point. The move to `server tool` was
// one commit; the stale mentions were found afterwards, one sweep at a time,
// in a Chinese guide, a GUI settings page, four CLI help strings and a
// troubleshooting row that had been pointing at a command which could not
// answer the question it was cited for.
// Spelled out per subcommand rather than as the `agenthub tool ` prefix,
// because that prefix also occurs in ordinary prose — the status meta-tool
// describes "the agenthub tool surface", which is a noun phrase and not an
// invocation. The narrower form is also the honest scope: what this catches
// is a spelling a reader could COPY, which is the one that misleads.
// The `profile tools` entries carry no trailing space: two hint strings kept
// teaching the retired spelling as 'agenthub profile tools' and a bare
// 'profile tools' — quote-closed, so a trailing-space pattern walked past
// both. The quoted bare form is listed on its own because hints drop the
// binary name, and what a hint shows is still what a reader will type.
var retiredCommands = map[string]string{
	"agenthub tool ls":       "agenthub server tool ls",
	"agenthub tool allow":    "agenthub server tool allow",
	"agenthub tool inspect":  "agenthub server tool inspect",
	"agenthub profile tools": "agenthub profile tool allow",
	"'profile tools'":        "'profile tool allow'",
}

// commandDocRoots are the trees whose text is read by a human or handed to an
// agent. Test files are excluded deliberately: a test asserting the retired
// spelling still works has to name it.
var commandDocRoots = []string{"docs", "skills", "internal", "cmd", "README.md"}

// TestNoDocumentTeachesARetiredCommand keeps the prose, the help strings and
// the GUI on the command tree that exists.
//
// Two exemptions, both narrow. canonical.md's retired-names table exists to
// write these down — that is its whole job. And the deprecation shims name
// the old spelling in the comment explaining what they forward and in the
// notice telling the user what to type instead; a shim that could not say
// which spelling it retires would be unreadable.
func TestNoDocumentTeachesARetiredCommand(t *testing.T) {
	repo := repoRoot(t)
	var offences []string
	for _, name := range commandDocRoots {
		root := filepath.Join(repo, name)
		// HARD failure, never a skip: the walk runs from the package
		// directory, where every one of these names is absent, so tolerating
		// a missing root is how this test silently passes on nothing at all
		// — which is exactly what it did the first time it was written.
		if _, err := os.Stat(root); err != nil {
			t.Fatalf("%s is missing; the check would cover nothing: %v", root, err)
		}
		err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() {
				if d.Name() == "node_modules" || d.Name() == "dist" {
					return filepath.SkipDir
				}
				return nil
			}
			if !isCommandDoc(path) || isRetiredNamesExempt(path) {
				return nil
			}
			data, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			for n, line := range strings.Split(string(data), "\n") {
				if shimLine(line) {
					continue
				}
				for old, now := range retiredCommands {
					if strings.Contains(line, old) {
						rel, _ := filepath.Rel(repo, path)
						offences = append(offences, fmt.Sprintf(
							"%s:%d names %q; the command is now %q", rel, n+1, strings.TrimSpace(old), strings.TrimSpace(now)))
					}
				}
			}
			return nil
		})
		if err != nil {
			t.Fatalf("walk %s: %v", root, err)
		}
	}
	if len(offences) > 0 {
		slices.Sort(offences)
		t.Errorf("retired command spellings are still being taught:\n  %s",
			strings.Join(offences, "\n  "))
	}
}

func isCommandDoc(path string) bool {
	if strings.HasSuffix(path, "_test.go") {
		return false
	}
	switch filepath.Ext(path) {
	case ".md", ".go", ".ts":
		return true
	}
	return false
}

// isRetiredNamesExempt covers canonical.md, whose retired-names table is the
// one place these spellings BELONG.
func isRetiredNamesExempt(path string) bool {
	return filepath.Base(path) == "canonical.md"
}

// shimLine spots the deprecation shims themselves — the comment explaining
// what a shim forwards, and the notice telling the user what to type. Both
// have to name the retired spelling to be worth reading.
func shimLine(line string) bool {
	return strings.Contains(line, "Shim") || strings.Contains(line, "shim") ||
		strings.Contains(line, "is now 'agenthub") || strings.Contains(line, "Deprecated:")
}
