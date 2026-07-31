package cli

import (
	"context"
	"fmt"
	"maps"
	"slices"
	"strings"

	"github.com/spf13/cobra"

	"github.com/dinstein/agent-hub/api"
	"github.com/dinstein/agent-hub/internal/confops"
	"github.com/dinstein/agent-hub/internal/discovery"
	"github.com/dinstein/agent-hub/internal/gateway"
	"github.com/dinstein/agent-hub/internal/registry"
	"github.com/dinstein/agent-hub/internal/scope"
)

// Shared plumbing for the command groups added by the CLI completion task:
// registry mutation helpers, the online-only gate, the CLI-owned state
// files, and the three-state selector parsing that profile and session
// both speak.

// stateDir resolves <data>/state, the home of the shared state files that are
// not the registry — the active-profile marker and the rate limiter's buckets.
// It does NOT create the directory (readers must not have side effects);
// writers call platform.EnsureDir themselves.
func (a *App) stateDir() (string, error) { return a.resolver.StateDir() }

// activeProfile reads the globally active profile name from the registry.
// An unreadable marker reads as "no active profile" — the same fail-closed
// direction dangling profile references take (docs/architecture.md §7): the failure
// mode of a corrupt marker must be "no narrowing source", never "some
// arbitrary profile".
func (a *App) activeProfile() (string, error) {
	store, _, err := a.openStore()
	if err != nil {
		return "", err
	}
	return confops.ActiveProfile(store)
}

// requireDaemon is the exit-4 gate of docs/modules/controlplane.md's online/offline matrix:
// commands whose subject is a daemon RUNTIME object (sessions, the event
// stream, a live audit tail) must refuse with E_DAEMON_DOWN rather than
// invent an offline answer.
func (a *App) requireDaemon(ctx context.Context) (*ctlClient, api.Hello, error) {
	socket, err := a.resolver.CtlSocketPath()
	if err != nil {
		return nil, api.Hello{}, err
	}
	hello, perr := pingDaemon(ctx, socket)
	if perr != nil {
		return nil, api.Hello{}, DaemonDownf("daemon is not reachable at %s", socket)
	}
	ctl, err := a.newCtlClient()
	if err != nil {
		return nil, api.Hello{}, err
	}
	return ctl, hello, nil
}

// addSelectorFlags declares the --only/--all/--none trio. Every command that
// narrows a server's tools declares it HERE rather than beside its own RunE,
// so the two layers cannot end up with the trio spelled two ways — a flag
// that exists at one altitude and not the other reads as a difference in the
// mechanism, which there is none of. Only the help strings differ, because
// only the altitude does.
func addSelectorFlags(cmd *cobra.Command, only *[]string, all, none *bool, onlyHelp, allHelp, noneHelp string) {
	cmd.Flags().StringSliceVar(only, "only", nil, onlyHelp)
	cmd.Flags().BoolVar(all, "all", false, allHelp)
	cmd.Flags().BoolVar(none, "none", false, noneHelp)
}

// parseSelectorFlags validates the mutually exclusive --only/--all/--none
// trio and returns the confops three-state selection. Exactly one must be
// present: an edit command with no edit is a usage error, not a silent
// no-op. The SEMANTICS of the three states live in internal/confops; this
// function only decides which one the operator asked for.
func parseSelectorFlags(cmd *cobra.Command, only []string, all, none bool) (confops.ToolSelection, error) {
	n := 0
	sel := confops.ToolSelection{}
	if len(only) > 0 || cmd.Flags().Changed("only") {
		n++
		sel = confops.ToolSelection{Mode: confops.ToolSelectOnly, Tools: dedupSorted(only)}
	}
	if all {
		n++
		sel = confops.ToolSelection{Mode: confops.ToolSelectAll}
	}
	if none {
		n++
		sel = confops.ToolSelection{Mode: confops.ToolSelectNone}
	}
	switch {
	case n == 0:
		e := Usagef("one of --only, --all or --none is required")
		e.Hint = helpHint(cmd)
		return confops.ToolSelection{}, e
	case n > 1:
		e := Usagef("--only, --all and --none are mutually exclusive")
		e.Hint = helpHint(cmd)
		return confops.ToolSelection{}, e
	}
	if sel.Mode == confops.ToolSelectOnly && len(sel.Tools) == 0 {
		e := Usagef("--only needs at least one tool name (use --none to block all)")
		e.Hint = helpHint(cmd)
		return confops.ToolSelection{}, e
	}
	return sel, nil
}

// unknownToolWarning cross-checks an `--only` list against the tool catalog
// agenthub last recorded for that server, and names the entries that are not
// in it.
//
// It WARNS rather than refuses, and the difference matters in both
// directions. An allow-list is an intersection, so a misspelled name lets
// nothing through for that server: the failure is fail-closed and therefore
// safe, but it is also completely invisible — the command reports success,
// `profile ls` shows the rule exactly as typed, and the tool is simply
// missing in the client, a symptom nobody traces back to a spelling mistake
// in a command that said OK. Refusing instead would break the legitimate
// case in the other direction: the cache is cold until a gateway has
// connected once, so a rule written ahead of the first connection has
// nothing to check against and must still be storable.
//
// Hence the three silences below — no `--only`, no readable cache, no
// catalog for this server — which all mean "no opinion", never "no problem".
func (a *App) unknownToolWarning(server string, sel confops.ToolSelection) []string {
	if sel.Mode != confops.ToolSelectOnly || len(sel.Tools) == 0 {
		return nil
	}
	cached, err := gateway.LoadToolCacheEntries(a.resolver, nil)
	if err != nil {
		return nil
	}
	defs := cached[server].Tools
	if len(defs) == 0 {
		return nil
	}
	known := make(map[string]bool, len(defs))
	for _, d := range defs {
		known[d.Name] = true
	}
	var absent []string
	for _, name := range sel.Tools {
		if !known[name] {
			absent = append(absent, fmt.Sprintf("%q", name))
		}
	}
	if len(absent) == 0 {
		return nil
	}
	return []string{fmt.Sprintf(
		"%s has no recorded tool named %s; the rule was stored, but an allow-list is an "+
			"intersection, so a name matching nothing lets nothing through. Check the spelling "+
			"with 'agenthub server inspect %s --tools'.",
		server, strings.Join(absent, ", "), server)}
}

// describeSelector renders a selector for the human table.
func describeSelector(sel registry.ToolSelector) string {
	var parts []string
	switch {
	case sel.Allow == nil:
		parts = append(parts, "all tools")
	case len(sel.Allow) == 0:
		parts = append(parts, "BLOCKED (no tools)")
	default:
		parts = append(parts, "only "+strings.Join(sel.Allow, ","))
	}
	return strings.Join(parts, "; ")
}

// describeServers renders a three-state server list for the human table.
func describeServers(list []string) string {
	switch {
	case list == nil:
		return "(all registered)"
	case len(list) == 0:
		return "(none)"
	default:
		return strings.Join(list, ",")
	}
}

// defaultProfileToken is what every listing calls the fallback a client with
// no profile of its own follows. It is ONE token in `profile ls` and in
// `client ls` on purpose: the two tables previously named the same thing
// "(active)" and nothing at all, so a reader had no way to connect a client
// row to anything the profile table showed. confops refuses to create a
// profile whose name could shadow it.
const defaultProfileToken = "(default)"

// describeDefaultProfile renders that fallback: the token alone when no
// active profile is set, and the token pointing at the active one when there
// is. A pointer that resolves nowhere carries the same MISSING marker a
// bound-but-missing profile does — it fail-closes to an empty scope in
// exactly the same way, and the two must not read differently.
func describeDefaultProfile(active string, dangling bool) string {
	if active == "" {
		return defaultProfileToken
	}
	text := defaultProfileToken + " -> " + active
	if dangling {
		text += "  " + missingProfileMarker
	}
	return text
}

// missingProfileMarker is the loud form of a profile reference that resolves
// nowhere: the resolver fail-closes it to an EMPTY scope, and a silent empty
// set would read as "this client has nothing configured".
const missingProfileMarker = "MISSING -> empty scope"

// describeDiscovery renders a discovery cell as the mode that will actually
// be used, marking the ones the row did not set itself.
//
// The column used to print the raw configured value, so "-" meant both "no
// mode here" and "the mode is decided elsewhere" — while the answer is three
// levels away (profile > governance.json > the built-in). Printing the
// resolved mode makes the column answer the question it is named after; the
// suffix keeps it from reading as a setting the profile owns.
func describeDiscovery(effective string, own bool) string {
	if own {
		return effective
	}
	return effective + " (inherited)"
}

// The three sources a discovery mode can come from, as the JSON output
// names them.
const (
	discoveryFromProfile = "profile"
	discoveryFromGlobal  = "global"
	discoveryFromBuiltin = "builtin"
)

// discoveryOf resolves how one profile's tools will be presented, and where
// that answer came from. An empty name asks about no profile at all — the
// global default, which is what an unbound client gets while 'profile use'
// is unset.
//
// The layer chain is walked by scope.DiscoveryFor, the same construction and
// the same precedence rule a live session goes through, and the built-in
// default is applied by discovery, which owns it. This function composes
// them; it decides nothing itself, because a listing that decided would
// eventually describe a resolution that no longer happens.
func discoveryOf(snap *registry.Snapshot, profileName string) (mode, source string) {
	raw, from, set := scope.DiscoveryFor(snap, profileName)
	// ModeOf also degrades a value no version of agenthub recognises — a
	// hand-edited governance.json — to the built-in, so an unreadable mode
	// is reported as the mode that will actually be used.
	resolved := discovery.ModeOf(&scope.EffectiveScope{Discovery: raw})
	switch {
	case !set || string(resolved) != string(raw):
		return string(resolved), discoveryFromBuiltin
	case from == scope.LayerProfile:
		return string(resolved), discoveryFromProfile
	default:
		return string(resolved), discoveryFromGlobal
	}
}

// parseToolSpecs parses the frozen `--tools s:t1,t2` session/scope flag
// (canonical.md §3) into serverID -> tool names. A repeated server merges.
// An empty tool list after the colon is block-all, which is why the empty
// slice is preserved rather than normalized to nil.
func parseToolSpecs(specs []string) (map[string][]string, error) {
	if len(specs) == 0 {
		return nil, nil
	}
	out := map[string][]string{}
	for _, spec := range specs {
		server, list, ok := strings.Cut(spec, ":")
		server = strings.TrimSpace(server)
		if !ok || server == "" {
			return nil, Usagef("--tools expects <server>:<tool>[,<tool>...], got %q", spec)
		}
		tools := out[server]
		if tools == nil {
			tools = []string{}
		}
		for _, t := range strings.Split(list, ",") {
			if t = strings.TrimSpace(t); t != "" {
				tools = append(tools, t)
			}
		}
		out[server] = dedupSorted(tools)
	}
	return out, nil
}

// dedupSorted trims, de-duplicates and sorts a string slice while keeping
// the nil/empty distinction: nil in stays nil, an all-blank input becomes
// the EMPTY (block-all) slice, never nil.
func dedupSorted(in []string) []string {
	if in == nil {
		return nil
	}
	out := make([]string, 0, len(in))
	for _, s := range in {
		if s = strings.TrimSpace(s); s != "" {
			out = append(out, s)
		}
	}
	slices.Sort(out)
	return slices.Compact(out)
}

// requireServer resolves a server id against the registry snapshot, turning
// a typo into exit 3 instead of a silently created ghost entry.
func requireServer(snap *registry.Snapshot, id string) (registry.ServerEntry, error) {
	doc, ok := snap.Servers.V.Servers[id]
	if !ok {
		e := NotFoundf(CodeServerNotFound, "no server %q", id)
		e.Hint = "run 'agenthub server ls' to see configured servers"
		return registry.ServerEntry{}, e
	}
	return doc.V, nil
}

// sortedKeys returns a map's keys in ascending order (deterministic output
// is a contract: both renderings and the golden tests depend on it).
func sortedKeys[V any](m map[string]V) []string {
	return slices.Sorted(maps.Keys(m))
}

// boolText renders a boolean for the human tables.
func boolText(v bool) string {
	if v {
		return "yes"
	}
	return "no"
}

// plural is a tiny helper for human output ("1 entry" / "3 entries").
func plural(n int, one, many string) string {
	if n == 1 {
		return fmt.Sprintf("%d %s", n, one)
	}
	return fmt.Sprintf("%d %s", n, many)
}
