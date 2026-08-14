package cli

import (
	"errors"
	"fmt"
	"io"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"github.com/dinstein/agent-hub/internal/httpbridge"
	"github.com/dinstein/agent-hub/internal/tier"
)

// The `token` group is the CLI face of the agent-token layer
// (docs/architecture.md#what-a-call-passes-through): the graded credentials for the daemon's HTTP
// data plane.
//
// INVARIANT, and the reason the types below have no value field: a token's
// PLAINTEXT is printed exactly once, by `token create`, and is never
// recoverable afterwards — the store keeps only its HMAC. `token ls` renders
// the display prefix and metadata. This mirrors the `secret` group's rule
// (never print a value) with one deliberate exception: the value has to
// leave the process once, or it could not be given to an agent at all.
//
// Group naming follows canonical.md §3: singular canonical name `token`,
// plural `tokens` as a cobra alias, list subcommand `ls`.

// Stable machine codes for the token commands' JSON failure envelope.
const (
	CodeTokenNotFound = "E_TOKEN_NOT_FOUND"
	CodeTokenExists   = "E_TOKEN_EXISTS"
	CodeTokenLimit    = "E_TOKEN_LIMIT"
)

// TokenRow is one stored token as rendered by `token ls`. There is
// deliberately no field carrying the token value or its HMAC.
type TokenRow struct {
	Name   string `json:"name"`
	Prefix string `json:"prefix"`
	Tier   string `json:"tier"`
	// Servers is the allowlist; null means "every server".
	Servers []string `json:"servers"`
	Profile string   `json:"profile,omitempty"`
	// State is active | revoked | expired | invalid.
	State     string     `json:"state"`
	CreatedAt time.Time  `json:"createdAt"`
	ExpiresAt *time.Time `json:"expiresAt,omitempty"`
	RevokedAt *time.Time `json:"revokedAt,omitempty"`
}

// TokenList is the `token ls` result.
type TokenList struct {
	Tokens []TokenRow `json:"tokens"`
}

// Human renders the table. Prefixes only — 12 characters identify a token in
// a list and reveal nothing usable.
func (l TokenList) Human(w io.Writer) error {
	if len(l.Tokens) == 0 {
		_, err := fmt.Fprintln(w, "no agent tokens stored")
		return err
	}
	tw := tabwriter.NewWriter(w, 0, 4, 2, ' ', 0)
	_, _ = fmt.Fprintln(tw, "NAME\tPREFIX\tTIER\tSERVERS\tPROFILE\tSTATE\tEXPIRES")
	for _, t := range l.Tokens {
		_, _ = fmt.Fprintf(tw, "%s\t%s…\t%s\t%s\t%s\t%s\t%s\n",
			t.Name, t.Prefix, t.Tier, serversText(t.Servers), dash(t.Profile),
			t.State, expiryText(t.ExpiresAt))
	}
	if err := tw.Flush(); err != nil {
		return err
	}
	_, err := fmt.Fprintln(w, "\ntoken values are shown once at creation and are never recoverable")
	return err
}

// TokenCreated is the `token create` result. Value is populated exactly
// once, on the creating invocation, and is the only place it ever appears.
type TokenCreated struct {
	Token TokenRow `json:"token"`
	Value string   `json:"value"`
}

// Human prints the value with the one-shot warning around it, and says where
// the credential can actually be presented.
//
// The endpoint line is not decoration: the data plane is opt-in and nothing
// listens by default, so a token minted without it is a key to a door that is
// not open. Printing a credential while leaving that unsaid is how an
// operator ends up debugging an agent's connection refusals.
func (c TokenCreated) Human(w io.Writer) error {
	_, err := fmt.Fprintf(w,
		"created agent token %q (tier %s, servers %s)\n\n  %s\n\n"+
			"This is the only time the value is shown. Store it now; agenthub keeps\n"+
			"only its HMAC and cannot print it again.\n\n"+
			"The MCP endpoint that accepts it is opt-in and off by default. Turn it\n"+
			"on with:  agenthub config set http.addr localhost:7777\n"+
			"then restart the hub (quit and reopen AgentHub, or 'agenthub daemon\n"+
			"restart --headless') and point the agent at http://localhost:7777/mcp\n"+
			"with this bearer token.\n",
		c.Token.Name, c.Token.Tier, serversText(c.Token.Servers), c.Value)
	return err
}

// TokenRevoked is the `token revoke` result.
type TokenRevoked struct {
	Name      string    `json:"name"`
	Prefix    string    `json:"prefix"`
	RevokedAt time.Time `json:"revokedAt"`
}

// Human renders the confirmation.
func (r TokenRevoked) Human(w io.Writer) error {
	_, err := fmt.Fprintf(w, "revoked agent token %q (%s…); existing sessions stop at their next request\n",
		r.Name, r.Prefix)
	return err
}

func (a *App) newTokenCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:     "token",
		Aliases: []string{"tokens"}, // singular canonical, plural alias (canonical.md §3)
		Short:   "Manage agent tokens for the daemon's HTTP data plane",
		Long: "Agent tokens are the graded credentials of the HTTP MCP endpoint.\n\n" +
			"Each token carries an operation tier (read | write | destructive), an\n" +
			"optional server allowlist, an optional profile pin and an optional\n" +
			"expiry. The tier is enforced by the pipeline's token tier gate against\n" +
			"each tool's declared annotations: a read token cannot invoke a writing\n" +
			"tool, and a tool whose server declared no annotations counts as\n" +
			"destructive.\n\n" +
			"Only the HMAC of a token is stored. The value is printed once.",
		Args: cobra.ArbitraryArgs,
		RunE: groupRunE,
	}
	cmd.AddCommand(a.newTokenLsCmd(), a.newTokenCreateCmd(), a.newTokenRevokeCmd())
	return cmd
}

// tokenStore opens the store rooted at the data directory.
func (a *App) tokenStore() (*httpbridge.Store, error) {
	dir, err := a.resolver.DataDir()
	if err != nil {
		return nil, err
	}
	store, err := httpbridge.OpenStore(dir)
	if err != nil {
		return nil, err
	}
	if a.lockTimeout > 0 {
		store.SetLockTimeout(a.lockTimeout)
	}
	return store, nil
}

func (a *App) newTokenCreateCmd() *cobra.Command {
	var (
		tierFlag string
		servers  []string
		profile  string
		expires  time.Duration
	)
	cmd := &cobra.Command{
		Use:   "create <name>",
		Short: "Mint an agent token and print its value once",
		Args:  exactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			store, err := a.tokenStore()
			if err != nil {
				return err
			}
			spec := httpbridge.CreateSpec{
				Name:    args[0],
				Tier:    tier.Tier(tierFlag),
				Servers: normalizeServerFlag(servers),
				Profile: profile,
			}
			if expires > 0 {
				spec.ExpiresAt = time.Now().Add(expires)
			}
			tok, value, err := store.Create(cmd.Context(), spec)
			if err != nil {
				return classifyTokenError(err)
			}
			return a.printer().Emit(TokenCreated{Token: tokenRow(tok, time.Now()), Value: value})
		},
	}
	cmd.Flags().StringVar(&tierFlag, "tier", string(tier.Read),
		"operation tier: read, write or destructive")
	cmd.Flags().StringSliceVar(&servers, "server", nil,
		"restrict the token to these servers (repeatable; omit for every server, '*' for every server explicitly)")
	cmd.Flags().StringVar(&profile, "profile", "",
		"pin the token to a profile (participates in the scope intersection)")
	cmd.Flags().DurationVar(&expires, "expires-in", 0,
		"expire the token after this duration (default: never)")
	return cmd
}

func (a *App) newTokenLsCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "ls",
		Short: "List agent tokens (prefixes and metadata only)",
		Args:  noArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			store, err := a.tokenStore()
			if err != nil {
				return err
			}
			toks, err := store.List()
			if err != nil {
				return classifyTokenError(err)
			}
			now := time.Now()
			out := TokenList{Tokens: make([]TokenRow, 0, len(toks))}
			for _, t := range toks {
				out.Tokens = append(out.Tokens, tokenRow(t, now))
			}
			return a.printer().Emit(out)
		},
	}
}

func (a *App) newTokenRevokeCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "revoke <name>",
		Short: "Revoke an agent token",
		Long: "Revoke one agent token. The record is kept (the name stays reserved and\n" +
			"the row stays visible in 'token ls') so ledger entries referring to it\n" +
			"keep resolving to exactly one credential.",
		Args: exactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			store, err := a.tokenStore()
			if err != nil {
				return err
			}
			tok, err := store.Revoke(cmd.Context(), args[0], time.Now())
			if err != nil {
				return classifyTokenError(err)
			}
			return a.printer().Emit(TokenRevoked{
				Name: tok.Name, Prefix: tok.Prefix, RevokedAt: tok.RevokedAt,
			})
		},
	}
}

// tokenRow projects a stored token for output. The HMAC never crosses this
// boundary.
func tokenRow(t httpbridge.Token, now time.Time) TokenRow {
	row := TokenRow{
		Name:      t.Name,
		Prefix:    t.Prefix,
		Tier:      string(t.Tier),
		Servers:   t.Servers,
		Profile:   t.Profile,
		State:     t.State(now),
		CreatedAt: t.CreatedAt,
	}
	if !t.ExpiresAt.IsZero() {
		e := t.ExpiresAt
		row.ExpiresAt = &e
	}
	if !t.RevokedAt.IsZero() {
		r := t.RevokedAt
		row.RevokedAt = &r
	}
	return row
}

// normalizeServerFlag keeps the nil-vs-empty distinction the store relies on:
// a flag that was never passed is nil ("every server"), and an explicitly
// empty value stays an empty allowlist ("nothing"), which is the closed
// direction.
func normalizeServerFlag(in []string) []string {
	if in == nil {
		return nil
	}
	out := make([]string, 0, len(in))
	for _, s := range in {
		if s = strings.TrimSpace(s); s != "" {
			out = append(out, s)
		}
	}
	return out
}

// classifyTokenError maps store failures onto the frozen exit-code table.
func classifyTokenError(err error) error {
	switch {
	case err == nil:
		return nil
	case errors.Is(err, httpbridge.ErrTokenNotFound), errors.Is(err, httpbridge.ErrAlreadyRevoked):
		return &Error{Code: CodeTokenNotFound, ExitCode: ExitNotFound, Message: err.Error(), Err: err}
	case errors.Is(err, httpbridge.ErrTokenExists):
		return &Error{
			Code: CodeTokenExists, ExitCode: ExitUsage, Message: err.Error(),
			Hint: "revoked tokens keep their name; pick another one", Err: err,
		}
	case errors.Is(err, httpbridge.ErrTooManyTokens):
		return &Error{
			Code: CodeTokenLimit, ExitCode: ExitGeneral, Message: err.Error(),
			Hint: "revoke tokens you no longer use", Err: err,
		}
	case errors.Is(err, httpbridge.ErrInvalidName), errors.Is(err, httpbridge.ErrInvalidTier):
		return &Error{Code: CodeUsage, ExitCode: ExitUsage, Message: err.Error(), Err: err}
	case errors.Is(err, httpbridge.ErrLockTimeout):
		return &Error{
			Code: CodeLockTimeout, ExitCode: ExitLocked,
			Message: err.Error(),
			Hint:    "another agenthub process holds the token-store lock; retry in a moment",
			Err:     err,
		}
	default:
		return err
	}
}

// serversText renders an allowlist for the human table.
func serversText(servers []string) string {
	switch {
	case servers == nil:
		return "(all)"
	case len(servers) == 0:
		return "(none)"
	default:
		return strings.Join(servers, ",")
	}
}

func expiryText(t *time.Time) string {
	if t == nil {
		return "never"
	}
	return t.UTC().Format(time.RFC3339)
}
