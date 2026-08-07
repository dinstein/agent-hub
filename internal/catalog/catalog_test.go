package catalog

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/dinstein/agent-hub/internal/confops"
	"github.com/dinstein/agent-hub/internal/registry"
)

// The seed directory is compiled into the binary, so every invariant it must
// hold is a build-time property. These tests are what keep the init-time
// panic in catalog.go from ever reaching a user.

func TestSeedIsValid(t *testing.T) {
	entries, err := parseSeed(seedJSON)
	if err != nil {
		t.Fatalf("embedded seed is invalid: %v", err)
	}
	if len(entries) < 12 {
		t.Fatalf("seed has %d entries, want at least 12", len(entries))
	}
	if !slices.IsSortedFunc(entries, func(a, b Entry) int { return strings.Compare(a.ID, b.ID) }) {
		t.Error("List() must be sorted by id")
	}
	for _, e := range entries {
		if e.Provenance != ProvenanceCurated {
			t.Errorf("%s: seed entries must be curated, got %q", e.ID, e.Provenance)
		}
		if e.Homepage == "" || e.Publisher == "" {
			t.Errorf("%s: a curated entry must name its publisher and homepage", e.ID)
		}
	}
}

// Every seed entry must survive the SAME validation a hand-typed entry gets.
// A curated definition that confops refuses is worse than no catalog: the
// user clicks "add" and gets an error they did not write.
func TestSeedEntriesPassConfopsValidation(t *testing.T) {
	for _, e := range List() {
		params := map[string]string{}
		for _, p := range e.Params {
			params[p.Name] = exampleValue(p)
		}
		entry, err := e.Render(params)
		if err != nil {
			t.Errorf("%s: render: %v", e.ID, err)
			continue
		}
		if err := confops.ValidateServerSpec(confops.ServerSpec{ID: e.ID, Entry: entry}); err != nil {
			t.Errorf("%s: confops refuses the curated definition: %v", e.ID, err)
		}
		if entry.Source != "catalog:"+e.ID {
			t.Errorf("%s: source = %q", e.ID, entry.Source)
		}
		if !entry.Enabled {
			t.Errorf("%s: a freshly added server must be enabled", e.ID)
		}
	}
}

func exampleValue(p Param) string {
	if p.Example != "" {
		return p.Example
	}
	return "value"
}

// TestSeedGolden pins the seed's user-visible shape: which servers are
// offered, how they are invoked, and — the load-bearing column — whether
// adding one is a single click. A change to any of those is a product
// decision, so it has to show up as a golden diff rather than as a silently
// different dialog.
func TestSeedGolden(t *testing.T) {
	var b strings.Builder
	for _, e := range List() {
		target := e.URL
		if e.Transport == registry.TransportStdio {
			target = strings.TrimSpace(e.Command + " " + strings.Join(e.Args, " "))
		}
		b.WriteString(strings.Join([]string{
			e.ID,
			e.Transport,
			target,
			"needsConfig=" + boolStr(e.NeedsConfig()),
			"keys=" + joinOrDash(e.RequiredKeys()),
			"params=" + joinOrDash(paramNames(e)),
			"auth=" + orDash(e.Auth),
		}, "\t"))
		b.WriteString("\n")
	}
	golden := filepath.Join("testdata", "seed.golden")
	got := b.String()
	if os.Getenv("UPDATE_GOLDEN") == "1" {
		if err := os.WriteFile(golden, []byte(got), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(golden)
	if err != nil {
		t.Fatalf("reading golden (regenerate with UPDATE_GOLDEN=1): %v", err)
	}
	if got != string(want) {
		t.Errorf("seed golden mismatch (regenerate with UPDATE_GOLDEN=1)\n got:\n%s\nwant:\n%s", got, want)
	}
}

func boolStr(b bool) string {
	if b {
		return "yes"
	}
	return "no"
}

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

func joinOrDash(v []string) string {
	if len(v) == 0 {
		return "-"
	}
	return strings.Join(v, ",")
}

func paramNames(e Entry) []string {
	out := make([]string, 0, len(e.Params))
	for _, p := range e.Params {
		out = append(out, p.Name)
	}
	return out
}

// The needsConfig split is the whole point of the catalog dialog: the
// no-input entries must be one click, and everything that would otherwise be
// added with a hole in it must not be.
func TestNeedsConfigMatrix(t *testing.T) {
	cases := []struct {
		name string
		e    Entry
		want bool
	}{
		{
			name: "no credential, no parameter",
			e:    Entry{Transport: "stdio", Command: "npx", Args: []string{"-y", "pkg"}},
			want: false,
		},
		{
			name: "required credential",
			e: Entry{
				Transport: "stdio", Command: "npx",
				Env:  map[string]string{"T": "${SECRET_T}"},
				Keys: []Credential{{Key: "T"}},
			},
			want: true,
		},
		{
			name: "optional credential only",
			e: Entry{
				Transport: "stdio", Command: "npx",
				Env:  map[string]string{"T": "${SECRET_T}"},
				Keys: []Credential{{Key: "T", Optional: true}},
			},
			want: false,
		},
		{
			name: "declared parameter",
			e: Entry{
				Transport: "stdio", Command: "npx", Args: []string{"{{dir}}"},
				Params: []Param{{Name: "dir"}},
			},
			want: true,
		},
		{
			name: "undeclared placeholder still needs config",
			e:    Entry{Transport: "stdio", Command: "npx", Args: []string{"{{dir}}"}},
			want: true,
		},
		{
			name: "placeholder in a url",
			e:    Entry{Transport: "http", URL: "https://{{host}}/mcp", Params: []Param{{Name: "host"}}},
			want: true,
		},
		{
			name: "placeholder in a header value",
			e: Entry{
				Transport: "http", URL: "https://x/mcp",
				Headers: map[string]string{"X-Team": "{{team}}"},
				Params:  []Param{{Name: "team"}},
			},
			want: true,
		},
		{
			name: "oauth alone is not configuration",
			e:    Entry{Transport: "http", URL: "https://x/mcp", Auth: AuthOAuth},
			want: false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.e.NeedsConfig(); got != tc.want {
				t.Errorf("NeedsConfig() = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestRenderSubstitutesAndRefuses(t *testing.T) {
	e := Entry{
		ID: "fs", Transport: "stdio", Command: "npx",
		Args:   []string{"-y", "pkg", "{{directory}}"},
		Env:    map[string]string{"T": "${SECRET_T}", "SCOPE": "{{directory}}/sub"},
		Keys:   []Credential{{Key: "T"}},
		Params: []Param{{Name: "directory"}},
	}

	got, err := e.Render(map[string]string{"directory": "/tmp/x"})
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	if got.Args[2] != "/tmp/x" {
		t.Errorf("args = %v", got.Args)
	}
	if got.Env["SCOPE"] != "/tmp/x/sub" {
		t.Errorf("env SCOPE = %q", got.Env["SCOPE"])
	}
	// A secret reference must survive verbatim: resolving it here would put
	// a credential into a registry document.
	if got.Env["T"] != "${SECRET_T}" {
		t.Errorf("secret placeholder was substituted: %q", got.Env["T"])
	}

	var perr *ParamError
	for _, tc := range []struct {
		name   string
		params map[string]string
		check  func(*ParamError) bool
	}{
		{"missing", nil, func(p *ParamError) bool { return slices.Contains(p.Missing, "directory") }},
		{"blank", map[string]string{"directory": "  "}, func(p *ParamError) bool {
			return slices.Contains(p.Missing, "directory")
		}},
		{"unknown", map[string]string{"directory": "/x", "nope": "1"}, func(p *ParamError) bool {
			return slices.Contains(p.Unknown, "nope")
		}},
		{"newline", map[string]string{"directory": "/x\n/y"}, func(p *ParamError) bool {
			return p.Invalid["directory"] != ""
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := e.Render(tc.params)
			if err == nil {
				t.Fatal("want a ParamError, got nil")
			}
			var pe *ParamError
			if !asParamError(err, &pe) {
				t.Fatalf("error = %T %v, want *ParamError", err, err)
			}
			if !tc.check(pe) {
				t.Errorf("ParamError = %+v", pe)
			}
			perr = pe
		})
	}
	_ = perr
}

func asParamError(err error, out **ParamError) bool {
	pe, ok := err.(*ParamError)
	if ok {
		*out = pe
	}
	return ok
}

// A caller that edits the entry it got back must not corrupt the shared
// seed: List/Get/Search hand out copies.
func TestEntriesAreCopies(t *testing.T) {
	first := List()[0]
	id := first.ID
	if len(first.Args) > 0 {
		first.Args[0] = "TAMPERED"
	}
	first.Tags = append(first.Tags, "tampered")
	again, ok := Get(id)
	if !ok {
		t.Fatalf("Get(%q) not found", id)
	}
	if len(again.Args) > 0 && again.Args[0] == "TAMPERED" {
		t.Error("mutating a returned entry changed the shared seed")
	}
	if slices.Contains(again.Tags, "tampered") {
		t.Error("mutating a returned entry's tags changed the shared seed")
	}
}

func TestGetUnknownID(t *testing.T) {
	if _, ok := Get("no-such-server"); ok {
		t.Error("Get returned an unknown id")
	}
	if _, ok := Get(""); ok {
		t.Error("Get returned the empty id")
	}
}

// Search is a user-facing ordering, so it is pinned: the same query must
// always produce the same list, and the obvious query must put the obvious
// entry first.
func TestSearchOrderingIsDeterministic(t *testing.T) {
	cases := []struct {
		query string
		first string
		// contains are ids the result must include.
		contains []string
		// excludes are ids the result must not include.
		excludes []string
	}{
		{query: "git", first: "git", contains: []string{"git", "github"}},
		{query: "github", first: "github"},
		{query: "FILE", first: "filesystem"},
		// A shared topic tag gathers every entry carrying it, and an entry
		// that merely sounds adjacent is not swept in: figma is a "frontend"
		// entry that renders in a browser, it does not drive one.
		{query: "browser", contains: []string{"chrome-devtools", "playwright"}, excludes: []string{"figma"}},
		// A term in the id outranks the same term as a tag, so the entry whose
		// name IS the query comes first.
		{query: "search", first: "brave-search", contains: []string{"cloudflare-docs"}},
		{query: "no-such-thing-at-all", excludes: []string{"git"}},
	}
	for _, tc := range cases {
		t.Run(tc.query, func(t *testing.T) {
			got := ids(Search(tc.query))
			for range 5 {
				if again := ids(Search(tc.query)); !slices.Equal(got, again) {
					t.Fatalf("unstable ordering: %v then %v", got, again)
				}
			}
			if tc.first != "" {
				if len(got) == 0 || got[0] != tc.first {
					t.Errorf("Search(%q) = %v, want %q first", tc.query, got, tc.first)
				}
			}
			for _, want := range tc.contains {
				if !slices.Contains(got, want) {
					t.Errorf("Search(%q) = %v, missing %q", tc.query, got, want)
				}
			}
			for _, bad := range tc.excludes {
				if slices.Contains(got, bad) {
					t.Errorf("Search(%q) = %v, must not contain %q", tc.query, got, bad)
				}
			}
		})
	}
}

// Multiple terms narrow (AND), they do not widen.
func TestSearchTermsAreConjunctive(t *testing.T) {
	both := ids(Search("git repository"))
	if len(both) == 0 {
		t.Fatal("Search(\"git repository\") found nothing")
	}
	for _, id := range both {
		if !slices.Contains(ids(Search("git")), id) {
			t.Errorf("%q matched the two-term query but not %q alone", id, "git")
		}
	}
	if len(both) >= len(ids(Search("git"))) {
		t.Errorf("two terms did not narrow: %v vs %v", both, ids(Search("git")))
	}
}

func TestSearchEmptyQueryListsEverything(t *testing.T) {
	if !slices.Equal(ids(Search("   ")), ids(List())) {
		t.Error("an empty query must return the full list")
	}
}

func ids(entries []Entry) []string {
	out := make([]string, 0, len(entries))
	for _, e := range entries {
		out = append(out, e.ID)
	}
	return out
}

// Load-time validation is the reason the init panic is unreachable in
// practice; each case below is one class of seed defect it must catch.
func TestParseSeedRejectsInvalidEntries(t *testing.T) {
	base := map[string]any{
		"id": "x", "name": "X", "description": "d", "provenance": "curated",
		"transport": "stdio", "command": "npx",
	}
	cases := []struct {
		name  string
		build func(map[string]any)
	}{
		{"no id", func(m map[string]any) { m["id"] = "" }},
		{"no description", func(m map[string]any) { m["description"] = "" }},
		{"unknown provenance", func(m map[string]any) { m["provenance"] = "trusted" }},
		{"unknown auth", func(m map[string]any) { m["auth"] = "apikey" }},
		{"unknown transport", func(m map[string]any) { m["transport"] = "carrier-pigeon" }},
		{"stdio without command", func(m map[string]any) { m["command"] = "" }},
		{"stdio with url", func(m map[string]any) { m["url"] = "https://x" }},
		{"http without url", func(m map[string]any) { m["transport"] = "http"; m["command"] = "" }},
		{"undeclared placeholder", func(m map[string]any) { m["args"] = []string{"{{dir}}"} }},
		{"unreferenced parameter", func(m map[string]any) {
			m["params"] = []map[string]string{{"name": "dir"}}
		}},
		{"undeclared credential", func(m map[string]any) {
			m["env"] = map[string]string{"T": "${SECRET_T}"}
		}},
		{"unreferenced credential", func(m map[string]any) {
			m["keys"] = []map[string]string{{"key": "T"}}
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			entry := map[string]any{}
			for k, v := range base {
				entry[k] = v
			}
			tc.build(entry)
			raw, err := json.Marshal(map[string]any{"version": 1, "entries": []any{entry}})
			if err != nil {
				t.Fatal(err)
			}
			if _, err := parseSeed(raw); err == nil {
				t.Errorf("parseSeed accepted %s", tc.name)
			}
		})
	}
}

func TestParseSeedRejectsWrongVersionAndDuplicates(t *testing.T) {
	entry := map[string]any{
		"id": "x", "name": "X", "description": "d", "provenance": "curated",
		"transport": "stdio", "command": "npx",
	}
	wrongVersion, err := json.Marshal(map[string]any{"version": 99, "entries": []any{entry}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := parseSeed(wrongVersion); err == nil {
		t.Error("parseSeed accepted a future seed version")
	}
	dup, err := json.Marshal(map[string]any{"version": 1, "entries": []any{entry, entry}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := parseSeed(dup); err == nil {
		t.Error("parseSeed accepted a duplicate id")
	}
	if _, err := parseSeed([]byte(`{"version":1,"entries":[{"id":"x","nope":1}]}`)); err == nil {
		t.Error("parseSeed accepted an unknown field")
	}
}

func TestPlaceholderScanning(t *testing.T) {
	cases := []struct {
		in     string
		params []string
		values map[string]string
		out    string
	}{
		{in: "plain", out: "plain"},
		{in: "{{a}}", params: []string{"a"}, values: map[string]string{"a": "1"}, out: "1"},
		{in: "x{{a}}y{{a}}", params: []string{"a", "a"}, values: map[string]string{"a": "1"}, out: "x1y1"},
		{in: "{{a}}", values: nil, out: "{{a}}", params: []string{"a"}},
		{in: "{{ a }}", out: "{{ a }}"},         // spaces: not a placeholder
		{in: "{{a", out: "{{a"},                 // unterminated: literal
		{in: "${SECRET_T}", out: "${SECRET_T}"}, // never touched
	}
	for _, tc := range cases {
		t.Run(tc.in, func(t *testing.T) {
			// slices.Equal treats nil and empty as equal, which is what the
			// "no placeholders" cases want.
			if got := placeholdersIn(tc.in); !slices.Equal(got, tc.params) {
				t.Errorf("placeholdersIn(%q) = %v, want %v", tc.in, got, tc.params)
			}
			if got := substitute(tc.in, tc.values); got != tc.out {
				t.Errorf("substitute(%q) = %q, want %q", tc.in, got, tc.out)
			}
		})
	}
}

func TestSecretRefScanning(t *testing.T) {
	e := Entry{
		Transport: "http", URL: "https://x/${SECRET_A}",
		Headers: map[string]string{"H": "Bearer ${SECRET_B}", "I": "${SECRET_A}"},
	}
	if got := e.SecretRefs(); !slices.Equal(got, []string{"A", "B"}) {
		t.Errorf("SecretRefs() = %v", got)
	}
}
