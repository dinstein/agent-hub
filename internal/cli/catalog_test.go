package cli

import (
	"slices"
	"strings"
	"testing"

	"github.com/dinstein/agent-hub/internal/catalog"
)

// The catalog round trip: browse, inspect, add, and see the definition land
// in the registry the same way `server add` puts one there.
func TestCatalogRoundTrip(t *testing.T) {
	setDataDir(t)

	var list CatalogList
	decodeInto(t, mustRun(t, "", "catalog", "ls", "--json"), &list)
	if len(list) != len(catalog.List()) {
		t.Fatalf("ls returned %d entries, want %d", len(list), len(catalog.List()))
	}

	var found CatalogList
	decodeInto(t, mustRun(t, "", "catalog", "search", "github", "--json"), &found)
	if len(found) == 0 || found[0].ID != "github" {
		t.Fatalf("search github = %+v", found)
	}

	// The parameter path, on an entry that declares one.
	var view CatalogEntryView
	decodeInto(t, mustRun(t, "", "catalog", "show", "filesystem", "--json"), &view)
	if view.ID != "filesystem" || !view.NeedsConfig {
		t.Fatalf("show = %+v", view)
	}
	// The add command is shown with placeholders, not with the example
	// value: a line that runs unchanged with someone else's data in it is
	// worse than one the user must obviously fill in.
	if !strings.Contains(view.AddCommand, "--param directory=<directory>") {
		t.Errorf("add command = %q", view.AddCommand)
	}

	var added CatalogAdded
	decodeInto(t, mustRun(t, "", "catalog", "add", "filesystem",
		"--name", "work-files", "--param", "directory=/tmp/work", "--json"), &added)
	if added.Added.ID != "work-files" || added.CatalogID != "filesystem" {
		t.Fatalf("added = %+v", added)
	}
	if added.Added.Source != "catalog:filesystem" {
		t.Errorf("source = %q", added.Added.Source)
	}
	if !slices.Contains(added.Added.Args, "/tmp/work") {
		t.Errorf("parameter not substituted: %v", added.Added.Args)
	}
	if len(added.NextSteps) != 0 {
		t.Errorf("next steps = %v, want none for a credential-free entry", added.NextSteps)
	}

	// The credential path, on an entry that declares one. The two are separate
	// entries because no curated entry carries both any more; Render's own
	// test covers a parameter and a secret in the same definition.
	var keyed CatalogEntryView
	decodeInto(t, mustRun(t, "", "catalog", "show", "brave-search", "--json"), &keyed)
	if !slices.Contains(keyed.RequiredKeys, "BRAVE_API_KEY") {
		t.Errorf("required keys = %v", keyed.RequiredKeys)
	}

	var withKey CatalogAdded
	decodeInto(t, mustRun(t, "", "catalog", "add", "brave-search",
		"--name", "web-search", "--json"), &withKey)
	// A secret reference reaches the registry VERBATIM; resolving it here
	// would put a credential into a registry document.
	if withKey.Added.Env["BRAVE_API_KEY"] != "${SECRET_BRAVE_API_KEY}" {
		t.Errorf("secret placeholder mangled: %q", withKey.Added.Env["BRAVE_API_KEY"])
	}
	if !slices.Contains(withKey.NextSteps, "agenthub secret set web-search BRAVE_API_KEY") {
		t.Errorf("next steps = %v", withKey.NextSteps)
	}

	// They really landed in the registry, under the names that were asked for.
	var servers ServerList
	decodeInto(t, mustRun(t, "", "server", "ls", "--json"), &servers)
	if len(servers) != 2 {
		t.Fatalf("server ls = %+v", servers)
	}
	for _, want := range []string{"web-search", "work-files"} {
		if !slices.ContainsFunc(servers, func(s ServerRow) bool { return s.ID == want }) {
			t.Errorf("server ls = %+v, missing %q", servers, want)
		}
	}
}

// The one-click case: no flags at all, and the human output still says what
// is left to do (nothing, here).
func TestCatalogAddOneClick(t *testing.T) {
	setDataDir(t)
	out := mustRun(t, "", "catalog", "add", "playwright")
	if !strings.Contains(out, "added: playwright (stdio, source=catalog:playwright)") {
		t.Errorf("output = %q", out)
	}
	if strings.Contains(out, "next:") {
		t.Errorf("a credential-free entry must not print next steps: %q", out)
	}
}

// An OAuth entry adds in one click, and the login it still needs is named.
func TestCatalogAddOAuthEntryNamesTheLogin(t *testing.T) {
	setDataDir(t)
	out := mustRun(t, "", "catalog", "add", "sentry")
	if !strings.Contains(out, "next: agenthub auth login sentry") {
		t.Errorf("output = %q", out)
	}
}

func TestCatalogFailureModes(t *testing.T) {
	cases := []struct {
		name string
		args []string
		want int
		// wantIn is a substring the stderr must contain.
		wantIn string
	}{
		{"unknown id on show", []string{"catalog", "show", "nope"}, ExitNotFound, "no catalog entry"},
		{"unknown id on add", []string{"catalog", "add", "nope"}, ExitNotFound, "no catalog entry"},
		{"missing parameter", []string{"catalog", "add", "filesystem"}, ExitUsage, "directory"},
		{
			"unknown parameter",
			[]string{"catalog", "add", "filesystem", "--param", "directory=/tmp", "--param", "nope=1"},
			ExitUsage, "nope",
		},
		{
			"malformed parameter",
			[]string{"catalog", "add", "filesystem", "--param", "NOEQUALS"},
			ExitUsage, "KEY=VALUE",
		},
		{"search without a query", []string{"catalog", "search"}, ExitUsage, ""},
		{"ls with an argument", []string{"catalog", "ls", "x"}, ExitUsage, ""},
		{"unknown subcommand", []string{"catalog", "bogus"}, ExitUsage, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			setDataDir(t)
			code, _, stderr := runCLI(t, "", tc.args...)
			if code != tc.want {
				t.Fatalf("exit = %d, want %d (stderr: %s)", code, tc.want, stderr)
			}
			if tc.wantIn != "" && !strings.Contains(stderr, tc.wantIn) {
				t.Errorf("stderr = %q, want it to mention %q", stderr, tc.wantIn)
			}
		})
	}
}

// A refused add must leave nothing behind: validation runs before the store
// is opened.
func TestCatalogAddRefusalWritesNothing(t *testing.T) {
	setDataDir(t)
	if code, _, _ := runCLI(t, "", "catalog", "add", "filesystem"); code != ExitUsage {
		t.Fatalf("exit = %d", code)
	}
	var servers ServerList
	decodeInto(t, mustRun(t, "", "server", "ls", "--json"), &servers)
	if len(servers) != 0 {
		t.Errorf("a refused add left %+v behind", servers)
	}
}

// Adding the same catalog entry twice is a conflict, not a silent
// replacement — the same rule `server add` follows.
func TestCatalogAddTwiceConflicts(t *testing.T) {
	setDataDir(t)
	mustRun(t, "", "catalog", "add", "chrome-devtools")
	code, _, stderr := runCLI(t, "", "catalog", "add", "chrome-devtools")
	if code != ExitGeneral {
		t.Fatalf("exit = %d, want %d (stderr: %s)", code, ExitGeneral, stderr)
	}
	if !strings.Contains(stderr, "already exists") {
		t.Errorf("stderr = %q", stderr)
	}
}

// Human output is rendered from the same structure as --json (the output
// package's contract), so the table must carry the fields a user picks from.
func TestCatalogHumanTable(t *testing.T) {
	setDataDir(t)
	out := mustRun(t, "", "catalog", "ls")
	for _, want := range []string{"ID", "TRANSPORT", "SETUP", "DESCRIPTION", "one-click", "needs directory"} {
		if !strings.Contains(out, want) {
			t.Errorf("table missing %q:\n%s", want, out)
		}
	}
}

// `catalog show` must not let "curated" read as "verified": the provenance
// line says what the grading is.
func TestCatalogShowQualifiesProvenance(t *testing.T) {
	setDataDir(t)
	out := mustRun(t, "", "catalog", "show", "playwright")
	if !strings.Contains(out, "not a verification") {
		t.Errorf("show output must qualify provenance:\n%s", out)
	}
}
