package cli

import (
	"fmt"
	"io"
	"strings"
	"text/tabwriter"

	"github.com/spf13/cobra"

	"github.com/dinstein/agent-hub/internal/confops"
)

// The `config` group is the front end of the governance key table, which
// lives in internal/confops so `agenthub config` and the control plane
// cannot disagree about what a key means. Everything here is rendering.

// resultBudgetPrefix mirrors the confops key family `resultBudget.<id|*>`.
const resultBudgetPrefix = confops.ResultBudgetPrefix

// withheldKeyPrefix is the one key family a reduced (release) help surface
// does not teach: the daemon's own HTTP listener, `http.addr` and the two
// confirmations that go with it.
//
// It is withheld for the same reason the whole Daemon group is. Those three
// keys configure a face that only exists while a daemon is running, and a
// shipped build that teaches how to bind it withholds every command that
// starts it, reads its state or mints a credential for it — so the listing
// would name a switch with no path around it.
//
// The decision lives HERE, not in the confops key table, because it is about
// what one front end teaches rather than what a key means: the GUI reads the
// same table through the control plane and keeps listing all three, which is
// how a hub with no command line gets an address at all.
//
// Withholding is not disabling, exactly as for a withheld command: `config
// get http.addr` and `config set http.addr` answer in a release build, and
// the daemon honors whatever is stored. What narrows is the listing, which is
// the recommendation.
const withheldKeyPrefix = "http."

// ConfigEntry is one governance key as both output modes render it.
type ConfigEntry struct {
	Key   string `json:"key"`
	Value string `json:"value"`
	Kind  string `json:"kind"`
	Doc   string `json:"doc,omitempty"`
}

// configEntryOf projects a confops entry into the rendered form.
func configEntryOf(e confops.GovernanceEntry) ConfigEntry {
	return ConfigEntry{Key: e.Key, Value: e.Value, Kind: e.Kind, Doc: e.Doc}
}

// ConfigList is the `config ls` result; `config get` reuses ConfigEntry.
type ConfigList struct {
	Entries []ConfigEntry `json:"entries"`
}

// Human renders the key table.
func (l ConfigList) Human(w io.Writer) error {
	tw := tabwriter.NewWriter(w, 0, 4, 2, ' ', 0)
	_, _ = fmt.Fprintln(tw, "KEY\tVALUE\tTYPE\tDESCRIPTION")
	for _, e := range l.Entries {
		_, _ = fmt.Fprintf(tw, "%s\t%s\t%s\t%s\n", e.Key, dash(e.Value), e.Kind, e.Doc)
	}
	return tw.Flush()
}

// Human renders one key.
func (e ConfigEntry) Human(w io.Writer) error {
	_, err := fmt.Fprintf(w, "%s\n", e.Value)
	return err
}

// ConfigSetResult is the `config set` result.
type ConfigSetResult struct {
	Key      string `json:"key"`
	Value    string `json:"value"`
	Previous string `json:"previous"`
	Changed  bool   `json:"changed"`
}

// Human renders the change.
func (r ConfigSetResult) Human(w io.Writer) error {
	if !r.Changed {
		_, err := fmt.Fprintf(w, "%s already %s\n", r.Key, dash(r.Value))
		return err
	}
	_, err := fmt.Fprintf(w, "%s: %s -> %s\n", r.Key, dash(r.Previous), dash(r.Value))
	return err
}

func (a *App) newConfigCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "config",
		Short: "Read and write the global governance switches (governance.json)",
		Args:  cobra.ArbitraryArgs,
		RunE:  groupRunE,
	}
	cmd.AddCommand(a.newConfigLsCmd(), a.newConfigGetCmd(), a.newConfigSetCmd())
	return cmd
}

func (a *App) newConfigGetCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "get <key>",
		Short: "Print one governance value",
		Args:  exactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			store, warnings, err := a.openStore()
			if err != nil {
				return err
			}
			entry, err := confops.GetGovernance(store.Snapshot().Governance.V, args[0])
			if err != nil {
				return opsError(err)
			}
			return a.printer().Emit(configEntryOf(entry), warnings...)
		},
	}
}

func (a *App) newConfigSetCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "set <key> <value>",
		Short: "Set one governance value",
		Args:  exactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			store, warnings, err := a.opsStore()
			if err != nil {
				return err
			}
			res, err := confops.SetGovernance(cmd.Context(), store, args[0], args[1], noPrecondition)
			warnings = append(warnings, res.Warnings...)
			if err != nil {
				return opsError(err)
			}
			return a.printer().Emit(ConfigSetResult{
				Key: res.Key, Value: res.Value, Previous: res.Previous, Changed: res.Changed,
			}, warnings...)
		},
	}
}

func (a *App) newConfigLsCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "ls",
		Short: "List every governance key with its current value",
		Args:  noArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			store, warnings, err := a.openStore()
			if err != nil {
				return err
			}
			entries := confops.ListGovernance(store.Snapshot().Governance.V)
			list := ConfigList{Entries: make([]ConfigEntry, 0, len(entries))}
			for _, e := range entries {
				if a.reducedHelp && strings.HasPrefix(e.Key, withheldKeyPrefix) {
					continue
				}
				list.Entries = append(list.Entries, configEntryOf(e))
			}
			return a.printer().Emit(list, warnings...)
		},
	}
}
