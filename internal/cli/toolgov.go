package cli

import (
	"errors"
	"fmt"
	"io"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"github.com/dinstein/agent-hub/internal/confops"
	"github.com/dinstein/agent-hub/internal/gateway"
	"github.com/dinstein/agent-hub/internal/integrity"
	"github.com/dinstein/agent-hub/internal/mcp"
)

// Tool-level governance: enable/disable, local overrides, fingerprint pins
// and the quarantine set. All four are backed by <state> stores that the
// gateway and the daemon consult too, so a decision taken here is the same
// decision the call plane enforces.
//
// Everything in this file works OFFLINE from the gateway's tool cache: the
// operator must be able to disable a suspicious tool without first starting
// the server that serves it. That is the whole point of a kill switch.

// ToolStateRow is one tool's governance state.
type ToolStateRow struct {
	Server   string `json:"server"`
	Tool     string `json:"tool"`
	Status   string `json:"status"`
	Disabled bool   `json:"disabled"`
	// CallAllowed folds Status and Disabled into the single call-gating
	// predicate the pipeline uses, so a reader never re-derives it.
	CallAllowed bool   `json:"call_allowed"`
	Reason      string `json:"reason,omitempty"`
	UpdatedAt   string `json:"updated_at,omitempty"`
}

// Human renders one state line.
func (r ToolStateRow) Human(w io.Writer) error {
	_, err := fmt.Fprintf(w, "%s/%s: status=%s disabled=%s callable=%s\n",
		r.Server, r.Tool, r.Status, boolText(r.Disabled), boolText(r.CallAllowed))
	return err
}

// ToolOverrideRow is one stored override.
type ToolOverrideRow struct {
	Server      string `json:"server"`
	Tool        string `json:"tool"`
	Name        string `json:"name,omitempty"`
	Description string `json:"description,omitempty"`
	Cleared     bool   `json:"cleared,omitempty"`
}

// Human renders the override.
func (r ToolOverrideRow) Human(w io.Writer) error {
	if r.Cleared {
		_, err := fmt.Fprintf(w, "override cleared: %s/%s\n", r.Server, r.Tool)
		return err
	}
	_, err := fmt.Fprintf(w, "override set: %s/%s name=%s desc=%s\n",
		r.Server, r.Tool, dash(r.Name), dash(truncateForLine(r.Description)))
	return err
}

// PinRow is one fingerprint baseline.
type PinRow struct {
	Server      string `json:"server"`
	Tool        string `json:"tool"`
	Hash        string `json:"hash"`
	SchemaVer   string `json:"hash_schema_version"`
	LastChanged string `json:"last_changed"`
	// Drift is the live comparison against the cached catalog: "", "new",
	// "changed" or "removed".
	Drift string `json:"drift,omitempty"`
}

// PinList is the `tool pin` result.
type PinList struct {
	Server      string   `json:"server"`
	Pins        []PinRow `json:"pins"`
	Rebaselined []string `json:"rebaselined,omitempty"`
}

// Human renders the pin table.
func (l PinList) Human(w io.Writer) error {
	if len(l.Rebaselined) > 0 {
		if _, err := fmt.Fprintf(w, "rebaselined %s of %s\n",
			plural(len(l.Rebaselined), "tool", "tools"), l.Server); err != nil {
			return err
		}
	}
	if len(l.Pins) == 0 {
		_, err := fmt.Fprintf(w, "no fingerprint baselines for %s\n", l.Server)
		return err
	}
	tw := tabwriter.NewWriter(w, 0, 4, 2, ' ', 0)
	_, _ = fmt.Fprintln(tw, "TOOL\tHASH\tSCHEMA\tDRIFT\tLAST CHANGED")
	for _, p := range l.Pins {
		_, _ = fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n",
			p.Tool, shortHash(p.Hash), p.SchemaVer, dash(p.Drift), p.LastChanged)
	}
	return tw.Flush()
}

// QuarantineRow is one quarantined tool, keyed by the CLIENT-VISIBLE
// exposed name (that is what an agent could call, so that is what the
// quarantine tracks).
type QuarantineRow struct {
	Exposed     string `json:"exposed"`
	Server      string `json:"server"`
	Tool        string `json:"tool"`
	Reason      string `json:"reason,omitempty"`
	PinnedHash  string `json:"pinned_hash,omitempty"`
	CurrentHash string `json:"current_hash,omitempty"`
	At          string `json:"at"`
}

// QuarantineList is the `tool quarantine ls` result.
type QuarantineList struct {
	Entries []QuarantineRow `json:"entries"`
}

// Human renders the quarantine table.
func (l QuarantineList) Human(w io.Writer) error {
	if len(l.Entries) == 0 {
		_, err := fmt.Fprintln(w, "quarantine is empty")
		return err
	}
	tw := tabwriter.NewWriter(w, 0, 4, 2, ' ', 0)
	_, _ = fmt.Fprintln(tw, "EXPOSED\tSERVER/TOOL\tREASON\tSINCE")
	for _, e := range l.Entries {
		_, _ = fmt.Fprintf(tw, "%s\t%s/%s\t%s\t%s\n", e.Exposed, e.Server, e.Tool, dash(e.Reason), e.At)
	}
	return tw.Flush()
}

// QuarantineRelease is the `tool quarantine release` result.
type QuarantineRelease struct {
	Exposed string `json:"exposed"`
	Server  string `json:"server"`
	Tool    string `json:"tool"`
	// Rebaselined reports whether the pin was moved to the current content
	// as part of the release. Releasing WITHOUT rebaselining would put the
	// tool straight back into quarantine on the next drift check.
	Rebaselined bool   `json:"rebaselined"`
	Note        string `json:"note,omitempty"`
}

// Human renders the release outcome.
func (r QuarantineRelease) Human(w io.Writer) error {
	if _, err := fmt.Fprintf(w, "released: %s (%s/%s)\n", r.Exposed, r.Server, r.Tool); err != nil {
		return err
	}
	if r.Rebaselined {
		if _, err := fmt.Fprintf(w, "pin rebaselined to the current definition\n"); err != nil {
			return err
		}
	}
	if r.Note != "" {
		_, err := fmt.Fprintf(w, "note: %s\n", r.Note)
		return err
	}
	return nil
}

// integrityStores bundles the three <state> stores this file writes.
type integrityStores struct {
	approvals  *integrity.ApprovalStore
	pins       *integrity.PinStore
	quarantine *integrity.QuarantineStore
}

func (a *App) integrityStores() (*integrityStores, error) {
	dir, err := a.stateDir()
	if err != nil {
		return nil, err
	}
	opts := integrity.Options{LockTimeout: a.lockTimeout}
	ap, err := integrity.OpenApprovalStore(dir, opts)
	if err != nil {
		return nil, err
	}
	pins, err := integrity.OpenPinStore(dir, opts)
	if err != nil {
		return nil, err
	}
	q, err := integrity.OpenQuarantineStore(dir, opts)
	if err != nil {
		return nil, err
	}
	return &integrityStores{approvals: ap, pins: pins, quarantine: q}, nil
}

// cachedToolSnapshot looks one tool up in the gateway's persisted catalog
// cache and converts it into the integrity snapshot shape.
func (a *App) cachedToolSnapshot(server, tool string) (integrity.ToolSnapshot, bool, error) {
	defs, err := a.cachedTools(server)
	if err != nil {
		return integrity.ToolSnapshot{}, false, err
	}
	for _, d := range defs {
		if d.Name == tool {
			return snapshotOf(d), true, nil
		}
	}
	return integrity.ToolSnapshot{}, false, nil
}

// cachedTools returns one server's cached tool definitions.
func (a *App) cachedTools(server string) ([]mcp.ToolDef, error) {
	cached, err := gateway.LoadToolCache(a.resolver, nil)
	if err != nil {
		return nil, err
	}
	return cached[server], nil
}

func snapshotOf(d mcp.ToolDef) integrity.ToolSnapshot {
	return integrity.ToolSnapshot{Name: d.Name, Description: d.Description, InputSchema: d.InputSchema}
}

// registerToolGovCmds attaches the governance subcommands to the `tool`
// group built in tool.go.
func (a *App) registerToolGovCmds(cmd *cobra.Command) {
	cmd.AddCommand(
		a.newToolDisableCmd(true),
		a.newToolDisableCmd(false),
		a.newToolOverrideCmd(),
		a.newToolPinCmd(),
		a.newToolQuarantineCmd(),
	)
}

// newToolDisableCmd builds `tool disable` / `tool enable`: the operator's
// kill switch, orthogonal to the approval state (integrity 7.5 — Disabled
// is a flag, not a state, so disabling never discards an approval).
func (a *App) newToolDisableCmd(disable bool) *cobra.Command {
	verb, short := "enable", "Re-enable a tool that was switched off"
	if disable {
		verb, short = "disable", "Switch a tool off for every client (approval state preserved)"
	}
	return &cobra.Command{
		Use:   verb + " <server> <tool>",
		Short: short,
		Args:  exactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			server, tool := args[0], args[1]
			state, err := a.opsState()
			if err != nil {
				return err
			}
			res, err := confops.SetToolEnabled(cmd.Context(), nil, state, server, tool, !disable,
				a.cachedToolSnapshot, noPrecondition)
			if err != nil {
				return classifyIntegrityError(opsError(err), server, tool)
			}
			return a.printer().Emit(toolStateRowOf(res.Record))
		},
	}
}

func toolStateRowOf(rec integrity.ApprovalRecord) ToolStateRow {
	return ToolStateRow{
		Server: rec.Server, Tool: rec.Tool, Status: string(rec.Status),
		Disabled: rec.Disabled, CallAllowed: rec.CallAllowed(),
		Reason: string(rec.Reason), UpdatedAt: rec.UpdatedAt.UTC().Format(time.RFC3339),
	}
}

func (a *App) newToolOverrideCmd() *cobra.Command {
	var (
		name  string
		desc  string
		clear bool
	)
	cmd := &cobra.Command{
		Use:   "override <server> <tool> [--name n] [--desc d] [--clear]",
		Short: "Locally rename a tool or replace a poisoned description",
		Long: "Override how one tool is presented to clients.\n\n" +
			"--desc is the neutralization path for a prompt-injection carrier: the\n" +
			"downstream keeps its description, agenthub simply stops forwarding it.\n" +
			"Overrides are keyed by the RAW downstream tool name, so a rename can\n" +
			"never move a tool out from under its own override.",
		Args: exactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			server, tool := args[0], args[1]
			nameSet, descSet := cmd.Flags().Changed("name"), cmd.Flags().Changed("desc")
			if clear && (nameSet || descSet) {
				e := Usagef("--clear cannot be combined with --name or --desc")
				e.Hint = helpHint(cmd)
				return e
			}
			if !clear && !nameSet && !descSet {
				e := Usagef("pass --name, --desc or --clear")
				e.Hint = helpHint(cmd)
				return e
			}
			stateDir, err := a.stateDir()
			if err != nil {
				return err
			}
			edit := confops.ToolOverrideEdit{Clear: clear}
			if nameSet {
				edit.Name = &name
			}
			if descSet {
				edit.Description = &desc
			}
			res, err := confops.SetToolOverride(cmd.Context(), nil, stateDir, server, tool, edit, noPrecondition)
			if err != nil {
				return opsError(err)
			}
			if res.Cleared {
				return a.printer().Emit(ToolOverrideRow{Server: server, Tool: tool, Cleared: true})
			}
			return a.printer().Emit(ToolOverrideRow{
				Server: server, Tool: tool, Name: res.Override.Name, Description: res.Override.Description,
			})
		},
	}
	cmd.Flags().StringVar(&name, "name", "", "expose the tool under this name instead")
	cmd.Flags().StringVar(&desc, "desc", "", "replace the downstream description (empty string blanks it)")
	cmd.Flags().BoolVar(&clear, "clear", false, "remove the override")
	return cmd
}

func (a *App) newToolPinCmd() *cobra.Command {
	var rebaseline bool
	cmd := &cobra.Command{
		Use:   "pin <server> [--rebaseline]",
		Short: "Show (or reset) a server's tool fingerprint baselines",
		Long: "Show the integrity fingerprint baselines of one server.\n\n" +
			"--rebaseline accepts the CURRENT definitions as the new baseline. It is\n" +
			"an explicit trust decision: every pending rug-pull mark for the server\n" +
			"is cleared, so run it only after reviewing the diff.",
		Args: exactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			server := args[0]
			stores, err := a.integrityStores()
			if err != nil {
				return err
			}
			defs, err := a.cachedTools(server)
			if err != nil {
				return err
			}
			list := PinList{Server: server, Pins: []PinRow{}}
			if rebaseline {
				if len(defs) == 0 {
					e := NotFoundf(CodeToolNotFound, "no cached tools for server %q", server)
					e.Hint = "connect a client once so the gateway can populate the tool cache"
					return e
				}
				for _, d := range defs {
					if _, err := stores.pins.Rebaseline(cmd.Context(), server, d.Name, snapshotOf(d)); err != nil {
						return classifyIntegrityError(err, server, d.Name)
					}
					list.Rebaselined = append(list.Rebaselined, d.Name)
				}
			}
			pins, err := stores.pins.Pins(cmd.Context())
			if err != nil {
				return classifyIntegrityError(err, server, "")
			}
			current := map[string]integrity.ToolSnapshot{}
			for _, d := range defs {
				current[d.Name] = snapshotOf(d)
			}
			for _, tool := range sortedKeys(pins[server]) {
				p := pins[server][tool]
				row := PinRow{
					Server: server, Tool: tool, Hash: p.Hash, SchemaVer: p.HashSchemaVersion,
					LastChanged: p.LastChanged.UTC().Format(time.RFC3339),
				}
				if snap, ok := current[tool]; !ok {
					row.Drift = "removed"
				} else if fp, ferr := integrity.Fingerprint(snap); ferr == nil && fp != p.Hash {
					row.Drift = "changed"
				}
				list.Pins = append(list.Pins, row)
			}
			for _, tool := range sortedKeys(current) {
				if _, ok := pins[server][tool]; !ok {
					list.Pins = append(list.Pins, PinRow{Server: server, Tool: tool, Drift: "new"})
				}
			}
			return a.printer().Emit(list)
		},
	}
	cmd.Flags().BoolVar(&rebaseline, "rebaseline", false, "accept the current definitions as the new baseline")
	return cmd
}

func (a *App) newToolQuarantineCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "quarantine",
		Short: "Inspect and release quarantined tools",
		Args:  cobra.ArbitraryArgs,
		RunE:  groupRunE,
	}
	cmd.AddCommand(a.newQuarantineLsCmd(), a.newQuarantineReleaseCmd())
	return cmd
}

func (a *App) newQuarantineLsCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "ls",
		Short: "List quarantined tools (keyed by the client-visible exposed name)",
		Args:  noArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			stores, err := a.integrityStores()
			if err != nil {
				return err
			}
			snap, err := stores.quarantine.Snapshot(cmd.Context())
			if err != nil {
				return classifyIntegrityError(err, "", "")
			}
			list := QuarantineList{Entries: []QuarantineRow{}}
			for _, exposed := range sortedKeys(snap) {
				e := snap[exposed]
				list.Entries = append(list.Entries, QuarantineRow{
					Exposed: exposed, Server: e.Server, Tool: e.Tool, Reason: e.Reason,
					PinnedHash: e.PinnedHash, CurrentHash: e.CurrentHash,
					At: e.At.UTC().Format(time.RFC3339),
				})
			}
			return a.printer().Emit(list)
		},
	}
}

func (a *App) newQuarantineReleaseCmd() *cobra.Command {
	var noRebaseline bool
	cmd := &cobra.Command{
		Use:   "release <exposed-name>",
		Short: "Release a quarantined tool and rebaseline its fingerprint",
		Args:  exactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			exposed := args[0]
			stores, err := a.integrityStores()
			if err != nil {
				return err
			}
			entry, found, err := stores.quarantine.Release(cmd.Context(), exposed)
			if err != nil {
				return classifyIntegrityError(err, "", "")
			}
			if !found {
				e := NotFoundf(CodeToolNotFound, "%q is not quarantined", exposed)
				e.Hint = "run 'agenthub tool quarantine ls' to see quarantined tools"
				return e
			}
			res := QuarantineRelease{Exposed: exposed, Server: entry.Server, Tool: entry.Tool}
			if noRebaseline {
				res.Note = "pin left untouched: the next drift check will quarantine this tool again"
				return a.printer().Emit(res)
			}
			snap, ok, err := a.cachedToolSnapshot(entry.Server, entry.Tool)
			if err != nil {
				return err
			}
			if !ok {
				res.Note = "no cached definition to rebaseline against; the pin was left untouched"
				return a.printer().Emit(res)
			}
			if _, err := stores.pins.Rebaseline(cmd.Context(), entry.Server, entry.Tool, snap); err != nil {
				return classifyIntegrityError(err, entry.Server, entry.Tool)
			}
			if _, err := stores.approvals.Approve(cmd.Context(), entry.Server, entry.Tool); err != nil &&
				!errors.Is(err, integrity.ErrNotFound) {
				return classifyIntegrityError(err, entry.Server, entry.Tool)
			}
			res.Rebaselined = true
			return a.printer().Emit(res)
		},
	}
	cmd.Flags().BoolVar(&noRebaseline, "no-rebaseline", false,
		"release without accepting the current definition (it will be re-quarantined)")
	return cmd
}

// classifyIntegrityError maps the integrity stores' sentinels onto the
// frozen exit-code table. A corrupt store is exit 7 (like a corrupt
// registry) and never reads as "nothing recorded" — treating it as empty
// would silently un-quarantine every isolated tool.
func classifyIntegrityError(err error, server, tool string) error {
	switch {
	case err == nil:
		return nil
	case errors.Is(err, integrity.ErrNotFound):
		e := NotFoundf(CodeToolNotFound, "no integrity record for %s/%s", server, tool)
		e.Hint = "connect a client once so the gateway observes the tool, or check the names"
		return e
	case errors.Is(err, integrity.ErrStoreCorrupt):
		return &Error{
			Code: CodeStateCorrupt, ExitCode: ExitLocked,
			Message: err.Error(),
			Hint:    "the state file was NOT rewritten; inspect it before retrying",
			Err:     err,
		}
	case errors.Is(err, integrity.ErrLockTimeout):
		return &Error{
			Code: CodeLockTimeout, ExitCode: ExitLocked,
			Message: err.Error(),
			Hint:    "another agenthub process holds the integrity lock; retry in a moment",
			Err:     err,
		}
	}
	return err
}

// shortHash renders a fingerprint compactly for tables while keeping its
// version prefix (the prefix is what says which formula produced it).
func shortHash(h string) string {
	const keep = 16
	if len(h) <= keep {
		return h
	}
	return h[:keep] + "…"
}

// truncateForLine bounds a description for one-line human output.
func truncateForLine(s string) string { return oneLine(s, descriptionColumnBytes) }
