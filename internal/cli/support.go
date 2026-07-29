package cli

import (
	"context"
	"fmt"
	"slices"
	"sort"
	"strings"

	"github.com/spf13/cobra"

	"github.com/dinstein/agent-hub/api"
	"github.com/dinstein/agent-hub/internal/confops"
	"github.com/dinstein/agent-hub/internal/registry"
)

// Shared plumbing for the command groups added by the CLI completion task:
// registry mutation helpers, the online-only gate, the CLI-owned state
// files, and the three-state selector parsing that profile and session
// both speak.

// stateDir resolves <data>/state, the home of the integrity stores and the
// shared state files. It does NOT create the directory (readers must not
// have side effects); writers call platform.EnsureDir themselves.
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

// mutate runs one registry read-modify-write under the cross-process lock
// and folds healed-quarantine reports into warnings, which is the shape
// every write command needs. It is Update plus the boilerplate that would
// otherwise be copy-pasted into a dozen RunE bodies.
func (a *App) mutate(ctx context.Context, fn func(tx *registry.Tx) error) ([]string, error) {
	store, warnings, err := a.openStore()
	if err != nil {
		return warnings, err
	}
	uerr := store.Update(ctx, fn)
	more, fatal := splitQuarantine(uerr)
	warnings = append(warnings, more...)
	return warnings, fatal
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
	if len(sel.Deny) > 0 {
		parts = append(parts, "deny "+strings.Join(sel.Deny, ","))
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
	sort.Strings(out)
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
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
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
