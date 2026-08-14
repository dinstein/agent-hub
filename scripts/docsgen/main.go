// Command docsgen regenerates the enumerations in docs/ that are derivable
// from the tree, so nobody maintains a second copy of them by hand.
//
// A generated block is delimited in the markdown by
//
//	<!-- BEGIN generated: <name> -->
//	<!-- END generated -->
//
// and everything between the two is replaced. `make docs-gen` rewrites them;
// `make docs-check` (which `make ci` runs) regenerates into memory and fails
// on any difference, so a hand edit is caught where a stale table would not
// be.
//
// It READS THE SOURCE rather than importing the packages, for the reason
// cmd/agenthub-gui/internal/healthgen gives for doing the same to api:
// importing would only prove the generator parrots itself, and here it would
// additionally require exporting a map internal/eventlog deliberately keeps
// unexported. Parsing means a declaration that changes shape is a loud
// failure rather than a silently smaller table.
package main

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

type block struct {
	Name string // the marker name
	File string // relative to the repository root
	Gen  func(root string) (string, error)
}

var blocks = []block{
	{Name: "event-kinds", File: "docs/subsystems/records.md", Gen: eventKindsTable},
}

func main() {
	check := flag.Bool("check", false, "fail instead of writing when a block is out of date")
	flag.Parse()

	root, err := repoRoot()
	if err != nil {
		fail(err)
	}
	stale := 0
	for _, b := range blocks {
		body, err := b.Gen(root)
		if err != nil {
			fail(fmt.Errorf("%s: %w", b.Name, err))
		}
		path := filepath.Join(root, filepath.FromSlash(b.File))
		before, err := os.ReadFile(path)
		if err != nil {
			fail(err)
		}
		after, err := replaceBlock(string(before), b.Name, body)
		if err != nil {
			fail(fmt.Errorf("%s: %w", b.File, err))
		}
		if after == string(before) {
			continue
		}
		if *check {
			stale++
			fmt.Fprintf(os.Stderr, "%s: the %q block is out of date; run `make docs-gen`\n", b.File, b.Name)
			continue
		}
		if err := os.WriteFile(path, []byte(after), 0o644); err != nil {
			fail(err)
		}
		fmt.Printf("regenerated %s in %s\n", b.Name, b.File)
	}
	if stale > 0 {
		os.Exit(1)
	}
}

func fail(err error) {
	fmt.Fprintln(os.Stderr, "docsgen:", err)
	os.Exit(1)
}

// repoRoot walks up from the working directory to the module root.
func repoRoot() (string, error) {
	dir, err := os.Getwd()
	if err != nil {
		return "", err
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", fmt.Errorf("no go.mod above %s", dir)
		}
		dir = parent
	}
}

// replaceBlock swaps the body between the named markers. A missing marker is
// an error rather than an append: a generator that invents its own location
// writes the table twice the day somebody moves it.
func replaceBlock(doc, name, body string) (string, error) {
	begin := "<!-- BEGIN generated: " + name + " -->"
	end := "<!-- END generated -->"
	i := strings.Index(doc, begin)
	if i < 0 {
		return "", fmt.Errorf("no %q marker", begin)
	}
	j := strings.Index(doc[i:], end)
	if j < 0 {
		return "", fmt.Errorf("no %q after the begin marker", end)
	}
	j += i
	return doc[:i+len(begin)] + "\n\n" + body + "\n" + doc[j:], nil
}

var (
	kindConst  = regexp.MustCompile(`(?m)^\t(Kind\w+)\s+Kind\s+=\s+"([a-z_]+)"`)
	scopeConst = regexp.MustCompile(`(?m)^\t(Scope\w+)\s+Scope\s+=\s+"([a-z_]+)"`)
	kindsMap   = regexp.MustCompile(`(?s)var allKinds = map\[Scope\]\[\]Kind\{(.*?)\n\}`)
	scopeEntry = regexp.MustCompile(`(?s)(Scope\w+): \{(.*?)\},\n`)
	disruptSet = regexp.MustCompile(`(?s)var disruptions = \[\]Kind\{(.*?)\n\}`)
	identifier = regexp.MustCompile(`\bKind\w+\b`)
)

// eventKindsTable renders the (scope, kind, class) table of the closed event
// vocabulary out of internal/eventlog's own declarations.
func eventKindsTable(root string) (string, error) {
	pkg := filepath.Join(root, "internal", "eventlog")
	kinds, err := os.ReadFile(filepath.Join(pkg, "eventlog.go"))
	if err != nil {
		return "", err
	}
	class, err := os.ReadFile(filepath.Join(pkg, "class.go"))
	if err != nil {
		return "", err
	}

	wire := map[string]string{}
	for _, m := range kindConst.FindAllStringSubmatch(string(kinds), -1) {
		wire[m[1]] = m[2]
	}
	for _, m := range scopeConst.FindAllStringSubmatch(string(kinds), -1) {
		wire[m[1]] = m[2]
	}
	if len(wire) == 0 {
		return "", fmt.Errorf("parsed no Kind constants; the declaration shape changed")
	}

	body := kindsMap.FindStringSubmatch(string(kinds))
	if body == nil {
		return "", fmt.Errorf("no `var allKinds = map[Scope][]Kind{` literal; the closed set moved")
	}
	disrupt := map[string]bool{}
	if d := disruptSet.FindStringSubmatch(string(class)); d != nil {
		for _, id := range identifier.FindAllString(d[1], -1) {
			disrupt[id] = true
		}
	} else {
		return "", fmt.Errorf("no `var disruptions = []Kind{` literal; the classification moved")
	}

	var rows []string
	entries := scopeEntry.FindAllStringSubmatch(body[1]+"\n", -1)
	if len(entries) == 0 {
		return "", fmt.Errorf("parsed no scopes out of allKinds")
	}
	for _, e := range entries {
		scope, ok := wire[e[1]]
		if !ok {
			return "", fmt.Errorf("allKinds names %s, which declares no wire value", e[1])
		}
		var routine, disruption []string
		for _, id := range identifier.FindAllString(e[2], -1) {
			w, ok := wire[id]
			if !ok {
				return "", fmt.Errorf("allKinds names %s, which declares no wire value", id)
			}
			if disrupt[id] {
				disruption = append(disruption, "`"+w+"`")
			} else {
				routine = append(routine, "`"+w+"`")
			}
		}
		sort.Strings(routine)
		sort.Strings(disruption)
		if len(routine) > 0 {
			rows = append(rows, fmt.Sprintf("| `%s` | %s | routine |", scope, strings.Join(routine, ", ")))
		}
		if len(disruption) > 0 {
			rows = append(rows, fmt.Sprintf("| `%s` | %s | disruption |", scope, strings.Join(disruption, ", ")))
		}
	}
	return "| Scope | Kinds | Class |\n|---|---|---|\n" + strings.Join(rows, "\n"), nil
}
