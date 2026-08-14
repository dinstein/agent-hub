package buildrules

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// TestEveryEventKindIsDocumented keeps internal/eventlog's closed vocabulary
// honest against the table that publishes it.
//
// The vocabulary is the whole reason the stream exists. Everything it
// records was already being logged as prose, and prose cannot be matched on:
// a UI timeline, a `--json` consumer or an alert needs values it is allowed
// to switch on. That promise is only as good as a reader's ability to find
// out what the values ARE, which is the table in docs/subsystems/records.md.
//
// A kind missing from that table is the failure worth catching, and it is
// invisible otherwise: the event is still written, the package still
// compiles, `make ci` is still green, and only the consumer that was
// supposed to recognize it silently does not — months later, in a UI that
// renders a blank row.
//
// The reverse direction is checked too. A documented kind that no longer
// exists sends a reader looking for records that can never appear, and they
// have no way to tell that from "this has not happened yet".
func TestEveryEventKindIsDocumented(t *testing.T) {
	root := repoRoot(t)
	declared := parseEventKinds(t, filepath.Join(root, "internal", "eventlog", "eventlog.go"))
	documented := parseDocumentedKinds(t, filepath.Join(root, "docs", "subsystems", "records.md"))

	if len(declared) == 0 {
		t.Fatal("no Kind constants found; this test asserted nothing")
	}
	for kind := range declared {
		if !documented[kind] {
			t.Errorf("internal/eventlog declares the kind %q, which docs/subsystems/records.md "+
				"does not list.\nA consumer switching on it has no way to learn it exists.", kind)
		}
	}
	for kind := range documented {
		if !declared[kind] {
			t.Errorf("docs/subsystems/records.md lists the event kind %q, which "+
				"internal/eventlog no longer declares.\nA reader waiting for it cannot tell "+
				"that from \"it has not happened yet\".", kind)
		}
	}
}

// eventKindDecl matches `KindSomething Kind = "wire_name"`, which is the one
// form the constants are written in.
var eventKindDecl = regexp.MustCompile(`^\s*Kind\w+\s+Kind\s*=\s*"([a-z_]+)"`)

func parseEventKinds(t *testing.T, path string) map[string]bool {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]bool{}
	for _, line := range strings.Split(string(data), "\n") {
		if m := eventKindDecl.FindStringSubmatch(line); m != nil {
			out[m[1]] = true
		}
	}
	return out
}

// documentedKind matches one backticked kind inside a table cell. Bare
// lowercase words count — `connected` and `stopped` are kinds — which is
// only safe because the scan is confined to the KINDS COLUMN of one table.
// Run over the whole file it would claim most of the prose.
var documentedKind = regexp.MustCompile("`([a-z][a-z_]*)`")

func parseDocumentedKinds(t *testing.T, path string) map[string]bool {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]bool{}
	var afterMarker, sawRow bool
	for _, line := range strings.Split(string(data), "\n") {
		if strings.Contains(line, eventKindTableMarker) {
			afterMarker = true
			continue
		}
		if !afterMarker {
			continue
		}
		trimmed := strings.TrimSpace(line)
		if !strings.HasPrefix(trimmed, "|") {
			if sawRow {
				break // the table ended; what follows is prose
			}
			continue // the blank line between the marker and the header
		}
		sawRow = true
		// | scope | kinds |  ->  ["", " scope ", " kinds ", ""]
		cells := strings.Split(trimmed, "|")
		if len(cells) < 3 {
			continue
		}
		// Column 2 only. The scope column holds `server`/`gateway`/`daemon`,
		// which are scopes and not kinds; reading the whole row would
		// "document" three values the package never declares.
		for _, m := range documentedKind.FindAllStringSubmatch(cells[2], -1) {
			out[m[1]] = true
		}
	}
	return out
}

// eventKindTableMarker is the sentinel comment that opens the table. It is
// explicit rather than inferred from a heading so that reformatting the
// surrounding section cannot silently switch this check off.
const eventKindTableMarker = "<!-- event-kinds -->"
