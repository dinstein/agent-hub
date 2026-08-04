package cli

import (
	"strings"
	"testing"

	"github.com/spf13/cobra"
)

// TestAShippedPageNeverRecommendsWhatItWithholds is the mechanical form of a
// rule this package has learned four separate times.
//
// The rule: a command a shipped page recommends must be a command that page
// teaches. Each time it broke the shape was identical — visible prose naming
// a command a release build hides, so the user is told to run something they
// cannot discover, cannot `--help`, and have no reason to believe exists.
// `catalog show` printed "store it with `agenthub secret set …`" while
// `secret` was withheld; `doctor`, the three record readers and `config` each
// arrived the same way, recommended or presupposed by a page that kept them
// back.
//
// Until now the rule lived entirely in prose: the comment above
// withheldCommands argues, one command at a time, why each of those is not
// withheld. That argument is what a reader consults after the fact, and it
// caught none of the four — because the drift is never in the withheld list.
// It is in a Short, a Long or an Example somewhere else, written by someone
// who was not thinking about the release page at all.
//
// Scope, deliberately narrow: the cobra help text of commands a RELEASE build
// shows. Two exclusions, each for a reason:
//
//   - Runtime error hints are out of scope. "run 'agenthub session ls'" inside
//     a session subcommand's error is not a recommendation to someone who
//     cannot see `session` — they reached it by running `session`. The rule is
//     about a PAGE recommending, and the page is the help text.
//   - A withheld command's own help text is out, for the same reason: it is
//     read only by someone who already found it.
func TestAShippedPageNeverRecommendsWhatItWithholds(t *testing.T) {
	found, visited := withheldRecommendations(newReleaseTestRoot(t))
	// A walk that visits nothing agrees with everything. The release page has
	// well over a dozen visible commands and subcommands.
	if visited < 10 {
		t.Fatalf("the walk visited %d visible commands; it is not reaching the tree", visited)
	}
	for _, v := range found {
		t.Errorf("release build: %s's help text recommends %q, which the same page withholds.\n"+
			"The user is told to run a command they cannot discover, cannot --help, and have no "+
			"reason to think exists.\nEither stop recommending it, or stop withholding it — this "+
			"package has chosen the second four times.\nmatched: %q", v.where, v.command, v.matched)
	}
}

// TestTheRecommendationCheckCanFail builds the shape that actually occurred —
// a visible command whose help text quotes a withheld one — and requires the
// scan to report it. Without this, a scan that silently matched nothing would
// pass for the rest of the project's life.
func TestTheRecommendationCheckCanFail(t *testing.T) {
	withheld := withheldCommands[0]
	root := &cobra.Command{Use: "agenthub"}
	visible := &cobra.Command{
		Use:   "catalog",
		Short: "Browse the curated servers",
		// The historical shape, near enough: visible prose telling the user
		// to run something the same build hides.
		Long: "Pick an entry, then start the hub with 'agenthub " + withheld + " start'.",
	}
	root.AddCommand(visible)

	found, visited := withheldRecommendations(root)
	if visited != 1 {
		t.Fatalf("visited %d commands, want the one added", visited)
	}
	if len(found) != 1 {
		t.Fatalf("the scan reported %d violations for a page that plainly has one: %+v", len(found), found)
	}
	if found[0].command != withheld || found[0].where != "catalog" {
		t.Errorf("reported %+v, want the catalog page recommending %q", found[0], withheld)
	}
}

// TestAWithheldCommandsOwnPageIsNotScanned pins the exclusion, so a later
// tightening cannot quietly turn it into a rule that every mention anywhere
// is a violation — which would fail on `daemon`'s own examples.
func TestAWithheldCommandsOwnPageIsNotScanned(t *testing.T) {
	withheld := withheldCommands[0]
	root := &cobra.Command{Use: "agenthub"}
	root.AddCommand(&cobra.Command{
		Use:    withheld,
		Hidden: true,
		Long:   "Start it with 'agenthub " + withheld + " start'.",
	})

	if found, visited := withheldRecommendations(root); len(found) != 0 || visited != 0 {
		t.Errorf("a withheld command's own page was scanned: %d violations over %d commands",
			len(found), visited)
	}
}

type withheldMention struct {
	where   string // the command path whose help text carries it
	command string // the withheld command it names
	matched string // the exact form found, so the failure is reproducible
}

// withheldRecommendations walks the visible half of a command tree and reports
// every help text that names a withheld command as something to run. It also
// returns how many commands it visited, so a caller can refuse a vacuous pass.
func withheldRecommendations(root *cobra.Command) (found []withheldMention, visited int) {
	withheld := map[string]bool{}
	for _, name := range withheldCommands {
		withheld[name] = true
	}

	var walk func(c *cobra.Command, path string)
	walk = func(c *cobra.Command, path string) {
		// A hidden command's page is reached only by someone who already
		// named it, and everything under it inherits that.
		if c.Hidden {
			return
		}
		visited++
		text := strings.Join([]string{c.Short, c.Long, c.Example}, "\n")
		for name := range withheld {
			// The quoted-invocation forms only. A sentence using the word as
			// a noun ("the agenthub daemon") is prose about the process, not
			// an instruction to run a command.
			for _, form := range []string{
				"`agenthub " + name + "`",
				"'agenthub " + name + "'",
				"agenthub " + name + " ",
				"agenthub " + name + "\n",
			} {
				if strings.Contains(text, form) {
					found = append(found, withheldMention{where: path, command: name, matched: form})
					break
				}
			}
		}
		for _, sub := range c.Commands() {
			walk(sub, path+" "+sub.Name())
		}
	}

	for _, c := range root.Commands() {
		walk(c, c.Name())
	}
	return found, visited
}
