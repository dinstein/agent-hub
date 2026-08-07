package cli

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"github.com/dinstein/agent-hub/internal/cli/output"
	"github.com/dinstein/agent-hub/internal/confops"
	"github.com/dinstein/agent-hub/internal/oauthflow"
	"github.com/dinstein/agent-hub/internal/registry"
)

// The `auth` group is the CLI face of internal/oauthflow (canonical.md §3:
// the canonical name is `auth`, never `oauth`).
//
// Two invariants this file exists to hold:
//
//  1. No credential is ever rendered. `auth status` reports issuer, expiry,
//     mode and whether a refresh token exists — never a token, never a
//     client secret. There is no --show escape hatch (docs/modules/controlplane.md rule 5).
//  2. Progress is a stream, results are a value. Every intermediate step
//     goes through Printer.Progress (NDJSON under --json, stderr otherwise)
//     and the command's single result value is the last thing written.

// AuthLoginResult is the `auth login` result.
type AuthLoginResult struct {
	Server string `json:"server"`
	// Mode is the mode actually used, which may differ from the requested
	// one (auto-selection, or the browser-open downgrade to manual).
	Mode      string `json:"mode"`
	Issuer    string `json:"issuer,omitempty"`
	Resource  string `json:"resource,omitempty"`
	Scope     string `json:"scope,omitempty"`
	ExpiresAt int64  `json:"expiresAt"`
	// ExpiresIn is seconds from now; 0 with ExpiresAt == 0 means the
	// provider issued no expiry at all ("never expires", docs/modules/oauth.md).
	ExpiresIn  int64 `json:"expiresIn"`
	HasRefresh bool  `json:"hasRefreshToken"`
	// Enabled reports whether this login also put a disabled server into
	// service. False means it was already enabled, is not in the registry,
	// or the enable failed (see warnings) — the credential is stored either
	// way.
	Enabled bool `json:"enabled"`
}

// Human renders the login confirmation.
func (r AuthLoginResult) Human(w io.Writer) error {
	if _, err := fmt.Fprintf(w, "authenticated: %s (%s)%s\n", r.Server, r.Mode, expiryPhrase(r.ExpiresAt, r.ExpiresIn)); err != nil {
		return err
	}
	if r.Enabled {
		_, err := fmt.Fprintf(w, "%s: enabled\n", r.Server)
		return err
	}
	return nil
}

// enableAfterLogin completes what `server add` deliberately left undone: the
// entry was written disabled because no credential existed, and one now
// does.
//
// Failure direction: FAIL-OPEN. The credential is already stored — the
// irreversible half succeeded — so a failed enable is a warning, never a
// failed login. An already-enabled server is left alone and reports false:
// nothing changed.
func (a *App) enableAfterLogin(ctx context.Context, serverID string) (bool, []string) {
	store, warnings, err := a.opsStore()
	if err != nil {
		return false, append(warnings,
			"credential stored, but the registry could not be reached to enable "+serverID+": "+err.Error())
	}
	doc, ok := store.Snapshot().Servers.V.Servers[serverID]
	if !ok {
		// Authorizing an id that is not in the registry is legitimate
		// (--issuer against a bare name); there is nothing to enable.
		return false, warnings
	}
	if doc.V.Enabled {
		return false, warnings
	}
	if _, err := confops.SetServerEnabled(ctx, store, serverID, true, noPrecondition); err != nil {
		return false, append(warnings,
			"credential stored, but "+serverID+" is still disabled: "+err.Error()+
				"; enable it with 'agenthub server enable "+serverID+"'")
	}
	return true, warnings
}

// AuthStatusRow is one server's authorization state.
type AuthStatusRow struct {
	Server    string `json:"server"`
	State     string `json:"state"` // authorized | expiring | expired | none
	Issuer    string `json:"issuer,omitempty"`
	Scope     string `json:"scope,omitempty"`
	ExpiresAt int64  `json:"expiresAt"`
	ExpiresIn int64  `json:"expiresIn"`
	// HasRefreshToken decides whether an expiry is recoverable without a
	// human; the token itself is never rendered.
	HasRefreshToken bool   `json:"hasRefreshToken"`
	ClientRegistrar string `json:"clientRegistrar,omitempty"`
	Detail          string `json:"detail,omitempty"`
}

// AuthStatusList is the `auth status` result.
type AuthStatusList []AuthStatusRow

// Human renders the status table.
func (l AuthStatusList) Human(w io.Writer) error {
	if len(l) == 0 {
		_, err := fmt.Fprintln(w, "no servers with stored credentials")
		return err
	}
	tw := tabwriter.NewWriter(w, 0, 4, 2, ' ', 0)
	_, _ = fmt.Fprintln(tw, "SERVER\tSTATE\tEXPIRES\tREFRESH\tISSUER")
	for _, r := range l {
		_, _ = fmt.Fprintf(tw, "%s\t%s\t%s\t%t\t%s\n",
			r.Server, r.State, expiryColumn(r.ExpiresAt, r.ExpiresIn), r.HasRefreshToken, r.Issuer)
	}
	return tw.Flush()
}

// AuthRefreshResult is the `auth refresh` result.
type AuthRefreshResult struct {
	Server     string `json:"server"`
	ExpiresAt  int64  `json:"expiresAt"`
	ExpiresIn  int64  `json:"expiresIn"`
	Superseded bool   `json:"superseded"`
}

// Human renders the refresh confirmation.
func (r AuthRefreshResult) Human(w io.Writer) error {
	if r.Superseded {
		_, err := fmt.Fprintf(w, "refreshed by another process: %s%s\n", r.Server, expiryPhrase(r.ExpiresAt, r.ExpiresIn))
		return err
	}
	_, err := fmt.Fprintf(w, "refreshed: %s%s\n", r.Server, expiryPhrase(r.ExpiresAt, r.ExpiresIn))
	return err
}

// AuthLogoutResult is the `auth logout` result.
type AuthLogoutResult struct {
	Server string `json:"server"`
}

// Human renders the logout confirmation.
func (r AuthLogoutResult) Human(w io.Writer) error {
	_, err := fmt.Fprintf(w, "logged out: %s (local credentials removed)\n", r.Server)
	return err
}

func (a *App) newAuthCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "auth",
		Short: "Sign in to servers that require an account, and manage those sign-ins",
		Long: "You usually get here because 'agenthub server test' reported that a server\n" +
			"refuses to answer until you authorize it. Sign in once with 'auth login' and\n" +
			"agenthub keeps and renews the token for every client.\n\n" +
			"Tokens are stored locally and never printed by any of these commands.",
		Args: cobra.ArbitraryArgs,
		RunE: groupRunE,
	}
	cmd.AddCommand(a.newAuthLoginCmd(), a.newAuthStatusCmd(), a.newAuthRefreshCmd(), a.newAuthLogoutCmd())
	return cmd
}

func (a *App) newAuthLoginCmd() *cobra.Command {
	var (
		manual      bool
		device      bool
		loopback    bool
		noBrowser   bool
		scopes      []string
		issuer      string
		authzEndpt  string
		redirectURI string
		allowLocal  bool
		timeout     time.Duration
	)
	cmd := &cobra.Command{
		Use:   "login <server>",
		Short: "Sign in to one server and store the token for every client to use",
		Long: "Signs you in once and stores the token, so your clients never handle the\n" +
			"credentials. Three ways to finish it:\n\n" +
			"  --loopback  opens your browser and catches the answer\n" +
			"  --device    shows a short code you type into the provider's site\n" +
			"  --manual    prints a link and waits for you to paste back where it sent you\n\n" +
			"Left alone it picks the device code if offered, else the browser, else manual.\n" +
			"With no browser available — over SSH, say — add --no-browser.",
		Args: exactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			serverID := args[0]
			mode, err := selectedMode(manual, device, loopback)
			if err != nil {
				return err
			}
			entry, warnings, err := a.serverEntry(serverID)
			if err != nil {
				return err
			}
			if issuer == "" && entry.OAuth != nil {
				issuer = entry.OAuth.Issuer
			}
			if len(scopes) == 0 && entry.OAuth != nil {
				scopes = entry.OAuth.Scopes
			}
			if authzEndpt == "" && entry.OAuth != nil {
				authzEndpt = entry.OAuth.AuthorizationEndpoint
			}
			resourceURL := entry.URL
			if resourceURL == "" && issuer == "" {
				return &Error{
					Code: CodeUsage, ExitCode: ExitUsage,
					Message: fmt.Sprintf("server %q has no url to authorize against", serverID),
					Hint:    "add the server with --url, or pass --issuer",
				}
			}
			resourceMeta := ""
			if entry.OAuth != nil {
				resourceMeta = entry.OAuth.ResourceMetadataURL
			}

			// The loopback carve-out follows the server's own provenance: a
			// local server's authorization endpoint may legitimately be
			// loopback too. Everything else is screened.
			deps, err := a.newOAuthDeps(allowLocal || entry.Provenance == registry.ProvenanceLocal)
			if err != nil {
				return err
			}
			p := a.printer()
			flow := oauthflow.NewFlow(deps.oauthClient, deps.store)

			req := oauthflow.LoginRequest{
				ServerID:              serverID,
				Issuer:                issuer,
				ResourceURL:           resourceURL,
				ResourceMetadataURL:   resourceMeta,
				Scopes:                scopes,
				AuthorizationEndpoint: authzEndpt,
				ClientName:            "agenthub",
				Mode:                  mode,
				Paste:                 a.pasteReader(p),
				Timeout:               timeout,
				OnDeviceCode: func(da oauthflow.DeviceAuthorization) {
					uri := da.VerificationURIComplete
					if uri == "" {
						uri = da.VerificationURI
					}
					p.Progress(output.ProgressEvent{
						Event:   "device_code",
						Message: fmt.Sprintf("open %s and enter the code %s", uri, da.UserCode),
						Fields: map[string]any{
							"verification_uri": da.VerificationURI,
							"user_code":        da.UserCode,
							"expires_in":       da.ExpiresIn,
						},
					})
				},
				OnPollInterval: func(d time.Duration) {
					p.Progress(output.ProgressEvent{
						Event:   "authorization_pending",
						Message: fmt.Sprintf("waiting for approval (next check in %s)…", d),
						Fields:  map[string]any{"interval_ms": d.Milliseconds()},
					})
				},
			}
			// Open stays nil when this host cannot show a browser: that is
			// exactly the signal SelectMode reads to downgrade to manual
			// (docs/modules/oauth.md). An explicit --loopback still gets an opener
			// so the failure is reported instead of silently rerouted.
			if !noBrowser && (canOpenBrowser() || mode == oauthflow.ModeLoopback) {
				req.Open = func(url string) error {
					p.Progress(output.ProgressEvent{
						Event:   "awaiting_browser",
						Message: "opening the authorization page in your browser…",
						Fields:  map[string]any{"url": url},
					})
					return openBrowser(url)
				}
			}
			if prev, perr := deps.store.LoadState(cmd.Context(), serverID); perr == nil {
				req.FixedCallbackPort = prev.CallbackPort
			}
			req.RedirectURI = redirectURI

			p.Progress(output.ProgressEvent{
				Event:   "discovering",
				Message: "discovering the authorization server…",
				Fields:  map[string]any{"server": serverID},
			})
			res, err := flow.Login(cmd.Context(), req)
			if err != nil {
				return authError(err)
			}
			// The credential now exists, which is the thing that was missing
			// when `server add` left the entry disabled. Completing that
			// second step here is why the two commands can stay separate
			// without making the OAuth path a three-command ritual.
			enabled, ewarn := a.enableAfterLogin(cmd.Context(), serverID)
			warnings = append(warnings, ewarn...)

			now := time.Now()
			return p.Emit(AuthLoginResult{
				Server:     serverID,
				Mode:       string(res.Mode),
				Issuer:     res.State.Issuer,
				Resource:   res.State.Resource,
				Scope:      res.State.Scope,
				ExpiresAt:  res.State.ExpiresAt,
				ExpiresIn:  secondsUntil(res.State.ExpiresAt, now),
				HasRefresh: res.State.RefreshToken != "",
				Enabled:    enabled,
			}, warnings...)
		},
	}
	cmd.Flags().BoolVar(&manual, "manual", false, "print a link to open yourself, then wait for you to paste the result back")
	cmd.Flags().BoolVar(&device, "device", false, "show a short code to type into the provider's website")
	cmd.Flags().BoolVar(&loopback, "loopback", false, "open your browser and catch the answer automatically")
	cmd.Flags().BoolVar(&noBrowser, "no-browser", false, "never open a browser; falls back to the manual sign-in")
	cmd.Flags().StringSliceVar(&scopes, "scopes", nil, "permissions to ask for, comma-separated; sent exactly as written")
	cmd.Flags().StringVar(&authzEndpt, "authorization-endpoint", "",
		"the exact sign-in address to use, e.g. https://idp.example.com/oauth/authorize/tenant1 "+
			"(only for providers that do not publish it correctly; better to get them to fix it)")
	cmd.Flags().StringVar(&issuer, "issuer", "", "address of the login service, when agenthub cannot work it out from the server")
	cmd.Flags().StringVar(&redirectURI, "redirect-uri", "",
		"the exact return address to use, e.g. http://localhost:8040/oauth2/callback "+
			"(for providers that only accept an address registered with them in advance)")
	cmd.Flags().BoolVar(&allowLocal, "allow-local", false,
		"allow a login service running on this machine (for a provider you host yourself)")
	cmd.Flags().DurationVar(&timeout, "timeout", 0, "how long to wait for you to finish in the browser (default 180s)")
	return cmd
}

func (a *App) newAuthStatusCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "status [server]",
		Short: "Show which servers you are signed in to, and when that expires",
		Long: "Also shows whether agenthub can renew each sign-in on its own. One that has\n" +
			"expired and cannot renew needs 'agenthub auth login <server>' again.",
		Args: rangeArgs(0, 1),
		RunE: func(cmd *cobra.Command, args []string) error {
			store, warnings, err := a.openStore()
			if err != nil {
				return err
			}
			deps, err := a.newOAuthDeps(false)
			if err != nil {
				return err
			}
			var ids []string
			if len(args) == 1 {
				if _, err := requireServer(store.Snapshot(), args[0]); err != nil {
					return err
				}
				ids = []string{args[0]}
			} else {
				for id := range store.Snapshot().Servers.V.Servers {
					ids = append(ids, id)
				}
				slices.Sort(ids)
			}

			now := time.Now()
			rows := make(AuthStatusList, 0, len(ids))
			for _, id := range ids {
				row := authStatusOf(cmd.Context(), deps, id, now)
				// Without an explicit server argument, a server that never
				// had credentials is noise, not information.
				if len(args) == 0 && row.State == "none" {
					continue
				}
				rows = append(rows, row)
			}
			return a.printer().Emit(rows, warnings...)
		},
	}
}

// authStatusOf reports one server's stored state. Every failure is folded
// into the row (State/Detail) rather than aborting the listing: `auth
// status` is a diagnostic, and one corrupt entry must not hide the rest.
func authStatusOf(ctx context.Context, deps *oauthDeps, id string, now time.Time) AuthStatusRow {
	row := AuthStatusRow{Server: id, State: "none"}
	st, err := deps.store.LoadState(ctx, id)
	if err != nil {
		if !errors.Is(err, oauthflow.ErrNoState) {
			row.State = "error"
			row.Detail = err.Error()
		}
		return row
	}
	row.Issuer = st.Issuer
	row.Scope = st.Scope
	row.ExpiresAt = st.ExpiresAt
	row.ExpiresIn = secondsUntil(st.ExpiresAt, now)
	row.HasRefreshToken = st.RefreshToken != ""
	row.ClientRegistrar = st.RegistrarKind

	if _, terr := deps.store.LoadAccessToken(ctx, id); terr != nil {
		// State without a token is the DCR-credentials-only shape: a login
		// was started (or the token write failed) but nothing is usable.
		row.State = "none"
		row.Detail = "client registration stored, no access token"
		return row
	}
	switch {
	case st.Expired(now):
		row.State = "expired"
	case st.NeedsRefresh(now):
		row.State = "expiring"
	default:
		row.State = "authorized"
	}
	return row
}

func (a *App) newAuthRefreshCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "refresh <server>",
		Short: "Renew a server's sign-in now instead of waiting for it to expire",
		Long: "agenthub renews tokens by itself before they run out, so this is mainly for\n" +
			"confirming that renewal still works.",
		Args: exactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			serverID := args[0]
			// The loopback carve-out follows the server's own provenance, so
			// a self-hosted local provider refreshes exactly like it logged
			// in. A server that is no longer in the registry (credentials
			// outliving the entry) gets the strict default.
			deps, err := a.newOAuthDeps(a.serverIsLocal(serverID))
			if err != nil {
				return err
			}
			p := a.printer()
			p.Progress(output.ProgressEvent{
				Event:   "refreshing",
				Message: "refreshing the access token…",
				Fields:  map[string]any{"server": serverID},
			})
			st, _, rerr := deps.coord.Refresh(cmd.Context(), serverID)
			superseded := isSuperseded(rerr)
			if rerr != nil && !superseded {
				return authError(rerr)
			}
			return p.Emit(AuthRefreshResult{
				Server:     serverID,
				ExpiresAt:  st.ExpiresAt,
				ExpiresIn:  secondsUntil(st.ExpiresAt, time.Now()),
				Superseded: superseded,
			})
		},
	}
}

func (a *App) newAuthLogoutCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "logout <server>",
		Short: "Forget the stored sign-in for one server",
		Long: "The server stays registered; calls to it fail until you log in again. This\n" +
			"clears only agenthub's copy — to revoke the access itself, use the provider's\n" +
			"own settings page.",
		Args: exactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			serverID := args[0]
			deps, err := a.newOAuthDeps(false)
			if err != nil {
				return err
			}
			// Clear is idempotent: logging out of a server with nothing
			// stored succeeds. The alternative (exit 3) would make cleanup
			// scripts branch on state they cannot observe without --json.
			if cerr := deps.store.Clear(cmd.Context(), serverID); cerr != nil {
				return cerr
			}
			return a.printer().Emit(AuthLogoutResult{Server: serverID})
		},
	}
}

// selectedMode maps the mutually exclusive mode flags to a Mode.
func selectedMode(manual, device, loopback bool) (oauthflow.Mode, error) {
	n := 0
	for _, b := range []bool{manual, device, loopback} {
		if b {
			n++
		}
	}
	if n > 1 {
		return oauthflow.ModeAuto, Usagef("--manual, --device and --loopback are mutually exclusive")
	}
	switch {
	case manual:
		return oauthflow.ModeManual, nil
	case device:
		return oauthflow.ModeDevice, nil
	case loopback:
		return oauthflow.ModeLoopback, nil
	default:
		return oauthflow.ModeAuto, nil
	}
}

// pasteReader builds the manual-mode callback reader: print the URL as a
// progress event, then read one line from stdin.
func (a *App) pasteReader(p *output.Printer) oauthflow.PasteReader {
	return func(_ context.Context, instr oauthflow.ManualInstructions) (string, error) {
		p.Progress(output.ProgressEvent{
			Event: "awaiting_paste",
			Message: fmt.Sprintf(
				"open this URL on any device with a browser:\n\n  %s\n\nthen paste the full callback URL (or just the code) here: ",
				instr.AuthorizationURL),
			Fields: map[string]any{
				"authorization_url": instr.AuthorizationURL,
				"redirect_uri":      instr.RedirectURI,
			},
		})
		line, err := bufio.NewReader(a.stdin).ReadString('\n')
		if err != nil && strings.TrimSpace(line) == "" {
			if errors.Is(err, io.EOF) {
				return "", fmt.Errorf("no callback pasted (stdin closed)")
			}
			return "", err
		}
		return strings.TrimSpace(line), nil
	}
}

// serverEntry loads one server entry from the registry.
func (a *App) serverEntry(id string) (registry.ServerEntry, []string, error) {
	store, warnings, err := a.openStore()
	if err != nil {
		return registry.ServerEntry{}, warnings, err
	}
	entry, err := requireServer(store.Snapshot(), id)
	if err != nil {
		return registry.ServerEntry{}, warnings, err
	}
	return entry, warnings, nil
}

// serverIsLocal reports whether the registry marks serverID as a local
// endpoint. A missing registry or entry answers false — the strict default.
func (a *App) serverIsLocal(id string) bool {
	entry, _, err := a.serverEntry(id)
	return err == nil && entry.Provenance == registry.ProvenanceLocal
}

// isSuperseded reports the "another writer already refreshed" outcome,
// which is a success with a different provenance, not a failure.
func isSuperseded(err error) bool { return errors.Is(err, oauthflow.ErrRefreshSuperseded) }

// authError maps an oauthflow failure onto the frozen exit-code table:
// everything the authorization server (or the user) rejected is exit 5,
// with the structured suggestion carried into the hint.
func authError(err error) error {
	var fe *oauthflow.FlowError
	if !errors.As(err, &fe) {
		return err
	}
	code, exit := CodeAuthFailed, ExitAuth
	switch fe.Type {
	case oauthflow.ErrorTypeBlocked, oauthflow.ErrorTypeTransport:
		code, exit = CodeGeneral, ExitGeneral
	case oauthflow.ErrorTypePersistence:
		code, exit = CodeGeneral, ExitGeneral
	}
	hint := fe.Suggestion
	if hint == "" && fe.Discovery == oauthflow.DiscoveryFailed {
		hint = "pass --issuer if the server publishes no protected-resource metadata"
	}
	// Message names what failed; Err carries the chain. Error() joins them
	// with ": ", so putting err.Error() in BOTH printed an OAuth failure —
	// discovery URL, status, provider body — twice inside one line.
	return &Error{Code: code, ExitCode: exit, Message: "authorization failed", Hint: hint, Err: err}
}

// secondsUntil returns the seconds left until a Unix expiry, clamped at 0.
// An expiry of 0 means "never expires" (docs/modules/oauth.md) and yields 0 too —
// the caller distinguishes the two by ExpiresAt.
func secondsUntil(expiresAt int64, now time.Time) int64 {
	if expiresAt == 0 {
		return 0
	}
	d := expiresAt - now.Unix()
	if d < 0 {
		return 0
	}
	return d
}

func expiryPhrase(expiresAt, expiresIn int64) string {
	if expiresAt == 0 {
		return " (no expiry advertised)"
	}
	if expiresIn == 0 {
		return " (expired)"
	}
	return fmt.Sprintf(" (expires in %s)", time.Duration(expiresIn)*time.Second)
}

func expiryColumn(expiresAt, expiresIn int64) string {
	switch {
	case expiresAt == 0:
		return "never"
	case expiresIn == 0:
		return "expired"
	default:
		return (time.Duration(expiresIn) * time.Second).String()
	}
}
