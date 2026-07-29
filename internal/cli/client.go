package cli

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/spf13/cobra"

	"github.com/dinstein/agent-hub/internal/clients"
	"github.com/dinstein/agent-hub/internal/gateway"
)

// GatewayEntry is the MCP server entry that `client connect` merges into an
// AI client's configuration: it makes the client spawn this very binary as
// its (single) MCP server via `agenthub connect --client <id>`.
type GatewayEntry struct {
	Command string   `json:"command"`
	Args    []string `json:"args"`
}

// ConnectPlan is the `client connect` result. With --dry-run it is the
// preview (DryRun true, nothing written); on the real path DryRun flips to
// false and Path/Backup/Changed report what happened on disk.
type ConnectPlan struct {
	Client  string       `json:"client"`
	Profile string       `json:"profile,omitempty"`
	DryRun  bool         `json:"dry_run"`
	Entry   GatewayEntry `json:"entry"`
	Path    string       `json:"path,omitempty"`
	Backup  string       `json:"backup,omitempty"`
	Changed bool         `json:"changed,omitempty"`
}

// Human renders the outcome plus the exact JSON fragment that was (or would
// be) merged into the client's MCP configuration.
func (p ConnectPlan) Human(w io.Writer) error {
	switch {
	case p.DryRun:
		// Naming the target file is the point of the preview now that the
		// default is the user-level one: "which file does this land in" is
		// the question --dry-run is asked to answer.
		target := "MCP configuration"
		if p.Path != "" {
			target = p.Path
		}
		if _, err := fmt.Fprintf(w, "dry-run: nothing written; would merge into %s's %s:\n", p.Client, target); err != nil {
			return err
		}
	case p.Changed:
		if _, err := fmt.Fprintf(w, "connected: wrote gateway entry to %s\n", p.Path); err != nil {
			return err
		}
		if p.Backup != "" {
			if _, err := fmt.Fprintf(w, "backup: %s\n", p.Backup); err != nil {
				return err
			}
		}
	default:
		if _, err := fmt.Fprintf(w, "connected: %s already up to date\n", p.Path); err != nil {
			return err
		}
	}
	snippet := map[string]map[string]GatewayEntry{"mcpServers": {"agenthub": p.Entry}}
	b, err := json.MarshalIndent(snippet, "", "  ")
	if err != nil {
		return err
	}
	_, err = fmt.Fprintf(w, "%s\n", b)
	return err
}

// DisconnectResult is the `client disconnect` result.
type DisconnectResult struct {
	Client  string   `json:"client"`
	Path    string   `json:"path"`
	Removed []string `json:"removed"`
	Backup  string   `json:"backup,omitempty"`
}

// Human renders the removal confirmation.
func (r DisconnectResult) Human(w io.Writer) error {
	_, err := fmt.Fprintf(w, "disconnected: removed %s from %s\n", strings.Join(r.Removed, ", "), r.Path)
	return err
}

// ConnectSnippet builds the gateway entry `client connect` will write into
// clientID's configuration. This function is the single seam between the
// --dry-run preview and the config writer: both go through it, so the
// preview and the actual write can never drift.
func ConnectSnippet(executable, clientID, profile string) ConnectPlan {
	args := []string{"connect", "--client", clientID}
	if profile != "" {
		args = append(args, "--profile", profile)
	}
	return ConnectPlan{
		Client:  clientID,
		Profile: profile,
		DryRun:  true, // the writer flips this after the write succeeds
		Entry:   GatewayEntry{Command: executable, Args: args},
	}
}

func (a *App) newClientCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:     "client",
		Aliases: []string{"clients"}, // singular canonical, plural alias (canonical.md §3)
		Short:   "Connect AI clients to the agenthub gateway",
		Args:    cobra.ArbitraryArgs,
		RunE:    groupRunE,
	}
	cmd.AddCommand(
		a.newClientDetectCmd(), a.newClientConnectCmd(),
		a.newClientDisconnectCmd(), a.newClientImportCmd(),
	)
	return cmd
}

func (a *App) newClientConnectCmd() *cobra.Command {
	var (
		profile   string
		path      string
		placement string
		bin       string
		dryRun    bool
	)
	cmd := &cobra.Command{
		Use:   "connect <client-id>",
		Short: "Write the agenthub gateway entry into a client's MCP configuration",
		Args:  exactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			clientID := args[0]
			exe := bin
			if exe == "" {
				exe = a.executable()
			}
			plan := ConnectSnippet(exe, clientID, profile)
			if dryRun {
				// Best-effort target: a preview stays available for clients
				// this binary cannot write (that is what --dry-run is for),
				// so an unresolvable target only leaves the path blank.
				if _, target, err := a.clientTarget(clientID, path, placement); err == nil {
					plan.Path = target
				}
				return a.printer().Emit(plan)
			}
			format, target, err := a.clientTarget(clientID, path, placement)
			if err != nil {
				return err
			}
			res, err := format.Connect(target, clients.Entry{
				Command: plan.Entry.Command,
				Args:    plan.Entry.Args,
			})
			if err != nil {
				return classifyClientsError(err)
			}
			plan.DryRun = false
			plan.Path = res.Path
			plan.Backup = res.Backup
			plan.Changed = res.Changed
			return a.printer().Emit(plan)
		},
	}
	cmd.Flags().StringVar(&profile, "profile", "", "profile to bind this client to (reserved for M1)")
	cmd.Flags().StringVar(&path, "path", "", "configuration file to write (overrides --placement)")
	cmd.Flags().StringVar(&placement, "placement", "", placementUsage)
	cmd.Flags().StringVar(&bin, "bin", "", "agenthub binary path to write into the entry (default: this executable)")
	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "preview the entry without writing anything")
	return cmd
}

func (a *App) newClientDisconnectCmd() *cobra.Command {
	var (
		path      string
		placement string
	)
	cmd := &cobra.Command{
		Use:   "disconnect <client-id>",
		Short: "Remove the agenthub gateway entry from a client's MCP configuration",
		Args:  exactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			clientID := args[0]
			format, target, err := a.clientTarget(clientID, path, placement)
			if err != nil {
				return err
			}
			// With no target named, a disconnect also looks at the client's
			// other location: entries written before the default moved to the
			// user level are still on disk, and answering "not connected"
			// while the entry sits in .mcp.json would be the worst answer
			// available. An explicit --path/--placement is an instruction and
			// is never widened.
			res := clients.Result{}
			if path == "" && placement == "" {
				res, err = clients.DisconnectDefault(format, a.clientBaseDir())
			} else {
				res, err = format.Disconnect(target)
			}
			if err != nil {
				return classifyClientsError(err)
			}
			return a.printer().Emit(DisconnectResult{
				Client:  clientID,
				Path:    res.Path,
				Removed: res.Removed,
				Backup:  res.Backup,
			})
		},
	}
	cmd.Flags().StringVar(&path, "path", "", "configuration file to edit (overrides --placement)")
	cmd.Flags().StringVar(&placement, "placement", "", placementUsage)
	return cmd
}

// placementUsage documents --placement on both connect and disconnect. The
// default is stated rather than implied: which file the entry lands in is
// exactly the thing a user is deciding here.
const placementUsage = "where the configuration file lives: user|project (default: user)"

// clientTarget resolves the Format for clientID and the configuration file to
// operate on. Precedence: an explicit --path, then --placement, then the
// client's default target (user-level, see clients.DefaultPlacement).
func (a *App) clientTarget(clientID, pathOverride, placement string) (clients.Format, string, error) {
	format, ok := clients.Lookup(clientID)
	if !ok {
		e := NotFoundf(CodeClientUnsupported, "client %q is not supported for direct configuration writing in M0", clientID)
		e.Hint = fmt.Sprintf("supported: %s (use 'client connect --dry-run' to preview the snippet for any client)",
			strings.Join(clients.IDs(), ", "))
		return nil, "", e
	}
	if pathOverride != "" {
		if placement != "" {
			e := Usagef("--path and --placement name two different targets; pass one")
			e.Hint = "--path is the file itself; --placement picks between the client's own user and project files"
			return nil, "", e
		}
		return format, pathOverride, nil
	}
	baseDir := a.clientBaseDir()
	if placement == "" {
		return format, format.DefaultPath(baseDir), nil
	}
	// An explicitly named placement is honoured exactly or refused: falling
	// back to the other one would write the entry into a file the user did
	// not ask for, which is the failure --placement exists to prevent.
	p := clients.Placement(placement)
	if p != clients.User && p != clients.Project {
		e := Usagef("unknown placement %q", placement)
		e.Hint = fmt.Sprintf("valid: %s, %s", clients.User, clients.Project)
		return nil, "", e
	}
	target := format.PathFor(baseDir, p)
	if target == "" {
		e := NotFoundf(CodeClientUnsupported,
			"client %q has no %s-level configuration file on this platform", clientID, p)
		e.Hint = "run 'agenthub client detect' to see the locations this client actually uses"
		return nil, "", e
	}
	return format, target, nil
}

// clientBaseDir is the working tree project-level client paths resolve
// against. An unresolvable working directory degrades to "" rather than
// failing the command: user-level targets — the default — do not need it.
func (a *App) clientBaseDir() string {
	cwd, err := os.Getwd()
	if err != nil {
		return ""
	}
	return cwd
}

// classifyClientsError maps internal/clients errors onto the typed CLI
// error table: unparseable existing configuration is E_INVALID_JSON
// (refused, never overwritten), a disconnect with nothing to remove is
// not-found (exit 3).
func classifyClientsError(err error) error {
	var pe *clients.ParseError
	if errors.As(err, &pe) {
		return &Error{
			Code: CodeInvalidJSON, ExitCode: ExitGeneral,
			Message: pe.Error(),
			Hint:    "fix or remove the file; agenthub refuses to overwrite configuration it cannot parse",
			Err:     pe.Err,
		}
	}
	var nc *clients.NotConnectedError
	if errors.As(err, &nc) {
		e := NotFoundf(CodeClientNotConnected, "%s", nc.Error())
		e.Hint = "only entries written by 'agenthub client connect' are removed"
		return e
	}
	return err
}

// newConnectCmd is the gateway entry point that client configurations
// invoke (`agenthub connect --client <id>`): it runs the stdio gateway on
// the process stdin/stdout until the client disconnects. Distinct from
// `client connect`, which writes the configuration snippet.
func (a *App) newConnectCmd() *cobra.Command {
	var (
		clientID string
		profile  string
	)
	cmd := &cobra.Command{
		Use:   "connect",
		Short: "Run the stdio gateway for a client (invoked by client MCP configurations)",
		Args:  noArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			if clientID == "" {
				e := Usagef("--client is required")
				e.Hint = helpHint(cmd)
				return e
			}
			// Detach from the client's process group so downstream children
			// cannot disturb a TUI client's terminal (SIGTTIN/SIGTTOU;
			// docs/flows.md, inherited from toolport). Only attempted on the
			// real stdio path (tests inject buffers and must not detach the
			// test process). Failing is normal when already a session/group
			// leader (interactive shells) and never fatal.
			if f, ok := a.stdin.(*os.File); ok && f == os.Stdin {
				if err := detachProcessGroup(); err != nil && os.Getenv("AGENTHUB_DEBUG") == "1" {
					_, _ = fmt.Fprintf(a.stderr, "agenthub: setsid: %v (continuing in current session)\n", err)
				}
			}
			err := gateway.Run(cmd.Context(), gateway.Config{
				ClientID:  clientID,
				In:        a.stdin,
				Out:       a.stdout,
				Resolver:  a.resolver,
				LogWriter: a.stderr,
				Version:   a.version,
			})
			if err != nil {
				return &Error{
					Code: CodeGeneral, ExitCode: ExitGeneral,
					Message: "gateway terminated", Err: err,
				}
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&clientID, "client", "", "client identity this gateway serves (scope routing key)")
	cmd.Flags().StringVar(&profile, "profile", "", "initial profile (reserved for M1)")
	return cmd
}
