package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"slices"
	"strings"
	"text/tabwriter"

	"github.com/spf13/cobra"

	"github.com/dinstein/agent-hub/internal/approval"
	"github.com/dinstein/agent-hub/internal/confops"
	"github.com/dinstein/agent-hub/internal/gateway"
	"github.com/dinstein/agent-hub/internal/integrity"
	"github.com/dinstein/agent-hub/internal/registry"
)

// sourceManual marks entries created interactively via the CLI (flags or
// pasted stdin JSON), as opposed to a catalog entry's "catalog:<id>".
const sourceManual = "manual"

// ServerRow is the per-server data structure both output modes render from.
//
// Headers are rendered with their VALUES INTACT only because a registry
// entry never holds a credential: values are ${SECRET_X} placeholders that
// name a vault entry (docs/modules/controlplane.md rule 5 — no CLI surface ever echoes a
// secret; resolution happens at connect time inside internal/downstream).
type ServerRow struct {
	ID        string            `json:"id"`
	Transport string            `json:"transport"`
	Command   string            `json:"command,omitempty"`
	Args      []string          `json:"args,omitempty"`
	Env       map[string]string `json:"env,omitempty"`
	Cwd       string            `json:"cwd,omitempty"`
	URL       string            `json:"url,omitempty"`
	Headers   map[string]string `json:"headers,omitempty"`
	// OAuth mirrors the entry's login hints. NeedsAuth is deliberately
	// absent: it is runtime state (a live 401), never configuration.
	OAuth      *registry.OAuthHint `json:"oauth,omitempty"`
	Provenance string              `json:"provenance,omitempty"`
	// Runtime is omitted for the host default so `--json` output of a
	// pre-M2 entry is byte-identical to what it always was.
	Runtime string                  `json:"runtime,omitempty"`
	Docker  *registry.DockerRuntime `json:"docker,omitempty"`
	Enabled bool                    `json:"enabled"`
	Source  string                  `json:"source,omitempty"`
	// Trace mirrors the entry's frame-log switch, omitted when off so the
	// --json output of an untraced entry is byte-identical to what it always
	// was.
	Trace bool `json:"trace,omitempty"`
}

// target is the connection target column: the command line for stdio, the
// endpoint URL for the HTTP transports. A containerized entry names its
// image first — "where does this run" is the question the column answers,
// and for a docker-runtime server the command alone answers it wrongly.
func (r ServerRow) target() string {
	if r.URL != "" {
		return r.URL
	}
	line := strings.TrimSpace(strings.Join(append([]string{r.Command}, r.Args...), " "))
	if r.Runtime == registry.RuntimeDocker && r.Docker != nil {
		return strings.TrimSpace("docker[" + r.Docker.Image + "] " + line)
	}
	return line
}

// ServerList is the `server ls` result. JSON shape: a plain array.
type ServerList []ServerRow

// Human renders the list as a table.
func (l ServerList) Human(w io.Writer) error {
	if len(l) == 0 {
		_, err := fmt.Fprintln(w, "no servers configured")
		return err
	}
	// Writes to a tabwriter only fail at Flush, which is where the error is
	// surfaced.
	// The TRACE column appears only when something is being traced. A trace
	// is a temporary debugging state that writes raw payloads to disk, so it
	// has to be visible in the listing an operator already reads — but a
	// column that says "off" on every row for the rest of time teaches
	// readers to stop seeing it, which is the opposite of the point.
	traced := false
	for _, r := range l {
		if r.Trace {
			traced = true
			break
		}
	}
	tw := tabwriter.NewWriter(w, 0, 4, 2, ' ', 0)
	head := "ID\tTRANSPORT\tENABLED\tSOURCE\tTARGET"
	if traced {
		head = "ID\tTRANSPORT\tENABLED\tTRACE\tSOURCE\tTARGET"
	}
	_, _ = fmt.Fprintln(tw, head)
	for _, r := range l {
		if traced {
			_, _ = fmt.Fprintf(tw, "%s\t%s\t%t\t%s\t%s\t%s\n",
				r.ID, r.Transport, r.Enabled, traceMark(r.Trace), r.Source, r.target())
			continue
		}
		_, _ = fmt.Fprintf(tw, "%s\t%s\t%t\t%s\t%s\n", r.ID, r.Transport, r.Enabled, r.Source, r.target())
	}
	return tw.Flush()
}

// traceMark renders the TRACE cell. "on" is spelled out and off is a dash,
// so the eye lands on the rows that are recording rather than on the rest.
func traceMark(on bool) string {
	if on {
		return "on"
	}
	return "-"
}

// AddedServers is the `server add` result.
type AddedServers struct {
	Added []ServerRow `json:"added"`
}

// Human renders one "added:" line per entry (docs/modules/controlplane.md example),
// then names the step that puts the server into service. An entry nobody is
// told to enable is indistinguishable from one that was never added.
func (a AddedServers) Human(w io.Writer) error {
	for _, r := range a.Added {
		if _, err := fmt.Fprintf(w, "added: %s (%s, source=%s, disabled)\n", r.ID, r.Transport, r.Source); err != nil {
			return err
		}
	}
	for _, r := range a.Added {
		if _, err := fmt.Fprintf(w, "next: agenthub server enable %s\n", r.ID); err != nil {
			return err
		}
	}
	return nil
}

// RemovedServer is the `server rm` result.
type RemovedServer struct {
	Removed string `json:"removed"`
}

// Human renders the removal confirmation.
func (r RemovedServer) Human(w io.Writer) error {
	_, err := fmt.Fprintf(w, "removed: %s\n", r.Removed)
	return err
}

func (a *App) newServerCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:     "server",
		Aliases: []string{"servers"}, // singular canonical, plural alias (canonical.md §3)
		Short:   "Add, switch on and inspect the MCP servers agenthub connects to",
		Long: "Servers are registered once here, for every client at the same time. 'add'\n" +
			"writes the definition and leaves it off; 'enable' connects and puts it into\n" +
			"service.\n\n" +
			"Enabled servers are the most any client can ever see, so 'disable' takes one\n" +
			"away from everybody at once and no profile can put it back.",
		Args: cobra.ArbitraryArgs,
		RunE: groupRunE,
	}
	cmd.AddCommand(
		a.newServerLsCmd(), a.newServerInspectCmd(), a.newServerAddCmd(), a.newServerRmCmd(),
		a.newServerToggleCmd(true), a.newServerToggleCmd(false),
		a.newServerTestCmd(), a.newServerTraceCmd(), a.newServerLogsCmd(),
	)
	return cmd
}

func (a *App) newServerAddCmd() *cobra.Command {
	var (
		cmdPath      string
		argv         []string
		envKV        []string
		cwd          string
		url          string
		transportOpt string
		headerKV     []string
		local        bool
		fromStdin    bool
		docker       dockerFlags
		oauth        oauthFlags
	)
	// `add` writes the definition and nothing else: no connection, no probe,
	// no enable. The entry lands DISABLED and `server enable` is the second,
	// separate step (which is where the probe lives).
	//
	// Two operations rather than one because they answer different
	// questions. `add` records what a server IS — pure configuration, no
	// network, deterministic, safe to script against a downstream that is
	// currently unreachable. `enable` declares the operator wants to USE it,
	// which is the only point at which "can we actually reach it?" is worth
	// asking. Folding the two together made `enabled` mean both "the user
	// wants this" and "it answered a probe", and a downstream that was
	// merely deploying at add time then looked like a server that had never
	// been added.
	//
	// Callers that want the one-shot experience compose the two: the GUI
	// does exactly that, and `catalog add` names the enable in its
	// NextSteps. Composition belongs to the caller; the primitives stay
	// separate.
	cmd := &cobra.Command{
		Use:   "add [<name>]",
		Short: "Add an MCP server, switched off, from flags or from JSON you paste in",
		Long: "Use --cmd for a server agenthub launches locally, --url for one behind an HTTP\n" +
			"endpoint, or --stdin to pipe in the JSON block a README gives you. (--runtime\n" +
			"docker fails outright rather than falling back to your machine.)\n\n" +
			"This only writes the definition down: nothing connects, and the server stays\n" +
			"off until 'agenthub server enable <id>' probes it and puts it into service.\n\n" +
			"Never put a real token in --env or --header: write ${SECRET_NAME} and the\n" +
			"value is looked up from agenthub's credential store at startup.",
		Args: rangeArgs(0, 1),
		RunE: func(cmd *cobra.Command, args []string) error {
			name := ""
			if len(args) == 1 {
				name = args[0]
			}

			var entries []namedEntry
			if fromStdin {
				if cmdPath != "" || len(argv) > 0 || len(envKV) > 0 || cwd != "" ||
					url != "" || transportOpt != "" || len(headerKV) > 0 || local ||
					docker.any() || oauth.any() {
					e := Usagef("--stdin cannot be combined with the definition flags")
					e.Hint = helpHint(cmd)
					return e
				}
				data, err := io.ReadAll(a.stdin)
				if err != nil {
					return fmt.Errorf("read stdin: %w", err)
				}
				entries, err = normalizeStdin(data, name)
				if err != nil {
					return err
				}
			} else {
				if name == "" {
					e := Usagef("a server name argument is required (or use --stdin)")
					e.Hint = helpHint(cmd)
					return e
				}
				entry, err := entryFromFlags(cmd, addFlags{
					command:   cmdPath,
					args:      argv,
					env:       envKV,
					cwd:       cwd,
					url:       url,
					transport: transportOpt,
					headers:   headerKV,
					local:     local,
					docker:    docker,
					oauth:     oauth,
				})
				if err != nil {
					return err
				}
				entries = []namedEntry{{Name: name, Entry: entry}}
			}

			// Validate BEFORE the store is opened: a rejected entry must not
			// leave a half-written registry behind, and the operator can
			// still fix what they typed. confops re-checks under the lock —
			// this call only moves the refusal earlier.
			specs := make([]confops.ServerSpec, 0, len(entries))
			for _, ne := range entries {
				spec := confops.ServerSpec{ID: ne.Name, Entry: ne.Entry}
				if err := confops.ValidateServerSpec(spec); err != nil {
					return opsError(err)
				}
				specs = append(specs, spec)
			}

			store, warnings, err := a.opsStore()
			if err != nil {
				return err
			}
			res, err := confops.AddServers(cmd.Context(), store, specs, noPrecondition)
			warnings = append(warnings, res.Warnings...)
			if err != nil {
				return opsError(err)
			}

			result := AddedServers{Added: make([]ServerRow, 0, len(res.Servers))}
			for _, spec := range res.Servers {
				result.Added = append(result.Added, rowFor(spec.ID, spec.Entry))
			}
			return a.printer().Emit(result, warnings...)
		},
	}
	cmd.Flags().StringVar(&cmdPath, "cmd", "", "program agenthub runs to start the server, e.g. npx")
	cmd.Flags().StringSliceVar(&argv, "args", nil, "arguments for that program, comma-separated")
	cmd.Flags().StringArrayVar(&envKV, "env", nil, "environment variable KEY=VALUE (repeatable); write ${SECRET_X} instead of a real secret")
	cmd.Flags().StringVar(&cwd, "cwd", "",
		"directory the server is started in; for a container that is a path inside it, same as --container-workdir")
	cmd.Flags().StringVar(&url, "url", "", "address of a server that already runs behind HTTP")
	cmd.Flags().StringVar(&transportOpt, "transport", "", "how to reach it: stdio (assumed with --cmd), http or sse (assumed with --url)")
	cmd.Flags().StringArrayVar(&headerKV, "header", nil, "HTTP header KEY=VALUE (repeatable); write ${SECRET_X} instead of a real secret")
	cmd.Flags().BoolVar(&local, "local", false,
		"allow a --url pointing at this machine (localhost); other private addresses stay refused")
	cmd.Flags().BoolVar(&fromStdin, "stdin", false,
		`read the server's JSON from standard input ({"mcpServers":{...}} or one bare entry)`)
	cmd.Flags().StringVar(&docker.runtime, "runtime", "",
		"run the server on this machine (host, the default) or inside a container (docker)")
	cmd.Flags().StringVar(&docker.image, "image", "",
		"container image to run; setting this turns on --runtime docker")
	cmd.Flags().StringArrayVar(&docker.mounts, "mount", nil,
		"directory of yours the container may see: SRC[:DST][:ro|rw] (repeatable; read-only unless you say rw)")
	cmd.Flags().StringVar(&docker.network, "network", "",
		"container network; by default the container gets no network at all")
	cmd.Flags().StringVar(&docker.memory, "memory", "", "how much memory the container may use, e.g. 512m")
	cmd.Flags().StringVar(&docker.cpus, "cpus", "", "how many CPUs the container may use, e.g. 1.5")
	cmd.Flags().StringVar(&docker.user, "container-user", "", "user the container runs as")
	cmd.Flags().StringVar(&docker.workdir, "container-workdir", "",
		"directory the container starts in; wins over --cwd when both are given")
	cmd.Flags().StringArrayVar(&docker.extra, "docker-arg", nil,
		"extra argument for 'docker run' (repeatable); it may not undo the container's restrictions")
	cmd.Flags().StringVar(&oauth.issuer, "oauth-issuer", "",
		"tell agenthub which login service to use, instead of asking the server")
	cmd.Flags().StringSliceVar(&oauth.scopes, "oauth-scope", nil,
		"permission to ask for at login (repeatable or comma-separated); sent exactly as written")
	cmd.Flags().StringVar(&oauth.resourceMetadata, "oauth-resource-metadata", "",
		"address of the server's login-details document, if it is not where agenthub would look")
	return cmd
}

// oauthFlags is the login-hint half of `server add`. The three fields are
// the whole of registry.OAuthHint: what a later `agenthub auth login` should
// discover against. They are transport-independent (an stdio child may proxy
// to a remote authorization server), so they are not part of either
// transport's flag group.
//
// needsAuth is deliberately absent and stays absent: whether a server
// currently requires authorization is runtime state, discovered by a live
// 401, never configuration.
type oauthFlags struct {
	issuer           string
	scopes           []string
	resourceMetadata string
}

func (f oauthFlags) any() bool {
	return f.issuer != "" || len(f.scopes) > 0 || f.resourceMetadata != ""
}

// hint builds the registry hint, or nil when no flag was given. nil rather
// than an empty struct: an entry that was never given hints must not grow an
// empty "oauth": {} block on disk, because that is a difference a diff shows
// and a reader has to explain.
func (f oauthFlags) hint() *registry.OAuthHint {
	if !f.any() {
		return nil
	}
	return &registry.OAuthHint{
		Issuer:              strings.TrimSpace(f.issuer),
		Scopes:              f.scopes,
		ResourceMetadataURL: strings.TrimSpace(f.resourceMetadata),
	}
}

// addFlags is the flag half of `server add`, grouped so the (already long)
// RunE keeps one statement per concern.
type addFlags struct {
	command   string
	args      []string
	env       []string
	cwd       string
	url       string
	transport string
	headers   []string
	local     bool
	docker    dockerFlags
	oauth     oauthFlags
}

// entryFromFlags builds one registry entry from the definition flags,
// inferring the transport from which of --cmd / --url is present and
// rejecting every combination that would silently ignore a flag.
func entryFromFlags(cmd *cobra.Command, f addFlags) (registry.ServerEntry, error) {
	var zero registry.ServerEntry
	usage := func(format string, a ...any) error {
		e := Usagef(format, a...)
		e.Hint = helpHint(cmd)
		return e
	}

	kind := f.transport
	switch {
	case kind == "":
		// Inference, not magic: exactly one of the two target flags decides.
		switch {
		case f.url != "" && f.command != "":
			return zero, usage("--cmd and --url are mutually exclusive; pick one transport")
		case f.url != "":
			kind = registry.TransportHTTP
		case f.command != "":
			kind = registry.TransportStdio
		default:
			return zero, usage("--cmd or --url is required (or use --stdin)")
		}
	case kind != registry.TransportStdio && kind != registry.TransportHTTP && kind != registry.TransportSSE:
		return zero, &Error{
			Code: CodeUnsupportedTransport, ExitCode: ExitUsage,
			Message: fmt.Sprintf("unknown transport %q", kind),
			Hint:    "supported transports: stdio, http (streamable-http), sse (legacy HTTP+SSE)",
		}
	}

	// Enabled is false: `add` writes configuration and touches nothing else
	// (see the newServerAddCmd header). `server enable` is the step that
	// probes and puts the server into service.
	entry := registry.ServerEntry{Transport: kind, Enabled: false, Source: sourceManual}
	// Login hints are transport-independent — attached before the transport
	// split so an stdio entry that proxies to a remote authorization server
	// keeps them too, exactly like the pasted-JSON path does.
	entry.OAuth = f.oauth.hint()
	if kind == registry.TransportStdio {
		if f.command == "" {
			return zero, usage("the stdio transport needs --cmd")
		}
		if f.url != "" || len(f.headers) > 0 || f.local {
			return zero, usage("--url/--header/--local apply to the http and sse transports only")
		}
		env, err := parseKVFlags("--env", f.env)
		if err != nil {
			return zero, err
		}
		entry.Command = f.command
		entry.Args = f.args
		entry.Env = env
		entry.Cwd = f.cwd
		if err := applyDockerFlags(&entry, f.docker, usage); err != nil {
			return zero, err
		}
		return entry, nil
	}
	if f.docker.any() {
		return zero, usage("--runtime/--image and the container flags apply to the stdio transport only")
	}

	if f.url == "" {
		return zero, usage("the %s transport needs --url", kind)
	}
	if f.command != "" || len(f.args) > 0 || len(f.env) > 0 || f.cwd != "" {
		return zero, usage("--cmd/--args/--env/--cwd apply to the stdio transport only")
	}
	headers, err := parseKVFlags("--header", f.headers)
	if err != nil {
		return zero, err
	}
	if err := validateEndpoint(f.url, f.local); err != nil {
		return zero, err
	}
	entry.URL = f.url
	entry.Headers = headers
	if f.local {
		entry.Provenance = registry.ProvenanceLocal
	}
	return entry, nil
}

// validateEndpoint screens an endpoint at the moment the operator can still
// fix it. The predicate and its narrow --local exception live in confops so
// `server add --url`, pasted stdin JSON and the control plane all refuse the
// same set of addresses.
func validateEndpoint(raw string, local bool) error {
	return opsError(confops.ValidateEndpoint(raw, local))
}

func (a *App) newServerLsCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "ls",
		Short: "List every server you have added, and whether it is switched on",
		Long: "This is what is registered, not what is working: a server listed here can\n" +
			"still fail to start. 'agenthub server test <id>' answers that.",
		Args: noArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			store, warnings, err := a.openStore()
			if err != nil {
				return err
			}
			snap := store.Snapshot()
			rows := make(ServerList, 0, len(snap.Servers.V.Servers))
			for name, doc := range snap.Servers.V.Servers {
				rows = append(rows, rowFor(name, doc.V))
			}
			slices.SortFunc(rows, func(x, y ServerRow) int { return strings.Compare(x.ID, y.ID) })
			return a.printer().Emit(rows, warnings...)
		},
	}
}

func (a *App) newServerRmCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "rm <id>",
		Short: "Delete a server and everything stored for it",
		Long: "Permanent, and it takes the whole footprint with it: credentials,\n" +
			"profile membership, governance rules naming it, integrity baselines,\n" +
			"approval grants and the cached tool list. Audit logs are kept.\n\n" +
			"To stop a server being used without losing any of that, use\n" +
			"'agenthub server disable' instead.",
		Args: exactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			id := args[0]
			store, warnings, err := a.opsStore()
			if err != nil {
				return err
			}
			deps, derr := a.newOAuthDeps(false)
			if derr != nil {
				return derr
			}
			opts := confops.RemoveOptions{Credentials: deps.chain}
			// State cleanups are best-effort by construction: each missing
			// store simply contributes nothing. A state dir that will not
			// resolve is reported once, as a warning, rather than blocking a
			// delete the registry has already accepted.
			stateDir, serr := a.stateDir()
			if serr != nil {
				warnings = append(warnings, fmt.Sprintf(
					"could not resolve the state directory (%v); integrity baselines and "+
						"approval grants of %q may survive", serr, id))
			} else {
				opts.State = a.serverStateForgetters(stateDir)
			}
			res, err := confops.RemoveServer(cmd.Context(), store, id, noPrecondition, opts)
			warnings = append(warnings, res.Warnings...)
			if err != nil {
				return opsError(err)
			}
			return a.printer().Emit(RemovedServer{Removed: id}, warnings...)
		},
	}
}

// serverStateForgetters builds the out-of-registry cleanups `server rm`
// runs. A store that cannot even be OPENED is skipped rather than failing the
// command: the registry entry is already gone by the time these run, and
// RemoveServer reports whatever fails as a warning naming what survived.
func (a *App) serverStateForgetters(stateDir string) []confops.StateForgetter {
	opts := integrity.Options{LockTimeout: a.lockTimeout}
	var out []confops.StateForgetter
	if pins, err := integrity.OpenPinStore(stateDir, opts); err == nil {
		out = append(out, pins)
	}
	if ap, err := integrity.OpenApprovalStore(stateDir, opts); err == nil {
		out = append(out, ap)
	}
	if q, err := integrity.OpenQuarantineStore(stateDir, opts); err == nil {
		out = append(out, q)
	}
	if al, err := approval.OpenAllowlist(stateDir); err == nil {
		out = append(out, al)
	}
	out = append(out,
		confops.StateFunc{
			Name: "tool overrides",
			Forget: func(_ context.Context, id string) error {
				return confops.ForgetServerOverrides(stateDir, id)
			},
		},
		confops.StateFunc{
			Name: "the cached tool list",
			Forget: func(_ context.Context, id string) error {
				return gateway.ForgetToolCache(a.resolver, id)
			},
		},
	)
	return out
}

func rowFor(name string, e registry.ServerEntry) ServerRow {
	return ServerRow{
		ID:         name,
		Transport:  e.TransportName(),
		Command:    e.Command,
		Args:       e.Args,
		Env:        e.Env,
		Cwd:        e.Cwd,
		URL:        e.URL,
		Headers:    e.Headers,
		OAuth:      e.OAuth,
		Provenance: e.Provenance,
		Runtime:    e.Runtime,
		Docker:     e.Docker,
		Enabled:    e.Enabled,
		Source:     e.Source,
		Trace:      e.Trace,
	}
}

// parseKVFlags parses repeated KEY=VALUE flags (--env, --header). flag is
// the flag name, used verbatim in the error so the message names what the
// operator actually typed.
func parseKVFlags(flag string, kvs []string) (map[string]string, error) {
	if len(kvs) == 0 {
		return nil, nil
	}
	out := make(map[string]string, len(kvs))
	for _, kv := range kvs {
		k, v, ok := strings.Cut(kv, "=")
		if !ok || strings.TrimSpace(k) == "" {
			return nil, Usagef("%s expects KEY=VALUE, got %q", flag, kv)
		}
		out[k] = v
	}
	return out, nil
}

// namedEntry pairs a server name with its normalized registry entry.
type namedEntry struct {
	Name  string
	Entry registry.ServerEntry
}

// stdinEntry is the shape accepted per server in pasted JSON. It matches the
// de-facto client convention ("command"/"args"/"env", optional "type") plus
// our own field names ("transport", "cwd", "headers").
type stdinEntry struct {
	Type      string            `json:"type,omitempty"`
	Transport string            `json:"transport,omitempty"`
	Command   string            `json:"command"`
	Args      []string          `json:"args,omitempty"`
	Env       map[string]string `json:"env,omitempty"`
	Cwd       string            `json:"cwd,omitempty"`
	URL       string            `json:"url,omitempty"`
	Headers   map[string]string `json:"headers,omitempty"`
	// OAuth carries the login hints (issuer / scopes / resource metadata
	// pointer) for a server whose authorization server cannot be found by
	// RFC 9728 discovery. NeedsAuth is deliberately not accepted: it is
	// runtime state (a live 401), never configuration.
	OAuth *registry.OAuthHint `json:"oauth,omitempty"`
}

// normalizeStdin turns pasted JSON into registry entries. Accepted shapes,
// in order of detection (docs/modules/controlplane.md: "paste a README's mcpServers
// fragment" is the highest-frequency action):
//
//  1. {"mcpServers": {"name": {...}, ...}}   client-config wrapper
//  2. {"command": ..., ...}                  single entry (name argument required)
//  3. {"name": {...}, ...}                   bare name->entry map
//
// A name argument may rename the entry only when exactly one entry is
// present. Only stdio entries are accepted in M0. Results are sorted by name
// for deterministic output.
func normalizeStdin(data []byte, nameArg string) ([]namedEntry, error) {
	invalid := func(format string, a ...any) *Error {
		return &Error{Code: CodeInvalidJSON, ExitCode: ExitGeneral, Message: fmt.Sprintf(format, a...)}
	}

	if len(strings.TrimSpace(string(data))) == 0 {
		return nil, invalid("stdin is empty; expected server JSON")
	}
	var top map[string]json.RawMessage
	if err := json.Unmarshal(data, &top); err != nil {
		return nil, invalid("stdin is not a JSON object: %v", err)
	}

	rawEntries := map[string]json.RawMessage{}
	switch {
	case top["mcpServers"] != nil:
		if err := json.Unmarshal(top["mcpServers"], &rawEntries); err != nil {
			return nil, invalid(`"mcpServers" must be an object of name -> entry: %v`, err)
		}
	case isStringField(top, "command") || isStringField(top, "url"):
		// Single bare entry.
		if nameArg == "" {
			return nil, Usagef("a server name argument is required when stdin holds a single entry")
		}
		rawEntries[nameArg] = json.RawMessage(data)
	default:
		rawEntries = top
	}
	if len(rawEntries) == 0 {
		return nil, invalid("no server entries found in stdin JSON")
	}
	if nameArg != "" {
		if len(rawEntries) > 1 {
			return nil, Usagef("name argument %q conflicts with %d entries in stdin JSON", nameArg, len(rawEntries))
		}
		// Exactly one entry: the name argument renames it.
		for _, v := range rawEntries {
			rawEntries = map[string]json.RawMessage{nameArg: v}
			break
		}
	}

	entries := make([]namedEntry, 0, len(rawEntries))
	for name, raw := range rawEntries {
		if strings.TrimSpace(name) == "" {
			return nil, invalid("server entries must have a non-empty name")
		}
		// DisallowUnknownFields: a key we do not model must be reported, not
		// dropped. Silently discarding one produces the worst failure shape
		// there is — `server add` reports success while the setting the user
		// pasted (an "oauth" block, say) is simply absent, and the mismatch
		// only surfaces much later as an unrelated-looking auth failure.
		dec := json.NewDecoder(bytes.NewReader(raw))
		dec.DisallowUnknownFields()
		var spec stdinEntry
		if err := dec.Decode(&spec); err != nil {
			return nil, invalid("server %q: %v", name, err)
		}
		// "type" is the de-facto client-config spelling, "transport" ours;
		// either may name the transport, and a bare "url" implies http
		// (which is what every published remote-server snippet looks like).
		kind := spec.Type
		if kind == "" {
			kind = spec.Transport
		}
		if kind == "" {
			if spec.URL != "" {
				kind = registry.TransportHTTP
			} else {
				kind = registry.TransportStdio
			}
		}
		// Client configs spell streamable-http several ways; normalize the
		// ones that unambiguously mean the same transport.
		switch kind {
		case "streamable-http", "streamableHttp", "http-stream":
			kind = registry.TransportHTTP
		}
		// Enabled is false for the same reason as the flags path: `add` is
		// configuration only, `enable` puts the server into service.
		entry := registry.ServerEntry{Transport: kind, Enabled: false, Source: sourceManual}
		// Login hints are transport-independent: carried over before the
		// transport switch so an stdio entry that proxies to a remote AS
		// keeps them too.
		entry.OAuth = spec.OAuth
		switch kind {
		case registry.TransportStdio:
			if spec.Command == "" {
				return nil, invalid("server %q: %q is required for the stdio transport", name, "command")
			}
			entry.Command = spec.Command
			entry.Args = spec.Args
			entry.Env = spec.Env
			entry.Cwd = spec.Cwd
		case registry.TransportHTTP, registry.TransportSSE:
			if spec.URL == "" {
				return nil, invalid("server %q: %q is required for the %s transport", name, "url", kind)
			}
			// Pasted JSON has no --local escape hatch on purpose: a snippet
			// copied from a README must not be able to point the connector
			// at a private address (fail-closed; `server add --url --local`
			// is the explicit, typed-by-a-human path).
			if err := validateEndpoint(spec.URL, false); err != nil {
				return nil, err
			}
			entry.URL = spec.URL
			entry.Headers = spec.Headers
		default:
			return nil, &Error{
				Code: CodeUnsupportedTransport, ExitCode: ExitGeneral,
				Message: fmt.Sprintf("server %q: unknown transport %q", name, kind),
				Hint:    "supported transports: stdio, http (streamable-http), sse (legacy HTTP+SSE)",
			}
		}
		entries = append(entries, namedEntry{Name: name, Entry: entry})
	}
	slices.SortFunc(entries, func(x, y namedEntry) int { return strings.Compare(x.Name, y.Name) })
	return entries, nil
}

// isStringField reports whether m[key] exists and is a JSON string — used to
// tell a single bare entry ({"command":"npx"}) apart from a name->entry map
// (where every value is an object).
func isStringField(m map[string]json.RawMessage, key string) bool {
	raw, ok := m[key]
	if !ok {
		return false
	}
	var s string
	return json.Unmarshal(raw, &s) == nil
}
