package buildrules

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// TestEveryEventKindHasAWriter is the check the vocabulary was missing.
//
// The constant, the closed set and the published table cannot disagree: the
// table is generated from the first two by `make docs-gen`, which `make ci`
// re-checks. All three can agree perfectly about a
// kind NOTHING EVER WRITES, and seven of them did: `secrets_missing`,
// `scope_changed`, `ctl_socket_lost` and four others were declared,
// documented and guarded while no code path emitted one.
//
// That failure is worse than an ordinary gap, because the vocabulary is
// published as a selector. `agenthub events --kind secrets_missing` was
// ACCEPTED and answered "no events" — the same answer as "this has not
// happened", which is the one confusion a closed set exists to prevent — and
// the GUI reserved a colour for a row that could never render. Nothing else
// catches it: the package compiles, the docs are honest about the set, and
// `make ci` stays green.
//
// A writer is any mention of the constant outside internal/eventlog itself.
// That is deliberately loose — this is a check against a kind with NO caller
// at all, not an audit of whether the call site is the right one, and a
// stricter rule would fail on legitimate indirection like the connectFailure
// classifier.
func TestEveryEventKindHasAWriter(t *testing.T) {
	root := repoRoot(t)
	declared := parseEventKindConsts(t, filepath.Join(root, "internal", "eventlog", "eventlog.go"))
	if len(declared) == 0 {
		t.Fatal("no Kind constants found; this test asserted nothing")
	}
	used := eventKindMentions(t, root)
	for name, wire := range declared {
		if !used[name] {
			t.Errorf("internal/eventlog declares %s (%q), which nothing outside the package writes.\n"+
				"A kind no writer emits is still offered as a --kind selector, and answers "+
				"\"no events\" — which reads as \"this has not happened\".\n"+
				"Either wire an emit site or drop the kind; do not leave it declared.", name, wire)
		}
	}
}

// eventKindConst matches `KindSomething Kind = "wire_name"` and captures both
// the Go identifier and the wire spelling.
var eventKindConst = regexp.MustCompile(`^\s*(Kind\w+)\s+Kind\s*=\s*"([a-z_]+)"`)

func parseEventKindConsts(t *testing.T, path string) map[string]string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]string{}
	for _, line := range strings.Split(string(data), "\n") {
		if m := eventKindConst.FindStringSubmatch(line); m != nil {
			out[m[1]] = m[2]
		}
	}
	return out
}

// eventKindMentions collects every `eventlog.KindX` written anywhere in the
// tree except internal/eventlog, whose own files are the declaration rather
// than a use. Test files count: a kind reachable only from a test is still
// a kind with a caller, and excluding them would fail the emit sites that
// are exercised exactly that way.
func eventKindMentions(t *testing.T, root string) map[string]bool {
	t.Helper()
	pattern := regexp.MustCompile(`eventlog\.(Kind\w+)`)
	skip := filepath.Join(root, "internal", "eventlog")
	out := map[string]bool{}
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			name := d.Name()
			if name == ".git" || name == "node_modules" || path == skip {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") {
			return nil
		}
		data, rerr := os.ReadFile(path)
		if rerr != nil {
			return rerr
		}
		for _, m := range pattern.FindAllSubmatch(data, -1) {
			out[string(m[1])] = true
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}
