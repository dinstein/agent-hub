package cli

import (
	"context"
	"fmt"
	"io"
	"strings"
	"text/tabwriter"

	"github.com/spf13/cobra"

	"github.com/dinstein/agent-hub/internal/secrets"
)

// `secret migrate` moves stored credentials between the two PERSISTENT vault
// backends (OS keyring ↔ secrets.enc).
//
// Why this is an explicit command rather than something agenthub does on its
// own: backend availability changes under the operator's feet — installing a
// desktop environment makes the keyring probe start succeeding, setting
// AGENTHUB_SECRET_KEY activates the enc file — and after such a change old
// credentials stay in the old backend. Automatic migration would move
// credentials the operator never asked to move, at a moment they are not
// watching. Moving a credential is theirs to trigger.
//
// The INERT direction is what makes the command worth having: nothing breaks
// when credentials sit in the old backend, because the four-level chain keeps
// resolving them from there — right up until that backend stops being
// available, at which point the credential appears to have vanished.

// SecretMigrateRow is the outcome for one ref. There is deliberately no value
// field here either (secret.go's invariant: no command in this group ever
// prints a secret value).
type SecretMigrateRow struct {
	Server string `json:"server"`
	Scope  string `json:"scope"`
	Key    string `json:"key"`
	// Status is migrated | skipped | failed. "skipped" means the ref was not
	// present in the source backend, which is not an error.
	Status string `json:"status"`
	Error  string `json:"error,omitempty"`
}

// SecretMigrateResult is the `secret migrate` result.
type SecretMigrateResult struct {
	From    string             `json:"from"`
	To      string             `json:"to"`
	DryRun  bool               `json:"dry_run,omitempty"`
	Results []SecretMigrateRow `json:"results"`
	// Failed counts refs whose migration did not complete. Any non-zero
	// value makes the command exit non-zero: a partial migration that
	// reported success would be discovered later, as a missing credential.
	Failed int `json:"failed"`
}

// Human renders one line per ref plus a summary.
func (r SecretMigrateResult) Human(w io.Writer) error {
	if len(r.Results) == 0 {
		_, err := fmt.Fprintf(w, "no secrets found in %s; nothing to migrate\n", r.From)
		return err
	}
	verb := "migrating"
	if !r.DryRun {
		verb = "migrated"
	}
	tw := tabwriter.NewWriter(w, 0, 4, 2, ' ', 0)
	_, _ = fmt.Fprintln(tw, "SERVER\tKEY\tSCOPE\tSTATUS\tDETAIL")
	for _, row := range r.Results {
		_, _ = fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n",
			row.Server, row.Key, row.Scope, row.Status, row.Error)
	}
	if err := tw.Flush(); err != nil {
		return err
	}
	if r.DryRun {
		_, err := fmt.Fprintf(w, "\ndry run: %s %d secret(s) from %s to %s; nothing was changed\n",
			verb, len(r.Results), r.From, r.To)
		return err
	}
	if _, err := fmt.Fprintf(w, "\n%s %d secret(s) from %s to %s\n",
		verb, len(r.Results)-r.Failed, r.From, r.To); err != nil {
		return err
	}
	if r.Failed > 0 {
		_, err := fmt.Fprintf(w,
			"%d failed and were left in %s as well as any partial copy in %s; "+
				"agenthub keeps both copies rather than risk dropping one\n",
			r.Failed, r.From, r.To)
		return err
	}
	return nil
}

func (a *App) newSecretMigrateCmd() *cobra.Command {
	var (
		from   string
		to     string
		dryRun bool
	)
	kinds := backendSpellings()
	cmd := &cobra.Command{
		Use:   "migrate --from <backend> --to <backend> [server] [--dry-run]",
		Short: "Move stored credentials between vault backends (" + kinds + ")",
		Long: "Move stored credentials from one persistent vault backend to another.\n\n" +
			"Backends: " + kinds + ". Environment variables are not a backend —\n" +
			"they are per-process input, so nothing can be migrated into or out of them.\n\n" +
			"Each secret is copied, READ BACK from the destination and only then\n" +
			"deleted from the source. If any step fails, both copies are kept and the\n" +
			"secret is reported as failed: a duplicated credential is recoverable, a\n" +
			"dropped one is not.\n\n" +
			"Values are never displayed. Pass a server id to migrate only that\n" +
			"server's credentials.",
		Args: rangeArgs(0, 1),
		RunE: func(cmd *cobra.Command, args []string) error {
			filterServer := ""
			if len(args) == 1 {
				filterServer = args[0]
			}
			fromKind, toKind, err := parseMigrateBackends(cmd, from, to)
			if err != nil {
				return err
			}
			res, err := a.migrateSecrets(cmd.Context(), fromKind, toKind, filterServer, dryRun)
			if err != nil {
				return err
			}
			if err := a.printer().Emit(res); err != nil {
				return err
			}
			if res.Failed > 0 {
				return &silentExitError{code: ExitGeneral}
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&from, "from", "", "source backend ("+kinds+")")
	cmd.Flags().StringVar(&to, "to", "", "destination backend ("+kinds+")")
	cmd.Flags().BoolVar(&dryRun, "dry-run", false,
		"list what would move without touching either backend")
	return cmd
}

// backendSpellings renders the frozen backend names for help text.
func backendSpellings() string {
	names := make([]string, 0, len(secrets.BackendKinds()))
	for _, k := range secrets.BackendKinds() {
		names = append(names, string(k))
	}
	return strings.Join(names, " | ")
}

// parseMigrateBackends validates --from/--to. Both are REQUIRED and must
// differ: there is no default direction, because guessing which way an
// operator meant to move their credentials is precisely the decision this
// command exists to leave with them.
func parseMigrateBackends(cmd *cobra.Command, from, to string) (secrets.BackendKind, secrets.BackendKind, error) {
	known := map[string]secrets.BackendKind{}
	for _, k := range secrets.BackendKinds() {
		known[string(k)] = k
	}
	pick := func(flag, val string) (secrets.BackendKind, error) {
		if val == "" {
			e := Usagef("--%s is required (%s)", flag, backendSpellings())
			e.Hint = helpHint(cmd)
			return "", e
		}
		k, ok := known[val]
		if !ok {
			e := Usagef("unknown backend %q for --%s (%s)", val, flag, backendSpellings())
			e.Hint = helpHint(cmd)
			return "", e
		}
		return k, nil
	}
	f, err := pick("from", from)
	if err != nil {
		return "", "", err
	}
	t, err := pick("to", to)
	if err != nil {
		return "", "", err
	}
	if f == t {
		return "", "", Usagef("--from and --to are both %q; nothing to migrate", f)
	}
	return f, t, nil
}

// migrateSecrets performs the migration through backend-level stores.
//
// The stores come from Chain.Backend and NOT from the *Chain itself. This is
// load-bearing, not stylistic: Chain.Get consults the environment levels
// first, so an AGENTHUB_SECRET_<KEY> variable would satisfy the read-back
// verification while the destination backend held nothing — and the source
// entry would then be deleted. secrets.Migrate documents this requirement;
// this is the caller that has to honor it.
func (a *App) migrateSecrets(ctx context.Context, from, to secrets.BackendKind, filterServer string, dryRun bool) (SecretMigrateResult, error) {
	chain, _, err := a.secretChain()
	if err != nil {
		return SecretMigrateResult{}, err
	}
	out := SecretMigrateResult{From: string(from), To: string(to), DryRun: dryRun, Results: []SecretMigrateRow{}}

	// Resolve BOTH backends before moving anything: discovering halfway
	// through that the destination cannot be written is how a half-migrated
	// vault happens.
	src, err := chain.Backend(ctx, from)
	if err != nil {
		return SecretMigrateResult{}, classifyBackendError(err, from)
	}
	dst, err := chain.Backend(ctx, to)
	if err != nil {
		return SecretMigrateResult{}, classifyBackendError(err, to)
	}

	refs, err := chain.List(ctx)
	if err != nil {
		return SecretMigrateResult{}, classifySecretsError(err)
	}

	// Narrow to refs the SOURCE actually holds. Chain.List is the union of
	// both backends, so migrating it wholesale would report every secret that
	// only ever lived in the destination as "skipped" — noise that hides the
	// refs that genuinely did not move.
	var present []secrets.Ref
	for _, ref := range refs {
		if filterServer != "" && ref.ServerID != filterServer {
			continue
		}
		_, ok, err := src.Get(ctx, ref)
		if err != nil {
			// Fail-closed: an unreadable source entry is reported, never
			// treated as absent — "absent" would silently drop it from the
			// migration and leave it behind.
			out.Results = append(out.Results, migrateRow(ref, "failed", err))
			out.Failed++
			continue
		}
		if ok {
			present = append(present, ref)
		}
	}

	if dryRun {
		for _, ref := range present {
			out.Results = append(out.Results, migrateRow(ref, "would migrate", nil))
		}
		return out, nil
	}

	for _, r := range secrets.Migrate(ctx, src, dst, present) {
		switch {
		case r.Err != nil:
			out.Results = append(out.Results, migrateRow(r.Ref, "failed", r.Err))
			out.Failed++
		case r.Migrated:
			out.Results = append(out.Results, migrateRow(r.Ref, "migrated", nil))
		default:
			// Present a moment ago, absent when Migrate read it: a concurrent
			// writer. Reported rather than dropped.
			out.Results = append(out.Results, migrateRow(r.Ref, "skipped", nil))
		}
	}
	return out, nil
}

func migrateRow(ref secrets.Ref, status string, err error) SecretMigrateRow {
	row := SecretMigrateRow{
		Server: ref.ServerID, Scope: scopeOrDefault(ref.Scope), Key: ref.Key, Status: status,
	}
	if err != nil {
		row.Error = err.Error()
	}
	return row
}

// classifyBackendError turns an unavailable backend into guidance. It is a
// usage problem, not an internal one: the operator named a backend this
// machine cannot serve.
func classifyBackendError(err error, kind secrets.BackendKind) error {
	if err == nil {
		return nil
	}
	if strings.Contains(err.Error(), secrets.ErrBackendUnavailable.Error()) {
		e := Usagef("%v", err)
		e.Hint = fmt.Sprintf("agenthub doctor reports which backends this machine can serve; "+
			"%q is not one of them right now", kind)
		return e
	}
	return classifySecretsError(err)
}
