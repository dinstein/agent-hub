package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"text/tabwriter"

	"github.com/spf13/cobra"

	"github.com/dinstein/agent-hub/internal/secrets"
)

// The `secret` group is the CLI face of the four-level vault chain.
//
// INVARIANT (docs/subsystems/cli.md rule 5): no command in this file ever prints a
// secret VALUE, and there is no --show escape hatch to add later. `ls`
// renders key names and the backend only; verification of a value is done
// by `agenthub server test`, which proves it works without putting it in
// the terminal scrollback. The types below carry no value field at all, so
// this is a type-level guarantee rather than a formatting convention.

// keyRegistryFileName mirrors internal/secrets' keyring key registry file.
// It is read (never written) here to classify a ref's backend WITHOUT
// touching the OS keychain — a `secret ls` must not trigger a keychain
// prompt just to draw a table.
const keyRegistryFileName = "keyring-keys.json"

// SecretRow is one stored secret. There is deliberately no value field.
type SecretRow struct {
	Server string `json:"server"`
	Scope  string `json:"scope"`
	Key    string `json:"key"`
	// Backend is where the value lives: env, keyring or enc-file.
	Backend string `json:"backend"`
	// Set is always true for a listed entry; it exists so a consumer can
	// join this list against a server's required keys without inferring
	// presence from list membership.
	Set bool `json:"set"`
}

// SecretList is the `secret ls` result.
type SecretList struct {
	Secrets []SecretRow `json:"secrets"`
}

// Human renders the table — key names and backends only.
func (l SecretList) Human(w io.Writer) error {
	if len(l.Secrets) == 0 {
		_, err := fmt.Fprintln(w, "no secrets stored")
		return err
	}
	tw := tabwriter.NewWriter(w, 0, 4, 2, ' ', 0)
	_, _ = fmt.Fprintln(tw, "SERVER\tKEY\tSCOPE\tBACKEND\tSET")
	for _, s := range l.Secrets {
		_, _ = fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n", s.Server, s.Key, s.Scope, s.Backend, boolText(s.Set))
	}
	if err := tw.Flush(); err != nil {
		return err
	}
	_, err := fmt.Fprintln(w, "\nvalues are never displayed; verify one with 'agenthub server test <server>'")
	return err
}

// SecretChange is the `secret set` / `secret rm` result. No value, ever.
type SecretChange struct {
	Action string `json:"action"`
	Server string `json:"server"`
	Key    string `json:"key"`
	Scope  string `json:"scope"`
}

// Human renders the confirmation.
func (c SecretChange) Human(w io.Writer) error {
	_, err := fmt.Fprintf(w, "%s: %s/%s (scope %s)\n", c.Action, c.Server, c.Key, c.Scope)
	return err
}

func (a *App) newSecretCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:     "secret",
		Aliases: []string{"secrets"}, // singular canonical, plural alias (canonical.md §3)
		Short:   "Manage downstream credentials (values are never displayed)",
		Args:    cobra.ArbitraryArgs,
		RunE:    groupRunE,
	}
	cmd.AddCommand(a.newSecretLsCmd(), a.newSecretSetCmd(), a.newSecretRmCmd(),
		a.newSecretMigrateCmd())
	return cmd
}

// secretChain builds the vault chain for this invocation.
func (a *App) secretChain() (*secrets.Chain, string, error) {
	dir, err := a.secretsDir()
	if err != nil {
		return nil, "", err
	}
	return secrets.NewChain(secrets.ChainConfig{Dir: dir}), dir, nil
}

func (a *App) newSecretSetCmd() *cobra.Command {
	var (
		fromStdin bool
		scopeName string
	)
	cmd := &cobra.Command{
		Use:   "set <server> <KEY> [--stdin]",
		Short: "Store a credential (read no-echo from the terminal, or from stdin)",
		Long: "Store one credential for a downstream server.\n\n" +
			"The value is read without echo from the terminal, or from stdin with\n" +
			"--stdin (trailing newline stripped) for pipelines and CI. It is written\n" +
			"to the OS keyring when one is available and to the encrypted file\n" +
			"otherwise; it is never written to the registry, which is plain\n" +
			"configuration and must never hold a credential.",
		Args: exactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			server, key := args[0], args[1]
			value, err := a.readSecretValue(cmd, fromStdin, server, key)
			if err != nil {
				return err
			}
			if value == "" {
				return Usagef("refusing to store an empty value for %s/%s", server, key)
			}
			chain, _, err := a.secretChain()
			if err != nil {
				return err
			}
			ref := secrets.Ref{ServerID: server, Scope: scopeName, Key: key}
			if err := chain.Set(cmd.Context(), ref, value); err != nil {
				return classifySecretsError(err)
			}
			return a.printer().Emit(SecretChange{
				Action: "stored", Server: server, Key: key, Scope: scopeOrDefault(scopeName),
			})
		},
	}
	cmd.Flags().BoolVar(&fromStdin, "stdin", false, "read the value from stdin instead of prompting")
	cmd.Flags().StringVar(&scopeName, "scope", "", "vault scope name (default "+secrets.DefaultScope+")")
	return cmd
}

// readSecretValue obtains the credential without ever echoing it.
//
// Failure direction: when the terminal cannot be switched to no-echo the
// command FAILS instead of falling back to an echoing read — a credential
// in the scrollback is exactly the outcome this command exists to prevent.
func (a *App) readSecretValue(cmd *cobra.Command, fromStdin bool, server, key string) (string, error) {
	if fromStdin {
		data, err := io.ReadAll(a.stdin)
		if err != nil {
			return "", fmt.Errorf("read stdin: %w", err)
		}
		return strings.TrimRight(string(data), "\r\n"), nil
	}
	f, ok := a.stdin.(*os.File)
	if !ok {
		e := Usagef("stdin is not a terminal; pass --stdin to read the value from the pipe")
		e.Hint = helpHint(cmd)
		return "", e
	}
	_, _ = fmt.Fprintf(a.stderr, "value for %s/%s (input hidden): ", server, key)
	value, err := readNoEcho(f)
	_, _ = fmt.Fprintln(a.stderr)
	if err != nil {
		e := Usagef("cannot read without echo (%v); pass --stdin to read the value from a pipe", err)
		e.Hint = helpHint(cmd)
		return "", e
	}
	return value, nil
}

func (a *App) newSecretRmCmd() *cobra.Command {
	var scopeName string
	cmd := &cobra.Command{
		Use:     "rm <server> <KEY>",
		Aliases: []string{"remove"},
		Short:   "Delete a stored credential from every writable backend",
		Args:    exactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			server, key := args[0], args[1]
			chain, _, err := a.secretChain()
			if err != nil {
				return err
			}
			ref := secrets.Ref{ServerID: server, Scope: scopeName, Key: key}
			if err := chain.Delete(cmd.Context(), ref); err != nil {
				return classifySecretsError(err)
			}
			return a.printer().Emit(SecretChange{
				Action: "removed", Server: server, Key: key, Scope: scopeOrDefault(scopeName),
			})
		},
	}
	cmd.Flags().StringVar(&scopeName, "scope", "", "vault scope name (default "+secrets.DefaultScope+")")
	return cmd
}

func (a *App) newSecretLsCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "ls [server]",
		Short: "List stored credential KEY NAMES and their backend (never the values)",
		Args:  rangeArgs(0, 1),
		RunE: func(cmd *cobra.Command, args []string) error {
			filterServer := ""
			if len(args) == 1 {
				filterServer = args[0]
			}
			list, err := a.listSecrets(cmd.Context(), filterServer)
			if err != nil {
				return err
			}
			return a.printer().Emit(list)
		},
	}
}

// listSecrets enumerates stored refs and classifies each one's backend
// WITHOUT reading any value and without probing the OS keychain.
func (a *App) listSecrets(ctx context.Context, filterServer string) (SecretList, error) {
	chain, dir, err := a.secretChain()
	if err != nil {
		return SecretList{}, err
	}
	refs, err := chain.List(ctx)
	if err != nil {
		return SecretList{}, classifySecretsError(err)
	}
	inKeyring := loadKeyringKeyNames(filepath.Join(dir, keyRegistryFileName))
	out := SecretList{Secrets: []SecretRow{}}
	for _, ref := range refs {
		if filterServer != "" && ref.ServerID != filterServer {
			continue
		}
		backend := "enc-file"
		if inKeyring[ref.StorageKey()] {
			backend = "keyring"
		}
		// The environment levels shadow both persistent backends, so they
		// are reported as what a resolution would actually use.
		if name := secrets.EnvName(ref.Key); name != secrets.EnvEncKey {
			if v, ok := os.LookupEnv(name); ok && strings.TrimSpace(v) != "" {
				backend = "env"
			}
		}
		out.Secrets = append(out.Secrets, SecretRow{
			Server: ref.ServerID, Scope: scopeOrDefault(ref.Scope), Key: ref.Key,
			Backend: backend, Set: true,
		})
	}
	return out, nil
}

// loadKeyringKeyNames reads the keyring key registry. A missing or
// unreadable file yields an empty set: the classification is cosmetic, and
// mislabeling a backend must never fail a listing.
func loadKeyringKeyNames(path string) map[string]bool {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	var doc struct {
		Keys []string `json:"keys"`
	}
	if json.Unmarshal(b, &doc) != nil {
		return nil
	}
	out := make(map[string]bool, len(doc.Keys))
	for _, k := range doc.Keys {
		out[k] = true
	}
	return out
}

func scopeOrDefault(s string) string {
	if s == "" {
		return secrets.DefaultScope
	}
	return s
}

// classifySecretsError maps vault failures onto the exit-code table. A
// wrong AGENTHUB_SECRET_KEY (or a corrupted secrets.enc) is an
// authentication failure, not a generic one: exit 5 tells a script the
// credential store — not the command — is the problem.
func classifySecretsError(err error) error {
	if err == nil {
		return nil
	}
	if strings.Contains(err.Error(), "cannot decrypt") {
		return &Error{
			Code: CodeAuthFailed, ExitCode: ExitAuth,
			Message: err.Error(),
			Hint:    "check AGENTHUB_SECRET_KEY; agenthub never overwrites a file it cannot decrypt",
			Err:     err,
		}
	}
	return err
}
