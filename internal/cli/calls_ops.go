package cli

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/spf13/cobra"

	"github.com/dinstein/agent-hub/internal/calllog"
)

// CallsExport is the summary emitted after an export file is made.
type CallsExport struct {
	Output   string `json:"output"`
	Events   int    `json:"events"`
	Skipped  int    `json:"skippedMalformed"`
	Payloads bool   `json:"payloads"`
}

func (e CallsExport) Human(w io.Writer) error {
	_, err := fmt.Fprintf(w, "exported %d event(s) to %s (payloads: %s, malformed skipped: %d)\n",
		e.Events, e.Output, boolText(e.Payloads), e.Skipped)
	return err
}

type auditExportRecord struct {
	Event              calllog.Event `json:"event"`
	Request            string        `json:"request,omitempty"`
	EffectiveArguments string        `json:"effectiveArguments,omitempty"`
	Result             string        `json:"result,omitempty"`
}

func (a *App) newCallsExportCmd() *cobra.Command {
	var output, sinceRaw string
	var payloads bool
	cmd := &cobra.Command{
		Use:   "export",
		Short: "Export audit events as JSONL to a new private file",
		Args:  noArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			if output == "" || output == "-" {
				return Usagef("--output must name a new file; stdout export is refused")
			}
			since, err := parseAuditSince(sinceRaw)
			if err != nil {
				return err
			}
			root, err := calllog.DefaultDir(a.resolver)
			if err != nil {
				return err
			}
			var keys *auditKeyCache
			if payloads {
				keys = newAuditKeyCache(a, cmd)
				defer keys.close()
			}
			out := CallsExport{Output: output, Payloads: payloads}
			err = writeNewAtomic(output, func(w io.Writer) error {
				enc := json.NewEncoder(w)
				out.Skipped, err = calllog.ScanEventsSince(root, since, func(e calllog.Event) error {
					record := auditExportRecord{Event: e}
					if payloads {
						call := AuditCall{}
						if err := decryptAuditCall(root, keys, []calllog.Event{e}, &call); err != nil {
							return err
						}
						record.Request = call.Request
						record.EffectiveArguments = call.EffectiveArguments
						record.Result = call.Result
					}
					if err := enc.Encode(record); err != nil {
						return err
					}
					out.Events++
					return nil
				})
				return err
			})
			if err != nil {
				return err
			}
			var warnings []string
			if payloads {
				warnings = append(warnings, "export contains decrypted credentials and private user data; protect and delete it when no longer needed")
			}
			return a.printer().Emit(out, warnings...)
		},
	}
	cmd.Flags().StringVar(&output, "output", "", "new JSONL output file (required; must not already exist)")
	cmd.Flags().StringVar(&sinceRaw, "since", "all", "look back by duration or RFC3339 time; use all for no time bound")
	cmd.Flags().BoolVar(&payloads, "payloads", false, "decrypt and include request, effective arguments and captured result")
	return cmd
}

func writeNewAtomic(path string, write func(io.Writer) error) (err error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return err
	}
	dir := filepath.Dir(abs)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".agenthub-audit-export-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer func() { _ = os.Remove(tmpName) }()
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := write(tmp); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	// Link publishes the complete temp inode only if the destination does not
	// already exist. That gives both atomic visibility and no-overwrite safety.
	if err := os.Link(tmpName, abs); err != nil {
		if errors.Is(err, os.ErrExist) {
			return Usagef("output %q already exists", path)
		}
		return fmt.Errorf("publish audit export: %w", err)
	}
	return nil
}

// CallsPrune is the result of applying the configured retention window.
type CallsPrune struct {
	DryRun bool     `json:"dryRun"`
	Before string   `json:"before"`
	Days   int      `json:"days"`
	Bytes  int64    `json:"bytes"`
	Names  []string `json:"names,omitempty"`
}

func (p CallsPrune) Human(w io.Writer) error {
	verb := "removed"
	if p.DryRun {
		verb = "would remove"
	}
	_, err := fmt.Fprintf(w, "%s %d expired day(s), %d bytes (before %s)\n", verb, p.Days, p.Bytes, p.Before)
	return err
}

func (a *App) newCallsPruneCmd() *cobra.Command {
	var dryRun bool
	cmd := &cobra.Command{
		Use:   "prune",
		Short: "Remove whole day partitions outside configured retention",
		Args:  noArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			store, _, err := a.openStore()
			if err != nil {
				return err
			}
			policy := store.Snapshot().Governance.V.ResolvedCalls()
			cutoff := time.Now().UTC().Truncate(24*time.Hour).AddDate(0, 0, -(policy.RetentionDays - 1))
			root, err := calllog.DefaultDir(a.resolver)
			if err != nil {
				return err
			}
			pruned, err := calllog.Prune(root, cutoff, dryRun)
			if err != nil {
				return err
			}
			return a.printer().Emit(CallsPrune{
				DryRun: dryRun, Before: cutoff.Format("2006-01-02"), Days: pruned.Days,
				Bytes: pruned.Bytes, Names: pruned.Names,
			})
		},
	}
	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "report expired partitions without deleting them")
	return cmd
}
