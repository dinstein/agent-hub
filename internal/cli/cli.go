// Package cli implements the agenthub command tree (connect, server, tool,
// client, daemon, grant, doctor) on top of cobra.
//
// Constraints (canonical.md §2, §3):
//   - Depends on the api client + internal/registry (plus the
//     zero-dependency foundations internal/platform and the sibling output
//     package); the daemon/gateway entry subcommands additionally assemble
//     internal/daemon and internal/gateway, and `server tool ls` reads the catalog
//     through internal/router + internal/discovery so the CLI ranks with the
//     SAME ranker the gateway's search_tools uses. Registry writes go
//     offline-direct through registry.Store.Update (cross-process .lock +
//     atomic write) until daemon-mediated writes land.
//   - Resource groups use the singular as the canonical command name with
//     the plural as a cobra alias (server/servers, client/clients).
//   - Every command supports --json; human and JSON output are rendered
//     from the same data structure via internal/cli/output.
//   - Exit codes follow the frozen table in docs/modules/controlplane.md (see errors.go);
//     cobra usage errors are funneled to exit 2 by construction.
package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"slices"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/dinstein/agent-hub/internal/cli/output"
	"github.com/dinstein/agent-hub/internal/platform"
	"github.com/dinstein/agent-hub/internal/registry"
)

// Options configures one CLI invocation. Everything is injectable so tests
// can run commands hermetically.
type Options struct {
	Version string
	Args    []string // argv without the program name
	Stdin   io.Reader
	Stdout  io.Writer
	Stderr  io.Writer
	// Resolver overrides platform path resolution (nil = real environment;
	// AGENTHUB_DATA_DIR et al. are honored through it either way).
	Resolver *platform.Resolver
	// LockTimeout overrides the registry lock timeout (0 = registry default).
	LockTimeout time.Duration
	// ReducedHelp drops the Daemon and Manage groups from the help pages,
	// leaving a shipped build's page at Setup -> Wire up -> connect. Set for
	// release builds only (main.channel); every one of those commands stays
	// registered and stays runnable — this narrows what the help page
	// TEACHES, never what the binary can do.
	//
	// What survives is the whole everyday path, plus the one command that says
	// which step of it broke: register a server and authorize it, build a
	// profile, bind a client to it, and `doctor` when one of those does not
	// work. Withholding the profile commands (as the Scope group once did) left
	// a shipped build able to connect a client while giving it no vocabulary
	// for what that client would then see; withholding `doctor` (as Manage
	// once did) left it able to describe the path but not to diagnose it.
	//
	// One field rather than one per group: there is a single reason to
	// withhold any of them (this is a shipped build) and no caller that wants
	// a partial reduction. Two bools driven off one comparison are two
	// chances for a third group to be added and left behind.
	ReducedHelp bool
}

// App carries the per-invocation state shared by all commands.
type App struct {
	version     string
	stdin       io.Reader
	stdout      io.Writer
	stderr      io.Writer
	resolver    *platform.Resolver
	lockTimeout time.Duration

	reducedHelp bool

	jsonOut bool // bound to the persistent --json flag
}

// Main runs the CLI and returns the process exit code. It is the single
// place where errors are classified (ExitCodeFor) and reported (Fail); RunE
// implementations only return typed errors and never print errors
// themselves.
func Main(opts Options) int {
	app := &App{
		version:     opts.Version,
		stdin:       opts.Stdin,
		stdout:      opts.Stdout,
		stderr:      opts.Stderr,
		resolver:    opts.Resolver,
		lockTimeout: opts.LockTimeout,

		reducedHelp: opts.ReducedHelp,
	}
	if app.stdin == nil {
		app.stdin = os.Stdin
	}
	if app.stdout == nil {
		app.stdout = os.Stdout
	}
	if app.stderr == nil {
		app.stderr = os.Stderr
	}
	if app.resolver == nil {
		app.resolver = platform.Default()
	}

	root := app.newRoot()
	root.SetArgs(opts.Args)
	root.SetIn(app.stdin)
	root.SetOut(app.stdout)
	root.SetErr(app.stderr)

	// Before cobra gets to answer a help flag, because answering it is the
	// bug (see helpForUnknownSubcommand).
	err := helpForUnknownSubcommand(root, opts.Args)
	if err == nil {
		err = root.ExecuteContext(context.Background())
	}
	code := ExitCodeFor(err)
	if err == nil {
		return code
	}
	var silent *silentExitError
	if errors.As(err, &silent) {
		return code // already reported through the output layer
	}
	// The bound flag may not have been parsed if the failure happened during
	// flag parsing itself, so fall back to scanning raw args for --json.
	jsonMode := app.jsonOut || slices.Contains(opts.Args, "--json")
	output.New(app.stdout, app.stderr, jsonMode).Fail(errorDetailFor(err))
	return code
}

// newRoot builds the cobra command tree. SilenceUsage/SilenceErrors are set
// because Main owns all error reporting and exit-code mapping; cobra must
// not print or wrap anything on its own.
func (a *App) newRoot() *cobra.Command {
	root := &cobra.Command{
		Use:   "agenthub",
		Short: "Give your AI clients MCP tools: add servers once, choose what each client sees",
		Long: "Add and authorize each MCP server once here, and every AI client reaches them\n" +
			"through agenthub. Bring servers in with 'agenthub server', hand them to a\n" +
			"client with 'agenthub client'; to give one client less than everything, put\n" +
			"it on a profile.",
		Version:           a.version,
		SilenceUsage:      true,
		SilenceErrors:     true,
		Args:              cobra.ArbitraryArgs,
		RunE:              groupRunE,
		CompletionOptions: cobra.CompletionOptions{DisableDefaultCmd: true},
	}
	root.SetVersionTemplate("agenthub {{.Version}}\n")
	// Global (cobra keeps this in a package var), so it governs EVERY group's
	// listing, not just the root's: declaration order becomes display order
	// everywhere. Alphabetical put `auth` at the head of Setup — the last step
	// of bringing a server in, shown as the first. Each group below is
	// therefore declared read-then-write, or in lifecycle order.
	cobra.EnableCommandSorting = false
	root.PersistentFlags().BoolVar(&a.jsonOut, "json", false,
		"emit a machine-readable JSON envelope instead of human output")
	// Funnel every flag-parse error (unknown flag, bad value, ...) into the
	// typed usage error so it maps to exit 2. Inherited by all subcommands.
	root.SetFlagErrorFunc(func(cmd *cobra.Command, err error) error {
		e := Usagef("%s", err.Error())
		e.Hint = helpHint(cmd)
		return e
	})
	// `server` leads and `catalog` trails: the catalog holds a small curated
	// set, so leading with it teaches a path that ends in "not listed" for
	// most servers. `agenthub server add --url ...` is the general answer and
	// is what Setup should show first.
	//
	// `secret` sits directly after `auth` because the two are one question
	// answered twice: how does this server prove who we are. `auth` covers the
	// servers that hand out their own credential, `secret` the ones that take
	// an API key you already hold, and a page showing only the first implies
	// the second is handled for you.
	//
	// It used to be withheld, on the reasoning that credentials are handled
	// for the operator anyway. They are not: no command but `secret set` ever
	// reads a credential (it is the only caller of readSecretValue), and
	// `catalog show` already prints "store it with 'agenthub secret set …'"
	// for every entry that needs one — a shipped build was recommending a
	// command its own help page withheld. The alternative a hidden `secret`
	// leaves is `--env KEY=<literal>`, which writes the key into the registry;
	// that is the one thing the registry must never hold, so the everyday path
	// has to show the way that does not.
	addGrouped(root, groupSetup, a.newServerCmd(), a.newAuthCmd(), a.newSecretCmd(), a.newCatalogCmd())
	// `profile` sits in Wire up beside `client` because the two halves of one
	// question live here: a profile says what a surface CONTAINS, `client
	// bind` says who gets it. Splitting them across a visible and a withheld
	// group left a shipped build able to connect a client but not to say what
	// that client would see.
	//
	// `skill` is deliberately NOT here. Materializing skill packages is a
	// separate job from giving a client MCP tools, and a shipped build's help
	// page is a recommendation of the everyday path — a third entry beside
	// `profile` and `client` reads as a third required step.
	addGrouped(root, groupWire, a.newProfileCmd(), a.newClientCmd())
	// Hidden takes these two groups off the help page only: cobra still
	// resolves and runs them, so `agenthub config ls` or `agenthub audit tail`
	// behaves identically in a release build. Routing them through the same
	// call as every other group is what keeps a newly added member of one of
	// them from being left visible.
	//
	// The daemon group is drawn along one line: every member is inert without
	// a running daemon. `session` and `events` say so in their own help text,
	// and `token` mints credentials for the daemon's HTTP data plane — with no
	// daemon the command has no subject. Grouping by that shared prerequisite
	// rather than by topic answers "is the daemon up?" once for the section
	// instead of once per command, and `daemon` leads so the answer is the
	// first thing on offer.
	addGroupedHidden(root, a.reducedHelp, groupDaemon,
		a.newDaemonCmd(), a.newSessionCmd(), a.newTokenCmd())
	// Everything else, and the title says so rather than naming a theme the
	// membership does not honor. These run against local state with nothing
	// started.
	addGroupedHidden(root, a.reducedHelp, groupManage,
		a.newConfigCmd(),
		a.newSkillCmd())
	// `doctor` is VISIBLE, in a shipped build too, and it is alone.
	//
	// It was in Manage, which made a release build teach a linear path —
	// register a server, authorize it, build a profile, bind a client — and
	// withhold the one command that says which step of it broke. That is the
	// same fault `secret` had: `catalog show` printed "store it with 'agenthub
	// secret set …'" while the help page hid `secret`, so a shipped build was
	// recommending a command it did not teach. Here the gap is worse than a
	// dangling recommendation — the everyday path has failure modes (no
	// handshake, a client config pointing at a stale binary, a cold launcher
	// cache) and withholding `doctor` left the user's next move unnamed.
	//
	// One member, and a group rather than a line in Wire up, because it answers
	// a different question from everything around it. Setup and Wire up are
	// steps to take; this is what to run when a step did not take. Filing it
	// under either would read as a third required step in a path that has two.
	//
	// If a second diagnostic ever earns a place on a shipped help page it
	// belongs here — but the bar is the same one `doctor` had to clear, which
	// is that a user following the everyday path is left stuck without it.
	addGrouped(root, groupDiagnose, a.newDoctorCmd())
	// The three record readers, VISIBLE for the same reason `doctor` is.
	//
	// They were in Manage, so a release build recorded every call a client
	// made and every state change a server went through, and then withheld
	// all three readers of that record. `doctor` answers "is the wiring
	// right"; nothing on a shipped help page answered "what did the client
	// actually call, and what did the server do afterwards" — the two
	// questions that arrive once the wiring IS right and an answer still
	// came back wrong. A ledger nothing on the page can open reads as no
	// ledger at all, which is the `secret` fault a third time.
	//
	// A group rather than three more lines under Diagnose, because these are
	// not only failure tools: `audit tail` is how an operator reads what a
	// client has been doing on a working installation. Diagnose is what to
	// run when a step did not take; this is what happened.
	//
	// It sits AFTER Diagnose, in triage order. `doctor` is the first move
	// when something is wrong and it decides in one command; these three are
	// the second, and they ask the reader to know what they are looking for.
	//
	// The membership is one line: each is a projection of a file on disk, and
	// none needs a daemon — which is why they cannot go back to Daemon above.
	// `events` was in Daemon while it subscribed to the SSE stream; it now
	// reads the event log, which a stdio gateway writes with no daemon in the
	// picture, so the one testable question that split is made on answers
	// differently. `audit` leads because the call is what a user came to ask
	// about; `events` then says what the server was doing at the time, and
	// `logs` carries the prose around both.
	addGrouped(root, groupObserve, a.newAuditCmd(), a.newEventsCmd(), a.newLogsCmd())
	addGrouped(root, groupEntry, a.newConnectCmd())
	return root
}

// Help groups, in the order the onboarding path actually runs: bring a server
// in -> choose a surface and hand it to an AI client -> start the daemon and
// watch what it serves -> everything else -> what to run when one of those did
// not work -> what actually happened afterwards. `connect` is held apart
// because it is the machine entry point a client's MCP config invokes; a human
// who types it gets a terminal that just hangs on stdio.
//
// There is no separate Scope group: narrowing is what a profile IS, so it
// belongs with the commands that build and hand out profiles rather than in a
// section of its own.
//
// The two WITHHELD groups are split on ONE testable question — does this
// command need a running daemon? — because that is the only line through the
// back half of the CLI that a reader can check against behavior. The former Govern and
// Operate split was thematic, and the themes did not survive contact with the
// membership: `token` is setup, not governance, and `skill` is not an
// operation. A heading that mis-sorts its own members teaches the wrong
// model of the tool, so the fallback group is named for what it honestly is —
// the remainder — rather than given a theme to break.
//
// Manage is now that remainder and nothing else: the three record readers it
// used to hold answer one question between them and have their own group, so
// what is left is a config editor and a skill materializer. That is a
// remainder worth keeping withheld, and no longer one big enough to be
// mistaken for a section.
//
// `secret` was the other member cited there, and it has since moved to Setup,
// where that sentence said it belonged all along. Manage's title drops
// "credentials" with it: naming a theme no member carries is the same fault
// read from the other end.
var (
	groupSetup  = &cobra.Group{ID: "setup", Title: "Setup — bring a downstream server in:"}
	groupWire   = &cobra.Group{ID: "wire", Title: "Wire up — choose a surface, hand it to an AI client:"}
	groupDaemon = &cobra.Group{ID: "daemon", Title: "Daemon — the coordination plane and what rides on it:"}
	groupManage = &cobra.Group{ID: "manage", Title: "Manage — governance and local state:"}
	// Diagnose sits after Manage and before the machine entry point, so on a
	// shipped page it lands directly under Wire up: the path, then what to run
	// when the path does not work.
	groupDiagnose = &cobra.Group{ID: "diagnose", Title: "Diagnose — when something is not working:"}
	// Observe follows Diagnose, and is the other visible group a shipped build
	// keeps: what was called, what changed, and the prose around both.
	groupObserve = &cobra.Group{ID: "observe", Title: "Observe — what a client called, and what changed:"}
	groupEntry   = &cobra.Group{ID: "entry", Title: "Machine entry point (not for humans):"}
)

// addGrouped stamps every command with its group before adding it. Cobra does
// NOT reject a missing GroupID — it silently files the command under
// "Additional Commands", so a forgotten assignment looks like a rendering
// quirk rather than a bug; tree_test.go asserts none is forgotten.
func addGrouped(root *cobra.Command, g *cobra.Group, cmds ...*cobra.Command) {
	addGroupedHidden(root, false, g, cmds...)
}

// addGroupedHidden adds a group whose commands are withheld from the help
// pages when hide is set. It skips AddGroup and leaves GroupID empty rather
// than only setting Hidden, because cobra renders a group's title from the
// group list alone: a declared group with nothing visible under it prints a
// bare heading, which advertises the very section being withheld. Leaving
// GroupID empty is safe precisely because the commands are hidden — the
// "Additional Commands" bucket that an unstamped command would fall into is
// itself only rendered for visible commands.
func addGroupedHidden(root *cobra.Command, hide bool, g *cobra.Group, cmds ...*cobra.Command) {
	if !hide {
		root.AddGroup(g)
	}
	for _, c := range cmds {
		if !hide {
			c.GroupID = g.ID
		}
		c.Hidden = hide
		root.AddCommand(c)
	}
}

// printer builds the output printer for the current invocation. Must be
// called after flag parsing (i.e. inside RunE), when --json is known.
func (a *App) printer() *output.Printer {
	return output.New(a.stdout, a.stderr, a.jsonOut)
}

// openStore resolves the registry directory (honoring AGENTHUB_DATA_DIR /
// AGENTHUB_REGISTRY via internal/platform) and opens the store. Healed
// quarantines are returned as warnings; only unhealable failures are fatal.
func (a *App) openStore() (*registry.Store, []string, error) {
	dir, err := a.resolver.RegistryDir()
	if err != nil {
		return nil, nil, err
	}
	store, oerr := registry.OpenOptions(dir, registry.Options{LockTimeout: a.lockTimeout})
	if store == nil {
		return nil, nil, oerr
	}
	warnings, fatal := splitQuarantine(oerr)
	if fatal != nil {
		return nil, warnings, fatal
	}
	return store, warnings, nil
}

// executable returns the path clients should invoke for the gateway entry:
// the current binary when resolvable, the bare frozen name otherwise.
func (a *App) executable() string {
	if exe, err := os.Executable(); err == nil && exe != "" {
		return exe
	}
	return "agenthub"
}

// groupRunE is the RunE shared by the root and every command group: bare
// invocation shows help (exit 0), an unmatched subcommand is a usage error
// (exit 2). Groups must set Args: cobra.ArbitraryArgs so unmatched names
// reach this function instead of cobra's untyped "unknown command" error.
func groupRunE(cmd *cobra.Command, args []string) error {
	if len(args) == 0 {
		return cmd.Help()
	}
	e := Usagef("unknown command %q for %q", args[0], cmd.CommandPath())
	e.Hint = helpHint(cmd)
	return e
}

// helpForUnknownSubcommand closes the one hole in "an unknown command is
// exit 2", which is otherwise guaranteed by construction (groupRunE).
//
// `agenthub secret get` is refused, correctly. `agenthub secret get --help`
// was not: cobra answers a help flag before RunE ever runs, so it printed the
// `secret` group's help page and exited 0. Which is the worst possible answer,
// because it is indistinguishable from the answer a REAL subcommand gives —
// a reader checking whether `secret get` exists gets a help page and a zero
// status, and concludes it does. It does not, and cannot: stored credential
// values have no read path at all, by design. The one command that would have
// contradicted that design was documented into existence by its own help.
//
// There are two doors into that hole, and closing one is not closing it:
// the help FLAG above, and the help COMMAND — `agenthub help secret get`,
// where cobra's own implementation resolves the deepest matching command and
// ignores whatever is left over, printing the same page with the same zero
// status. Both are the same question ("document this name for me") and now get
// the same answer when the name does not exist.
//
// Scoped to exactly that hole. Without a request for help the RunE path already
// answers, and a command with no subcommands is entitled to positional args —
// only a group, asked about a name it does not have, is answered here.
func helpForUnknownSubcommand(root *cobra.Command, args []string) error {
	path, wantsHelp := helpRequest(args)
	if !wantsHelp {
		return nil
	}
	target, rest, err := root.Find(path)
	if err != nil || target == nil || !target.HasSubCommands() {
		return nil
	}
	for _, arg := range rest {
		// A leftover non-flag token on a group can only be a name it does
		// not have: had it been a subcommand, Find would have descended.
		if strings.HasPrefix(arg, "-") {
			continue
		}
		e := Usagef("unknown command %q for %q", arg, target.CommandPath())
		e.Hint = helpHint(target)
		return e
	}
	return nil
}

// helpRequest reports whether args ask for help and, if so, the command path
// they ask about. `help <path...>` and `<path...> --help` are one question
// asked two ways, so they resolve to one path here rather than to two checks.
//
// The scan stops at the first non-flag token that is not `help`: from there on
// the args are a command path, where only a help flag can still be asking.
func helpRequest(args []string) (path []string, wantsHelp bool) {
	for i, arg := range args {
		if arg == "help" {
			return args[i+1:], true
		}
		if !strings.HasPrefix(arg, "-") {
			break
		}
	}
	if slices.ContainsFunc(args, isHelpFlag) {
		return args, true
	}
	return nil, false
}

// isHelpFlag reports whether arg asks for help.
func isHelpFlag(arg string) bool {
	return arg == "-h" || arg == "--help" || strings.HasPrefix(arg, "--help=")
}

// helpHint is the standard hint attached to usage errors.
func helpHint(cmd *cobra.Command) string {
	return fmt.Sprintf("see '%s --help'", cmd.CommandPath())
}

// exactArgs is cobra.ExactArgs with a typed usage error (exit 2).
func exactArgs(n int) cobra.PositionalArgs {
	return func(cmd *cobra.Command, args []string) error {
		if len(args) != n {
			e := Usagef("%q accepts %d arg(s), received %d", cmd.CommandPath(), n, len(args))
			e.Hint = helpHint(cmd)
			return e
		}
		return nil
	}
}

// noArgs is cobra.NoArgs with a typed usage error (exit 2).
func noArgs(cmd *cobra.Command, args []string) error {
	if len(args) > 0 {
		e := Usagef("%q accepts no arguments, received %d", cmd.CommandPath(), len(args))
		e.Hint = helpHint(cmd)
		return e
	}
	return nil
}

// rangeArgs is cobra.RangeArgs with a typed usage error (exit 2).
func rangeArgs(minArgs, maxArgs int) cobra.PositionalArgs {
	return func(cmd *cobra.Command, args []string) error {
		if len(args) < minArgs || len(args) > maxArgs {
			e := Usagef("%q accepts between %d and %d arg(s), received %d",
				cmd.CommandPath(), minArgs, maxArgs, len(args))
			e.Hint = helpHint(cmd)
			return e
		}
		return nil
	}
}
