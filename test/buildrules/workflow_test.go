package buildrules

import (
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
)

// `make ci-full` is documented as "everything the CI workflow runs", and
// AGENTS.md tells you to run it before pushing. That promise is what makes a
// local green run mean anything, and until now it rested on the two files
// being kept in step by eye.
//
// The direction that hurts is CI running something `ci-full` does not: the
// contributor gets a green local run and a red push, which is the exact
// experience the target exists to prevent. The other direction is harmless —
// `ci-full` may do more.

var (
	// `run: make foo` and `make foo` inside a `run: |` block alike.
	workflowMake = regexp.MustCompile(`\bmake\s+([a-z][a-z0-9-]*)`)
	// `target: dep dep dep  ## comment`
	makeRule = regexp.MustCompile(`^([a-z][a-z0-9-]*):([^=]*?)(?:##.*)?$`)
)

// TestCIWorkflowRunsNothingCIFullSkips holds the promise `ci-full` makes.
func TestCIWorkflowRunsNothingCIFullSkips(t *testing.T) {
	root := repoRoot(t)
	covered := targetClosure(t, filepath.Join(root, "Makefile"), "ci-full")
	if len(covered) <= 1 {
		t.Fatal("ci-full resolved to no prerequisites; the Makefile's shape changed and " +
			"this check would pass for any workflow at all")
	}

	wf := filepath.Join(root, ".github", "workflows", "ci.yml")
	data, err := os.ReadFile(wf)
	if err != nil {
		t.Fatalf("reading the workflow: %v", err)
	}
	var invoked []string
	for _, m := range workflowMake.FindAllStringSubmatch(string(data), -1) {
		if !slices.Contains(invoked, m[1]) {
			invoked = append(invoked, m[1])
		}
	}
	if len(invoked) == 0 {
		t.Fatal("the workflow invokes no make targets; the parse is broken, not the workflow")
	}
	slices.Sort(invoked)

	for _, target := range invoked {
		if !slices.Contains(covered, target) {
			t.Errorf("the CI workflow runs `make %s`, which `make ci-full` does not reach.\n"+
				"ci-full covers: %s\n"+
				"A contributor who runs ci-full before pushing would get a green local run "+
				"and a red push — the one thing that target exists to prevent.",
				target, strings.Join(covered, " "))
		}
	}
}

// TestCIWorkflowDoesNotReimplementAMakeTarget. The workflow used to inline its
// own copy of the depguard proof — the same `go test | tee` plus grep that
// ci-depguard-proof runs — so the load-bearing check had two implementations
// and changing one silently left the other behind. Whatever a make target
// already does, the workflow calls; it does not spell out again.
func TestCIWorkflowDoesNotReimplementAMakeTarget(t *testing.T) {
	root := repoRoot(t)
	data, err := os.ReadFile(filepath.Join(root, ".github", "workflows", "ci.yml"))
	if err != nil {
		t.Fatalf("reading the workflow: %v", err)
	}
	// Commands only. The prose around them is free to NAME a target's
	// innards — that is what a comment is for — and an earlier version of
	// this check that scanned the whole file flagged two comments explaining
	// why the split exists.
	commands := workflowCommands(string(data))
	for _, giveaway := range []string{
		"internal/depguardtest",
		"golangci-lint run",
		"go build ./...",
		"go test ./...",
	} {
		if strings.Contains(commands, giveaway) {
			t.Errorf("the workflow runs %q itself instead of calling the make target "+
				"that already does it; the two then drift apart in silence", giveaway)
		}
	}
}

// workflowCommands returns the workflow's shell content with YAML comments
// removed, so prose about a target is not mistaken for a second copy of it.
func workflowCommands(text string) string {
	var b strings.Builder
	for _, line := range strings.Split(text, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "#") {
			continue
		}
		b.WriteString(line)
		b.WriteByte('\n')
	}
	return b.String()
}

// targetClosure returns name plus every prerequisite reachable from it.
func targetClosure(t *testing.T, makefile, name string) []string {
	t.Helper()
	data, err := os.ReadFile(makefile)
	if err != nil {
		t.Fatalf("reading the Makefile: %v", err)
	}
	deps := map[string][]string{}
	for _, line := range strings.Split(string(data), "\n") {
		if strings.HasPrefix(line, "\t") || strings.HasPrefix(line, "#") {
			continue
		}
		m := makeRule.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		for _, d := range strings.Fields(m[2]) {
			if d != "|" { // order-only marker
				deps[m[1]] = append(deps[m[1]], d)
			}
		}
	}

	var out []string
	var walk func(string)
	walk = func(n string) {
		if slices.Contains(out, n) {
			return
		}
		out = append(out, n)
		for _, d := range deps[n] {
			walk(d)
		}
	}
	walk(name)
	slices.Sort(out)
	return out
}
